// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package baidupcs

// batch_deferred_real_e2e_test.go 真实网盘 e2e：**新目录树批量上传（PutDeferred）+ 批量延迟校验（VerifyMany）**。
//
// 为什么需要它：新目录的 Stat 索引延迟 5–10 分钟 ⇒ 逐块即时校验必然超时，故生产写入走
// `PutDeferred`（只传不校验），全部写完后由 `VerifyMany` 统一校验 + 秒传自愈（见
// docs/external-dependencies.md 与 storage.go 的契约注释）。此前这条路径**只有 fake 单测**，
// 缺真实网盘覆盖（清理时发现 `TestSyncEngine_LocalToBaidu` 用的是 fake 存储、0.004s 空跑）。
//
// 断言（真实副作用）：
//  1. 新目录下 N 个文件全部 PutDeferred 成功；
//  2. VerifyMany 逐项 State == VerifyOK（不多不少、顺序一致）；
//  3. **回读内容**与本地逐字节一致（强断言，不依赖远端 ETag 语义）；
//  4. 清理远端产物（不留垃圾）。
//
// 前置与运行（与 real_network_compare_e2e_test.go 同款门控：BDUSS 从 BaiduPCS-Go 配置读，二进制在 PATH）：
//
//	cd pkg/volume/ext/baidupcs && GOWORK=off go test -tags=e2e -run TestE2E_BatchDeferredUploadAndVerify -count=1 -v -timeout 20m .

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

func TestE2E_BatchDeferredUploadAndVerify(t *testing.T) {
	bduss := e2eBDUSSFromConfig(t)
	if bduss == "" {
		t.Skip("BDUSS 为空（~/.config/BaiduPCS-Go/pcs_config.json 未登录）")
	}
	binPath, ok := e2eBinaryAvailable()
	if !ok {
		t.Skip("BaiduPCS-Go 二进制不在 PATH")
	}
	tmpRoot := filepath.Join(t.TempDir(), "tmp")
	if err := os.MkdirAll(tmpRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := NewStorage(StorageConfig{
		Root: "/来自：本地电脑", TempDir: tmpRoot, BDUSS: bduss, BinaryPath: binPath,
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}

	// 唯一**新目录**（新目录正是 Stat 延迟窗口的场景）+ 3 个确定性内容文件。
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	dir := "e2ebatch/" + uniq
	type item struct {
		key     string
		content []byte
		md5     string
	}
	names := []string{"a.bin", "sub/b.bin", "c.txt"}
	sizes := []int{300 << 10, 512 << 10, 4096}
	items := make([]item, 0, len(names))
	stageDir := filepath.Join(t.TempDir(), "stage")
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, name := range names {
		content := make([]byte, sizes[i])
		for j := range content {
			content[j] = byte(j*7 + i*13 + 1)
		}
		staged := filepath.Join(stageDir, fmt.Sprintf("s%d.bin", i))
		if werr := os.WriteFile(staged, content, 0o600); werr != nil {
			t.Fatal(werr)
		}
		items = append(items, item{key: dir + "/" + name, content: content, md5: md5Hex(string(content))})
	}

	cleanupRemote := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, it := range items {
			_ = st.Delete(ctx, it.key)
		}
	}
	cleanupRemote() // 先清一次（防历史残留），结束后再清
	t.Cleanup(cleanupRemote)

	// ① 批量上传（PutDeferred：只传不校验；hints 带 md5/size 以免后端读回）
	upStart := time.Now()
	for i, it := range items {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		hints := &PutHints{Md5: it.md5, Size: int64(len(it.content))}
		perr := st.PutDeferred(ctx, it.key, filepath.Join(stageDir, fmt.Sprintf("s%d.bin", i)), int64(len(it.content)), hints)
		cancel()
		if perr != nil {
			t.Fatalf("PutDeferred(%s): %v", it.key, perr)
		}
	}
	t.Logf("PutDeferred 完成：%d 个文件，耗时 %v（新目录，未做即时校验）", len(items), time.Since(upStart).Round(time.Millisecond))

	// ② 批量延迟校验（VerifyMany）：逐项必须 OK（允许后端秒传自愈，结果仍是 OK）
	verifyItems := make([]syncpkg.VerifyItem, 0, len(items))
	for _, it := range items {
		verifyItems = append(verifyItems, syncpkg.VerifyItem{Key: it.key, MD5: it.md5})
	}
	vctx, vcancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer vcancel()
	res := st.VerifyMany(vctx, verifyItems)
	if len(res) != len(verifyItems) {
		t.Fatalf("VerifyMany 返回 %d 项，期望 %d 项（契约：逐项表达）", len(res), len(verifyItems))
	}
	for i, r := range res {
		if r.State != syncpkg.VerifyOK {
			t.Fatalf("VerifyMany[%d] key=%s state=%s reason=%q（期望 ok）", i, r.Item.Key, r.State, r.Reason)
		}
		t.Logf("  ✓ verify %s state=%s etag=%s refreshe=%v", r.Item.Key, r.State, r.RemoteETag, r.Refreshed)
	}

	// ③ 回读内容（强断言：不依赖远端 ETag 语义）
	for _, it := range items {
		gctx, gcancel := context.WithTimeout(context.Background(), 3*time.Minute)
		rc, _, gerr := st.Get(gctx, it.key)
		if gerr != nil {
			gcancel()
			t.Fatalf("Get(%s): %v", it.key, gerr)
		}
		got, rerr := io.ReadAll(rc)
		_ = rc.Close()
		gcancel()
		if rerr != nil {
			t.Fatalf("读回 %s: %v", it.key, rerr)
		}
		if !bytes.Equal(got, it.content) {
			t.Fatalf("回读内容与本地不一致：key=%s 远端 %d 字节(md5=%s) vs 本地 %d 字节(md5=%s)",
				it.key, len(got), md5Hex(string(got)), len(it.content), it.md5)
		}
	}
	t.Logf("回读内容全部逐字节一致：%d/%d", len(items), len(items))
}
