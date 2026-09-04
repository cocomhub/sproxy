// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// seedTestRing 构造一个含单 AK（ak/sk）的 Ring 快照（plain 条目）。
func seedTestRing(t *testing.T, ak, skHex string, expire bool) []accesskey.Key {
	t.Helper()
	sk, err := hex.DecodeString(skHex)
	if err != nil || len(sk) != 32 {
		t.Fatalf("skHex 非法: %q", skHex)
	}
	ring := accesskey.NewRing()
	if err := ring.UpsertAK(ak, "test"); err != nil {
		t.Fatalf("UpsertAK: %v", err)
	}
	opts := []accesskey.EntryOption{accesskey.WithMeta(accesskey.Meta{Type: "initial"})}
	if expire {
		opts = append(opts, accesskey.WithExpiresAt(ring.CoreEntry(ak).CreatedAt.Add(-1)))
	}
	if _, err := ring.AddKey(ak, sk, opts...); err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	return ring.Snapshot()
}

// TestCredentialStore_SaveLoadRoundtrip 验证 Save(ring 快照) → Load 等价还原
// （AK/条目/SK 字节一致）。
func TestCredentialStore_SaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	st := NewCredentialStore(filepath.Join(dir, "tenant-a", "meta"))

	orig := seedTestRing(t, "sk-test-aabbcc", testAccessSecret, false)
	if err := st.Save(orig); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != len(orig) {
		t.Fatalf("len = %d, want %d", len(got), len(orig))
	}
	if got[0].AK != orig[0].AK {
		t.Errorf("AK = %q, want %q", got[0].AK, orig[0].AK)
	}
	if len(got[0].Entries) != len(orig[0].Entries) {
		t.Fatalf("entries = %d, want %d", len(got[0].Entries), len(orig[0].Entries))
	}
	for i := range orig[0].Entries {
		want := orig[0].Entries[i]
		have := got[0].Entries[i]
		if have.ID != want.ID || have.Kind != want.Kind || have.Meta.Type != want.Meta.Type {
			t.Errorf("entry %d 元数据不一致: %+v vs %+v", i, have, want)
		}
		if string(have.SK) != string(want.SK) {
			t.Errorf("entry %d SK 字节不一致", i)
		}
	}
}

// TestCredentialStore_LoadMissing 验证文件不存在时 Load 返回空（非错）。
func TestCredentialStore_LoadMissing(t *testing.T) {
	st := NewCredentialStore(filepath.Join(t.TempDir(), "tenant", "meta"))
	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load 缺失文件应返回 nil,nil: %v", err)
	}
	if got != nil {
		t.Fatalf("got = %v, want nil", got)
	}
}

// TestCredentialStore_LoadCorrupt 验证文件损坏（非法 JSON）返回错误（fail-closed）。
func TestCredentialStore_LoadCorrupt(t *testing.T) {
	dir := t.TempDir()
	meta := filepath.Join(dir, "tenant", "meta")
	if err := os.MkdirAll(meta, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(meta, "credentials.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := NewCredentialStore(meta)
	if _, err := st.Load(); err == nil {
		t.Fatal("损坏文件 Load 应返回 error")
	}
}

// TestCredentialStore_SaveNoTmpLeftover 验证 Save 后无 .tmp 残留。
func TestCredentialStore_SaveNoTmpLeftover(t *testing.T) {
	dir := t.TempDir()
	st := NewCredentialStore(filepath.Join(dir, "tenant", "meta"))
	if err := st.Save(seedTestRing(t, "sk-aabbcc", testAccessSecret, false)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "tenant", "meta"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("Save 后不应残留 .tmp: %s", e.Name())
		}
	}
}

// TestCredentialStore_ConcurrentSave 验证并发 Save 不损坏（-race）。
func TestCredentialStore_ConcurrentSave(t *testing.T) {
	dir := t.TempDir()
	st := NewCredentialStore(filepath.Join(dir, "tenant", "meta"))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := st.Save(seedTestRing(t, "sk-aabbcc", testAccessSecret, false)); err != nil {
				t.Errorf("并发 Save: %v", err)
			}
		})
	}
	wg.Wait()
	// 最终文件必须可解析且等价（任一次 Save 的产物）。
	got, err := st.Load()
	if err != nil {
		t.Fatalf("并发 Save 后 Load: %v", err)
	}
	if len(got) != 1 || got[0].AK != "sk-aabbcc" {
		t.Fatalf("并发 Save 后文件损坏: %+v", got)
	}
}

// TestNewAnonymousKey_Format 验证首启 anonymous 凭据生成格式。
func TestNewAnonymousKey_Format(t *testing.T) {
	ak, sk, err := newAnonymousKey()
	if err != nil {
		t.Fatalf("newAnonymousKey: %v", err)
	}
	if !strings.HasPrefix(ak, "sk-") || len(ak) != len("sk-")+32 {
		t.Errorf("AK 格式非法: %q", ak)
	}
	if len(sk) != 64 {
		t.Errorf("SK 长度 = %d, want 64", len(sk))
	}
	if _, err := hex.DecodeString(sk); err != nil {
		t.Errorf("SK 非 hex: %v", err)
	}
}

// TestCredentialStore_FileLayout 验证路径为 <metaDir>/credentials.json。
func TestCredentialStore_FileLayout(t *testing.T) {
	meta := filepath.Join(t.TempDir(), "tenant", "meta")
	st := NewCredentialStore(meta)
	if st.path != filepath.Join(meta, "credentials.json") {
		t.Fatalf("path = %q", st.path)
	}
	if err := st.Save(seedTestRing(t, "sk-aabbcc", testAccessSecret, false)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(meta, "credentials.json")); err != nil {
		t.Fatalf("credentials.json: %v", err)
	}
	// 磁盘 JSON 可反序列化为 credentialsFile（结构稳定契约）。
	data, err := os.ReadFile(filepath.Join(meta, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f credentialsFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("磁盘 JSON 反序列化: %v", err)
	}
	if f.Version != 1 || len(f.Keys) != 1 {
		t.Fatalf("磁盘格式异常: %+v", f)
	}
}
