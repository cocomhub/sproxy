// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestPikpakEncryptedSecretStore 验证凭据**加密落盘**（C5）：写 → 读回一致；磁盘原始
// 字节不含明文凭据（shardseal 加密）；主密钥落默认 secrets 卷；跨进程重建（新 store
// 实例同根）仍可读。
func TestPikpakEncryptedSecretStore(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ctx := context.Background()

	store, err := pikpakEncryptedSecretStore(ctx, root, "", nil)
	if err != nil {
		t.Fatalf("encrypted store assemble: %v", err)
	}
	const cred = `{"access_token":"ta","refresh_token":"top-secret-rt"}`
	if werr := store.Write(ctx, "pikpak-a.json", []byte(cred)); werr != nil {
		t.Fatal(werr)
	}
	// 读回一致。
	got, err := store.Read(ctx, "pikpak-a.json")
	if err != nil || string(got) != cred {
		t.Fatalf("read mismatch: %v %q", err, got)
	}
	// 磁盘原始字节不含明文凭据（加密卷随机容器/blob，内容经 shardseal）——
	// 递归遍历加密根下全部文件（顶层为随机容器目录，blob 在其内）。
	encRoot := filepath.Join(root, "pikpak-secrets")
	blobFound := false
	werr := filepath.WalkDir(encRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		blobFound = true
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if bytes.Contains(b, []byte("top-secret-rt")) {
			return fmt.Errorf("磁盘文件 %s 含明文凭据（应加密落盘）", p)
		}
		return nil
	})
	if werr != nil {
		t.Fatal(werr)
	}
	if !blobFound {
		t.Fatal("加密卷应存在 blob 文件（凭据已落盘）")
	}
	// 主密钥在默认 secrets 卷（<root>/secrets/pikpak-master）。
	master := filepath.Join(root, "secrets", "pikpak-master")
	if fi, serr := os.Stat(master); serr != nil || fi.Size() == 0 {
		t.Fatalf("主密钥应落默认 secrets 卷: %v", serr)
	}
	// 跨进程重建（新 store 实例同根，主密钥复用）仍可读。
	store2, err := pikpakEncryptedSecretStore(ctx, root, "", nil)
	if err != nil {
		t.Fatalf("re-assemble: %v", err)
	}
	got2, err := store2.Read(ctx, "pikpak-a.json")
	if err != nil || string(got2) != cred {
		t.Fatalf("跨进程重建读回失败: %v %q", err, got2)
	}
}

// TestPikpakEncryptedSecretStore_CustomRoot 自定义 secrets_dir（config pikpak.secrets_dir
// 覆盖）落不同根；主密钥仍默认 secrets 卷。
func TestPikpakEncryptedSecretStore_CustomRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	custom := filepath.Join(t.TempDir(), "custom-secrets")
	ctx := context.Background()

	store, err := pikpakEncryptedSecretStore(ctx, root, custom, nil)
	if err != nil {
		t.Fatal(err)
	}
	if werr := store.Write(ctx, "pikpak-x.json", []byte(`{"access_token":"tx"}`)); werr != nil {
		t.Fatal(werr)
	}
	if fi, serr := os.Stat(filepath.Join(custom, ".")); serr != nil || !fi.IsDir() {
		t.Fatalf("自定义加密根应已创建: %v", serr)
	}
	if _, rerr := store.Read(ctx, "pikpak-x.json"); rerr != nil {
		t.Fatalf("自定义根读回失败: %v", rerr)
	}
}
