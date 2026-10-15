// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_wrapper_wire_test.go 验证 **config 声明嵌套封装卷的装配接线**（阶段 2 迁移后）：
// 用户面 `type: <wrapper 名>` + `extra.target: "<卷>/<子目录>"` 在装配期被翻译为 enclosure
// 声明（`nested_enclosure.go: TranslateNestedVolume`），随之而来的是底座三项能力：
//
//   - **封印注入**：源卷（main）在该子区间上封印——客户端直连读写被拒（写恒拒 ErrSealed、
//     读按 hidden 视为不存在）；这是「占用子目录只读」的唯一强制点（取代过渡层的 links +
//     写面 guard）；
//   - **容量池继承**：`Pool(vault)` = `quota.NewPoolWithParent(源卷池, max_bytes)`（提交进
//     自身池即沿父链聚合到源卷池）；
//   - **互斥**：两条声明同一区间 → 装配 fail-closed（`enclosure.ErrConflictClaim` /
//     `ErrFootprintOverlap`）。
//
// 文件来源：原 `config_writer_test.go`（C4 config 写回机制，已移除）+ 过渡层的
// `WireConfigNestedWrappers` 用例（机制已随阶段 2 迁移删除）。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/enclosure"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// configWireTestType 是 config 声明封装卷测试类型：既作建卷 schema 后端（非嵌套形态用），
// 也作**封装处理层**（嵌套 target 翻译后由它构造后端）。
const configWireTestType = "config-wire-test"

var registerConfigWireOnce sync.Once

func registerConfigWireBackend() {
	registerConfigWireOnce.Do(func() {
		registry.RegisterBackend(configWireTestType, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
			return &wrapperSchemaBackend{}, nil
		})
		registerFixtureWrapper(configWireTestType)
	})
}

