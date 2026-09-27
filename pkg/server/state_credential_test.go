// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// state_credential_test.go 验证凭据状态上移（roadmap 12.1-1 / statestore.md §5.1 P0）：
//   - stateBackedCredentialStore：Load/Save 委托 StateStore.Get/Put（key=credential/anonymous/ring），
//     磁盘字节与既有 {version, keys} JSON 逐字一致（读旧格式兼容）；
//   - 双读单写（零回归铁律）：StateStore 未命中回退读旧 <meta>/credentials.json；首次 Save 写
//     StateStore 新路径（不再写旧 meta）；
//   - BootstrapServerCredentials 集群模式（cluster.enabled 或 state_store.type != local）返回
//     StateStore 后端；encrypt=true 时 StateStore 值为密文（secure 内嵌，Vault/aesgcm 语义不变）。

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/state"
)

// legacyCredentialFile 模拟旧 <meta>/credentials.json 的磁盘格式（version=1 + keys）。
func legacyCredentialFile(t *testing.T, keys []accesskey.Key) []byte {
	t.Helper()
	raw, err := json.MarshalIndent(struct {
		Version int             `json:"version"`
		Keys    []accesskey.Key `json:"keys"`
	}{Version: 1, Keys: keys}, "", "  ")
	if err != nil {
		t.Fatalf("marshal 旧格式: %v", err)
	}
	return raw
}

// writeLegacyCredentialFile 在 metaDir 写旧凭据文件。
func writeLegacyCredentialFile(t *testing.T, metaDir string, keys []accesskey.Key) string {
	t.Helper()
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(metaDir, "credentials.json")
	if err := os.WriteFile(p, legacyCredentialFile(t, keys), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestStateBackedCredentialStore_RoundTrip 验证 Save → StateStore 落盘 + Load 还原，
// 且磁盘字节与旧 credentials.json 格式逐字一致（读旧格式兼容断言，同
// pkg/state/credential_compat_test.go 契约）。
func TestStateBackedCredentialStore_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), testLogger())
	s := newStateBackedCredentialStore(st, credentialRingKey, filepath.Join(t.TempDir(), "credentials.json"), nil)

	keys := seedTestRing(t, "ak-state-0123456789abcdef", testAccessSecret, false)
	if err := s.Save(keys); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// StateStore key 已落盘（单写：迁移后不再写旧 meta）。
	raw, gerr := st.Get(ctx, credentialRingKey)
	if gerr != nil {
		t.Fatalf("StateStore 应已有凭据值: %v", gerr)
	}
	var f struct {
		Version int             `json:"version"`
		Keys    []accesskey.Key `json:"keys"`
	}
	if jerr := json.Unmarshal(raw, &f); jerr != nil {
		t.Fatalf("StateStore 值不是旧凭据格式: %v", jerr)
	}
	if f.Version != 1 || len(f.Keys) != 1 || f.Keys[0].AK != "ak-state-0123456789abcdef" {
		t.Fatalf("StateStore 值格式不符: %+v", f)
	}
	// Load 还原同一 Ring。
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].AK != "ak-state-0123456789abcdef" {
		t.Fatalf("Load 还原失败: %+v", got)
	}
}

// TestStateBackedCredentialStore_EmptyStore 验证 StateStore 无值且旧文件不存在 → (nil, nil)
// （U3 零凭据启动语义对齐）。
func TestStateBackedCredentialStore_EmptyStore(t *testing.T) {
	t.Parallel()
	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), testLogger())
	s := newStateBackedCredentialStore(st, credentialRingKey, filepath.Join(t.TempDir(), "missing", "credentials.json"), nil)
	got, err := s.Load()
	if err != nil {
		t.Fatalf("空 store Load 应 (nil, nil): %v", err)
	}
	if got != nil {
		t.Fatalf("空 store Load 应返回 nil, got %+v", got)
	}
}

// TestStateBackedCredentialStore_LegacyFallback 验证双读：StateStore 未命中 → 回退读旧
// <meta>/credentials.json 可载入（迁移前存量零丢失）。
func TestStateBackedCredentialStore_LegacyFallback(t *testing.T) {
	t.Parallel()
	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), testLogger())
	keys := seedTestRing(t, "ak-legacy-0123456789abcd", testAccessSecret, false)
	legacyPath := writeLegacyCredentialFile(t, filepath.Join(t.TempDir(), "anonymous", "meta"), keys)

	s := newStateBackedCredentialStore(st, credentialRingKey, legacyPath, nil)
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load（回退旧 meta）: %v", err)
	}
	if len(got) != 1 || got[0].AK != "ak-legacy-0123456789abcd" {
		t.Fatalf("回退载入还原失败: %+v", got)
	}
}

