// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package sproxy_test

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// encVolumeMagic 是加密卷根密文容器魔数头（与 pkg/storage 内部常量同值：
// [magic][chunkSize] + 逐块 GCM）。测试侧硬编码，用于断言「磁盘上确实是密文容器」。
const encVolumeMagic = "SPROXY-FS-ENC-v1"

// e2e_cli_encrypted_volume_test.go 覆盖 **at-rest 加密卷（volumes[].extra.encrypt）** 的
// 生产可用性（真实 sproxy 二进制 + 真实 sclient 二进制）：
//
//  1. 启动：加密卷装配成功（缺 key 会 fail-closed，故启动成功本身就是断言）；
//  2. 上传：整文件上传落盘为**密文**（密文容器魔数头 + 不含明文片段）；
//  3. 读：全量下载与 Range 区间下载**透明解密**且逐字节一致（Content-Range 按明文坐标）；
//  4. 卷内 secrets 目录同样密文（回归 #776：密钥材料此前明文落在默认卷根下）。
func TestE2E_CLI_EncryptedVolume_AtRestCiphertextRoundTrip(t *testing.T) {
	t.Parallel()
	// 32B raw key（decodeVolumeKey 支持 raw 32B 或 base64）。
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte((i*7 + 3) % 256)
	}
	keyFile := filepath.Join(t.TempDir(), "vol.key")
	if err := os.WriteFile(keyFile, key, 0o600); err != nil {
		t.Fatalf("写 key 文件失败: %v", err)
	}
	extraConfig := fmt.Sprintf(
		"placement: prefer-default\nvolumes:\n  - name: main\n    extra:\n      encrypt: true\n      encrypt_key_file: %q\n",
		filepath.ToSlash(keyFile))
	env := startCLIEnv(t, extraConfig)

	// 内容 > 64KiB（跨多块 GCM），含可检索的明文标记片段。
	const marker = "PLAINTEXT-MARKER-SHOULD-NEVER-APPEAR-ON-DISK"
	content := make([]byte, 160*1024)
	for i := range content {
		content[i] = byte((i * 13) % 251)
	}
	copy(content[70000:], []byte(marker))
	local := filepath.Join(env.TmpDir, "secret.bin")
	if err := os.WriteFile(local, content, 0o644); err != nil {
		t.Fatalf("写本地文件失败: %v", err)
	}

	// 2) 上传（整文件路径）。
	env.sclient(t, env.TmpDir, "upload", "secret.bin")

	onDisk := findFilesNamed(t, env.StorageRoot, "secret.bin")
	if len(onDisk) != 1 {
		t.Fatalf("上传后卷根应恰有 1 个 secret.bin, got %d: %v", len(onDisk), onDisk)
	}
	raw, err := os.ReadFile(onDisk[0])
	if err != nil {
		t.Fatalf("读取落盘文件失败: %v", err)
	}
	if !bytes.HasPrefix(raw, []byte(encVolumeMagic)) {
		t.Fatalf("加密卷落盘文件应以密文容器魔数 %q 开头（got 前 16B=%q）", encVolumeMagic, raw[:min(16, len(raw))])
	}
	if bytes.Contains(raw, []byte(marker)) {
		t.Fatal("加密卷落盘内容不得含明文标记（at-rest 加密未生效）")
	}
	if bytes.Equal(raw, content) {
		t.Fatal("加密卷落盘内容不得等于明文")
	}

	// 3) 全量下载：透明解密 + 逐字节一致（HTTP 面，按明文坐标）。
	status, _, body := rangeRequest(t, env.BaseURL+"/download?filename=secret.bin", "")
	var hdr http.Header
	if status != http.StatusOK {
		t.Fatalf("全量下载应 200, got %d（%s）", status, string(body[:min(200, len(body))]))
	}
	if !bytes.Equal(body, content) {
		t.Fatalf("全量下载解密后应逐字节一致（len=%d want=%d）", len(body), len(content))
	}
	// 区间下载：跨块边界（64KiB）+ 命中明文标记区间。
	status, hdr, body = rangeRequest(t, env.BaseURL+"/download?filename=secret.bin", "bytes=70000-70199")
	if status != http.StatusPartialContent {
		t.Fatalf("Range 下载应 206, got %d", status)
	}
	if want := fmt.Sprintf("bytes 70000-70199/%d", len(content)); hdr.Get("Content-Range") != want {
		t.Fatalf("Content-Range=%q want %q（明文坐标）", hdr.Get("Content-Range"), want)
	}
	if !bytes.Equal(body, content[70000:70200]) {
		t.Fatal("Range 解密段不等于原明文段")
	}

	// 4) 回归 #776：默认卷根下 secrets 目录同样密文落盘。
	secretsDir := filepath.Join(env.StorageRoot, "secrets")
	entries, derr := os.ReadDir(secretsDir)
	if derr != nil {
		t.Fatalf("默认卷根下应有 secrets 目录: %v", derr)
	}
	if len(entries) == 0 {
		t.Fatal("secrets 目录应至少有一个密钥材料文件")
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// LAYOUT_VERSION 是 storage.OpenRoot 用裸 os.Root 写的**卷级布局标记**（非密钥材料，
		// 须在加密配置生效前可读；capacity 亦显式排除）——属设计，不作密文断言。
		if e.Name() == "LAYOUT_VERSION" {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(secretsDir, e.Name()))
		if rerr != nil {
			t.Fatalf("读取 secrets/%s 失败: %v", e.Name(), rerr)
		}
		if !bytes.HasPrefix(b, []byte(encVolumeMagic)) {
			t.Fatalf("secrets/%s 应为密文容器（#776），got 前 16B=%q", e.Name(), b[:min(16, len(b))])
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("secrets 目录应至少有一个密钥材料文件（非 LAYOUT_VERSION）")
	}
}
