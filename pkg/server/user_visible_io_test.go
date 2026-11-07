// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/cocomhub/sproxy/pkg/rootfs"
	"github.com/cocomhub/sproxy/pkg/storage"
)

func newEncRoot(t *testing.T) *storage.Root {
	t.Helper()
	rt, err := storage.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if serr := rt.SetEncryption(bytes.Repeat([]byte{7}, 32)); serr != nil {
		t.Fatalf("SetEncryption: %v", serr)
	}
	return rt
}

// TestUserVisibleIO_EncryptedRoundTrip 用户可见读写入口必须透明加解密：
// 写 = 密文落盘；读 = 明文返回；长度 = 明文长度（否则 Content-Length 与实体不符）。
// 变异：把 writeUserVisible 改成 raw OpenFile（写明文）→ 明文断言红；把 openUserVisibleRead
// 改成 raw Open → 读到密文断言红。
func TestUserVisibleIO_EncryptedRoundTrip(t *testing.T) {
	t.Parallel()
	rt := newEncRoot(t)
	if err := rt.MkdirAll("user", 0o755); err != nil {
		t.Fatal(err)
	}
	plain := []byte("hello-plaintext-content")
	wc, err := rootfs.WriteUserVisible(rt, "user/a.bin", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("writeUserVisible: %v", err)
	}
	if _, werr := wc.Write(plain); werr != nil {
		t.Fatalf("write: %v", werr)
	}
	if cerr := wc.Close(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}

	// 落盘字节必须是密文（不含明文片段）。
	raw, rerr := os.ReadFile(rt.AbsPath() + "/user/a.bin")
	if rerr != nil {
		t.Fatalf("读原始文件: %v", rerr)
	}
	if bytes.Contains(raw, []byte("hello-plaintext")) {
		t.Fatal("加密卷写入口必须透明加密落盘（明文落盘 = at-rest 加密被绕过）")
	}

	// 用户可见读：明文 + 明文长度。
	rc, oerr := rootfs.OpenUserVisibleRead(rt, "user/a.bin")
	if oerr != nil {
		t.Fatalf("openUserVisibleRead: %v", oerr)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, plain) {
		t.Fatalf("应读到明文, got %q", got)
	}
	size, serr := rootfs.UserVisibleSize(rt, "user/a.bin")
	if serr != nil {
		t.Fatalf("userVisibleSize: %v", serr)
	}
	if size != int64(len(plain)) {
		t.Fatalf("用户可见长度应为明文长度 %d, got %d（密文长度会让客户端 unexpected EOF）", len(plain), size)
	}
}

// TestUserVisibleIO_PlainZeroRegression 非加密卷行为与既有完全一致（raw 读写）。
func TestUserVisibleIO_PlainZeroRegression(t *testing.T) {
	t.Parallel()
	rt, err := storage.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if merr := rt.MkdirAll("user", 0o755); merr != nil {
		t.Fatal(merr)
	}
	data := []byte("plain-bytes")
	wc, werr := rootfs.WriteUserVisible(rt, "user/p.bin", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if werr != nil {
		t.Fatalf("writeUserVisible: %v", werr)
	}
	if _, err := wc.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := wc.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(rt.AbsPath() + "/user/p.bin")
	if !bytes.Equal(raw, data) {
		t.Fatalf("非加密卷应原样落盘, got %q", raw)
	}
	if size, serr := rootfs.UserVisibleSize(rt, "user/p.bin"); serr != nil || size != int64(len(data)) {
		t.Fatalf("长度应为 %d, got %d err=%v", len(data), size, serr)
	}
	if _, oerr := rootfs.OpenUserVisibleRead(rt, "user/missing.bin"); oerr == nil {
		t.Fatal("不存在的路径应报错")
	}
}
