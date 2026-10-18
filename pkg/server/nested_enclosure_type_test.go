// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// nested_enclosure_type_test.go 锁定「**enclosure 是内部装配机制，不是一种对外卷类型**」
// （方案甲）：嵌套封装卷（用户面 `type: <wrapper>` + `extra.target: "<卷>/<子目录>"`）经装配层
// 翻译后，**卷描述 Type 保持 wrapper 类型**，只有「后端构造」走 enclosure 路径。三项用户可见
// 后果都是本文件的断言对象：
//
//   - 回显：`/api/volumes` 的 type 是 wrapper 类型（内部机制不外泄，CLI/config 同源）；
//   - 能力：`SchemeOf(type)` 有协议（cloud 转存/归档到该卷不再被拒——此前 type=enclosure
//     无协议 ⇒ `checkTransferVolumePreflight` 恒拒）、`CategoryOf(type)=="wrapper"`
//     （建卷表单/防环校验据 wrapper 语义）；
//   - 装配不变式：封印注入、容量池继承、声明可解析（删除/引用链路）全部照旧。
//
// 变异：把 `TranslateNestedDecl` 的返回改回 `enclosure.TypeEnclosure` ⇒ 本文件断言全红
// （type 泄漏 + 协议丢失）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/enclosure"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// nestedTypeTestType 是「既是 backend（声明协议）又是 wrapper」的测试类型：申请协议的 backend
// 工厂**恒失败**——嵌套声明必须由 enclosure 路径接管构造。若装配层的 dispatch 判据退回
// 「仅按 deferred 类型」，assembleVolumes 会调用该工厂 ⇒ 装配期即红。
const nestedTypeTestType = "nested-type-test"

// nestedTypeTestProto 是 nestedTypeTestType 声明的协议（SchemeOf 反查用）。
const nestedTypeTestProto = "nested-type-test-proto"

var registerNestedTypeTestOnce sync.Once

// registerNestedTypeTestAssets 注册测试类型（backend 带协议 + 同名 wrapper），幂等。
func registerNestedTypeTestAssets() {
	registerNestedTypeTestOnce.Do(func() {
		registry.RegisterBackend(nestedTypeTestType, func(context.Context, volume.Volume) (registry.ExternalBackend, error) {
			return nil, errString("nested-type-test: 嵌套声明不得经 registry.NewBackend 构造")
		}, nestedTypeTestProto)
	})
	registerFixtureWrapper(nestedTypeTestType) // 注册为封装处理层（同名幂等）
}

// volumesAPIType 走真实 `/api/volumes` handler 取指定卷回显的 type。
func volumesAPIType(t *testing.T, h *Handlers, volName string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/volumes", nil)
	req = req.WithContext(withActor(req.Context(), "alice"))
	rec := httptest.NewRecorder()
	h.listVolumesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/volumes = %d (body: %s)", rec.Code, rec.Body.String())
	}
	var out volumesListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析 /api/volumes 响应失败: %v (body: %s)", err, rec.Body.String())
	}
	for _, v := range out.Volumes {
		if v.Name == volName {
			return v.Type
		}
	}
	t.Fatalf("/api/volumes 未列出卷 %q: %s", volName, rec.Body.String())
	return ""
}