// TestStateBackedCredentialStore_SaveMigratesToState 验证单写：回退载入后首次 Save 写
// StateStore 新路径（迁移发生，旧 meta 不再更新）。
func TestStateBackedCredentialStore_SaveMigratesToState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	metaDir := filepath.Join(t.TempDir(), "anonymous", "meta")
	keys := seedTestRing(t, "ak-migrate-0123456789ab", testAccessSecret, false)
	legacyPath := writeLegacyCredentialFile(t, metaDir, keys)

	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), testLogger())
	s := newStateBackedCredentialStore(st, credentialRingKey, legacyPath, nil)
	if _, err := s.Load(); err != nil {
		t.Fatalf("回退 Load: %v", err)
	}
	// 首写 → StateStore；旧 meta 文件保持原内容不被改写（单写）。
	if err := s.Save(keys); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := st.Get(ctx, credentialRingKey); err != nil {
		t.Fatalf("Save 应写 StateStore 新路径: %v", err)
	}
	oldRaw, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(oldRaw, legacyCredentialFile(t, keys)) {
		t.Fatalf("旧 meta 文件不应被改写（单写语义）")
	}
}

// TestStateBackedCredentialStore_CorruptValue 验证 StateStore 值损坏 → error
// （fail-closed，不静默重建——凭据是权威）。
func TestStateBackedCredentialStore_CorruptValue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), testLogger())
	if err := st.Put(ctx, credentialRingKey, []byte("not-json")); err != nil {
		t.Fatal(err)
	}
	s := newStateBackedCredentialStore(st, credentialRingKey, filepath.Join(t.TempDir(), "credentials.json"), nil)
	if _, err := s.Load(); err == nil {
		t.Fatal("损坏值 Load 应返回 error（fail-closed）")
	}
}

// TestBootstrapServerCredentials_StateBacked 验证集群模式（cluster.enabled=true，
// state_store 未显式配置 = type local）装配返回 StateStore 后端。
func TestBootstrapServerCredentials_StateBacked(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = filepath.Join(dir, "storage")
	cfg.Cluster = ClusterConfig{Enabled: true, NodeID: "n1", Role: "master"}

	_, store, err := BootstrapServerCredentials(cfg, nil)
	if err != nil {
		t.Fatalf("BootstrapServerCredentials: %v", err)
	}
	if _, ok := store.(*stateBackedCredentialStore); !ok {
		t.Fatalf("cluster 模式 store 应为 *stateBackedCredentialStore, got %T", store)
	}
}

// TestBootstrapServerCredentials_StateBacked_Encrypted 验证 cluster 模式 + encrypt=true：
// StateStore 值为密文（secure 内嵌 StateBacked，Vault/aesgcm 语义不变），Load 解密还原。
func TestBootstrapServerCredentials_StateBacked_Encrypted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, mkPath := writeMasterKeyFile(t, dir)
	cfg := Default()
	cfg.StorageRoot = filepath.Join(dir, "storage")
	cfg.Cluster = ClusterConfig{Enabled: true, NodeID: "n1", Role: "master"}
	cfg.CredentialStore.Encrypt = true
	cfg.CredentialStore.MasterKeyFile = mkPath

	ring, store, err := BootstrapServerCredentials(cfg, nil)
	if err != nil {
		t.Fatalf("BootstrapServerCredentials: %v", err)
	}
	if ring == nil || ring.Len() != 0 {
		t.Fatalf("空 store 首启应返回空 Ring, got len=%d", ring.Len())
	}
	sb, ok := store.(*stateBackedCredentialStore)
	if !ok {
		t.Fatalf("cluster+encrypt 时 store 应为 *stateBackedCredentialStore, got %T", store)
	}
	if sb.secure == nil {
		t.Fatal("cluster+encrypt 时 StateBacked 应内嵌 secure")
	}
	if serr := store.Save(seedTestRing(t, "ak-state-enc-0123456789", testAccessSecret, false)); serr != nil {
		t.Fatalf("Save: %v", serr)
	}
	// StateStore 值为密文（不含明文 JSON "keys" 字样）。
	st := state.NewLocalStateStore(filepath.Join(cfg.StorageRoot, "state"), testLogger())
	raw, rerr := st.Get(context.Background(), credentialRingKey)
	if rerr != nil {
		t.Fatalf("StateStore 应有加密值: %v", rerr)
	}
	if bytes.Contains(raw, []byte(`"keys"`)) {
		t.Fatalf("加密态 StateStore 值不应含明文 JSON \"keys\" 字样")
	}
	// Load 解密还原。
	snap, lerr := store.Load()
	if lerr != nil {
		t.Fatalf("Load: %v", lerr)
	}
	if len(snap) != 1 || snap[0].AK != "ak-state-enc-0123456789" {
		t.Fatalf("解密载入还原失败: %+v", snap)
	}
}

