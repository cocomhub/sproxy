// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"bytes"
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// encrypted_ledger_test.go 钉住加密卷上**写入/覆盖/删除三处口径一致**（2026-10-14 容量口径收口）。
//
// 为什么需要它：口径细节极易写错——`root.Stat()` 在加密卷返回**密文（物理）**长度，而「本次写入
// 字节数」「客户端声明长度」是**逻辑**长度。把物理值再折算一次 = 双重折算 ⇒ 过度释放 ⇒ 配额虚低
// （超限写入通过）；把逻辑值不折算 ⇒ 少释放 ⇒ 幽灵占用（假 507）。故用**精确相等**钉住：
//
//	写 A(1000)+B(1000) ⇒ Usage == physical(1000)*2
//	覆盖 A 为 500      ⇒ Usage == physical(500) + physical(1000)
//	删除 A             ⇒ Usage == physical(1000)     ← 双重折算会让它变成 1020（少 32）
//
// 变异：Commit 折算 prev / 删除释放再折算 / 覆盖不折算 written ⇒ 对应用例红。

// encLedgerEnv 建加密卷环境。
func encLedgerEnv(t *testing.T) *dirsEnv {
	t.Helper()
	e := writeTargetEnv(t)
	if serr := e.tenantFor("alice").Root().SetEncryption(bytes.Repeat([]byte{9}, 32)); serr != nil {
		t.Fatalf("SetEncryption: %v", serr)
	}
	return e
}

func encWrite(t *testing.T, e *dirsEnv, rel string, n int) {
	t.Helper()
	if _, err := e.svc.WriteTarget(context.Background(), WriteTargetInput{
		Owner: "alice", RemotePath: rel, ClientSize: int64(n),
	}, bytes.NewReader(bytes.Repeat([]byte("z"), n))); err != nil {
		t.Fatalf("write %s(%d): %v", rel, n, err)
	}
}

// TestEncryptedLedger_WriteOverwriteDeleteExact：三处口径精确一致。
func TestEncryptedLedger_WriteOverwriteDeleteExact(t *testing.T) {
	t.Parallel()
	e := encLedgerEnv(t)
	ctx := context.Background()
	encWrite(t, e, "a.bin", 1000)
	encWrite(t, e, "b.bin", 1000)
	sc := e.quotaScopeFor("alice", "user/a.bin")
	if sc == nil {
		t.Fatal("user 桶 Scope 不可用")
	}
	p1000 := storage.EncryptedPhysicalSize(1000)
	if got := sc.Usage(); got != 2*p1000 {
		t.Fatalf("两次写入后 Usage=%d want %d", got, 2*p1000)
	}
	// 覆盖 a.bin 为 500：prev 是**物理**（来自 Stat）⇒ 只折算 written。
	encWrite(t, e, "a.bin", 500)
	p500 := storage.EncryptedPhysicalSize(500)
	if got := sc.Usage(); got != p500+p1000 {
		t.Fatalf("覆盖写后 Usage=%d want %d（双重折算 prev 会偏低）", got, p500+p1000)
	}
	// 删除 a.bin：info 来自 `OpenDecrypted` 的 Stat ⇒ **逻辑** ⇒ 必须折算为物理（少释放 ⇒ 幽灵占用 52）。
	if _, err := e.svc.DeleteFile(ctx, DeleteFileInput{
		Owner: "alice", RemotePath: "a.bin", SoftDelete: false,
		ExpectedChecksum: testutil.SHA256Hex(bytes.Repeat([]byte("z"), 500)),
	}); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if got := sc.Usage(); got != p1000 {
		t.Fatalf("删除后 Usage=%d want %d（多释放 = 配额虚低）", got, p1000)
	}
}

// TestEncryptedLedger_LogicalSizeAccessor：口径判据的事实基础——Stat 是物理、LogicalSize 是逻辑。
func TestEncryptedLedger_LogicalSizeAccessor(t *testing.T) {
	t.Parallel()
	e := encLedgerEnv(t)
	encWrite(t, e, "c.bin", 3000)
	root := e.tenantFor("alice").Root()
	fi, serr := root.Stat("user/c.bin")
	if serr != nil {
		t.Fatalf("Stat: %v", serr)
	}
	if fi.Size() != storage.EncryptedPhysicalSize(3000) {
		t.Fatalf("Stat 应为物理长度 %d, got %d", storage.EncryptedPhysicalSize(3000), fi.Size())
	}
	ls, lerr := root.LogicalSize("user/c.bin")
	if lerr != nil {
		t.Fatalf("LogicalSize: %v", lerr)
	}
	if ls != 3000 {
		t.Fatalf("LogicalSize 应为逻辑长度 3000, got %d", ls)
	}
}
