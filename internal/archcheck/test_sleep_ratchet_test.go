// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// test_sleep_ratchet_test.go 是「测试内固定等待只减不增」的**棘轮门禁**（R14）。
//
// 动机：本仓测试里有 160 处 `time.Sleep(...)`，分两类——
//  1. 状态轮询（等异步操作完成）：繁忙 CI 上可能**不够长**（flake 温床，本仓已多次踩到），
//     空闲机器上又白等；应改成条件轮询 pkg/testutil.WaitFor。
//  2. 有意占位（保持连接/持锁一段时间）：属正当用法，不该改。
//
// 一次性全部改完不现实（面大、且第 2 类不该动），所以这里只做**棘轮**：冻结当前每文件计数，
// **只许减少**；未登记的文件预算为 0。确需新增固定等待时，在下表加一行并写明理由——
// 让「加了一个 sleep」成为一次显式决策，而不是顺手写下。
//
// 口径：只统计字面量 `time.Sleep(` 的**出现次数**（不做语义分析）。因此注释里的示例也会被
// 计入，属**保守**方向（宁可多算）。
//
// 收敛方式（每次转换完顺手更新预算，棘轮才会紧）：
//
//	testutil.WaitFor(t, 30*time.Second, func() bool { return 观测到的条件 }, "失败说明")

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// testSleepTotalBudget 是全仓测试文件里 `time.Sleep(` 的出现次数上限（冻结值，只减不增）。
// 2026-09-18 +1：owner_login_e2e_test.go 结果区条件轮询（Playwright 等待必需，已登记文件预算）。
// 2026-09-25 +1：cmd/sproxy/graceful_restart_unix_test.go fd helper 子进程 accept 轮询
// （10s 有界；子进程启动/调度不可条件化，已登记文件预算）。
// 2026-09-25 +1：test/e2e_cli_upgrade_test.go ETXTBSY 重试间隔（10×100ms 有界；
// 原子替换后等待文件句柄释放，无事件可轮询，已登记文件预算）。
const testSleepTotalBudget = 61

// testSleepBudgets 是每文件预算（冻结值）。未列出的测试文件预算为 0。
// 数字对应 2026-09-14 的实测快照；转换掉一处就顺手下调，勿上调。
var testSleepBudgets = map[string]int{
	"cmd/sproxy/mesh_node_test.go":                        1,
	"cmd/sproxy/root_extra_test.go":                       1,
	"cmd/sproxy/graceful_restart_unix_test.go":            1, // fd helper 子进程 accept 轮询（10s 有界；子进程启动不可条件化）
	"pkg/client/mesh_refresh_test.go":                     1,
	"pkg/cloud/manager_test.go":                           4,
	"pkg/cloud/quota_writer_test.go":                      3,
	"pkg/downloader/http_downloader_test.go":              1,
	"pkg/server/relay_stream_test.go":                     1,
	"pkg/server/notify_test.go":                           4,
	"pkg/tunnel/hub/ext/kad/kad_test.go":                  1,
	"web/e2e/notify_panel_test.go":                        1,
	"pkg/server/metrics_test.go":                          2,
	"web/e2e/trash_view_e2e_test.go":                      1,
	"pkg/tunnel/hub/federation_test.go":                   2,
	"pkg/tunnel/hub/signaling_client_test.go":             1,
	"pkg/tunnel/mesh/mesh_test.go":                        4,
	"pkg/tunnel/p2p/manual_signal_test.go":                1,
	"pkg/tunnel/relay/leaf_contract_test.go":              2,
	"pkg/tunnel/relay/leaf_test.go":                       1,
	"pkg/tunnel/tunnel_longlived_test.go":                 1,
	"pkg/tunnel/xfer/ext/quic/quic_conn_internal_test.go": 3,
	"pkg/tunnel/xfer/ext/webrtc/turnrest_test.go":         1,
	"pkg/tunnel/xfer/internal/tcp/tcp_test.go":            1,
	"pkg/tunnel/xfer/internal/tcp/tcp_tls_test.go":        1,
	"pkg/syncmgr/manager_test.go":                         1, // 终态轮询 10ms 间隔（并发安全回归测试，见 TestInjectResultsForTest_ConcurrentWithFinishTask）
	"test/e2e_mesh_node_test.go":                          3,
	"test/e2e_mesh_rr_test.go":                            3,
	"test/e2e_cli_upgrade_test.go":                        1, // ETXTBSY 重试间隔（10×100ms 有界；原子替换后等待句柄释放，无事件可轮询）
	"test/e2e_relay_test.go":                              1,
	"test/chaos/chaos_test.go":                            1, // NetPartition 恢复轮询 200ms（条件轮询替代；分区恢复无事件可轮询）
	"test/chaos/net_chaos.go":                             2, // Pause 挂起轮询 50ms + Delay 注入（网络混沌必需固定等待）
	"test/chaos/node.go":                                  1, // waitReady 就绪轮询 200ms（条件轮询替代）
	"pkg/server/cluster_index_sync_test.go":               4, // Watch/resync 条件轮询 50ms（集群同步事件无固定等待替代）
	"pkg/server/event_index_bridge_test.go":               3, // 桥接条件轮询 50ms + 非文件事件负例 200ms + owner 可见性等待 20ms（事件驱动失效无固定等待替代）
	"web/e2e/owner_login_e2e_test.go":                     1, // 结果区条件轮询 200ms 间隔（Playwright 等待必需）
}