// TestConfigDeclaredNested_TranslatedToEnclosure 装配 config 声明嵌套封装卷：
// 翻译为 enclosure 声明 → 源卷封印生效 + 容量池继承源卷池。
func TestConfigDeclaredNested_TranslatedToEnclosure(t *testing.T) {
	t.Parallel()
	mainRoot := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = mainRoot
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: mainRoot, VolCapacity: 1 << 30},
		{Name: "vault", Type: configWireTestType, VolCapacity: 100 << 20, Extra: map[string]any{"target": "main/videos"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	registerConfigWireBackend()
	h := buildVolSetHandlers(t, cfg)

	// 翻译结果：注册进卷集的形态是 enclosure（用户面 Extra 不变，仅内存翻译）。
	v, ok := h.volSet.ByName("vault")
	if !ok || v.Type != enclosure.TypeEnclosure {
		t.Fatalf("config 声明应翻译为 enclosure 卷: ok=%v type=%q", ok, v.Type)
	}
	if v.Extra["source"] != "main:videos" || v.Extra["wrapper"] != configWireTestType {
		t.Fatalf("翻译产物 extra 错误: %+v", v.Extra)
	}
	if v.Extra["target"] != "main/videos" {
		t.Fatalf("用户面 target 应保留（回显/幂等）: %+v", v.Extra)
	}

	// ① 封印生效：源卷 main 上出现 "videos" 规则（写恒拒 / 读 hidden）。
	rt := h.volSet.Root("main")
	if rt == nil {
		t.Fatal("main 本地根未装配")
	}
	rule := rt.SealAt("videos/x.bin")
	if rule == nil || rule.Wrapper != "vault" {
		t.Fatalf("源卷 main 应在 videos 上被 vault 封印, got %+v（规则集 %+v）", rule, rt.Seals())
	}
	if err := storage.SealError(rule, "videos/x.bin", true); !errors.Is(err, storage.ErrSealed) {
		t.Fatalf("封印区写的错误应可归因 ErrSealed: %v", err)
	}

	// ② 容量池继承：Pool(vault) 挂到 Pool(main) 之下（MaxBytes = vol_capacity）。
	p := h.volSet.Pool("vault")
	if p == nil || p.MaxBytes() != 100<<20 {
		t.Fatalf("vault 继承池缺失/容量错: %+v", p)
	}
	if p.Parent() != h.volSet.Pool("main") {
		t.Fatal("vault 池应挂到 main 池之下（配额继承）")
	}
}

// TestConfigDeclaredNested_ConflictFailClosed 两条声明同一区间 → 装配 fail-closed。
func TestConfigDeclaredNested_ConflictFailClosed(t *testing.T) {
	t.Parallel()
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
	_, err := assembleVolumes(cfg, testLogger())
	if err == nil {
		t.Fatal("同区间双声明应装配失败（fail-closed）")
	}
	if !errors.Is(err, enclosure.ErrConflictClaim) && !errors.Is(err, enclosure.ErrFootprintOverlap) {
		t.Fatalf("错误应可归因互斥占用哨兵: %v", err)
	}
	// 父子区间同样互斥（足迹重叠，过渡层 PathsOverlap 语义的底座等价物）。
	cfg.Volumes[2].Extra = map[string]any{"target": "main/videos/sub"}
	if _, ferr := assembleVolumes(cfg, testLogger()); ferr == nil || !errors.Is(ferr, enclosure.ErrFootprintOverlap) {
		t.Fatalf("父子区间应判足迹重叠: %v", ferr)
	}
}

// TestTranslateNestedVolume 用户面 → enclosure 声明的翻译语义（纯函数，钉住兼容面）：
//   - 嵌套 target + 已注册 wrapper 类型 → 翻译（type/extra 改写，原 target 保留）；
//   - 非嵌套（无分隔符 / `local` / 卷名或子路径空）→ 不翻译；
//   - 非封装处理层类型 → 不翻译（普通外部卷零回归）；
//   - 已翻译形态（type=enclosure）→ 幂等不再翻译。
func TestTranslateNestedVolume(t *testing.T) {
	t.Parallel()
	registerFixtureWrapper("translate-test-wrap")
	cases := []struct {
		name    string
		vol     volume.Volume
		want    bool
		wantSrc string
	}{
		{"嵌套翻译", volume.Volume{Name: "v", Type: "translate-test-wrap", Extra: map[string]any{"target": "main/videos"}}, true, "main:videos"},
		{"多级子路径", volume.Volume{Name: "v", Type: "translate-test-wrap", Extra: map[string]any{"target": "main/a/b"}}, true, "main:a/b"},
		{"无分隔符不翻译", volume.Volume{Name: "v", Type: "translate-test-wrap", Extra: map[string]any{"target": "main"}}, false, ""},
		{"local 不翻译", volume.Volume{Name: "v", Type: "translate-test-wrap", Extra: map[string]any{"target": "local"}}, false, ""},
		{"尾斜杠不翻译", volume.Volume{Name: "v", Type: "translate-test-wrap", Extra: map[string]any{"target": "main/"}}, false, ""},
		{"未注册类型不翻译", volume.Volume{Name: "v", Type: "ghost-type", Extra: map[string]any{"target": "main/videos"}}, false, ""},
		{"已是 enclosure 幂等", volume.Volume{Name: "v", Type: enclosure.TypeEnclosure, Extra: map[string]any{"target": "main/videos", "source": "main:videos"}}, false, ""},
		{"本地卷不翻译", volume.Volume{Name: "v", Type: volume.TypeLocal, Extra: map[string]any{"target": "main/videos"}}, false, ""},
	}
	for _, tc := range cases {
		got, translated := TranslateNestedVolume(tc.vol)
		if translated != tc.want {
			t.Errorf("%s: translated=%v want %v", tc.name, translated, tc.want)
			continue
		}
		if !translated {
			if got.Type != tc.vol.Type {
				t.Errorf("%s: 未翻译时不应改写 type", tc.name)
			}
			continue
		}
		if got.Type != enclosure.TypeEnclosure || got.Extra["source"] != tc.wantSrc {
			t.Errorf("%s: 翻译产物错误 type=%q extra=%+v", tc.name, got.Type, got.Extra)
		}
		if got.Extra["wrapper"] != tc.vol.Type {
			t.Errorf("%s: wrapper 应为原 type, got %v", tc.name, got.Extra["wrapper"])
		}
		if got.Extra["target"] != tc.vol.Extra["target"] {
			t.Errorf("%s: 原 target 应保留（回显/审计）", tc.name)
		}
	}
}

// TestTranslateNestedVolume_CapacityToMaxBytes 卷级容量翻译为声明自身上限（过渡层语义等价）：
// 显式 max_bytes 优先，不被 capacity 覆盖。
func TestTranslateNestedVolume_CapacityToMaxBytes(t *testing.T) {
	t.Parallel()
	registerFixtureWrapper("translate-cap-wrap")
	v := volume.Volume{Name: "v", Type: "translate-cap-wrap", Capacity: 4096, Extra: map[string]any{"target": "main/videos"}}
	got, ok := TranslateNestedVolume(v)
	if !ok || got.Extra["max_bytes"] != int64(4096) {
		t.Fatalf("capacity 应翻译为 max_bytes: ok=%v extra=%+v", ok, got.Extra)
	}
	v2 := volume.Volume{Name: "v", Type: "translate-cap-wrap", Capacity: 4096,
		Extra: map[string]any{"target": "main/videos", "max_bytes": float64(512)}}
	got2, _ := TranslateNestedVolume(v2)
	if got2.Extra["max_bytes"] != float64(512) {
		t.Fatalf("显式 max_bytes 应优先, got %v", got2.Extra["max_bytes"])
	}
	if !strings.Contains(got2.Extra["source"].(string), ":") {
		t.Fatalf("翻译应产出 source, got %v", got2.Extra)
	}
}
