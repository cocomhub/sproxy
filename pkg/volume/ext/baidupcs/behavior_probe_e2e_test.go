// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package baidupcs

// behavior_probe_e2e_test.go —— 【行为探针】用**测试目录/测试文件**逐项测量 baidupcs 各方法与
// BaiduPCS-Go 对应行为的**真实差异**，产出可复核的实测数据（不接受推测结论）。
//
// 待回答的问题（每项都有明确度量）：
//   P1. `Put` 之后 `Meta`(Stat) 与「父目录列举(ls)」各自多久可见？（基线参照）
//   P2. **裸 move**（不经我们的 post-Stat）的返回/耗时/错误码是什么？
//   P3. **move 之后** `Meta` 多久可见？`ls` 多久可见？（若两者差异大 ⇒ Stat 可见性滞后成立）
//   P4. 生产装配（binary `mv` 优先 + 库 `Meta`）下，`Storage.Move` 是否会出现
//       「移动已成功但 Move() 返回 ErrNotFound」？（`storage.go:1106` 的 post-Stat）
//   P5. CLI `BaiduPCS-Go mv` 侧的同指标（作为对照）。
//   P6. 各方法的**API 调用次数**（计数 adapter）⇒ 迁移失败路径为何放大调用量。
//
// 全部操作在独立测试根 `/_probe/<uniq>/` 下进行，收尾用 CLI `rm` 递归删除（自己的测试数据）。
// 前置：WSL 已登录 BaiduPCS-Go（`~/.config/BaiduPCS-Go/pcs_config.json` 含 BDUSS）+ 二进制在
// PATH（或 `~/go/bin`）。
// 运行：
//
//	cd pkg/volume/ext/baidupcs && GOWORK=off go test -tags=e2e -run TestE2E_BehaviorProbe -count=1 -v -timeout 40m
import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	bdlib "github.com/qjfoidnh/BaiduPCS-Go/baidupcs"
)

// probeRootBase 是探针在**真实网盘**上的隔离根目录。
// 真实路径经环境变量 `BAIDUPCS_E2E_ROOT` 运行期传入（AGENTS.md 硬规则 21b：仓库内一律通用占位，
// 真实数据不得硬编码入库）。
func probeRootBase(t *testing.T) string {
	t.Helper()
	root := os.Getenv("BAIDUPCS_E2E_ROOT")
	if root == "" {
		t.Skip("未设置 BAIDUPCS_E2E_ROOT（真实网盘隔离根目录）⇒ 跳过 e2e 探针")
	}
	return root
}

// probeResult 是一条测量结果（用于汇总打印 + 落盘）。
type probeResult struct {
	name    string
	detail  string
	seconds float64
}

// probeEnv 是一次探针运行的共享环境。
type probeEnv struct {
	t       *testing.T
	bduss   string
	bin     string
	root    string // 远端探针根（绝对路径）
	st      *Storage
	lib     *libraryAdapter
	prod    *Storage // 生产装配：binary Move 优先 + 库 Meta
	results []probeResult
}

func (e *probeEnv) rec(name, detail string, d time.Duration) {
	e.t.Logf("[探针] %-46s %-70s %.2fs", name, detail, d.Seconds())
	e.results = append(e.results, probeResult{name: name, detail: detail, seconds: d.Seconds()})
}

// newProbeEnv 装配探针环境（库 adapter + 生产装配 adapter 各一份）。
func newProbeEnv(t *testing.T) *probeEnv {
	t.Helper()
	bduss := e2eBDUSSFromConfig(t)
	if bduss == "" {
		t.Skip("BDUSS 为空")
	}
	bin, ok := e2eBinaryAvailable()
	if !ok {
		t.Skip("BaiduPCS-Go 二进制不在 PATH")
	}
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	root := path.Join(probeRootBase(t), uniq)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// ① 纯库路径：Move 也走库（用于隔离「库 move API」本身的行为）。
	cl, err := NewClient(bduss, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	lib := newLibraryAdapter(cl, logger)
	st, err := NewStorage(StorageConfig{Root: root, BDUSS: bduss, TempDir: t.TempDir(),
		Adapter: lib, Logger: logger})
	if err != nil {
		t.Fatalf("NewStorage(lib): %v", err)
	}
	// ② 生产装配：binary Move 优先（CLI `mv`）→ 失败回退库；Meta 走库。
	prod, err := NewStorage(StorageConfig{Root: root, BDUSS: bduss, TempDir: t.TempDir(),
		BinaryPath: bin, Logger: logger})
	if err != nil {
		t.Fatalf("NewStorage(prod): %v", err)
	}
	env := &probeEnv{t: t, bduss: bduss, bin: bin, root: root, st: st, lib: lib, prod: prod}
	t.Cleanup(func() { env.cleanup() })
	return env
}

// cleanup 递归删除探针测试目录（自己的测试数据）。
func (e *probeEnv) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, e.bin, "rm", e.root).CombinedOutput(); err != nil {
		e.t.Logf("[清理] CLI rm %s 失败: %v（%s）——请手工清理", e.root, err, truncate(string(out), 200))
		return
	}
	e.t.Logf("[清理] 已删除探针测试目录 %s", e.root)
}

