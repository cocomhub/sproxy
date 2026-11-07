// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package baidupcs

// uploadcore_wiring_e2e_test.go —— **真实网盘**验证自研上传内核（S5 验收）。
//
// 为什么用 nucleus 模式：auto 模式在内核失败时会降级既有路径（WARN）⇒ Put 仍成功，
// 无法区分"内核真的能用"与"降级掩盖了失败"。nucleus **不降级、失败即报错** ⇒ 成功即证明
// 内核路径（rapid→slices→merge→verify）在真实百度网盘上确实可用。
//
// 运行（WSL，需 ~/.config/BaiduPCS-Go/pcs_config.json + BAIDUPCS_E2E_ROOT）：
//   cd pkg/volume/ext/baidupcs &&
//   BAIDUPCS_E2E_ROOT='<真实根>' GOTOOLCHAIN=local GOWORK=off ~/sdk/go1.27.2/bin/go \
//     test -tags=e2e -run TestE2E_UploadCoreNucleus -count=1 -v -timeout 20m
//
// 真实路径只经环境变量运行期传入（仓库内无真实数据，硬规则 21b）；本用例自行清理上传产物。

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path"
	"testing"
	"time"
)

// TestE2E_UploadCoreNucleus 内核（nucleus）在真实网盘上完成一次完整上传，且 md5 一致。
func TestE2E_UploadCoreNucleus(t *testing.T) {
	// sproxy:serial: 真实网盘 e2e（共享远端资源 + 限流），必须串行执行
	root := probeRootBase(t) // 环境变量门控（未设置 ⇒ skip）
	bduss := e2eBDUSSFromConfig(t)
	if bduss == "" {
		t.Skip("BDUSS 为空")
	}
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	remoteRoot := path.Join(root, "core", uniq)
	// key 是**相对 Root** 的路径（Storage.Root 已设为 remoteRoot；传完整路径会被再拼一次）。
	key := "nucleus.bin"
	// Debug 级 stderr：直接观察内核打点（stage=upload 含 upload_id；phase=upload 含耗时/错误）。
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cl, err := NewClient(bduss, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	lib := newLibraryAdapter(cl, logger)
	st, err := NewStorage(StorageConfig{Root: remoteRoot, BDUSS: bduss, TempDir: t.TempDir(),
		Adapter: lib, UploadCore: "nucleus", Logger: logger})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	ctx := context.Background()
	// 库上传会自动创建父目录；收尾删除本次唯一文件（探针目录隔离）。
	defer func() {
		if derr := st.Delete(ctx, key); derr != nil {
			t.Logf("清理 %s 失败（可人工删除）: %v", key, derr)
		}
	}()

	// 1MiB 确定性内容（> 0，走完整 rapid→slices→merge；小块便于快速验证）。
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte((i*7 + 13) % 251)
	}
	sum := md5.Sum(data)
	wantMD5 := hex.EncodeToString(sum[:])

	start := time.Now()
	meta, err := st.Put(ctx, key, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("内核上传失败（nucleus 不降级 ⇒ 即内核路径不可用）: %v", err)
	}
	// 数据一致性红线：远端 md5 必须等于本地 md5。
	if meta == nil || meta.ETag != wantMD5 {
		got := "<nil>"
		if meta != nil {
			got = meta.ETag
		}
		t.Fatalf("md5 不一致：远端 %s != 本地 %s", got, wantMD5)
	}
	t.Logf("[nucleus] Put 成功 耗时=%.2fs size=%d md5=%s", time.Since(start).Seconds(), len(data), meta.ETag)
}

// TestE2E_UploadLegacyPathContrast 对照：**既有路径**（legacy）上传同一内容。
// 若 legacy 成功而 nucleus 失败 ⇒ 差异在内核参数（uploadid/分片方案）；若都失败 ⇒ 环境/账号问题。
func TestE2E_UploadLegacyPathContrast(t *testing.T) {
	// sproxy:serial: 真实网盘 e2e（共享远端资源 + 限流），必须串行执行
	root := probeRootBase(t)
	bduss := e2eBDUSSFromConfig(t)
	if bduss == "" {
		t.Skip("BDUSS 为空")
	}
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	remoteRoot := path.Join(root, "core", uniq)
	key := "legacy.bin" // 相对 Root（同上）
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cl, err := NewClient(bduss, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	lib := newLibraryAdapter(cl, logger)
	st, err := NewStorage(StorageConfig{Root: remoteRoot, BDUSS: bduss, TempDir: t.TempDir(),
		Adapter: lib, UploadCore: "legacy", AllowLibraryUpload: true, Logger: logger})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	ctx := context.Background()
	defer func() { _ = st.Delete(ctx, key) }()

	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte((i*7 + 13) % 251)
	}
	sum := md5.Sum(data)
	meta, err := st.Put(ctx, key, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("legacy 上传失败（说明不是内核特有参数问题）: %v", err)
	}
	if meta == nil || meta.ETag != hex.EncodeToString(sum[:]) {
		t.Fatalf("legacy md5 不一致")
	}
	t.Logf("[legacy] Put 成功 md5=%s", meta.ETag)
}
