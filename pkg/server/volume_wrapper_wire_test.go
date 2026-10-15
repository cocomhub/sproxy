// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_wrapper_wire_test.go 验证 **config 声明嵌套封装卷的装配接线**
// （`WireConfigNestedWrappers`）：登记互斥占用 + 配额委托 + 写保护，冲突 fail-closed。
//
// 文件来源：原 `config_writer_test.go`（C4 config 写回机制）——该机制已随阶段 2 迁移移除
// （运行时用户卷的持久化归 `user_volume_store` 台账），此处只保留与配置声明接线相关的用例。

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// configWireTestType 是 config 声明封装卷测试类型（可携带 Extra.target；FS 恒 nil——
// Wire 只读 Extra.target，不触碰后端 FS）。
const configWireTestType = "config-wire-test"

var registerConfigWireOnce sync.Once

func registerConfigWireBackend() {
	registerConfigWireOnce.Do(func() {
		registry.RegisterBackend(configWireTestType, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
			return &wrapperSchemaBackend{}, nil
		})
	})
}

// TestWireConfigNestedWrappers_ConfigDeclared 装配 config 声明嵌套封装卷：登记占用 +
// 配额委托 + 写保护；冲突 fail-closed 返回错误。
func TestWireConfigNestedWrappers_ConfigDeclared(t *testing.T) {
	mainRoot := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = mainRoot
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: mainRoot, VolCapacity: 1 << 30},
		// config 声明嵌套封装（仿 secretdata；type 未注册后端不影响 Wire——只读 Extra.target）。
		{Name: "vault", Type: configWireTestType, VolCapacity: 100 << 20, Extra: map[string]any{"target": "main/videos"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	registerConfigWireBackend()
	h := buildVolSetHandlers(t, cfg)
	if err := h.WireConfigNestedWrappers(); err != nil {
		t.Fatalf("WireConfigNestedWrappers: %v", err)
	}
	// 占用登记：main 应含 {videos, vault} 关联。
	refs := h.links().refsOfBase("main")
	if len(refs) != 1 || refs[0].Wrapper != "vault" || refs[0].Subdir != "videos" {
		t.Fatalf("config 声明 vault 关联未登记: %+v", refs)
	}
	// 配额委托：Pool(vault) = 底层池子 Scope（MaxBytes = vol_capacity）。
	if p := h.volSet.Pool("vault"); p == nil || p.MaxBytes() != 100<<20 {
		t.Fatalf("config 声明 vault 委托池缺失/容量错: %+v", p)
	}
	// 写保护：main/videos/* 命中占用 → 拒绝写。
	if err := h.checkWrapperOccupiedWrite("main", "videos/x"); err == nil {
		t.Fatal("config 声明 vault 应使 main/videos 写保护生效")
	}
}

// TestWireConfigNestedWrappers_ConflictFailClosed 冲突（子目录已被另一封装卷占用）→ 启动
// 失败（fail-closed 报错）。
func TestWireConfigNestedWrappers_ConflictFailClosed(t *testing.T) {
	mainRoot := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = mainRoot
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: mainRoot, VolCapacity: 1 << 30},
		{Name: "vaultA", Type: configWireTestType, VolCapacity: 100 << 20, Extra: map[string]any{"target": "main/videos"}},
		{Name: "vaultB", Type: configWireTestType, VolCapacity: 50 << 20, Extra: map[string]any{"target": "main/videos"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	registerConfigWireBackend()
	h := buildVolSetHandlers(t, cfg)
	err := h.WireConfigNestedWrappers()
	if err == nil {
		t.Fatal("冲突 config 声明应启动失败（fail-closed）")
	}
	if !strings.Contains(err.Error(), "子目录已被封装卷") {
		t.Fatalf("错误应引导互斥占用: %v", err)
	}
}
