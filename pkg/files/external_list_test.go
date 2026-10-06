// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// external_list_test.go 是任务 7「/api/files?volume=<外部卷> 外部卷 ListDir 透传」的
// 域级测试：外部卷（secretdata 等无 *storage.Tenant 的卷）经装配层注入的
// ExternalVolumeSource 路由到后端 ListDir，返回明文目录视图（Name/Size/IsDir，
// 无 checksum/volume）；无权卷保持 404 不泄存在性。
//
// 与既有多卷族共用 dirsEnv 替身：enableExternalVolume 在卷集上挂载外部卷描述 +
// 目录浏览能力 fake，testRuntime.Volumes() 经 dirsExternalSet 包装成
// ExternalVolumeSource（生产等价物是 pkg/server 的 filesVolumeSet）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// dirsExternalSet 是 dirsEnv 的多卷集包装：嵌入 *registry.Set 继承 VolumeSet 方法，
// 并实现 ExternalVolumeSource（按卷名取外部卷目录浏览能力，来自 dirsEnv.externalVols）。
type dirsExternalSet struct {
	*registry.Set
	ext map[string]ExternalVolume
}

func (a dirsExternalSet) ExternalVolume(name string) ExternalVolume {
	return a.ext[name]
}

// enableExternalVolume 在卷集上挂载一个外部卷（卷描述 + 目录浏览能力 fake），重建
// Service。需要先启用至少一个本地默认卷（供 owner 租户解析与本地路径兼容）。外部卷
// 不建 storage.Root（无 *storage.Tenant，路由经 ExternalVolumeSource 直连 ListDir）。
func (e *dirsEnv) enableExternalVolume(t *testing.T, extVol volume.Volume, ext ExternalVolume) {
	t.Helper()
	if e.volSet == nil {
		e.enableVolumes(t, "default")
	}
	if e.externalVols == nil {
		e.externalVols = map[string]ExternalVolume{}
	}
	e.externalVols[extVol.Name] = ext
	e.volSet = registry.NewSet(append(e.volSet.All(), extVol), e.volRoots, nil, e.pools, e.volSet.Default().Name)
	e.rebuild()
}

// fakeExternalVolume 是 ExternalVolume 的测试 fake：固定返回条目，记录 ListDir 收到的
// rel（断言 owner 限定），listErr 非 nil 时模拟后端失败。
type fakeExternalVolume struct {
	entries []ExternalEntry
	listErr error
	gotRel  atomic.Value // string：最近一次 ListDir 的 rel（"not-called" 初始）
}

func (f *fakeExternalVolume) ListDir(_ context.Context, rel string) ([]ExternalEntry, error) {
	f.gotRel.Store(rel)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.entries, nil
}

// listFiles 用 httptest 走 ListFiles 处理器（GET /api/files），返回 recorder。
func listFiles(svc *Service, actor, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/files"+query, nil)
	req.Header.Set("X-Test-Actor", actor)
	rec := httptest.NewRecorder()
	svc.ListFiles(rec, req)
	return rec
}