// cliUpload 用 CLI 上传本地小文件到远端目录（铺夹具用；**不经过** Storage.Put 的可见性等待）。
func (e *probeEnv) cliUpload(ctx context.Context, localPath, remoteDir string) {
	e.t.Helper()
	start := time.Now()
	out, err := exec.CommandContext(ctx, e.bin, "upload", localPath, remoteDir).CombinedOutput()
	if err != nil {
		e.t.Fatalf("CLI upload %s → %s 失败（%.1fs）: %v（%s）",
			localPath, remoteDir, time.Since(start).Seconds(), err, truncate(string(out), 200))
	}
	e.t.Logf("    · CLI upload 完成 %s → %s（%.2fs）", path.Base(localPath), remoteDir, time.Since(start).Seconds())
}

// fixture 在本地写一个小文件并用 CLI 上传到远端目录（铺夹具）。
func (e *probeEnv) fixture(ctx context.Context, remoteDir, name string, content []byte) string {
	e.t.Helper()
	local := filepath.Join(e.t.TempDir(), name)
	if err := os.WriteFile(local, content, 0o600); err != nil {
		e.t.Fatalf("写本地夹具: %v", err)
	}
	e.cliUpload(ctx, local, remoteDir)
	return path.Join(remoteDir, name)
}

// putFile 在探针根下写入一个小文件（自动创建父目录）。
func (e *probeEnv) putFile(ctx context.Context, st *Storage, key string, content []byte) {
	e.t.Helper()
	e.t.Logf("    · Put 开始 key=%s（父目录需已存在；新目录树会走可见性等待，可能数分钟）", key)
	start := time.Now()
	if _, err := st.Put(ctx, key, bytes.NewReader(content)); err != nil {
		e.t.Fatalf("Put(%s) 失败（耗时 %.1fs）: %v", key, time.Since(start).Seconds(), err)
	}
	e.t.Logf("    · Put 完成 key=%s 耗时 %.2fs", key, time.Since(start).Seconds())
}

// mkdirRemote 用 CLI 逐级创建远端目录，并等待其在 `ls` 中可见（限定 3 分钟）。
// **为什么用 CLI 预建目录**：新建目录树的 Meta 可见性延迟可达数分钟（本仓
// `storage.go:1396` 实测 >90s）——若把该成本混进 P2–P6，会掩盖我们要测的
// 「move 之后 Stat 是否延迟」，且让单次运行不可控。
func (e *probeEnv) mkdirRemote(ctx context.Context, dir string) {
	e.t.Helper()
	// 逐级创建（CLI mkdir 不保证 -p 语义）
	parts := strings.Split(strings.Trim(dir, "/"), "/")
	cur := ""
	for _, seg := range parts {
		cur += "/" + seg
		if out, err := exec.CommandContext(ctx, e.bin, "mkdir", cur).CombinedOutput(); err != nil {
			e.t.Logf("    · mkdir %s: %v（%s）", cur, err, truncate(string(out), 80))
		}
	}
	if n, d, ok := e.pollCLI(ctx, path.Dir(dir), path.Base(dir), time.Second, 3*time.Minute); ok {
		e.t.Logf("    · 预建目录可见 %s（attempts=%d, %.1fs）", dir, n, d.Seconds())
	} else {
		e.t.Logf("    · 预建目录 3 分钟内未在 ls 中可见 %s（继续，用例内会如实记录）", dir)
	}
}

