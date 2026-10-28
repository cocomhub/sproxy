// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"errors"
	"testing"

	"github.com/cocomhub/sproxy/pkg/downloader"
)

// fakeIdemAPI 测试用内存 API（幂等查找最小面）。
type fakeIdemAPI struct {
	root    []FileMeta           // 网盘根
	pack    []FileMeta           // Pack From Shared 内容
	files   map[string]*FileMeta // 全盘（FindByID/ListRecursive 用）
	parents map[string]*FileMeta // parent ID → 元信息
}

func (f *fakeIdemAPI) List(_ context.Context, parentID string) ([]FileMeta, error) {
	if parentID == "" {
		return f.root, nil
	}
	if parentID == "pack" {
		return f.pack, nil
	}
	return nil, nil
}

func (f *fakeIdemAPI) FindByID(_ context.Context, id string) (*FileMeta, error) {
	if fm, ok := f.files[id]; ok {
		return fm, nil
	}
	if p, ok := f.parents[id]; ok {
		return p, nil
	}
	return nil, ErrFileNotFound
}

func (f *fakeIdemAPI) ListRecursive(_ context.Context, _ string) ([]FileMeta, error) {
	var out []FileMeta
	for _, fm := range f.files {
		out = append(out, *fm)
	}
	return out, nil
}

func newIdemAPI() *fakeIdemAPI {
	packID := "pack"
	parent := &FileMeta{ID: packID, Name: "Pack From Shared", Kind: driveKindFolder}
	existed := &FileMeta{ID: "f1", Name: "EXAMPLE-183.mp4", Size: 100, Kind: driveKindFile, Hash: "H1", ParentID: packID}
	return &fakeIdemAPI{
		root:    []FileMeta{*parent},
		pack:    []FileMeta{*existed},
		files:   map[string]*FileMeta{"f1": existed},
		parents: map[string]*FileMeta{packID: parent},
	}
}

// TestCheckRestoreCopy_Hit 命中既有副本（Pack From Shared 同名同 size 同 hash）→ 复用。
func TestCheckRestoreCopy_Hit(t *testing.T) {
	t.Parallel()
	api := newIdemAPI()
	rc := checkRestoreCopy(context.Background(), api, &downloader.ResolvedFile{
		Name: "EXAMPLE-183.mp4", Size: 100, Hash: "H1",
	})
	if !rc.OK || rc.File == nil || rc.File.ID != "f1" {
		t.Fatalf("checkRestoreCopy = %+v, want 命中 f1", rc)
	}
	if !rc.IsRestoreCopy {
		t.Fatal("Pack From Shared 内的副本应标 IsRestoreCopy=true（可登记 AutoDelete）")
	}
}

// TestCheckRestoreCopy_HashMismatch 同名同 size 但 hash 不等 → 拒绝复用（防内容错配）。
func TestCheckRestoreCopy_HashMismatch(t *testing.T) {
	t.Parallel()
	api := newIdemAPI()
	rc := checkRestoreCopy(context.Background(), api, &downloader.ResolvedFile{
		Name: "EXAMPLE-183.mp4", Size: 100, Hash: "DIFFERENT",
	})
	if rc.OK {
		t.Fatalf("checkRestoreCopy 应拒绝 hash 不等的复用: %+v", rc)
	}
}

// TestCheckRestoreCopy_Miss 无同名副本 → 不命中（调用方走 RestoreShare）。
func TestCheckRestoreCopy_Miss(t *testing.T) {
	t.Parallel()
	api := newIdemAPI()
	rc := checkRestoreCopy(context.Background(), api, &downloader.ResolvedFile{
		Name: "OTHER.mp4", Size: 200, Hash: "H2",
	})
	if rc.OK {
		t.Fatalf("checkRestoreCopy 不应命中不存在的副本: %+v", rc)
	}
}

// TestCheckRestoreCopy_UserFile 用户自有同内容文件（非 Pack From Shared）→ 复用但不标 IsRestoreCopy。
func TestCheckRestoreCopy_UserFile(t *testing.T) {
	t.Parallel()
	api := newIdemAPI()
	// 用户自有文件在根目录（parent 非 Pack From Shared）
	own := &FileMeta{ID: "own1", Name: "EXAMPLE-183.mp4", Size: 100, Kind: driveKindFile, Hash: "H1", ParentID: "rootparent"}
	api.pack = nil          // 清空 pack（走全盘兜底）
	delete(api.files, "f1") // 清掉既有副本，只剩用户自有文件
	api.files["own1"] = own
	api.parents["rootparent"] = &FileMeta{ID: "rootparent", Name: "root", Kind: driveKindFolder}
	rc := checkRestoreCopy(context.Background(), api, &downloader.ResolvedFile{
		Name: "EXAMPLE-183.mp4", Size: 100, Hash: "H1",
	})
	if !rc.OK || rc.File.ID != "own1" {
		t.Fatalf("checkRestoreCopy 应命中用户自有文件: %+v", rc)
	}
	if rc.IsRestoreCopy {
		t.Fatal("用户自有文件不得标 IsRestoreCopy（防 AutoDelete 误删自有文件）")
	}
}

