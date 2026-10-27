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
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files/meta"
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

// ---- U2：VolumeView 按用途访问器 ----

// decoratedFS 是装饰后的标记 FS（区分 raw 与装饰产物）。
type decoratedFS struct {
	stubFS
	use ViewUse
}

// TestSet_VolumeView_UserViewDecorated 钉住：`ViewUser` 经注入 decorator 装饰（返回 ≠ raw）；
// secrets use 不入通用表（R26 白名单维持）；未知卷名 fail-closed。
func TestSet_VolumeView_UserViewDecorated(t *testing.T) {
	t.Parallel()
	raw := &stubFS{}
	called := 0
	set := NewSet([]volume.Volume{{Name: "v1", Type: volume.TypeLocal}}, nil, nil, nil, "v1")
	set.SetVolumeViewDecorator(func(name string, use ViewUse, f syncpkg.FS) syncpkg.FS {
		called++
		return &decoratedFS{stubFS: stubFS{}, use: use}
	})
	if err := set.RegisterStorageVolume(fakeStorageVolume{name: "v1", fs: raw}); err != nil {
		t.Fatalf("RegisterStorageVolume: %v", err)
	}
	got, ok := set.VolumeView("v1", ViewUser)
	if !ok || got == nil {
		t.Fatal("ViewUser 应返回视图")
	}
	if called != 1 {
		t.Fatalf("decorator 调用次数 = %d, want 1", called)
	}
	if got == syncpkg.FS(raw) {
		t.Fatal("ViewUser 应返回装饰后视图（≠ raw）")
	}
	if d, ok := got.(*decoratedFS); !ok || d.use != ViewUser {
		t.Fatal("ViewUser 应经 decorator 且 use 正确传递")
	}
	// secrets 不入通用表（R26 白名单维持）。
	if _, ok := set.VolumeView("v1", ViewUse("secrets")); ok {
		t.Fatal("secrets use 应被拒（不入通用表）")
	}
	if _, ok := set.VolumeView("nope", ViewUser); ok {
		t.Fatal("未知卷名应 (nil,false)")
	}
}

// TestSet_VolumeView_MetaViewUse 钉住 ViewMeta 经同一 decorator（use 区分）。
func TestSet_VolumeView_MetaViewUse(t *testing.T) {
	t.Parallel()
	set := NewSet([]volume.Volume{{Name: "v1", Type: volume.TypeLocal}}, nil, nil, nil, "v1")
	set.SetVolumeViewDecorator(func(name string, use ViewUse, f syncpkg.FS) syncpkg.FS {
		return &decoratedFS{use: use}
	})
	if err := set.RegisterStorageVolume(fakeStorageVolume{name: "v1", fs: &stubFS{}}); err != nil {
		t.Fatalf("RegisterStorageVolume: %v", err)
	}
	got, ok := set.VolumeView("v1", ViewMeta)
	if !ok {
		t.Fatal("ViewMeta 应返回视图")
	}
	if d, ok := got.(*decoratedFS); !ok || d.use != ViewMeta {
		t.Fatal("ViewMeta 的 use 应正确传递")
	}
}

// TestSet_VolumeView_DefaultDecoratorIsTrusted 钉住：未注入 decorator 时**内置 trusted 装饰链**
// 生效（Ruling，2026-10-28）——生产与测试同为 Guard(Wrap) 语义（meta sidecar 生成/校验、
// Delete 联动 sidecar+记账），不依赖装配层记得注入；避免「生产有装饰、测试裸 FS」漂移。
func TestSet_VolumeView_DefaultDecoratorIsTrusted(t *testing.T) {
	t.Parallel()
	raw := &stubFS{}
	set := NewSet([]volume.Volume{{Name: "v1", Type: volume.TypeLocal}}, nil, nil, nil, "v1")
	if err := set.RegisterStorageVolume(fakeStorageVolume{name: "v1", fs: raw}); err != nil {
		t.Fatalf("RegisterStorageVolume: %v", err)
	}
	got, ok := set.VolumeView("v1", ViewUser)
	if !ok || got == nil {
		t.Fatal("应返回视图")
	}
	if got == syncpkg.FS(raw) {
		t.Fatal("默认装饰器应套 trusted.Guard(Wrap)——不得返回裸 FS")
	}
	// 视图应实现 meta.Provider（Guard/Wrap 的 FileMeta 读侧）。
	if _, ok := got.(meta.Provider); !ok {
		t.Fatal("默认装饰视图应实现 meta.Provider")
	}
}

// ---- U4：local:// scheme 解析 ----

// TestSet_ResolveURL_LocalScheme 钉住 `local://<卷>/<rel>` 经统一访问器读本地卷内容。
func TestSet_ResolveURL_LocalScheme(t *testing.T) {
	t.Parallel()
	set := NewSet([]volume.Volume{{Name: "v1", Type: volume.TypeLocal}}, nil, nil, nil, "v1")
	if err := set.RegisterStorageVolume(fakeStorageVolume{
		name: "v1", fs: &stubFS{content: map[string]string{"anonymous/user/a.txt": "hello"}},
	}); err != nil {
		t.Fatalf("RegisterStorageVolume: %v", err)
	}
	// 让 local 协议已声明（测试进程内注册一次；用 Once 防重注册 panic）。
	registerLocalTestScheme()
	rc, err := set.ResolveURL(context.Background(), "local://v1/a.txt")
	if err != nil {
		t.Fatalf("local:// 解析失败: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("读内容: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("内容 = %q, want %q", got, "hello")
	}
}

// TestSet_ResolveURL_LocalScheme_TraversalRejected 钉住路径穿越被拒（fail-closed）。
func TestSet_ResolveURL_LocalScheme_TraversalRejected(t *testing.T) {
	t.Parallel()
	set := NewSet([]volume.Volume{{Name: "v1", Type: volume.TypeLocal}}, nil, nil, nil, "v1")
	if err := set.RegisterStorageVolume(fakeStorageVolume{name: "v1", fs: &stubFS{}}); err != nil {
		t.Fatalf("RegisterStorageVolume: %v", err)
	}
	registerLocalTestScheme()
	if _, err := set.ResolveURL(context.Background(), "local://v1/../../etc/passwd"); err == nil {
		t.Fatal("路径穿越应被拒（fail-closed）")
	}
}

// TestSet_ResolveURL_LocalScheme_UnknownVolume 钉住未知卷 fail-closed。
func TestSet_ResolveURL_LocalScheme_UnknownVolume(t *testing.T) {
	t.Parallel()
	set := NewSet(nil, nil, nil, nil, "v1")
	registerLocalTestScheme()
	if _, err := set.ResolveURL(context.Background(), "local://nope/a.txt"); err == nil {
		t.Fatal("未知卷应报错（fail-closed）")
	}
}

var localTestSchemeOnce sync.Once

// registerLocalTestScheme 在测试进程内声明 `local` 协议（RegisterBackend 重名 panic ⇒ Once）。
func registerLocalTestScheme() {
	localTestSchemeOnce.Do(func() {
		RegisterBackend(volume.TypeLocal, func(context.Context, volume.Volume) (ExternalBackend, error) {
			return nil, errors.New("test local backend 不用于构造")
		}, "local")
	})
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