// pollStat 轮询 `Stat`（= 库 Meta）直到可见；返回尝试次数、耗时、最后一次错误。
func pollStat(ctx context.Context, st *Storage, key string, interval, max time.Duration) (int, time.Duration, error) {
	start := time.Now()
	var lastErr error
	for attempt := 1; ; attempt++ {
		if _, err := st.Stat(ctx, key); err == nil {
			return attempt, time.Since(start), nil
		} else {
			lastErr = err
			if !errors.Is(err, ErrNotFound) {
				return attempt, time.Since(start), err // 非 NotFound ⇒ 记录并停（不掩盖）
			}
		}
		if time.Since(start) >= max {
			return attempt, time.Since(start), lastErr
		}
		select {
		case <-ctx.Done():
			return attempt, time.Since(start), ctx.Err()
		case <-time.After(interval):
		}
	}
}

// pollList 轮询「父目录列举」直到同名条目可见（`visibleViaList` 同源判据）。
func pollList(ctx context.Context, st *Storage, key string, interval, max time.Duration) (int, time.Duration, bool) {
	dir, base := path.Split(strings.TrimSuffix(key, "/"))
	dir = strings.TrimSuffix(dir, "/")
	start := time.Now()
	for attempt := 1; ; attempt++ {
		if _, ok := st.visibleViaList(ctx, dir, base); ok {
			return attempt, time.Since(start), true
		}
		if time.Since(start) >= max {
			return attempt, time.Since(start), false
		}
		select {
		case <-ctx.Done():
			return attempt, time.Since(start), false
		case <-time.After(interval):
		}
	}
}

// pollCLI 轮询 CLI `ls <远端目录>` 是否列出 base（对照 BaiduPCS-Go 侧的可见性）。
func (e *probeEnv) pollCLI(ctx context.Context, remoteDir, base string, interval, max time.Duration) (int, time.Duration, bool) {
	start := time.Now()
	for attempt := 1; ; attempt++ {
		out, _ := exec.CommandContext(ctx, e.bin, "ls", remoteDir).CombinedOutput()
		if strings.Contains(string(out), base) {
			return attempt, time.Since(start), true
		}
		if time.Since(start) >= max {
			return attempt, time.Since(start), false
		}
		select {
		case <-ctx.Done():
			return attempt, time.Since(start), false
		case <-time.After(interval):
		}
	}
}

// ---------------------------------------------------------------- 用例

