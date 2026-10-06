// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// config_writer_test.go 验证 C4（2026-10-06 用户裁决）的两块能力：
//  1. FileConfigWriter：/api/volumes/user 创建/删除后把卷追加/移除 config volumes 段
//     （yaml round-trip + 原子写；重名更新/不存在移除 no-op）；
//  2. WireConfigNestedWrappers：config 声明的嵌套封装卷（target=<卷>/<子目录>）接线互斥
//     占用 + 写保护 + 配额委托（fail-closed 冲突 → 错误）。

import (
	"context"
	"os"
	"path/filepath"
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

// TestFileConfigWriter_AppendRemove 追加/更新/移除 round-trip（yaml 读写）。
func TestFileConfigWriter_AppendRemove(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sproxy.yaml")
	// 初始 config：main 本地卷。
	if err := os.WriteFile(path, []byte("addr: :18083\nvolumes:\n  - name: main\n    type: local\n    root: ./storage\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	w := NewFileConfigWriter(path)

	// 追加用户卷。
	if err := w.AppendVolume(UserVolume{Name: "videos", Type: "secretdata", Capacity: 100 << 20, Extra: map[string]any{"target": "main/videos"}}); err != nil {
		t.Fatalf("AppendVolume: %v", err)
	}
	// 重名追加 → 原地更新（不重复）。
	if err := w.AppendVolume(UserVolume{Name: "videos", Type: "secretdata", Capacity: 200 << 20, Extra: map[string]any{"target": "main/videos"}}); err != nil {
		t.Fatalf("AppendVolume(dup): %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	body := string(data)
	if strings.Count(body, "name: videos") != 1 {
		t.Fatalf("videos 应恰 1 条（重名更新）: %s", body)
	}
	if !strings.Contains(body, "vol_capacity: 209715200") {
		t.Fatalf("更新后容量应为 209715200: %s", body)
	}
	if !strings.Contains(body, "target: main/videos") {
		t.Fatalf("extra.target 应写回: %s", body)
	}
	if strings.ContainsRune(body, 0xFEFF) {
		t.Fatalf("config 写回不应含 BOM")
	}

	// 移除。
	if err := w.RemoveVolume("videos"); err != nil {
		t.Fatalf("RemoveVolume: %v", err)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), "name: videos") {
		t.Fatalf("移除后 videos 不应存在: %s", data)
	}
	// 不存在 → no-op 成功。
	if err := w.RemoveVolume("no-such"); err != nil {
		t.Fatalf("RemoveVolume(no-such) 应 no-op: %v", err)
	}
}

// TestFileConfigWriter_NonexistentFile 目标文件不存在 → 新建写回（不报错）。
func TestFileConfigWriter_NonexistentFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sproxy.yaml")
	w := NewFileConfigWriter(path)
	if err := w.AppendVolume(UserVolume{Name: "v", Type: "secretdata"}); err != nil {
		t.Fatalf("AppendVolume(nonexistent): %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config 文件应被新建: %v", err)
	}
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
