// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// state_store_config_test.go 是 state_store 配置段（pkg/server/config）的解析/默认值/校验
// 用例。凭据 store 的 StateStore 后端与 BootstrapServerCredentials 已下沉
// pkg/server/credentials，其存储语义用例随包迁移；本文件只保留不依赖凭据装配的配置面断言。

import "testing"

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