// TestE2E_BehaviorProbe 依次执行全部测量并打印汇总。
func TestE2E_BehaviorProbe(t *testing.T) {
	env := newProbeEnv(t)
	ctx := context.Background()

	// 预建所有用例要用的**父目录**（把"新目录树可见性延迟"从 P2–P6 中隔离出去）
	for _, d := range []string{"p1", "p2/A/child", "p4/A/child", "p5/A/child", "p6/A/child"} {
		env.mkdirRemote(ctx, path.Join(env.root, d))
	}

	// P0（可选，PROBE_NEWDIR=1）：**全新目录树** Put 的端到端耗时可观测（默认跳过，
	// 因为该路径会走 waitPutVisible，实测可达 4.5 分钟/次）。
	t.Run("P0_Put_NewDir", func(t *testing.T) {
		if os.Getenv("PROBE_NEWDIR") != "1" {
			t.Skip("设置 PROBE_NEWDIR=1 才测（该路径会走可见性等待，耗时可达数分钟）")
		}
		key := "p0-newdir/deep/file.bin"
		start := time.Now()
		if _, err := env.st.Put(ctx, key, bytes.NewReader([]byte("probe-p0"))); err != nil {
			env.rec("P0 新目录树 Put", fmt.Sprintf("FAILED %v", err), time.Since(start))
			return
		}
		env.rec("P0 新目录树 Put 端到端", "成功", time.Since(start))
	})

	// P1：基线——**已存在的父目录**下，`Storage.Put` 的端到端耗时（有界 150s，超时如实记录）。
	t.Run("P1_Put_Bounded", func(t *testing.T) {
		key := "p1/base2.bin"
		pctx, cancel := context.WithTimeout(ctx, 150*time.Second)
		defer cancel()
		start := time.Now()
		if _, err := env.st.Put(pctx, key, bytes.NewReader([]byte("probe-p1"))); err != nil {
			env.rec("P1 Storage.Put(已存在目录,150s 上限)", fmt.Sprintf("FAILED %v", err), time.Since(start))
			return
		}
		env.rec("P1 Storage.Put(已存在目录)", "成功", time.Since(start))
		if n, d, err := pollStat(ctx, env.st, key, 500*time.Millisecond, 2*time.Minute); err == nil {
			env.rec("P1 Put 后 Meta 可见", fmt.Sprintf("attempts=%d", n), d)
		} else {
			env.rec("P1 Put 后 Meta 可见", fmt.Sprintf("attempts=%d FAILED %v", n, err), d)
		}
	})

	// P2+P3：裸 move（不经 post-Stat）→ 再分别测 Meta / ls 可见延迟。
	t.Run("P2P3_BareMove_ThenVisibility", func(t *testing.T) {
		env.fixture(ctx, env.remoteFor("p2/A/child"), "file.bin", []byte("probe-p23"))
		src, dst := env.remoteFor("p2/A"), env.remoteFor("p2/B")
		start := time.Now()
		mErr := env.lib.Move(ctx, src, dst)
		env.rec("P2 裸 move(from dir) 返回", fmt.Sprintf("err=%v", mErr), time.Since(start))
		if mErr != nil {
			t.Logf("  裸 move 失败：errors.Is(ErrNotFound)=%v；原文=%v", errors.Is(mErr, ErrNotFound), mErr)
		}
		child := "p2/B/child/file.bin"
		if n, d, err := pollStat(ctx, env.st, child, time.Second, 3*time.Minute); err == nil {
			env.rec("P3 move 后 Meta(子路径)可见", fmt.Sprintf("attempts=%d", n), d)
		} else {
			env.rec("P3 move 后 Meta(子路径)可见", fmt.Sprintf("attempts=%d FAILED %v", n, err), d)
		}
		if n, d, ok := pollList(ctx, env.st, child, time.Second, 3*time.Minute); ok {
			env.rec("P3 move 后 ls(子路径)可见", fmt.Sprintf("attempts=%d", n), d)
		} else {
			env.rec("P3 move 后 ls(子路径)可见", fmt.Sprintf("attempts=%d 未可见", n), d)
		}
	})

	// P4：生产装配（binary mv 优先 + 库 Meta）下的 `Storage.Move`（内含 post-Stat）。
	t.Run("P4_StorageMove_ProductionPath", func(t *testing.T) {
		env.fixture(ctx, env.remoteFor("p4/A/child"), "file.bin", []byte("probe-p4"))
		start := time.Now()
		_, mErr := env.prod.Move(ctx, "p4/A", "p4/B")
		env.rec("P4 Storage.Move(生产装配)", fmt.Sprintf("err=%v", mErr), time.Since(start))
		if mErr != nil {
			t.Logf("  Storage.Move 返回错误：errors.Is(ErrNotFound)=%v；原文=%v",
				errors.Is(mErr, ErrNotFound), mErr)
			// 关键判定：错误是否发生在「移动之后」——用 ls 直接看物理结果
			if n, d, ok := pollList(ctx, env.prod, "p4/B/child/file.bin", time.Second, 2*time.Minute); ok {
				env.rec("P4 判定：移动物理上已完成", fmt.Sprintf("ls 可见 attempts=%d ⇒ Move() 报错=假失败", n), d)
			} else {
				env.rec("P4 判定：移动物理上未完成", fmt.Sprintf("ls 亦不可见 attempts=%d ⇒ 真失败", n), d)
			}
		}
	})

	// P5：CLI 对照——`BaiduPCS-Go mv` 一次，再轮询 `ls`。
	t.Run("P5_CLI_Move", func(t *testing.T) {
		env.fixture(ctx, env.remoteFor("p5/A/child"), "file.bin", []byte("probe-p5"))
		src, dst := env.remoteFor("p5/A"), env.remoteFor("p5/B")
		start := time.Now()
		out, err := exec.CommandContext(ctx, env.bin, "mv", src, dst).CombinedOutput()
		env.rec("P5 CLI mv 返回", fmt.Sprintf("err=%v out=%s", err, truncate(string(out), 80)), time.Since(start))
		if n, d, ok := env.pollCLI(ctx, env.remoteFor("p5/B/child"), "file.bin", time.Second, 3*time.Minute); ok {
			env.rec("P5 CLI mv 后 ls 可见", fmt.Sprintf("attempts=%d", n), d)
		} else {
			env.rec("P5 CLI mv 后 ls 可见", fmt.Sprintf("attempts=%d 未可见", n), d)
		}
	})

	// P6：调用次数（计数 adapter）——`Storage.Move` / `Stat` / `List` 各发几次 API。
	t.Run("P6_CallCounts", func(t *testing.T) {
		cnt := &probeCountingAdapter{inner: env.lib}
		st2, err := NewStorage(StorageConfig{Root: env.root, BDUSS: env.bduss,
			TempDir: t.TempDir(), Adapter: cnt,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatalf("NewStorage(counting): %v", err)
		}
		env.fixture(ctx, env.remoteFor("p6/A/child"), "file.bin", []byte("probe-p6"))
		cnt.reset()
		if _, err := st2.Move(ctx, "p6/A", "p6/A2"); err != nil {
			t.Logf("  (count) Move 返回 err=%v", err)
		}
		mv := cnt.snapshot()
		env.rec("P6 Storage.Move 调用数", mv.String(), 0)
		cnt.reset()
		_, _ = st2.Stat(ctx, "p6/A2/child/file.bin")
		env.rec("P6 Storage.Stat 调用数", cnt.snapshot().String(), 0)
		cnt.reset()
		_, _ = st2.List(ctx, "p6/A2")
		env.rec("P6 Storage.List 调用数", cnt.snapshot().String(), 0)
	})

	env.summary()
}

// remoteFor 把探针内的相对 key 转为网盘绝对路径。
func (e *probeEnv) remoteFor(key string) string { return path.Join(e.root, key) }

// summary 打印实测汇总（本用例的**唯一结论来源**）。
func (e *probeEnv) summary() {
	e.t.Log("================ 行为探针实测汇总 ================")
	for _, r := range e.results {
		e.t.Logf("  %-46s %-70s %6.2fs", r.name, r.detail, r.seconds)
	}
	e.t.Log("=================================================")
}

// probeCountingAdapter 统计底层 adapter 的实际调用次数（P6 用）。
type probeCountingAdapter struct {
	inner Adapter
	mu    sync.Mutex
	n     map[string]int
}

func (c *probeCountingAdapter) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n = map[string]int{}
}

