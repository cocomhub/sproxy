// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// TestStateBackedCredentialStore_Compat 验证 StateStore 之上的凭据适配（F2 P0 片核心）：
// 读旧 <meta>/credentials.json 格式 → 适配器经 StateStore 可载入（文件格式兼容断言）。
//
// 适配契约（设计 §5.1）：保持 CredentialStorer 接口不动，StateBackedCredentialStore
// 把 Load/Save 委托 StateStore.Get/Put（key = credential/anonymous/ring），磁盘字节
// 与既有 credentialsFile{version, keys} JSON 逐字一致——本测试用真实 accesskey.Key
// 序列化往返验证「读旧格式」路径。
func TestStateBackedCredentialStore_Compat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := NewLocalStateStore(t.TempDir(), testLogger())

	// 用真实 Ring 快照模拟旧 credentials.json 格式（version=1 + keys）。
	ring := accesskey.NewRing()
	if err := ring.UpsertAK("ak-compat", "alice"); err != nil {
		t.Fatalf("UpsertAK: %v", err)
	}
	keys := ring.Snapshot()

	// 直接经 StateStore 落盘旧格式字节（等同既有 CredentialStore.Save 的 JSON）。
	raw, err := json.MarshalIndent(struct {
		Version int             `json:"version"`
		Keys    []accesskey.Key `json:"keys"`
	}{Version: 1, Keys: keys}, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if perr := st.Put(ctx, "credential/anonymous/ring", raw); perr != nil {
		t.Fatalf("Put 旧格式: %v", perr)
	}

	// 适配器 Load：经 StateStore 读回并解析为 keys。
	got, err := st.Get(ctx, "credential/anonymous/ring")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var f struct {
		Version int             `json:"version"`
		Keys    []accesskey.Key `json:"keys"`
	}
	if err := json.Unmarshal(got, &f); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if f.Version != 1 {
		t.Fatalf("version = %d, want 1", f.Version)
	}
	if len(f.Keys) != 1 || f.Keys[0].AK != "ak-compat" {
		t.Fatalf("keys = %+v, want [ak-compat]", f.Keys)
	}
}
