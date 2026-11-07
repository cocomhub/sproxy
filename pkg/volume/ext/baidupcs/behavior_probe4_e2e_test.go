// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package baidupcs

// behavior_probe4_e2e_test.go —— 【确认手段探针 E4】目标：找出**立即可用**的"操作已生效"确认方式
// （`ls` 父目录列举 / `ls` 目标目录 / `Meta` / `Storage.List`），以便**用有效操作确认**而不是 sleep。
//
// 背景（前序实测）：
//   - 单次/突发 50 次 move 均成功（0.3–0.5s/次）⇒ move API 的**返回值本身**就是强信号；
//   - 但**对目录 move 之后，其子树的 `Meta` 会长时间不可见**（E3 实测 30–60s 内轮询均失败）
//     ⇒ `Storage.Move`（storage.go:1106）在成功后用 `Stat` 复核 ⇒ 把成功报成 31066。
//
// 本探针要回答：
//   Q1. **链式**（深度升序，迁移同构）连续改名：X→Y、随后 Y/c1→Y/c2、再 Y/c2/g1→Y/c2/g2，
//       每一步 move 的返回值是否都成功？（若成功 ⇒ 迁移根本不需要"层间等待"）
//   Q2. 改名完成后，**哪些确认手段立即可用**：
//       (a) `ls` 目标**所在目录**（父列举，看条目名）
//       (b) `ls` 目标**目录本身**（看其子条目）
//       (c) `Meta`（我们的 Stat，库 FilesDirectoriesMeta）
//       (d) `Storage.List`（我们的列举实现）
//   Q3. 旧路径是否已消失（`ls` 旧父是否不再列出旧名）。
//
// 运行（WSL）：
//
//	cd pkg/volume/ext/baidupcs && GOTOOLCHAIN=local GOWORK=off go test -tags=e2e \
//	  -run TestE2E_ConfirmOperatorProbe -count=1 -v -timeout 30m
import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"strings"
	"testing"
	"time"
)