func (c *probeCountingAdapter) hit(op string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	c.n[op]++
}

// probeCallSnapshot 是一次计数快照。
type probeCallSnapshot struct{ n map[string]int }

func (s probeCallSnapshot) String() string {
	keys := make([]string, 0, len(s.n))
	for k := range s.n {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	total := 0
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, s.n[k]))
		total += s.n[k]
	}
	return strings.Join(parts, " ") + fmt.Sprintf(" total=%d", total)
}

func (c *probeCountingAdapter) snapshot() probeCallSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.n))
	for k, v := range c.n {
		out[k] = v
	}
	return probeCallSnapshot{n: out}
}

func (c *probeCountingAdapter) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	c.hit("Upload")
	return c.inner.Upload(ctx, localPath, targetPath, overwrite)
}

func (c *probeCountingAdapter) Download(ctx context.Context, remotePath, localPath string) error {
	c.hit("Download")
	return c.inner.Download(ctx, remotePath, localPath)
}

func (c *probeCountingAdapter) Move(ctx context.Context, from, to string) error {
	c.hit("Move")
	return c.inner.Move(ctx, from, to)
}

func (c *probeCountingAdapter) Copy(ctx context.Context, from, to string) error {
	c.hit("Copy")
	return c.inner.Copy(ctx, from, to)
}

// meta/List 转发（让 Storage.metadata() 仍拿到库能力，同时计数）。
func (c *probeCountingAdapter) Meta(ctx context.Context, remotePath string) (*ObjectMeta, error) {
	c.hit("Meta")
	if mp, ok := c.inner.(metadataProvider); ok {
		return mp.Meta(ctx, remotePath)
	}
	return nil, ErrUnsupported
}

func (c *probeCountingAdapter) List(ctx context.Context, remotePath string) ([]ObjectMeta, error) {
	c.hit("List")
	if mp, ok := c.inner.(metadataProvider); ok {
		return mp.List(ctx, remotePath)
	}
	return nil, ErrUnsupported
}

func (c *probeCountingAdapter) Delete(ctx context.Context, remotePath string) error {
	c.hit("Delete")
	if d, ok := c.inner.(deleter); ok {
		return d.Delete(ctx, remotePath)
	}
	return ErrUnsupported
}

var (
	_ Adapter          = (*probeCountingAdapter)(nil)
	_ metadataProvider = (*probeCountingAdapter)(nil)
	_ deleter          = (*probeCountingAdapter)(nil)
	_                  = bdlib.CpMvJSON{} // 保持库依赖显式（裸 move 场景使用库能力时便于扩展）
	_                  = os.Getenv        // 保持 os 依赖（扩展测量用）
)
