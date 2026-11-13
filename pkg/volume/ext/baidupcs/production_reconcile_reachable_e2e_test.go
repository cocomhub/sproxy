// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package baidupcs

// production_reconcile_reachable_e2e_test.go —— 【功能真实有效性证明】
//
// 背景（实测 gap，2026-11-13）：对账此前只在 `libraryAdapter.Meta` 里填 `BlockList`，
// 而**生产是 binary 优先**，且实测 **CLI `meta` 输出不含 `block_list`**
// ⇒ 对账在真实生产装配下**永不生效**（空转）。修复后新增 `blockListProvider` 可选能力 +
// `Storage.blockListOf()`（沿 binary→库兜底下探）+ `fetchBlockList`。
//
// 本测试用**生产式装配**（`BinaryPath` 非空、`Adapter` 留空 ⇒ 默认 binary 优先 + 库兜底）
// 端到端断言三件事，缺一即说明功能仍无效：
//  1. `blockListOf()` **非 nil**（生产装配下确实拿得到该能力）；
//  2. `fetchBlockList()` 返回**非空**列表（真实取到了远端 `block_list`）；
//  3. `reconcileBlockList(fetched, 本地 4 MiB 分块 md5)` == **(true, true)**
//     ⇒ 上传后逐块对账可判定为「内容已在云端」（这正是上传后校验要用的结论）。
//
// 前置/运行：同 `md5_conv_probe_e2e_test.go`（`BAIDUPCS_E2E_ROOT` + `BDUSS` + 真实 binary）。
import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestE2E_ProductionReconcileReachable 见文件头。
func TestE2E_ProductionReconcileReachable(t *testing.T) {
	// sproxy:serial: 真实网盘 + 独占测试目录，天然串行
	root := probeRootBase(t)
	bduss := strings.TrimSpace(os.Getenv("BDUSS"))
	if bduss == "" {
		bduss = e2eBDUSSFromConfig(t)
	}
	if bduss == "" {
		t.Skip("BDUSS 为空（环境变量与 BaiduPCS-Go 配置都没读到）")
	}
	bin, ok := e2eBinaryAvailable()
	if !ok {
		t.Skip("BaiduPCS-Go 二进制不可用（PATH 或 ~/go/bin）")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cli, err := NewClient(bduss, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	dir := path.Join(root, "recon-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	if perr := cli.PCS().Mkdir(dir); perr != nil {
		t.Fatalf("Mkdir %s: %v", dir, perr)
	}
	t.Cleanup(func() {
		if perr := cli.PCS().Remove(dir); perr != nil {
			t.Logf("清理 %s 失败（可手工删除）: %v", dir, perr)
		}
	})

	// ★ 生产式装配：BinaryPath 非空 + Adapter 留空 ⇒ 默认「binary 优先 + 库兜底」。
	st, serr := NewStorage(StorageConfig{
		Root: dir, BDUSS: bduss, BinaryPath: bin, TempDir: t.TempDir(), Logger: logger,
	})
	if serr != nil {
		t.Fatalf("NewStorage(生产式装配): %v", serr)
	}

	// 断言 ①：生产装配下能拿到 blockListProvider（沿 binary→库兜底下探）
	if p := st.blockListOf(); p == nil {
		t.Fatalf("生产式装配下 blockListOf() 为 nil ⇒ 逐块对账将空转（功能无效）")
	} else {
		t.Logf("① blockListOf() 可达：%T", p)
	}

	// 上传一份**全新随机内容**（唯一文件名，保证真实上传）
	name := fmt.Sprintf("recon-%d.bin", time.Now().UnixNano())
	local := filepath.Join(t.TempDir(), name)
	if werr := probeMkRandFile(local, probeEnvInt("MD5PROBE_SIZE", 8<<20)); werr != nil {
		t.Fatalf("生成随机文件: %v", werr)
	}
	uctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	out, uerr := exec.CommandContext(uctx, bin, "upload", "--policy", "overwrite", local, dir+"/").CombinedOutput()
	cancel()
	if uerr != nil {
		t.Fatalf("上传失败: %v\n%s", uerr, probeTruncate(string(out), 300))
	}
	if strings.Contains(string(out), "秒传") {
		t.Fatalf("命中秒传 ⇒ 本轮夹具无效（随机内容不应命中）")
	}

	// 断言 ②：真实取到远端 block_list（走 binaryAdapter→库兜底 的库 meta 接口）
	blocks := st.fetchBlockList(context.Background(), name)
	if len(blocks) == 0 {
		t.Fatalf("fetchBlockList 为空 ⇒ 生产装配下取不到 block_list（功能无效）")
	}
	t.Logf("② fetchBlockList 取到 %d 个分块（前 2：%v）", len(blocks), probeTruncate(strings.Join(blocks, ","), 70))

	// 断言 ③：与本地 4 MiB 分块 md5 逐块对账 ⇒ 可判定且一致
	localBlocks, whole, berr := localBlockMD5s(local, uploadBlockSize)
	if berr != nil {
		t.Fatalf("本地分块 md5: %v", berr)
	}
	equal, decidable := reconcileBlockList(blocks, localBlocks)
	t.Logf("③ 本地整文件 md5=%s 本地块数=%d ｜ reconcile=(equal=%v, decidable=%v)",
		probeTruncate(whole, 16), len(localBlocks), equal, decidable)
	if !decidable || !equal {
		t.Fatalf("上传后逐块对账未判定为一致（equal=%v decidable=%v）⇒ 功能未真实生效", equal, decidable)
	}

	// 断言 ④（端到端）：走**生产校验入口**，应直接判定通过（而非退化到 size 容差）
	if _, done, cerr := st.checkAfterUploadUnreliableMD5(context.Background(), name,
		&stagedUpload{md5: whole, size: int64(len(local)), blockMD5s: localBlocks},
		&ObjectMeta{MD5Unreliable: true}, 1); cerr != nil || !done {
		t.Fatalf("生产校验入口未按 block_list 判定通过：done=%v err=%v", done, cerr)
	}
	t.Logf("④ 生产校验入口（MD5Unreliable 分支）已按 block_list 判定通过 ✅")

	_ = os.Remove(local)
	_ = cli.PCS().Remove(path.Join(dir, name))
}