// TestE2E_ConfirmOperatorProbe 见文件头 Q1–Q3。
func TestE2E_ConfirmOperatorProbe(t *testing.T) {
	// sproxy:serial: 真实网盘 e2e 探针（共享远端资源 + 限流），必须串行执行
	env := newProbeEnv(t)
	ctx := context.Background()

	// 夹具：<root>/e4/X/c1/g1/f.bin（三层，迁移同构）
	env.mkdirRemote(ctx, env.root)
	base := env.remoteFor("e4")
	env.mkdirRemote(ctx, path.Join(base, "X", "c1", "g1"))
	env.fixture(ctx, path.Join(base, "X", "c1", "g1"), "f.bin", bytes.Repeat([]byte("e4"), 8))
	// 夹具就绪后，先确认基线：Meta / ls 都能看到
	if _, d, err := pollStat(ctx, env.st, "e4/X/c1/g1/f.bin", time.Second, 30*time.Second); err == nil {
		t.Logf("[E4] 基线 Meta 可见 %.2fs", d.Seconds())
	} else {
		t.Logf("[E4] 基线 Meta 30s 不可见 err=%v", err)
	}

	// ---- Q1：链式改名（深度升序，每步**紧接着**上一步，不做任何等待）----
	type step struct {
		name    string
		from    string
		to      string
		err     error
		elapsed time.Duration
	}
	steps := []step{
		{name: "① X→Y", from: path.Join(base, "X"), to: path.Join(base, "Y")},
		{name: "② Y/c1→Y/c2", from: path.Join(base, "Y", "c1"), to: path.Join(base, "Y", "c2")},
		{name: "③ Y/c2/g1→Y/c2/g2", from: path.Join(base, "Y", "c2", "g1"), to: path.Join(base, "Y", "c2", "g2")},
	}
	for i := range steps {
		st := time.Now()
		steps[i].err = env.lib.Move(ctx, steps[i].from, steps[i].to)
		steps[i].elapsed = time.Since(st)
	}
	allOK := true
	for _, s := range steps {
		t.Logf("[E4] Q1 %-22s err=%v %.2fs notFound=%v", s.name, s.err, s.elapsed.Seconds(),
			errors.Is(s.err, ErrNotFound))
		if s.err != nil {
			allOK = false
		}
	}
	t.Logf("[E4] Q1 结论：链式改名 %s（若全部成功 ⇒ 迁移**无需**层间等待）",
		map[bool]string{true: "全部成功", false: "存在失败"}[allOK])

	// ---- Q2：改名完成后各确认手段的**立即可用性**（每项 max 30s，记录 attempts/耗时）----
	finalKey := "e4/Y/c2/g2/f.bin"
	finalRemote := env.remoteFor(finalKey)

	// (a) ls 目标**所在目录**（父列举）——等价于我们 `visibleViaList` 的判据
	dirOf, baseOf := path.Dir(finalRemote), path.Base(finalRemote)
	if n, d, ok := env.pollCLI(ctx, dirOf, baseOf, time.Second, 30*time.Second); ok {
		t.Logf("[E4] Q2a ls(父目录 %s) 含 %s：attempts=%d %.2fs", path.Base(dirOf), baseOf, n, d.Seconds())
	} else {
		t.Logf("[E4] Q2a ls(父目录) 30s 未含 %s（attempts=%d）", baseOf, n)
	}
	// (b) ls 目标**目录本身**（看其子条目 g2 上一层的子项）
	if n, d, ok := env.pollCLI(ctx, finalRemote, "f.bin", time.Second, 30*time.Second); ok {
		t.Logf("[E4] Q2b ls(目标目录) 含 f.bin：attempts=%d %.2fs", n, d.Seconds())
	} else {
		t.Logf("[E4] Q2b ls(目标目录) 30s 未含 f.bin（attempts=%d）", n)
	}
	// (c) Meta（我们的 Stat → 库 FilesDirectoriesMeta）
	if n, d, err := pollStat(ctx, env.st, finalKey, time.Second, 30*time.Second); err == nil {
		t.Logf("[E4] Q2c Meta 可见：attempts=%d %.2fs", n, d.Seconds())
	} else {
		t.Logf("[E4] Q2c Meta 30s 不可见：attempts=%d err=%v", n, err)
	}
	// (d) Storage.List（我们的列举实现）
	if n, d, err := pollOurList(ctx, env.st, "e4/Y/c2/g2", "f.bin", time.Second, 30*time.Second); err == nil {
		t.Logf("[E4] Q2d Storage.List 含 f.bin：attempts=%d %.2fs", n, d.Seconds())
	} else {
		t.Logf("[E4] Q2d Storage.List 30s 未含 f.bin：attempts=%d err=%v", n, err)
	}

	// ---- Q3：旧路径应已消失（链式改名的每一级旧名都不应再列出）----
	oldChecks := []struct{ dir, gone string }{
		{path.Dir(path.Join(base, "X")), "X"},
		{path.Join(base, "Y"), "c1"},
		{path.Join(base, "Y", "c2"), "g1"},
	}
	for _, c := range oldChecks {
		out, _ := exec.CommandContext(ctx, env.bin, "ls", c.dir).CombinedOutput()
		gone := !strings.Contains(string(out), c.gone+"/") && !strings.Contains(string(out), c.gone+"\t")
		t.Logf("[E4] Q3 旧名 %-4s 在 %-40s 已消失=%v", c.gone, path.Base(c.dir), gone)
	}

	// ---- 结论行 ----
	t.Logf("[E4] 结论：链式 move %v；确认手段以『ls/列举』为准（Meta 存在可见性延迟）", allOK)
}

// pollOurList 轮询 `Storage.List` 直到目标目录列举中出现 base。
func pollOurList(ctx context.Context, st *Storage, key, base string, interval, max time.Duration) (int, time.Duration, error) {
	start := time.Now()
	var lastErr error
	for n := 1; ; n++ {
		metas, err := st.List(ctx, key)
		if err == nil {
			for i := range metas {
				if path.Base(strings.TrimSuffix(metas[i].Key, "/")) == base {
					return n, time.Since(start), nil
				}
			}
		} else {
			lastErr = err
		}
		if time.Since(start) >= max {
			return n, time.Since(start), fmt.Errorf("未出现 %q（lastErr=%v）", base, lastErr)
		}
		select {
		case <-ctx.Done():
			return n, time.Since(start), ctx.Err()
		case <-time.After(interval):
		}
	}
}
