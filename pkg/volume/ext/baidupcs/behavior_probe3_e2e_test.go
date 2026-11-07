// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package baidupcs

// behavior_probe3_e2e_test.go —— 【依赖链探针 E3】复现「迁移为什么拿到 31066」的最小场景：
// 先改名**父目录**，随后**立刻**对**子目录**再发起 move（迁移正是这个依赖链）。
// 单次 move（P2/P4/P5）与连续 50 次 move（突发探针）均已实测成功 ⇒ 唯一剩下的可测假设是
// 「对**刚随父移动过的子路径**再发 move」会被服务端以 31066 拒绝（索引未随父移动更新），
// 而 Stat/ls 可能已经能看到（P3 已证）。
//
// 运行（WSL）：
//
//	cd pkg/volume/ext/baidupcs && GOTOOLCHAIN=local GOWORK=off go test -tags=e2e \
//	  -run TestE2E_ParentChildMoveProbe -count=1 -v -timeout 30m
import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"testing"
	"time"
)

// TestE2E_ParentChildMoveProbe 逐轮执行「改父 → 立刻改子」，记录每一步的真实结果。
func TestE2E_ParentChildMoveProbe(t *testing.T) {
	// sproxy:serial: 真实网盘 e2e 探针（共享远端资源 + 限流），必须串行执行
	env := newProbeEnv(t)
	ctx := context.Background()
	const rounds = 5

	// 夹具：rounds 个 <root>/e3/<i>/X/child/f.bin（CLI 铺设，避免 Storage.Put 的等待）。
	env.mkdirRemote(ctx, env.root)
	for i := 0; i < rounds; i++ {
		base := env.remoteFor(fmt.Sprintf("e3/%02d", i))
		env.mkdirRemote(ctx, path.Join(base, "X", "child"))
		env.fixture(ctx, path.Join(base, "X", "child"), "f.bin", bytes.Repeat([]byte("e3"), 8))
	}
	t.Logf("[E3] 夹具就绪：%d 轮", rounds)

	parentFail, childFail, childNotFound := 0, 0, 0
	var childDetail []string
	for i := 0; i < rounds; i++ {
		base := env.remoteFor(fmt.Sprintf("e3/%02d", i))
		// ① 改父：X → Y
		st := time.Now()
		pErr := env.lib.Move(ctx, path.Join(base, "X"), path.Join(base, "Y"))
		t.Logf("[E3] 轮 %d ① 改父 X→Y err=%v %.2fs", i, pErr, time.Since(st).Seconds())
		if pErr != nil {
			parentFail++
			t.Logf("[E3]    父改名失败 notFound=%v err=%v", errors.Is(pErr, ErrNotFound), pErr)
			continue
		}
		// ② **立刻**改子：<刚改的父>/child → child2
		childSrc := path.Join(base, "Y", "child")
		childDst := path.Join(base, "Y", "child2")
		st2 := time.Now()
		cErr := env.lib.Move(ctx, childSrc, childDst)
		nf := errors.Is(cErr, ErrNotFound)
		t.Logf("[E3] 轮 %d ② 立刻改子 Y/child→Y/child2 err=%v %.2fs notFound=%v",
			i, cErr, time.Since(st2).Seconds(), nf)
		if cErr != nil {
			childFail++
			if nf {
				childNotFound++
			}
			childDetail = append(childDetail, fmt.Sprintf("i=%d notFound=%v err=%v", i, nf, cErr))
		}
		// ③ 立即重试一次（区分瞬时/稳定）
		if cErr != nil {
			if err := env.lib.Move(ctx, childSrc, childDst); err == nil {
				t.Logf("[E3]    子改名重试成功（瞬时）")
			} else {
				t.Logf("[E3]    子改名重试仍失败：%v", err)
			}
		}
		// ④ 改子后：**分别**测量 Meta 判据与「父目录列举」判据的可见时间（各 30s 上限）。
		target := path.Join(base, "Y", "child2", "f.bin")
		if n, d, err := pollStat(ctx, env.st, target, time.Second, 30*time.Second); err == nil {
			t.Logf("[E3]    ④ Meta 可见 attempts=%d %.2fs", n, d.Seconds())
		} else {
			t.Logf("[E3]    ④ Meta 30s 内不可见 attempts=%d err=%v", n, err)
		}
		if n, d, ok := pollList(ctx, env.st, target, time.Second, 30*time.Second); ok {
			t.Logf("[E3]    ④ ls 可见 attempts=%d %.2fs", n, d.Seconds())
		} else {
			t.Logf("[E3]    ④ ls 30s 内不可见 attempts=%d", n)
		}
		// ⑤ **再往后一步**：对刚改过的子目录**内部**再发 move（迁移的下一步动作）是否 31066？
		innerSrc := path.Join(base, "Y", "child2")
		innerDst := path.Join(base, "Y", "child3")
		st3 := time.Now()
		iErr := env.lib.Move(ctx, innerSrc, innerDst)
		t.Logf("[E3]    ⑤ 连续对刚改过的子树再 move err=%v %.2fs notFound=%v",
			iErr, time.Since(st3).Seconds(), errors.Is(iErr, ErrNotFound))
	}

	t.Logf("[E3] 汇总：父改名失败=%d/%d；子改名失败=%d/%d（其中 ErrNotFound=%d）",
		parentFail, rounds, childFail, rounds, childNotFound)
	for _, d := range childDetail {
		t.Logf("[E3]   子失败明细：%s", d)
	}
	verdict := "改父→立刻改子 全部成功 ⇒ 依赖链不是 31066 的原因"
	if childFail > 0 {
		verdict = "改父→立刻改子 **出现失败** ⇒ 依赖链即 31066 的复现条件（需按此设计规避）"
	}
	t.Logf("[E3] 结论：%s", verdict)
}