// TestCheckRestoreCopy_APIError 全盘列出失败 → 保守不命中（调用方走 restore，但至少不误复用）。
func TestCheckRestoreCopy_APIError(t *testing.T) {
	t.Parallel()
	api := newIdemAPI()
	api.files = map[string]*FileMeta{} // 模拟空
	// 让 ListRecursive 报错
	errAPI := &errIdemAPI{inner: api}
	rc := checkRestoreCopy(context.Background(), errAPI, &downloader.ResolvedFile{Name: "x.mp4", Size: 1})
	if rc.OK {
		t.Fatalf("API 错误应保守不命中: %+v", rc)
	}
}

type errIdemAPI struct{ inner restoreIdempotentAPI }

func (e *errIdemAPI) List(ctx context.Context, pid string) ([]FileMeta, error) {
	return e.inner.List(ctx, pid)
}
func (e *errIdemAPI) FindByID(ctx context.Context, id string) (*FileMeta, error) {
	return e.inner.FindByID(ctx, id)
}
func (e *errIdemAPI) ListRecursive(ctx context.Context, _ string) ([]FileMeta, error) {
	return nil, errors.New("list failed")
}

// TestCheckRestoreCopy_StaleEntry 列表返回陈旧条目（文件已被清理）→ 保守不命中。
// 实测缺陷（2026-10-27）：pack 列表仍返回已删除条目，直接复用死 id → CLI 下载失败。
func TestCheckRestoreCopy_StaleEntry(t *testing.T) {
	t.Parallel()
	api := newIdemAPI()
	// 让 FindByID 对命中 id 报 not found（模拟文件已删）
	stale := &staleIdemAPI{inner: api, goneID: "f1"}
	rc := checkRestoreCopy(context.Background(), stale, &downloader.ResolvedFile{
		Name: "EXAMPLE-183.mp4", Size: 100, Hash: "H1",
	})
	if rc.OK {
		t.Fatalf("陈旧条目应保守不命中（走 RestoreShare）: %+v", rc)
	}
}

type staleIdemAPI struct {
	inner  restoreIdempotentAPI
	goneID string
}

func (s *staleIdemAPI) List(ctx context.Context, pid string) ([]FileMeta, error) {
	return s.inner.List(ctx, pid)
}

func (s *staleIdemAPI) FindByID(ctx context.Context, id string) (*FileMeta, error) {
	if id == s.goneID {
		return nil, ErrFileNotFound
	}
	return s.inner.FindByID(ctx, id)
}

func (s *staleIdemAPI) ListRecursive(ctx context.Context, pid string) ([]FileMeta, error) {
	return s.inner.ListRecursive(ctx, pid)
}

// TestLocateRestoredFileIn_Folder RestoreShare 返回「Pack From Shared」文件夹 id 时必须
// 进文件夹找目标文件（实测 2026-10-27：直接拿文件夹 id 做 CLI 下载报 no download URL）。
func TestLocateRestoredFileIn_Folder(t *testing.T) {
	t.Parallel()
	api := newIdemAPI()
	// pack（id="pack"）内有 EXAMPLE-183.mp4 与另一个视频
	api.pack = append(api.pack,
		FileMeta{ID: "f-other", Name: "other.mp4", Size: 50, Kind: driveKindFile, ParentID: "pack"})
	got, err := locateRestoredFileIn(context.Background(), api, "pack", "EXAMPLE-183.mp4")
	if err != nil {
		t.Fatalf("locateRestoredFileIn: %v", err)
	}
	if got.ID != "f1" {
		t.Fatalf("应精确命中 EXAMPLE-183.mp4（f1），got %s", got.ID)
	}
}

// TestLocateRestoredFileIn_File 非文件夹 id → 直接返回。
func TestLocateRestoredFileIn_File(t *testing.T) {
	t.Parallel()
	api := newIdemAPI()
	got, err := locateRestoredFileIn(context.Background(), api, "f1", "")
	if err != nil || got.ID != "f1" {
		t.Fatalf("locateRestoredFileIn(file) = %+v, %v", got, err)
	}
}
