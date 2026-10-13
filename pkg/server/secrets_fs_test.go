// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// secrets_fs_test.go 验证「卷内 secrets/ 目录经 storage.Root」的适配器（`rootSecretsFS`）：
//  1. 加密卷上写入 → 磁盘为**密文**，读回明文（at-rest 加密对密钥文件同样生效）；
//  2. 封印生效（secrets 目录被封装卷占用时，直连读写被拒）；
//  3. 本地 0600 加固经回调生效。
//
// **无兼容负担**（用户裁定 2026-10-13：尚未上线）：不做历史明文迁移，非加密容器按存储层
// 既有报错直接失败（fail-closed，不静默降级）。

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// newEncryptedSecretsRoot 建一个已启用 at-rest 加密的卷根。
func newEncryptedSecretsRoot(t *testing.T) *storage.Root {
	t.Helper()
	rt, err := storage.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if serr := rt.SetEncryption(bytes.Repeat([]byte{3}, 32)); serr != nil {
		t.Fatalf("SetEncryption: %v", serr)
	}
	return rt
}

// TestRootSecretsFS_EncryptedVolumeStoresCiphertext 加密卷上密钥必须**密文落盘**（否则
// at-rest 加密对该配置形同虚设：卷内容全加密而密钥明文）。
//
// 变异：WriteFile 回退裸 os I/O（不经 Root）→ 磁盘出现明文 → 红。
func TestRootSecretsFS_EncryptedVolumeStoresCiphertext(t *testing.T) {
	t.Parallel()
	rt := newEncryptedSecretsRoot(t)
	fsys := NewRootSecretsFS(rt, "secrets", nil)
	const secret = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	if err := fsys.WriteFile(context.Background(), "datakey", strings.NewReader(secret), int64(len(secret)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	abs, ok := rt.Abs("secrets/datakey")
	if !ok {
		t.Fatal("Abs(secrets/datakey) 失败")
	}
	raw, rerr := os.ReadFile(abs)
	if rerr != nil {
		t.Fatalf("读落盘: %v", rerr)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("加密卷上密钥不得明文落盘（got %q）", raw)
	}
	if !bytes.HasPrefix(raw, []byte("SPROXY-FS-ENC-v1")) {
		t.Fatalf("落盘应为加密容器格式，got %q", raw[:min(24, len(raw))])
	}
	rc, oerr := fsys.OpenRead(context.Background(), "datakey")
	if oerr != nil {
		t.Fatalf("OpenRead: %v", oerr)
	}
	got, _ := readAllString(rc)
	if got != secret {
		t.Fatalf("读回 = %q，期望 %q", got, secret)
	}
}

// TestRootSecretsFS_SealedDirDenied 封印生效：secrets 目录被封装卷占用后，直连读写被拒
// （裸 LocalFS 做不到这一点）。
func TestRootSecretsFS_SealedDirDenied(t *testing.T) {
	t.Parallel()
	rt := newEncryptedSecretsRoot(t)
	if serr := rt.SetSeals([]storage.SealRule{{Path: "secrets", Wrapper: "vault"}}); serr != nil {
		t.Fatalf("SetSeals: %v", serr)
	}
	fsys := NewRootSecretsFS(rt, "secrets", nil)
	werr := fsys.WriteFile(context.Background(), "k", strings.NewReader("v"), 1, 0)
	if !errors.Is(werr, storage.ErrSealed) {
		t.Fatalf("封印路径写入应被拒（ErrSealed）, got %v", werr)
	}
}

// TestRootSecretsFS_ChmodSecretHardensMode 经 Root 的视图必须以**回调**方式保留「本地 0600」
// 加固：`secrets.Manager` 的权限收紧此前靠 `*syncpkg.LocalFS` 类型断言，而经 Root 的视图不是
// LocalFS ⇒ 断言会静默跳过（加固消失）。`ChmodSecret` 提供等价能力。
//
// 变异：回调不接线（`Manager.chmodSecret` 忽略回调）→ 权限停留在写入时的 0644 → 红。
func TestRootSecretsFS_ChmodSecretHardensMode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows 无 POSIX 权限位语义（os.Chmod 仅只读位）")
	}
	rt := newEncryptedSecretsRoot(t)
	fsys := NewRootSecretsFS(rt, "secrets", nil)
	rootFS, ok := fsys.(*rootSecretsFS)
	if !ok {
		t.Fatalf("视图类型 = %T", fsys)
	}
	if err := fsys.WriteFile(context.Background(), "k", strings.NewReader("v"), 1, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	abs, _ := rt.Abs("secrets/k")
	before, serr := os.Stat(abs)
	if serr != nil {
		t.Fatal(serr)
	}
	if before.Mode().Perm() == 0o600 {
		t.Fatal("前置条件不成立：写后权限应尚未收紧（否则本用例不具区分力）")
	}
	if err := rootFS.ChmodSecret("k"); err != nil {
		t.Fatalf("ChmodSecret: %v", err)
	}
	after, serr := os.Stat(abs)
	if serr != nil {
		t.Fatal(serr)
	}
	if after.Mode().Perm() != 0o600 {
		t.Fatalf("权限应收紧为 0600, got %o", after.Mode().Perm())
	}
}

// readAllString 读取并关闭 reader（测试辅助）。
func readAllString(rc io.ReadCloser) (string, error) {
	defer rc.Close()
	b, err := io.ReadAll(rc)
	return string(b), err
}
