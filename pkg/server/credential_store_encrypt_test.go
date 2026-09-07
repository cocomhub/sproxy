// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// writeMasterKeyFile 写一个 base64 32B master key 文件（openssl rand -base64 32 形态，
// 带尾换行），返回 key 字节与文件路径。
func writeMasterKeyFile(t *testing.T, dir string) ([]byte, string) {
	t.Helper()
	key := bytes.Repeat([]byte{0x4d}, 32) // "M"
	p := filepath.Join(dir, "master.key")
	content := base64.StdEncoding.EncodeToString(key) + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return key, p
}

// TestBootstrapServerCredentials_EncryptEnabled 验证 credential_store.encrypt=true 装配：
// BootstrapServerCredentials 返回的 store 为 *accesskey.EncryptingStorer；经其 Save 落盘
// 为密文（不含明文 JSON "keys" 字样）；再次 Bootstrap 载入能解密还原同一 Ring 快照
// （master key 文件读入 → AES-256-GCM 解密全链路）。
func TestBootstrapServerCredentials_EncryptEnabled(t *testing.T) {
	dir := t.TempDir()
	_, mkPath := writeMasterKeyFile(t, dir)
	cfg := Default()
	cfg.StorageRoot = filepath.Join(dir, "storage")
	cfg.CredentialStore.Encrypt = true
	cfg.CredentialStore.MasterKeyFile = mkPath

	ring, store, err := BootstrapServerCredentials(cfg, nil)
	if err != nil {
		t.Fatalf("BootstrapServerCredentials: %v", err)
	}
	if ring == nil || ring.Len() != 0 {
		t.Fatalf("空 store 首启应返回空 Ring, got len=%d", ring.Len())
	}
	enc, ok := store.(*accesskey.EncryptingStorer)
	if !ok {
		t.Fatalf("encrypt=true 时 store 应为 *accesskey.EncryptingStorer, got %T", store)
	}
	// 经返回 store 持久化（模拟 register 后 persistCredentials 路径）。
	orig := seedTestRing(t, "ak-encrypt-0123456789abcdef", testAccessSecret, false)
	if serr := enc.Save(orig); serr != nil {
		t.Fatalf("Save: %v", serr)
	}
	metaDir := filepath.Join(cfg.StorageRoot, anonymousOwner, "meta")
	disk, err := os.ReadFile(filepath.Join(metaDir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(disk, []byte(`"keys"`)) {
		t.Fatalf("加密态磁盘不应含明文 JSON \"keys\" 字样")
	}

	// 再次 Bootstrap（重启模拟）→ 解密载入还原同一 Ring。
	ring2, store2, err := BootstrapServerCredentials(cfg, nil)
	if err != nil {
		t.Fatalf("BootstrapServerCredentials(second): %v", err)
	}
	if _, ok := store2.(*accesskey.EncryptingStorer); !ok {
		t.Fatalf("second store 应为 *accesskey.EncryptingStorer, got %T", store2)
	}
	snap := ring2.Snapshot()
	if len(snap) != 1 || snap[0].AK != "ak-encrypt-0123456789abcdef" {
		t.Fatalf("解密载入还原失败: %+v", snap)
	}
}

// TestBootstrapServerCredentials_EncryptWrongKey 验证 master key 错时 Bootstrap 解密
// 失败（fail-closed：拒绝启动，不静默重建）。
func TestBootstrapServerCredentials_EncryptWrongKey(t *testing.T) {
	dir := t.TempDir()
	_, mkPath := writeMasterKeyFile(t, dir)
	cfg := Default()
	cfg.StorageRoot = filepath.Join(dir, "storage")
	cfg.CredentialStore.Encrypt = true
	cfg.CredentialStore.MasterKeyFile = mkPath

	_, store, err := BootstrapServerCredentials(cfg, nil)
	if err != nil {
		t.Fatalf("BootstrapServerCredentials: %v", err)
	}
	if serr := store.Save(seedTestRing(t, "ak-encrypt-0123456789abcdef", testAccessSecret, false)); serr != nil {
		t.Fatalf("Save: %v", serr)
	}
	// 换一个不同 master key 文件再 Bootstrap → 解密失败。
	otherKey := bytes.Repeat([]byte{0x5e}, 32)
	otherPath := filepath.Join(dir, "other.key")
	if err := os.WriteFile(otherPath, []byte(base64.StdEncoding.EncodeToString(otherKey)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.CredentialStore.MasterKeyFile = otherPath
	if _, _, err := BootstrapServerCredentials(cfg, nil); err == nil {
		t.Fatal("master key 错时 Bootstrap 应返回 error（fail-closed）")
	}
}

// TestBootstrapServerCredentials_EncryptMissingKey 验证 encrypt=true 但 master_key_file
// 与环境变量均缺省时 Bootstrap 报错（fail-fast）。
func TestBootstrapServerCredentials_EncryptMissingKey(t *testing.T) {
	t.Setenv(CredentialMasterKeyEnv, "")
	cfg := Default()
	cfg.StorageRoot = filepath.Join(t.TempDir(), "storage")
	cfg.CredentialStore.Encrypt = true
	if _, _, err := BootstrapServerCredentials(cfg, nil); err == nil {
		t.Fatal("缺少 master key 来源时 Bootstrap 应报错")
	} else if !strings.Contains(err.Error(), "master_key_file") {
		t.Fatalf("错误信息应提示 master_key_file/env: %v", err)
	}
}

// TestResolveCredentialMasterKey_EnvFallback 验证 master key 解析来源顺序：master_key_file
// 优先；为空时回落环境变量 CredentialMasterKeyEnv（base64 32B）；环境变量非法报错。
func TestResolveCredentialMasterKey_EnvFallback(t *testing.T) {
	dir := t.TempDir()
	key, mkPath := writeMasterKeyFile(t, dir)
	cfg := Default()

	// 仅文件：返回文件内容。
	cfg.CredentialStore.MasterKeyFile = mkPath
	got, err := resolveCredentialMasterKey(cfg)
	if err != nil {
		t.Fatalf("resolve(file): %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Fatalf("文件 key 解析不一致")
	}

	// S-3：file 与 env 同时设置 → file 优先（resolveCredentialMasterKey 的 if 顺序钉死）。
	envKey := bytes.Repeat([]byte{0x9e}, 32)
	t.Setenv(CredentialMasterKeyEnv, base64.StdEncoding.EncodeToString(envKey))
	cfg.CredentialStore.MasterKeyFile = mkPath
	gotFileWins, err := resolveCredentialMasterKey(cfg)
	if err != nil {
		t.Fatalf("resolve(file+env): %v", err)
	}
	if !bytes.Equal(gotFileWins, key) {
		t.Fatalf("file 与 env 同时设置时应以 file 为准, got %x", gotFileWins)
	}
	if bytes.Equal(gotFileWins, envKey) {
		t.Fatalf("file 与 env 同时设置时不应取 env")
	}

	// 文件为空 + 环境变量：回落 env。
	cfg.CredentialStore.MasterKeyFile = ""
	got2, err := resolveCredentialMasterKey(cfg)
	if err != nil {
		t.Fatalf("resolve(env): %v", err)
	}
	if !bytes.Equal(got2, envKey) {
		t.Fatalf("env key 解析不一致")
	}

	// 环境变量非法 base64 → 报错。
	t.Setenv(CredentialMasterKeyEnv, "!!not-base64!!")
	if _, serr := resolveCredentialMasterKey(cfg); serr == nil {
		t.Fatal("非法 env master key 应报错")
	}
}

// TestConfigValidate_CredentialStoreEncrypt 验证 Validate 对 credential_store 的装配门禁：
// encrypt=true 且文件与 env 均缺省 → error；任一来源存在 → 通过（master_key_file 文件
// 可读性由装配层校验，Validate 不 stat 文件）。
func TestConfigValidate_CredentialStoreEncrypt(t *testing.T) {
	dir := t.TempDir()
	_, mkPath := writeMasterKeyFile(t, dir)

	t.Run("缺省来源报错", func(t *testing.T) {
		t.Setenv(CredentialMasterKeyEnv, "")
		cfg := Default()
		cfg.CredentialStore.Encrypt = true
		if err := cfg.Validate(); err == nil {
			t.Fatal("encrypt=true 且无任何 master key 来源时 Validate 应报错")
		}
	})
	t.Run("master_key_file 通过", func(t *testing.T) {
		cfg := Default()
		cfg.CredentialStore.Encrypt = true
		cfg.CredentialStore.MasterKeyFile = mkPath
		if err := cfg.Validate(); err != nil {
			t.Fatalf("配置了 master_key_file 应通过 Validate: %v", err)
		}
	})
	t.Run("环境变量通过", func(t *testing.T) {
		t.Setenv(CredentialMasterKeyEnv, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x6f}, 32)))
		cfg := Default()
		cfg.CredentialStore.Encrypt = true
		if err := cfg.Validate(); err != nil {
			t.Fatalf("配置了 env master key 应通过 Validate: %v", err)
		}
	})
	t.Run("默认关闭通过", func(t *testing.T) {
		cfg := Default()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("encrypt 默认关应通过 Validate: %v", err)
		}
	})
}
