// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// volume_category_test.go 钉住**决策 3「卷类型分类改注入式」**：wrapper 分类的事实源在
// registry（`CategoryOf`，含 wrapper 处理层注册表的动态投影），领域包不得 import
// pkg/volume/registry（门禁 R2）——故经可选能力 VolumeTypeClassifier 由装配层注入，
// **领域侧不再硬编码类型名单**；未注入时回落内置基线（零回归）。
//
// 三类断言：
//   - (a) 注入分类器时，第三方 wrapper 类型（测试经 registry.MarkWrapperType 登记，
//     与生产封装处理层注册同路径）在外部卷列表条目上拿到 volume_category == "wrapper"；
//   - (b) 未注入 → 回落内置基线（secretdata → wrapper）；
//   - (c) 非 wrapper 类型不被误标。

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// registryForwardClassifier 镜像装配层适配器（pkg/server 的 filesVolumeSet.CategoryOf
// → registry.CategoryOf）：领域侧不经 registry 判分类，测试注入本转发器以验证「经
// MarkWrapperType 登记的第三方 wrapper 类型」能流入列表分类。
type registryForwardClassifier struct{}

func (registryForwardClassifier) CategoryOf(typ string) string { return registry.CategoryOf(typ) }

// stubVolumeClassifier 是可控分类器（按表返回；未命中 → linked，与 registry 对未登记
// 外部类型的口径一致）。
type stubVolumeClassifier struct{ cats map[string]string }

func (c stubVolumeClassifier) CategoryOf(typ string) string {
	if cat, ok := c.cats[typ]; ok {
		return cat
	}
	return "linked"
}

// externalListCategory 走 ListFiles 处理器（GET /api/files?volume=<vol>）取首个**文件**
// 条目的 volume_category。
func externalListCategory(t *testing.T, svc *Service, vol string) string {
	t.Helper()
	rec := listFiles(svc, "ownerA", "?volume="+vol+"&subdir=")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/files?volume=%s 状态码 = %d, 期望 200; body=%s", vol, rec.Code, rec.Body.String())
	}
	var resp ListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v; body=%s", err, rec.Body.String())
	}
	for _, f := range resp.Files {
		if !f.IsDir {
			return f.VolumeCategory
		}
	}
	t.Fatalf("?volume=%s 响应无文件条目: %+v", vol, resp.Files)
	return ""
}

// TestVolumeCategoryFor_FallbackWithoutClassifier 钉住 (b)：**未注入分类器**时回落内置
// 基线名单（wrapper 三类型标 wrapper，其余不填）——测试/旧装配零回归。
func TestVolumeCategoryFor_FallbackWithoutClassifier(t *testing.T) {
	t.Parallel()
	e := newDirsEnv(t) // dirsEnv 默认不注入分类器
	for _, tc := range []struct{ typ, want string }{
		{"secretdata", "wrapper"},
		{"secrets", "wrapper"},
		{"egress", "wrapper"},
		{"baidupcs", ""},
		{"local", ""},
		{"", ""},
	} {
		if got := e.svc.volumeCategoryFor(tc.typ); got != tc.want {
			t.Errorf("未注入分类器时 volumeCategoryFor(%q) = %q, 期望 %q（内置基线兜底）", tc.typ, got, tc.want)
		}
	}
}

// TestVolumeCategoryFor_InjectedClassifierWins 钉住**去硬编码**的核心语义：注入的分类器
// **优先于内置名单**——分类器的判断既可变（把内置的 secretdata 判为非 wrapper）也可扩
// （判定内置名单没有的新类型）。
func TestVolumeCategoryFor_InjectedClassifierWins(t *testing.T) {
	t.Parallel()
	e := newDirsEnv(t)
	e.volumeClassifier = stubVolumeClassifier{cats: map[string]string{
		"secretdata": "linked",  // 与内置名单相反 → 必须以分类器为准（证明名单不再生效）
		"xxx-enc":    "wrapper", // 内置名单没有的新类型 → 必须以分类器为准（证明可扩展）
	}}
	e.rebuild()

	if got := e.svc.volumeCategoryFor("secretdata"); got != "" {
		t.Errorf("分类器把 secretdata 归 linked 时应不填分类，got %q（分类器须优先于内置名单）", got)
	}
	if got := e.svc.volumeCategoryFor("xxx-enc"); got != "wrapper" {
		t.Errorf("分类器把 xxx-enc 归 wrapper 时应填 wrapper，got %q", got)
	}
	if got := e.svc.volumeCategoryFor("baidupcs"); got != "" {
		t.Errorf("分类器把 baidupcs 归 linked 时应不填分类，got %q", got)
	}
}

// TestList_ExternalVolume_Category_ThirdPartyWrapperType 钉住 (a)：**第三方 wrapper 类型**
// （测试经 registry.MarkWrapperType 登记，与生产封装处理层注册路径同构）在外部卷列表条目
// 上拿到 volume_category == "wrapper"。若分类仍靠领域侧硬编码名单，本用例必红
// （该类型名不在名单里）。
func TestList_ExternalVolume_Category_ThirdPartyWrapperType(t *testing.T) {
	t.Parallel()
	const typ = "files-category-third-party-wrapper"
	// 登记为 wrapper category（与生产封装处理层注册同路径），用例结束经
	// UnmarkWrapperTypeForTest 撤销——免全局永久登记污染其它用例。
	registry.MarkWrapperType(typ)
	t.Cleanup(func() { registry.UnmarkWrapperTypeForTest(typ) })

	e := newDirsEnv(t)
	e.volumeClassifier = registryForwardClassifier{} // 装配层等价适配器
	fake := &fakeExternalVolume{entries: []ExternalEntry{
		{Name: "dir", IsDir: true},
		{Name: "a.bin", Size: 10},
	}}
	e.enableExternalVolume(t, volume.Volume{Name: "third-x", Type: typ}, fake)

	if got := externalListCategory(t, e.svc, "third-x"); got != "wrapper" {
		t.Fatalf("第三方 wrapper 类型 %q 的条目 volume_category = %q, 期望 wrapper"+
			"（分类须经注入的分类器，而非领域侧硬编码名单）", typ, got)
	}
}

// TestList_ExternalVolume_Category_NonWrapperNotLabeled 钉住 (c)：注入分类器后，非 wrapper
// 类型仍不填 volume_category（避免 Web UI 对普通 linked 卷误显 🔒）。
func TestList_ExternalVolume_Category_NonWrapperNotLabeled(t *testing.T) {
	t.Parallel()
	e := newDirsEnv(t)
	e.volumeClassifier = registryForwardClassifier{}
	fake := &fakeExternalVolume{entries: []ExternalEntry{{Name: "a.bin", Size: 10}}}
	e.enableExternalVolume(t, volume.Volume{Name: "linked-y", Type: "baidupcs"}, fake)

	if got := externalListCategory(t, e.svc, "linked-y"); got != "" {
		t.Fatalf("linked 类型卷条目 volume_category = %q, 期望空（不误标 wrapper）", got)
	}
}
