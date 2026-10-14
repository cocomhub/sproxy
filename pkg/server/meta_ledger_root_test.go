// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// meta_ledger_root_test.go 钉住「卷内元数据台账经 storage.Root」的服务端两处：
//   - 分享链接台账（meta/share，含 **token 凭据**与目标 rel）；
//   - 计量台账（meta/usage）。
// 二者都由**对应卷**决定是否加密（使用方不理解加密态）：加密卷上必须密文落盘、且可读回
// （重启恢复）；封印语义由 storage.Root 自带（云状态/标签侧已有同类用例）。
//
// 变异：写回退裸 os.WriteFile → 明文断言红；读回退裸 os.ReadFile → 恢复断言红。

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// scanLedgerPlaintext 递归扫描 dir，返回内容包含 needle 的文件路径。
func scanLedgerPlaintext(t *testing.T, dir, needle string) []string {
	t.Helper()
	var hits []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // 遍历容错：只关心是否出现明文
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		if bytes.Contains(b, []byte(needle)) {
			hits = append(hits, p)
		}
		return nil
	})
	return hits
}

func TestSharePersist_EncryptedVolumeCiphertextAndRestore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := testRootAt(t, dir)
	if err := root.SetEncryption(bytes.Repeat([]byte{11}, 32)); err != nil {
		t.Fatalf("SetEncryption: %v", err)
	}
	ss := NewShareStore(testLogger())
	ss.EnablePersist(root, "meta/share")
	const marker = "confidential-name.txt"
	link, err := ss.Create(shareSpec{
		Filename: marker, TenantID: "alice", Rel: "user/" + marker, Owner: "ak-1",
		TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 加密卷上不得明文落盘分享内容（token 出现在文件名属文件系统元数据，不在断言范围）。
	if hits := scanLedgerPlaintext(t, dir, marker); len(hits) > 0 {
		t.Fatalf("加密卷上分享台账不得明文落盘（命中 %v）", hits)
	}
	raw, rerr := os.ReadFile(filepath.Join(dir, "meta", "share", link.Token+".json"))
	if rerr != nil {
		t.Fatalf("读取台账文件（裸读应仍可打开）: %v", rerr)
	}
	if bytes.Contains(raw, []byte(marker)) || bytes.Contains(raw, []byte("\"tenant_id\"")) {
		t.Fatal("加密卷上分享台账内容必须是密文")
	}
	// 重启（新实例）→ 经卷句柄解密读回。
	ss2 := NewShareStore(testLogger())
	ss2.EnablePersist(root, "meta/share")
	got := ss2.Peek(link.Token)
	if got == nil || got.Rel != "user/"+marker {
		t.Fatalf("重启后应恢复分享链接, got %+v", got)
	}
}

func TestUsageStore_EncryptedVolumeCiphertextAndReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := testRootAt(t, dir)
	if err := root.SetEncryption(bytes.Repeat([]byte{12}, 32)); err != nil {
		t.Fatalf("SetEncryption: %v", err)
	}
	const kind = "confidential_kind"
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	s1 := newUsageStore(root, usageRel, testLogger())
	s1.RecordUsageAt("owner-a", kind, 7, at)
	if err := s1.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if hits := scanLedgerPlaintext(t, dir, kind); len(hits) > 0 {
		t.Fatalf("加密卷上计量台账不得明文落盘（命中 %v）", hits)
	}
	// 重启（新实例，同一卷句柄）→ 经卷句柄解密读回。
	s2 := newUsageStore(root, usageRel, testLogger())
	if got := s2.Daily("owner-a", "2026-09-01"); got[kind] != 7 {
		t.Fatalf("重启后应恢复计量数据（经 Root 解密）, got %+v", got)
	}
}

// TestMetaLedger_SealedPathsRejected 封印（封装卷占用的区间）上两处台账写入都必须 fail-closed。
func TestMetaLedger_SealedPathsRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := testRootAt(t, dir)
	if err := root.SetSeals([]storage.SealRule{{Path: "meta", Wrapper: "vault"}}); err != nil {
		t.Fatalf("SetSeals: %v", err)
	}
	ss := NewShareStore(testLogger())
	ss.EnablePersist(root, "meta/share")
	if _, err := ss.Create(shareSpec{Filename: "a.txt", TenantID: "alice", Rel: "user/a.txt", Owner: "ak-1", TTL: time.Hour}); err != nil {
		t.Fatalf("Create 不应因持久化失败而报错（分享是易失资源）: %v", err)
	}
	if hits := scanLedgerPlaintext(t, dir, "a.txt"); len(hits) > 0 {
		t.Fatalf("封印区间不得被绕过写入（命中 %v）", hits)
	}
	s := newUsageStore(root, usageRel, testLogger())
	s.RecordUsageAt("owner-a", "requests", 1, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if err := s.Flush(); err == nil {
		t.Fatal("封印区间上计量台账写入应报错（Flush 返回错误）")
	}
}
