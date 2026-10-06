// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_links_test.go 验证嵌套封装（已创建卷 + 新子目录）的互斥占用 + 关联生命周期：
//   - 占用冲突（同目录/父/子）409；同卷不同分支、跨卷放行；
//   - 子目录已存在 → 409 不创建；
//   - 删底层卷有引用 → 409；删封装卷清关联 → 可删底层。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// nestedRootType 是可作嵌套底层的用户卷 backend（FS 为 LocalFS，供 FSFor/Stat 解析底层
// 子目录存在性）。factory 从 extra.root 建本地根。
const nestedRootType = "nested-volroot"

// nestedRootBackend 以 LocalFS 为 FS 的用户卷 backend。
type nestedRootBackend struct {
	fs syncpkg.FS
}

func (b *nestedRootBackend) FS() syncpkg.FS  { return b.fs }
func (b *nestedRootBackend) Close() error    { return nil }
func (b *nestedRootBackend) Usage() int64    { return 0 }
func (b *nestedRootBackend) Capacity() int64 { return 0 }

var registerNestedRootOnce sync.Once

func registerNestedRootBackend() {
	registerNestedRootOnce.Do(func() {
		registry.RegisterBackend(nestedRootType, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
			root, _ := v.Extra["root"].(string)
			if root == "" {
				return nil, fmt.Errorf("nested-root: extra.root 必填")
			}
			return &nestedRootBackend{fs: syncpkg.NewLocalFS(root, nil)}, nil
		})
	})
}

// newNestedWrapperAPIHandlers 装配嵌套封装测试 Handlers（main 本地卷 + 用户卷 store +
// wrapper 类型 + nested 底层类型）。
func newNestedWrapperAPIHandlers(t *testing.T) (*Handlers, *UserVolumeStore) {
	t.Helper()
	registerUserVolWrapBackend() // wrapper 类型（Schema target, allow_wrapper）
	registerNestedRootBackend()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	store := NewUserVolumeStore(cfg.StorageRoot)
	h.SetUserVolumeStore(store)
	return h, store
}

// postUserVol 发送创建用户卷请求。
func postUserVol(t *testing.T, mux *http.ServeMux, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/volumes/user", bytes.NewReader(data))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// delUserVol 发送删除用户卷请求。
func delUserVol(t *testing.T, mux *http.ServeMux, name string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/api/volumes/user?name="+name, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestNested_Occupancy_OverlapRejected 互斥占用：同目录/子目录冲突 409；不同分支放行。
func TestNested_Occupancy_OverlapRejected(t *testing.T) {
	t.Parallel()
	h, _ := newNestedWrapperAPIHandlers(t)
	mux := userVolWrap(h, "alice")
	wrap := func(name, target string) *httptest.ResponseRecorder {
		return postUserVol(t, mux, map[string]any{
			"name": name, "type": userVolWrapTestType,
			"extra": map[string]any{"target": target},
		})
	}
	// 首个占用 main/videos → 200 + 登记关联
	if rec := wrap("wrap1", "main/videos"); rec.Code != http.StatusOK {
		t.Fatalf("wrap1 main/videos = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if refs := h.links().refsOfBase("main"); len(refs) != 1 || refs[0].Wrapper != "wrap1" || refs[0].Subdir != "videos" {
		t.Fatalf("wrap1 后 main 应恰 1 条关联 {videos,wrap1}, got %+v", refs)
	}
	// 同目录冲突
	if rec := wrap("wrap2", "main/videos"); rec.Code != http.StatusConflict {
		t.Fatalf("wrap2 main/videos(同目录) = %d, want 409", rec.Code)
	}
	// 子目录冲突（wrap1 的 videos 是其父）
	if rec := wrap("wrap3", "main/videos/sub"); rec.Code != http.StatusConflict {
		t.Fatalf("wrap3 main/videos/sub(子路径) = %d, want 409", rec.Code)
	}
	// 父目录冲突：先占 main/album/photos，再占其父 main/album → 409
	if rec := wrap("wrapA", "main/album/photos"); rec.Code != http.StatusOK {
		t.Fatalf("wrapA main/album/photos = %d, want 200", rec.Code)
	}
	if rec := wrap("wrapB", "main/album"); rec.Code != http.StatusConflict {
		t.Fatalf("wrapB main/album(父路径) = %d, want 409", rec.Code)
	}
	// 前缀相似但不重叠（main/vide 非 videos 的父/子/同目录）→ 放行
	if rec := wrap("wrapC", "main/vide"); rec.Code != http.StatusOK {
		t.Fatalf("wrapC main/vide = %d, want 200 (前缀相似不重叠)", rec.Code)
	}
	// 同卷不同分支放行
	if rec := wrap("wrap5", "main/photos"); rec.Code != http.StatusOK {
		t.Fatalf("wrap5 main/photos = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if rec := wrap("wrap6", "main/movies"); rec.Code != http.StatusOK {
		t.Fatalf("wrap6 main/movies = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestNested_SubdirExists_409 子目录已存在（防与底层卷既有数据混合）→ 409 不创建。
func TestNested_SubdirExists_409(t *testing.T) {
	t.Parallel()
	h, _ := newNestedWrapperAPIHandlers(t)
	// 预创建 main 根下的 videos 目录（模拟既有数据）
	if err := os.MkdirAll(filepath.Join(h.volSet.Default().RootDir, "videos"), 0o755); err != nil {
		t.Fatalf("mkdir main/videos: %v", err)
	}
	mux := userVolWrap(h, "alice")
	rec := postUserVol(t, mux, map[string]any{
		"name": "wrap1", "type": userVolWrapTestType,
		"extra": map[string]any{"target": "main/videos"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("子目录已存在 = %d, want 409 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestNested_Lifecycle_DeleteBottomRefs 关联生命周期：建 wrapper 登记 → 删底层 409 →
// 删 wrapper 清登记 → 可删底层。
func TestNested_Lifecycle_DeleteBottomRefs(t *testing.T) {
	t.Parallel()
	h, _ := newNestedWrapperAPIHandlers(t)
	mux := userVolWrap(h, "alice")
	root := t.TempDir()
	// 建用户底层卷 basevol + 封装卷 wrap 指向 basevol/vault
	if rec := postUserVol(t, mux, map[string]any{
		"name": "basevol", "type": nestedRootType,
		"extra": map[string]any{"root": root},
	}); rec.Code != http.StatusOK {
		t.Fatalf("basevol = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if rec := postUserVol(t, mux, map[string]any{
		"name": "wrapV", "type": userVolWrapTestType,
		"extra": map[string]any{"target": "basevol/vault"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("wrapV = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	// 底层卷被引用 → 删 basevol 409
	if rec := delUserVol(t, mux, "basevol"); rec.Code != http.StatusConflict {
		t.Fatalf("删被引用的 basevol = %d, want 409 (body: %s)", rec.Code, rec.Body.String())
	}
	// 删封装卷 wrapV → 清关联
	if rec := delUserVol(t, mux, "wrapV"); rec.Code != http.StatusOK {
		t.Fatalf("删 wrapV = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(h.links().refsOfBase("basevol")) != 0 {
		t.Fatalf("删 wrapV 后 basevol 关联未清空: %+v", h.links().refsOfBase("basevol"))
	}
	// 底层卷不再被引用 → 可删
	if rec := delUserVol(t, mux, "basevol"); rec.Code != http.StatusOK {
		t.Fatalf("删 basevol = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}