// sleepRatchetSelfPath 是本门禁自身（相对仓库根）：它的注释与自检夹具里必然出现 `time.Sleep(`
// 字面量，若不排除会把门禁自己算进预算（实测即如此：一开就把总数推高 11 处而自报 FAIL）。
const sleepRatchetSelfPath = "internal/archcheck/test_sleep_ratchet_test.go"

// countTestSleeps 统计 root 下所有 `*_test.go` 里 `time.Sleep(` 的出现次数（按文件）。
// 跳过隐藏目录与构建/依赖目录（统一口径见 repo_walk_test.go 的 repoScanSkipDir），
// 并排除本门禁自身（见 sleepRatchetSelfPath）。
func countTestSleeps(root string) (map[string]int, error) {
	counts := map[string]int{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if path != root && repoScanSkipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		if rel == sleepRatchetSelfPath {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if n := strings.Count(string(data), "time.Sleep("); n > 0 {
			counts[rel] = n
		}
		return nil
	})
	return counts, err
}

// TestTestSleepRatchet 断言测试内固定等待只减不增（见文件头）。
func TestTestSleepRatchet(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	actual, err := countTestSleeps(root)
	if err != nil {
		t.Fatalf("统计 time.Sleep: %v", err)
	}

	total := 0
	files := make([]string, 0, len(actual))
	for f := range actual {
		files = append(files, f)
	}
	sort.Strings(files)

	for _, f := range files {
		n := actual[f]
		total += n
		budget, known := testSleepBudgets[f]
		if !known {
			t.Errorf("%s 有 %d 处 time.Sleep，但不在 testSleepBudgets 中（未登记文件预算为 0）。\n"+
				"优先改用条件轮询：testutil.WaitFor(t, max(timeout, 30*time.Second), func() bool { ... }, \"失败说明\")；\n"+
				"确需固定等待（保持连接/持锁等），在 test_sleep_ratchet_test.go 的 testSleepBudgets 加一行并写明理由。", f, n)
			continue
		}
		if n > budget {
			t.Errorf("%s 的 time.Sleep 由 %d 增至 %d（棘轮只减不增）。\n"+
				"优先改用条件轮询：testutil.WaitFor(t, max(timeout, 30*time.Second), func() bool { ... }, \"失败说明\")。", f, budget, n)
		}
	}
	if total > testSleepTotalBudget {
		t.Errorf("全仓 time.Sleep 总数 %d 超过冻结上限 %d（棘轮只减不增）", total, testSleepTotalBudget)
	}
	t.Logf("time.Sleep 现状：%d 处 / %d 个文件（上限 %d）", total, len(files), testSleepTotalBudget)
}

// TestCountTestSleeps_SelfCheck 是棘轮门禁的自检：门禁坏掉（口径漏掉子目录 / 误统计非测试文件）
// 时会静默放行，故用临时目录固定三条口径。
func TestCountTestSleeps_SelfCheck(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("建目录 %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("写 %s: %v", rel, err)
		}
	}
	write("pkg/a_test.go", "time.Sleep(time.Millisecond)\ntime.Sleep(time.Second)\n")
	write("pkg/sub/b_test.go", "time.Sleep(time.Millisecond)\n")
	write("pkg/prod.go", "time.Sleep(time.Millisecond)\n")
	write(".hidden/c_test.go", "time.Sleep(time.Millisecond)\n")
	write("build/d_test.go", "time.Sleep(time.Millisecond)\n")

	counts, err := countTestSleeps(root)
	if err != nil {
		t.Fatalf("countTestSleeps: %v", err)
	}
	if got := counts["pkg/a_test.go"]; got != 2 {
		t.Errorf("pkg/a_test.go = %d, want 2", got)
	}
	if got := counts["pkg/sub/b_test.go"]; got != 1 {
		t.Errorf("子目录必须被统计：pkg/sub/b_test.go = %d, want 1", got)
	}
	for _, mustNotExist := range []string{"pkg/prod.go", ".hidden/c_test.go", "build/d_test.go"} {
		if _, ok := counts[mustNotExist]; ok {
			t.Errorf("%s 不应被统计（非测试文件 / 隐藏目录 / 构建目录）", mustNotExist)
		}
	}
	if len(counts) != 2 {
		t.Fatalf("应只统计 2 个文件，实际 %d：%v", len(counts), counts)
	}
}
