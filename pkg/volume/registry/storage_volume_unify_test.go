// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package registry

// storage_volume_unify_test.go —— 统一存储卷模型 U1/U2/U4 的接入测试。
//
// 设计：`docs/designs/2026-10-28-unified-volume-model.md`（本文件覆盖 U1 的
// `Set.StorageVolume` / `RegisterStorageVolume`；U2 的 `VolumeView` 与 U4 的
// `local://` 解析在同一文件追加，便于对照「同一 Set 的统一视图」）。
//
// `fakeStorageVolume`（接缝证明的最小实现）已在 storage_volume_kind_test.go 定义，
// 本文件复用，不再重复。

import (
	"context"
	"io"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// TestSet_StorageVolume_LocalReturnsBackend 钉住：装配层把本地卷（`localBackend`）登记后，
// `Set.StorageVolume(name)` 能取到**统一身份**（而非只能经 `Set.Root` 拿裸 root）。
func TestSet_StorageVolume_LocalReturnsBackend(t *testing.T) {
	t.Parallel()
	set := NewSet([]volume.Volume{{Name: "v1", Type: volume.TypeLocal}},
		nil, nil, nil, "v1")
	if err := set.RegisterStorageVolume(fakeStorageVolume{name: "v1", fs: &stubFS{}}); err != nil {
		t.Fatalf("RegisterStorageVolume: %v", err)
	}
	sv, ok := set.StorageVolume("v1")
	if !ok || sv == nil {
		t.Fatal("本地卷应经 StorageVolume 访问器取到")
	}
	if sv.Name() != "v1" || sv.Kind() != volume.KindStorage {
		t.Fatalf("Name/Kind 错误: %v %v", sv.Name(), sv.Kind())
	}
}

// TestSet_StorageVolume_UnknownReturnsFalse 钉住未知卷名 fail-closed（不返回 nil,nil 之外的
// 伪成功）。
func TestSet_StorageVolume_UnknownReturnsFalse(t *testing.T) {
	t.Parallel()
	set := NewSet(nil, nil, nil, nil, "default")
	if sv, ok := set.StorageVolume("nope"); ok || sv != nil {
		t.Fatalf("未知卷名应 (nil,false)，got %v %v", sv, ok)
	}
}

// stubFS 是 syncpkg.FS 的最小实现（测试用，仅 OpenRead/Stat 有内容语义）。
type stubFS struct {
	content map[string]string
}

func (s *stubFS) ListDir(context.Context, string) ([]syncpkg.Entry, error) { return nil, nil }
func (s *stubFS) Stat(context.Context, string) (*syncpkg.Entry, error)     { return nil, nil }
func (s *stubFS) OpenRead(_ context.Context, path string) (io.ReadCloser, error) {
	if s.content == nil {
		return nil, syncpkg.ErrUnsupported
	}
	v, ok := s.content[path]
	if !ok {
		return nil, syncpkg.ErrUnsupported
	}
	return io.NopCloser(strings.NewReader(v)), nil
}
func (s *stubFS) WriteFile(context.Context, syncpkg.LogicalKey, string, io.Reader, int64, int64) error {
	return nil
}
func (s *stubFS) Rename(context.Context, string, string) error { return nil }
func (s *stubFS) Delete(context.Context, string) error         { return nil }
func (s *stubFS) MakeDir(context.Context, string) error        { return nil }
