// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enclosure

// nested_test.go 锁定**用户面 target 语法 → 装配侧声明词汇**的翻译兼容面（纯函数，随
// `pkg/server/enclosure` 切片整体迁移）。装配级行为（封印注入 / 容量池继承 / API 回显 /
// 声明识别）由装配根 `pkg/server` 的集成测试覆盖。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	volenclosure "github.com/cocomhub/sproxy/pkg/volume/enclosure"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// translateTestWrapper 是翻译测试用的封装处理层替身：翻译只查注册表（LookupWrapper），
// 不构造后端，故 Build 恒报错——若误走装配路径会立即暴露。
type translateTestWrapper struct{ name string }

func (w translateTestWrapper) Name() string { return w.name }

func (w translateTestWrapper) Build(context.Context, volume.Volume, syncpkg.FS, map[string]any) (registry.ExternalBackend, error) {
	return nil, errors.New("translate-test wrapper: 翻译测试不应构造后端")
}

var translateWrapperOnce sync.Map // name → *sync.Once

// registerTranslateWrapper 注册翻译测试用封装处理层（同名幂等；RegisterWrapper 重复注册会 panic）。
func registerTranslateWrapper(name string) {
	onceAny, _ := translateWrapperOnce.LoadOrStore(name, &sync.Once{})
	onceAny.(*sync.Once).Do(func() { volenclosure.RegisterWrapper(translateTestWrapper{name: name}) })
}

// TestTranslateNestedVolume 用户面 → 装配侧声明词汇的翻译语义（纯函数，钉住兼容面）：
//   - 嵌套 target + 已注册 wrapper 类型 → 翻译（补 source，**Type 不变**，原 target 保留）；
//   - 非嵌套（无分隔符 / `local` / 卷名或子路径空）→ 不翻译；
//   - 非封装处理层类型 → 不翻译（普通外部卷零回归）；
//   - `type: enclosure`（拒绝判据 token，非可写卷类型 / 非 wrapper 注册名）→ 恒定不翻译。
func TestTranslateNestedVolume(t *testing.T) {
	t.Parallel()
	registerTranslateWrapper("translate-test-wrap")
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
		{"type:enclosure（非可写类型）不翻译", volume.Volume{Name: "v", Type: volenclosure.TypeEnclosure, Extra: map[string]any{"target": "main/videos", "source": "main:videos"}}, false, ""},
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
		if got.Type != tc.vol.Type {
			t.Errorf("%s: 翻译不得改写 Type（对外身份 = wrapper 类型），got %q want %q", tc.name, got.Type, tc.vol.Type)
		}
		if got.Extra["source"] != tc.wantSrc {
			t.Errorf("%s: 翻译产物错误 type=%q extra=%+v", tc.name, got.Type, got.Extra)
		}
		// wrapper 键**不再写出**（用户裁定 2026-10-15：wrapper 恒等于卷类型 ⇒ 冗余键，
		// 装配侧读取时缺省回落 v.Type；显式写出且不一致则 fail-closed）。
		if _, has := got.Extra["wrapper"]; has {
			t.Errorf("%s: 翻译不得再写冗余 wrapper 键, got %v", tc.name, got.Extra["wrapper"])
		}
		if got.Extra["target"] != tc.vol.Extra["target"] {
			t.Errorf("%s: 原 target 应保留（回显/审计）", tc.name)
		}
	}
}

// TestTranslateNestedVolume_CapacityToMaxBytes 卷级容量翻译为声明自身上限（语义等价）：
// 显式 max_bytes 优先，不被 capacity 覆盖。
func TestTranslateNestedVolume_CapacityToMaxBytes(t *testing.T) {
	t.Parallel()
	registerTranslateWrapper("translate-cap-wrap")
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

// TestParseNestedTarget 用户面嵌套语法 `<卷名>/<子路径>` 的解析边界：返回 base（卷名）+
// sub（去首尾 `/`，可多级）；缺分隔符 / 卷名空 / 子路径空 ⇒ ok=false。
func TestParseNestedTarget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		target   string
		wantBase string
		wantSub  string
		wantOK   bool
	}{
		{"空串", "", "", "", false},
		{"纯空白", "   ", "", "", false},
		{"无分隔符", "main", "", "", false},
		{"前导斜杠（卷名空）", "/videos", "", "", false},
		{"尾斜杠（子路径空）", "main/", "", "", false},
		{"仅斜杠", "///", "", "", false},
		{"单级子路径", "main/videos", "main", "videos", true},
		{"多级子路径", "main/a/b", "main", "a/b", true},
		{"首尾空白", " main/videos ", "main", "videos", true},
		{"重复斜杠归一", "main//videos", "main", "videos", true},
		{"尾斜杠归一", "main/videos/", "main", "videos", true},
	}
	for _, tc := range cases {
		base, sub, ok := ParseNestedTarget(tc.target)
		if ok != tc.wantOK || base != tc.wantBase || sub != tc.wantSub {
			t.Errorf("%s: ParseNestedTarget(%q)=(%q,%q,%v), want (%q,%q,%v)",
				tc.name, tc.target, base, sub, ok, tc.wantBase, tc.wantSub, tc.wantOK)
		}
	}
}

// TestTranslateNestedDecl_NilExtraAndEmptyType 翻译入口的 nil/空输入安全：空类型、nil extra、
// 缺 target 均不翻译且原样返回（调用方走既有路径，零回归）。
func TestTranslateNestedDecl_NilExtraAndEmptyType(t *testing.T) {
	t.Parallel()
	registerTranslateWrapper("translate-decl-wrap")
	cases := []struct {
		name  string
		typ   string
		extra map[string]any
	}{
		{"空类型", "", nil},
		{"本地卷", volume.TypeLocal, map[string]any{"target": "main/videos"}},
		{"nil extra", "translate-decl-wrap", nil},
		{"缺 target", "translate-decl-wrap", map[string]any{"root": "/tmp/x"}},
	}
	for _, tc := range cases {
		typ, extra, ok := TranslateNestedDecl(tc.typ, tc.extra, 4096)
		if ok {
			t.Errorf("%s: 不应翻译, got type=%q extra=%+v", tc.name, typ, extra)
			continue
		}
		if typ != tc.typ {
			t.Errorf("%s: 未翻译时 type 应原样返回, got %q want %q", tc.name, typ, tc.typ)
		}
		if len(extra) != len(tc.extra) {
			t.Errorf("%s: 未翻译时 extra 应原样返回, got %+v want %+v", tc.name, extra, tc.extra)
		}
	}
}