// TestList_ExternalVolume_PlainMeta 钉住：?volume=<外部卷> 时 List 透传到该卷 ListDir，
// 返回明文目录视图（movie.mp4 明文大小 2048、dir 目录条目），且不带 checksum 字段
// （外部卷目录视图不暴露加密元数据；卷名标注 v.Name 供 Web UI 卷徽标）；ListDir 收到
// 的 rel 是 owner 限定的键空间（共享卷 owner 前缀隔离）。
func TestList_ExternalVolume_PlainMeta(t *testing.T) {
	t.Parallel()
	e := newDirsEnv(t)
	fake := &fakeExternalVolume{entries: []ExternalEntry{
		{Name: "dir", IsDir: true},
		{Name: "movie.mp4", Size: 2048},
	}}
	e.enableExternalVolume(t, volume.Volume{Name: "secrets-x", Type: "secretdata"}, fake)

	rec := listFiles(e.svc, "ownerA", "?volume=secrets-x&subdir=")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/files?volume=secrets-x 状态码 = %d, 期望 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp ListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v; body=%s", err, rec.Body.String())
	}
	if resp.Total != 2 {
		t.Fatalf("total = %d, 期望 2; files=%+v", resp.Total, resp.Files)
	}
	// movie.mp4：明文大小 + 卷名标注（供 Web UI 卷徽标）+ 无 checksum（外部卷目录
	// 视图不暴露加密元数据）。
	found := false
	for _, f := range resp.Files {
		switch f.Name {
		case "dir":
			if !f.IsDir || f.Size != 0 {
				t.Fatalf("dir 条目 = %+v, 期望 IsDir=true Size=0", f)
			}
		case "movie.mp4":
			found = true
			if f.IsDir {
				t.Fatalf("movie.mp4 误标为目录: %+v", f)
			}
			if f.Size != 2048 {
				t.Fatalf("movie.mp4 明文大小 = %d, 期望 2048", f.Size)
			}
			if f.Checksum != "" {
				t.Fatalf("movie.mp4 泄漏 checksum 字段: %+v", f)
			}
			if f.Volume != "secrets-x" {
				t.Fatalf("movie.mp4 卷 = %q, 期望 secrets-x（外部卷目录视图标注卷名供徽标）", f.Volume)
			}
			// 任务 9：wrapper 加密卷（Type=secretdata）的条目补 category 标注（供 Web UI
			// 卷徽标细化），目录条目不填。
			if f.VolumeCategory != "wrapper" {
				t.Fatalf("movie.mp4 卷分类 = %q, 期望 wrapper（secretdata 是 wrapper 卷）", f.VolumeCategory)
			}
		}
	}
	if !found {
		t.Fatalf("响应缺 movie.mp4; files=%+v", resp.Files)
	}
	// ListDir 收到 owner 限定的键空间（共享卷带 owner 前缀）。
	if got, _ := fake.gotRel.Load().(string); got != "ownerA/user" {
		t.Fatalf("ListDir rel = %q, 期望 ownerA/user（owner 限定）", got)
	}
}

// TestList_ExternalVolume_CategoryOnlyWrapper 钉住任务 9：非 wrapper 外部卷（如 baidupcs，
// 普通 linked 类型）的列表条目不填 volume_category（零值 omitempty，避免误标 🔒）。
func TestList_ExternalVolume_CategoryOnlyWrapper(t *testing.T) {
	t.Parallel()
	e := newDirsEnv(t)
	fake := &fakeExternalVolume{entries: []ExternalEntry{{Name: "a.bin", Size: 10}}}
	e.enableExternalVolume(t, volume.Volume{Name: "linked-x", Type: "baidupcs"}, fake)

	rec := listFiles(e.svc, "ownerA", "?volume=linked-x&subdir=")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/files?volume=linked-x 状态码 = %d, 期望 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp ListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v; body=%s", err, rec.Body.String())
	}
	for _, f := range resp.Files {
		if f.VolumeCategory != "" {
			t.Fatalf("linked 类型外部卷不应填 volume_category: %+v", f)
		}
	}
}

// TestList_ExternalVolume_Unauthorized_404 钉住安全红线：无 ACL 的 owner 请求
// ?volume=<他人外部卷> → 404（fail-closed 不泄卷存在性，与既有 ?volume= 行为一致），
// 且不触发该卷的 ListDir。
func TestList_ExternalVolume_Unauthorized_404(t *testing.T) {
	t.Parallel()
	e := newDirsEnv(t)
	fake := &fakeExternalVolume{entries: []ExternalEntry{{Name: "movie.mp4", Size: 2048}}}
	e.enableExternalVolume(t, volume.Volume{
		Name: "secrets-x",
		Type: "secretdata",
		ACL:  volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{"otherOwner": {}}},
	}, fake)

	rec := listFiles(e.svc, "ownerA", "?volume=secrets-x&subdir=")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("无权 owner 状态码 = %d, 期望 404; body=%s", rec.Code, rec.Body.String())
	}
	if got, _ := fake.gotRel.Load().(string); got != "" {
		t.Fatalf("无权 owner 不应触发 ListDir（泄露卷存在性）; rel=%q", got)
	}
}