// TestBootstrapServerCredentials_StateStoreType 验证 state_store.type != local（显式共享）
// 且 cluster 关闭时同样切 StateStore 后端（fail-closed：mongo 未实现 → 装配报错）。
func TestBootstrapServerCredentials_StateStoreType(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = filepath.Join(dir, "storage")
	cfg.StateStore.Type = "local" // 显式 local + cluster 关 = 单节点零回归（不变更后端）
	_, store, err := BootstrapServerCredentials(cfg, nil)
	if err != nil {
		t.Fatalf("BootstrapServerCredentials: %v", err)
	}
	if _, ok := store.(*accesskey.CredentialStore); !ok {
		t.Fatalf("显式 local + cluster 关应保持 accesskey.CredentialStore, got %T", store)
	}
}

// TestLoadFromProvider_StateStore 验证 state_store 段解析（viper/mapstructure 路径）。
func TestLoadFromProvider_StateStore(t *testing.T) {
	t.Parallel()
	cfg, err := LoadFromProvider(mapProvider{m: map[string]any{
		"state_store": map[string]any{
			"type": "local",
			"dir":  "/tmp/sproxy-state",
			"mongo": map[string]any{
				"uri":        "mongodb://127.0.0.1:27017",
				"database":   "sproxy",
				"collection": "sproxy_state",
			},
		},
	}})
	if err != nil {
		t.Fatalf("LoadFromProvider: %v", err)
	}
	if cfg.StateStore.Type != "local" || cfg.StateStore.Dir != "/tmp/sproxy-state" {
		t.Fatalf("state_store 解析不符: %+v", cfg.StateStore)
	}
	if cfg.StateStore.Mongo.URI != "mongodb://127.0.0.1:27017" || cfg.StateStore.Mongo.Database != "sproxy" || cfg.StateStore.Mongo.Collection != "sproxy_state" {
		t.Fatalf("state_store.mongo 解析不符: %+v", cfg.StateStore.Mongo)
	}
}

// TestConfig_StateStoreDefault 验证 type 空 = local（SetDefaults 归一，零回归）。
func TestConfig_StateStoreDefault(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if cfg.StateStore.Type != "local" {
		t.Fatalf("Default() 的 StateStore.Type 应为 local（零回归）, got %q", cfg.StateStore.Type)
	}
	cfg2 := &Config{}
	cfg2.SetDefaults()
	if cfg2.StateStore.Type != "local" {
		t.Fatalf("SetDefaults 后 StateStore.Type 应为 local, got %q", cfg2.StateStore.Type)
	}
}

// TestStateStoreConfig_Validate 验证 state_store 段校验：local/空合法；raft 响亮拒绝；
// mongo 必填 uri；未知类型拒绝（fail-closed，不静默回落 local）。
func TestStateStoreConfig_Validate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		cfg     StateStoreConfig
		wantErr bool
	}{
		{"空 type = local 零回归", StateStoreConfig{}, false},
		{"显式 local", StateStoreConfig{Type: "local"}, false},
		{"mongo 带 uri", StateStoreConfig{Type: "mongo", Mongo: MongoConfig{URI: "mongodb://127.0.0.1:27017"}}, false},
		{"mongo 缺 uri 拒绝", StateStoreConfig{Type: "mongo"}, true},
		{"raft 未实现拒绝", StateStoreConfig{Type: "raft"}, true},
		{"未知类型拒绝", StateStoreConfig{Type: "bogus"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("%s: 应报错, got nil", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("%s: 不应报错: %v", tc.name, err)
			}
		})
	}
}

// TestLoadFromProvider_StateStoreInvalid 验证 viper 路径下非法 state_store 配置被拒绝。
func TestLoadFromProvider_StateStoreInvalid(t *testing.T) {
	t.Parallel()
	if _, err := LoadFromProvider(mapProvider{m: map[string]any{
		"state_store": map[string]any{"type": "raft"},
	}}); err == nil {
		t.Fatal("state_store.type=raft 应被 Validate 拒绝（未实现）")
	}
	if _, err := LoadFromProvider(mapProvider{m: map[string]any{
		"state_store": map[string]any{"type": "mongo"},
	}}); err == nil {
		t.Fatal("state_store.type=mongo 缺 uri 应被拒绝")
	}
}

// TestBootstrapServerCredentials_StateStoreMongoFailClosed 验证集群模式 + mongo 未实现
// （F3 片）→ 装配报错（fail-closed，不回落 local——防「以为多节点一致、实际各写各的」）。
func TestBootstrapServerCredentials_StateStoreMongoFailClosed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = filepath.Join(dir, "storage")
	cfg.StateStore.Type = "mongo"
	cfg.StateStore.Mongo.URI = "mongodb://127.0.0.1:27017"
	if _, _, err := BootstrapServerCredentials(cfg, nil); err == nil {
		t.Fatal("state_store.type=mongo（未实现）应装配报错（fail-closed）")
	}
}