// TestNestedDecl_ConfigKeepsWrapperType config 装配的嵌套封装卷：卷描述 Type 保持 wrapper 类型，
// 协议/分类按该类型语义可得（cloud 转存目标可解析），同时封印/继承池/声明解析全部照旧。
func TestNestedDecl_ConfigKeepsWrapperType(t *testing.T) {
	t.Parallel()
	registerNestedTypeTestAssets()
	mainDir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = mainDir
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: mainDir, VolCapacity: 1 << 30},
		{Name: "vault", Type: nestedTypeTestType, VolCapacity: 8 << 20, Extra: map[string]any{"target": "main/videos"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	if aerr := enclosure.AttachAll(context.Background(), h.volSet, testLogger(), enclosure.AttachOptions{}); aerr != nil {
		t.Fatalf("AttachAll: %v", aerr)
	}

	// ① 卷描述 Type = wrapper 类型（enclosure 只是装配机制，不写进对外描述）。
	v, ok := h.volSet.ByName("vault")
	if !ok || v.Type != nestedTypeTestType {
		t.Fatalf("嵌套封装卷的 Type 应保持 wrapper 类型: ok=%v type=%q", ok, v.Type)
	}
	// ② /api/volumes 回显同型（用户面不泄漏内部机制）。
	if got := volumesAPIType(t, h, "vault"); got != nestedTypeTestType {
		t.Fatalf("/api/volumes 回显 type=%q, want %q（内部机制不得外泄）", got, nestedTypeTestType)
	}
	// ③ 协议/分类按 wrapper 类型语义可得 ⇒ cloud 转存/归档目标可解析（旁路修复点）。
	if got := registry.SchemeOf(v.Type); got != nestedTypeTestProto {
		t.Fatalf("SchemeOf(%q)=%q, want %q（无协议则转存/归档恒被拒）", v.Type, got, nestedTypeTestProto)
	}
	if got := registry.CategoryOf(v.Type); got != "wrapper" {
		t.Fatalf("CategoryOf(%q)=%q, want wrapper", v.Type, got)
	}
	fsys, scheme, _, tok := h.externalVolumeTransferFS("vault")
	if fsys == nil || !tok || scheme != nestedTypeTestProto {
		t.Fatalf("转存目标卷不可解析: fs=%v scheme=%q ok=%v", fsys, scheme, tok)
	}

	// ④ 装配不变式：封印生效（占用区间只读）+ 继承池挂源卷池之下。
	rt := h.volSet.Root("main")
	if rt == nil {
		t.Fatal("main 本地根未装配")
	}
	if rule := rt.SealAt("videos/x.bin"); rule == nil || rule.Wrapper != "vault" {
		t.Fatalf("源卷 main 的 videos 区间应被 vault 封印, rules=%+v", rt.Seals())
	}
	p, mp := h.volSet.Pool("vault"), h.volSet.Pool("main")
	if p == nil || p.MaxBytes() != 8<<20 || p.Parent() != mp {
		t.Fatalf("vault 继承池错误: pool=%+v parent=%v", p, p.Parent())
	}
	// 后端由 enclosure 路径构造（wrapper 区域视图），非按类型分派。
	if h.volSet.External("vault") == nil {
		t.Fatal("vault 后端应已挂回卷集（enclosure 路径补装）")
	}

	// ⑤ 声明仍可被 Parse 识别（删除时的引用检查/拆除清理依赖它）。
	decls, derr := enclosure.Parse(h.volSet.All())
	if derr != nil {
		t.Fatalf("Parse: %v", derr)
	}
	d, ok := declByName(decls, "vault")
	if !ok || d.Wrapper != nestedTypeTestType || d.Source != "main" || d.Path != "videos" {
		t.Fatalf("嵌套封装卷应仍是可解析声明: ok=%v decl=%+v", ok, d)
	}
}

// TestNestedDecl_FlatWrapperVolumeNotEnclosure 反向守卫：**非嵌套**的 wrapper 类型卷
// （type 已注册 wrapper、但 target 不是 `<卷>/<子路径>`）不得被误判为封装声明——仍走按类型
// 分派的常规构造，且不产生「继承池」等封装装配痕迹。
func TestNestedDecl_FlatWrapperVolumeNotEnclosure(t *testing.T) {
	t.Parallel()
	registerConfigWireBackend() // configWireTestType：既是 backend 也是 wrapper
	mainDir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = mainDir
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: mainDir, VolCapacity: 1 << 30},
		{Name: "flat", Type: configWireTestType, Extra: map[string]any{"target": "local"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	v, ok := h.volSet.ByName("flat")
	if !ok || v.Type != configWireTestType {
		t.Fatalf("flat 卷元数据错误: ok=%v type=%q", ok, v.Type)
	}
	if _, has := v.Extra[enclosure.SourceKey]; has {
		t.Fatalf("非嵌套形态不应产出 source 声明键: %+v", v.Extra)
	}
	if h.volSet.External("flat") == nil {
		t.Fatal("非嵌套 wrapper 类型卷应经 registry.NewBackend 常规装配")
	}
	if p := h.volSet.Pool("flat"); p == nil || p.Parent() != nil {
		t.Fatalf("非嵌套卷不应有继承池（parent 应为 nil）: %+v", p)
	}
}

// TestNestedDecl_RuntimeCreateKeepsWrapperType 运行期建卷 API 路径同口径：新建/删除的运行时卷
// 描述 Type 保持 wrapper 类型（回显一致），删除时声明仍被识别并完成封印回收。
func TestNestedDecl_RuntimeCreateKeepsWrapperType(t *testing.T) {
	t.Parallel()
	h, _ := newNestedWrapperAPIHandlers(t)
	mux := userVolWrap(h, "alice")
	const name = "vault-rt"
	rec := postUserVolume(t, mux, map[string]any{
		"name": name, "type": wrapperFSType,
		"extra": map[string]any{"target": "main/alice/user/docs"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("建嵌套封装卷 = %d (body: %s)", rec.Code, rec.Body.String())
	}
	v, ok := h.volSet.ByName(name)
	if !ok || v.Type != wrapperFSType {
		t.Fatalf("运行时卷描述 Type 应保持 wrapper 类型: ok=%v type=%q", ok, v.Type)
	}
	if got := volumesAPIType(t, h, name); got != wrapperFSType {
		t.Fatalf("/api/volumes 回显 type=%q, want %q", got, wrapperFSType)
	}
	// 删除：声明仍被识别（封印回收 + 运行时卷摘除）。
	rt := h.volSet.Root("main")
	req := httptest.NewRequest(http.MethodDelete, "/api/volumes/user?name="+name, nil)
	drec := httptest.NewRecorder()
	mux.ServeHTTP(drec, req)
	if drec.Code != http.StatusOK {
		t.Fatalf("删嵌套封装卷 = %d (body: %s)", drec.Code, drec.Body.String())
	}
	if h.volSet.External(name) != nil {
		t.Fatal("删除后应摘除运行时卷")
	}
	if rule := rt.SealAt("alice/user/docs/x.bin"); rule != nil {
		t.Fatalf("删除后源区间封印应回收, got %+v", rule)
	}
}

// TestIsDeclared_DeclarationVocabulary 钉住声明判据（装配机制的唯一事实源）：
//   - **wrapper 类型** + `extra.source` → 认领（嵌套形态，唯一形态）；
//   - wrapper 类型无 source（普通 wrapper 卷，如 target: local 的本地整卷形态）→ 不认领；
//   - 非 wrapper 类型（含 `type: enclosure`——它不是可写卷类型，LookupWrapper 恒不命中）
//     即使带 source → 不认领（普通卷零回归）。
func TestIsDeclared_DeclarationVocabulary(t *testing.T) {
	t.Parallel()
	registerFixtureWrapper("isdeclared-wrap")
	cases := []struct {
		name string
		vol  volume.Volume
		want bool
	}{
		{"嵌套 wrapper 声明", volume.Volume{Name: "v", Type: "isdeclared-wrap", Extra: map[string]any{"source": "main:x"}}, true},
		{"wrapper 类型无 source", volume.Volume{Name: "v", Type: "isdeclared-wrap", Extra: map[string]any{"target": "main/x"}}, false},
		{"非 wrapper 类型带 source", volume.Volume{Name: "v", Type: "plain-external", Extra: map[string]any{"source": "main:x"}}, false},
		{"type:enclosure 不再认领（非可写类型）", volume.Volume{Name: "v", Type: enclosure.TypeEnclosure, Extra: map[string]any{"source": "main:x", "wrapper": "isdeclared-wrap"}}, false},
		{"本地卷", volume.Volume{Name: "v", Type: volume.TypeLocal, Extra: map[string]any{"source": "main:x"}}, false},
	}
	for _, tc := range cases {
		if got := enclosure.IsDeclared(tc.vol); got != tc.want {
			t.Errorf("%s: IsDeclared=%v want %v", tc.name, got, tc.want)
		}
	}
}
