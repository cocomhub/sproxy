// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// TestEncryptFinalizeRoot 加密卷分块上传的 finalize：在途明文 → 密文落位 + 删除在途文件；
// 读回（OpenDecrypted）得到原明文。
// 变异：改回 atomicRenameRoot（raw 落位）→ 密文断言红（明文落盘 = at-rest 绕过）。
func TestEncryptFinalizeRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if serr := root.SetEncryption(bytes.Repeat([]byte{9}, 32)); serr != nil {
		t.Fatalf("SetEncryption: %v", serr)
	}
	if merr := root.MkdirAll("user", 0o755); merr != nil {
		t.Fatal(merr)
	}
	plain := []byte("chunked-resume-plaintext")
	if werr := os.WriteFile(dir+"/user/a.uploading", plain, 0o600); werr != nil {
		t.Fatal(werr)
	}

	if ferr := encryptFinalizeRoot(root, "user/a.uploading", "user/a.bin"); ferr != nil {
		t.Fatalf("encryptFinalizeRoot: %v", ferr)
	}
	// 在途文件已删。
	if _, serr := root.Stat("user/a.uploading"); serr == nil {
		t.Fatal("在途临时文件应被删除")
	}
	// 落盘字节为密文。
	raw, rerr := os.ReadFile(dir + "/user/a.bin")
	if rerr != nil {
		t.Fatalf("读最终文件: %v", rerr)
	}
	if bytes.Contains(raw, []byte("chunked-resume-plaintext")) {
		t.Fatal("加密卷 finalize 必须透明加密落盘（明文落盘 = 评审 P0 分块上传面未修）")
	}
	// 用户可见读回明文。
	rc, oerr := root.OpenDecrypted("user/a.bin")
	if oerr != nil {
		t.Fatalf("OpenDecrypted: %v", oerr)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, plain) {
		t.Fatalf("读回应为明文, got %q", got)
	}
}

// TestOpenBlockWrite_EncryptedRefused 块写是随机定位写，与 GCM 流式加密不兼容 ⇒ 加密卷
// **显式拒绝**（禁静默降级为明文落盘）。
func TestOpenBlockWrite_EncryptedRefused(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	// 先以明文卷建立租户（懒建），再给租户根注入 at-rest 加密态。
	if _, err := env.svc.OpenBlockWrite(context.Background(), "alice", "docs/a.bin", "main", 8, 0); err != nil {
		t.Fatalf("明文卷建会话应成功: %v", err)
	}
	tnt := env.tenants["alice"]
	if tnt == nil || tnt.Root() == nil {
		t.Fatal("租户应已建立")
	}
	if serr := tnt.Root().SetEncryption(bytes.Repeat([]byte{3}, 32)); serr != nil {
		t.Fatalf("SetEncryption: %v", serr)
	}
	_, oerr := env.svc.OpenBlockWrite(context.Background(), "alice", "docs/b.bin", "main", 8, 0)
	if oerr == nil {
		t.Fatal("加密卷块写必须被拒绝")
	}
	if !strings.Contains(oerr.Error(), "加密") {
		t.Fatalf("拒绝原因应显式说明加密卷不支持, got %v", oerr)
	}
}
