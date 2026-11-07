// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package baidupcs

// behavior_probe2_e2e_test.go —— 【突发探针】回答「迁移为什么会拿到 31066」：
// 单次 move 已实测成功（见 behavior_probe_e2e_test.go 的 P2/P4/P5：0.44s / 1.01s / 0.86s），
// 因此需要验证**连续大量 move** 是否在某个点开始被服务端以 31066 拒绝（限流伪装成"文件不存在"），
// 还是每次都稳定成功。
//
// 方法：预建 N 个独立目录（各含 1 个文件，CLI 上传），然后**连续**做 N 次**裸 move**
// （`libraryAdapter.Move`，不经我们的 post-Stat），逐条记录：序号 / 结果 / 耗时 / 错误原文 /
// 是否 ErrNotFound；再对失败项**立即重试 1 次**，看是否为瞬时（限流）还是稳定失败。
//
// 运行（WSL）：
//
//	cd pkg/volume/ext/baidupcs && GOTOOLCHAIN=local GOWORK=off go test -tags=e2e \
//	  -run TestE2E_BurstMoveProbe -count=1 -v -timeout 40m
import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"testing"
	"time"
)

// burstCount 是突发 move 次数（可通过环境变量覆盖，便于按需放大/缩小）。
func burstCount() int {
	if v := os.Getenv("BURST_N"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 && n <= 500 {
			return n
		}
	}
	return 50
}

// TestE2E_BurstMoveProbe 连续 N 次裸 move，逐条记录真实结果。
func TestE2E_BurstMoveProbe(t *testing.T) {
	env := newProbeEnv(t)
	ctx := context.Background()
	n := burstCount()

	// 1) 铺夹具：N 个目录，各 1 个文件（CLI；不经 Storage.Put）。
	env.mkdirRemote(ctx, env.root)
	local := path.Join(t.TempDir(), "b.bin")
	if err := os.WriteFile(local, bytes.Repeat([]byte("burst"), 16), 0o600); err != nil {
		t.Fatal(err)
	}
	prepStart := time.Now()
	for i := 0; i < n; i++ {
		dir := env.remoteFor(fmt.Sprintf("burst/%03d", i))
		env.mkdirRemote(ctx, dir)
		if out, err := exec.CommandContext(ctx, env.bin, "upload", local, dir).CombinedOutput(); err != nil {
			t.Fatalf("夹具上传失败 i=%d: %v（%s）", i, err, truncate(string(out), 150))
		}
	}
	t.Logf("[突发探针] 夹具就绪：%d 个目录，耗时 %.1fs", n, time.Since(prepStart).Seconds())

	// 2) 连续裸 move（模拟迁移的连续改名压力），逐条记录。
	type rec struct {
		i        int
		err      error
		dur      time.Duration
		notFound bool
	}
	var recs []rec
	burstStart := time.Now()
	for i := 0; i < n; i++ {
		src := env.remoteFor(fmt.Sprintf("burst/%03d", i))
		dst := env.remoteFor(fmt.Sprintf("burst/%03d-renamed", i))
		st := time.Now()
		err := env.lib.Move(ctx, src, dst)
		recs = append(recs, rec{i: i, err: err, dur: time.Since(st), notFound: errors.Is(err, ErrNotFound)})
	}
	t.Logf("[突发探针] 连续 %d 次裸 move 总耗时 %.1fs（均 %.2fs/次）",
		n, time.Since(burstStart).Seconds(), time.Since(burstStart).Seconds()/float64(n))

	// 3) 统计 + 失败明细 + 立即重试 1 次。
	fail, notFound := 0, 0
	var firstFail int = -1
	for _, r := range recs {
		if r.err == nil {
			continue
		}
		fail++
		if r.notFound {
			notFound++
		}
		if firstFail < 0 {
			firstFail = r.i
		}
	}
	t.Logf("[突发探针] 结果：成功=%d 失败=%d（其中 ErrNotFound=%d），首个失败序号=%d",
		n-fail, fail, notFound, firstFail)
	for _, r := range recs {
		if r.err == nil {
			continue
		}
		t.Logf("[突发探针]   失败 i=%d dur=%.2fs notFound=%v err=%v", r.i, r.dur.Seconds(), r.notFound, r.err)
	}

	// 失败项立即重试一次（判断瞬时 vs 稳定）。
	retried, recovered := 0, 0
	for _, r := range recs {
		if r.err == nil {
			continue
		}
		retried++
		src := env.remoteFor(fmt.Sprintf("burst/%03d", r.i))
		dst := env.remoteFor(fmt.Sprintf("burst/%03d-renamed", r.i))
		if err := env.lib.Move(ctx, src, dst); err == nil {
			recovered++
		} else {
			t.Logf("[突发探针]   重试仍失败 i=%d err=%v", r.i, err)
		}
	}
	t.Logf("[突发探针] 失败项立即重试：%d/%d 成功（瞬时=%v）", recovered, retried, recovered == retried && retried > 0)

	// 4) 抽查：前 3 个已改名的目录，其内容是否随目录一起移动（子路径可见性）。
	for i := 0; i < 3 && i < n; i++ {
		if recs[i].err != nil {
			continue
		}
		child := env.remoteFor(fmt.Sprintf("burst/%03d-renamed/b.bin", i))
		if _, d, err := pollStat(ctx, env.st, child, time.Second, 60*time.Second); err == nil {
			t.Logf("[突发探针]   子路径可见 burst/%03d-renamed/b.bin 用时 %.2fs", i, d.Seconds())
		} else {
			t.Logf("[突发探针]   子路径不可见 burst/%03d-renamed/b.bin err=%v", i, err)
		}
	}

	// 5) 结论行（供人工复核）。
	verdict := "单次与突发均成功 ⇒ 31066 与并发/连续性无关"
	if fail > 0 && recovered == retried {
		verdict = "突发出现失败但立即重试全部成功 ⇒ 属**瞬时**（限流/抖动），需退避重试"
	} else if fail > 0 {
		verdict = "突发出现**稳定失败** ⇒ 需进一步定位（非单纯限流）"
	}
	t.Logf("[突发探针] 结论：%s", verdict)
}
