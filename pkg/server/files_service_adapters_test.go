// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// files_service_adapters_test.go 覆盖 pkg/files 适配器（filesRuntime / filesVolumeSet /
// filesExternalDeleter / externalVolumeOps）的**回落与错误分支**：
//
//   - 「未装配 / 未知卷 / 未授权 / 非法路径」必须是**静默未命中**（false/nil）——不泄卷存在性，
//     也不得让调用方误以为操作已裁决；
//   - 「已裁决」路径必须映射为领域 `*files.HTTPError`（400/409/500），不得退化成裸错误；
//   - 错误必须**如实传播**（读失败不吞成空内容）。
//
// 既有 files_external_ops_test.go 已钉住各方法的主干（成功/封印/IO/校验和），本文件只补
// 主干之外的回落与错误分支，避免与它重复。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// TestFilesStorageManager_ChunkedAccounting 验证容量管理器适配：MaxBytes/Usage 透传，
// 分块预留计入用量、释放归零（分块上传的容量账本唯一入口）。
func TestFilesStorageManager_ChunkedAccounting(t *testing.T) {
	t.Parallel()
	m := capacity.NewStorageManager(t.TempDir(), 4096, nil, testLogger())
	t.Cleanup(m.Stop)
	a := filesStorageManager{m: m, waitTimeout: time.Second}

	if got := a.MaxBytes(); got != 4096 {
		t.Fatalf("MaxBytes=%d want 4096", got)
	}
	if got := a.Usage(); got != 0 {
		t.Fatalf("初始 Usage=%d want 0", got)
	}
	if err := a.WaitReserveChunked(t.Context(), 512); err != nil {
		t.Fatalf("WaitReserveChunked: %v", err)
	}
	if got := a.Usage(); got != 512 {
		t.Fatalf("预留后 Usage=%d want 512", got)
	}
	a.ReleaseChunked(512)
	if got := a.Usage(); got != 0 {
		t.Fatalf("释放后 Usage=%d want 0", got)
	}
}

// TestFilesRuntime_UnassembledCapabilities 验证适配器对「未装配」返回 nil 接口而非
// typed-nil（领域的「未装配」判断依赖接口 == nil）。
func TestFilesRuntime_UnassembledCapabilities(t *testing.T) {
	t.Parallel()
	if got := (filesRuntime{h: &Handlers{}}).Volumes(); got != nil {
		t.Fatalf("volSet 未装配时 Volumes() 应为 nil, got %T", got)
	}
	if got := (filesRuntime{h: &Handlers{}}).Capacity(); got != nil {
		t.Fatalf("storageMgr 未装配时 Capacity() 应为 nil, got %T", got)
	}
}

// TestFilesRuntime_AssembledCapabilities 验证已装配时适配器透传卷集与容量账本。
func TestFilesRuntime_AssembledCapabilities(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = dir
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: dir}}
	h := buildVolSetHandlers(t, cfg)
	m := capacity.NewStorageManager(t.TempDir(), 8192, nil, testLogger())
	t.Cleanup(m.Stop)
	h.storageMgr = m

	r := filesRuntime{h: h}
	vols := r.Volumes()
	if vols == nil {
		t.Fatal("已装配 volSet 时 Volumes() 不得为 nil")
	}
	if vols.Default().Name == "" {
		t.Fatal("卷集应提供默认卷")
	}
	cap := r.Capacity()
	if cap == nil {
		t.Fatal("已装配 storageMgr 时 Capacity() 不得为 nil")
	}
	if cap.MaxBytes() != 8192 {
		t.Fatalf("Capacity().MaxBytes=%d want 8192", cap.MaxBytes())
	}
}

// TestFilesVolumeSet_ListDirViewFallbacks 验证卷视图列举的回落契约：未装配（无 host/无 Set）、
// 未知卷、未授权 owner → (nil,false,nil)；非法子路径 → 400；底层列举失败 → 错误上报。
func TestFilesVolumeSet_ListDirViewFallbacks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	if entries, ok, err := (filesVolumeSet{}).ListDirView(ctx, "main", "alice", ""); entries != nil || ok || err != nil {
		t.Fatalf("未装配应 (nil,false,nil), got (%v,%v,%v)", entries, ok, err)
	}

	fs := newOpsFS()
	fs.put("alice/user/a.bin", "abc")
	fs.mkdir("alice/user/dir")
	h, _ := newOpsEnv(t, fs)
	set := filesVolumeSet{Set: h.volSet, sinkHost: h}

	if _, ok, err := set.ListDirView(ctx, "nope", "alice", ""); ok || err != nil {
		t.Fatalf("未知卷应 (_,false,nil), got (%v,%v)", ok, err)
	}
	restricted := volume.Volume{Name: "ext", Type: "ops-ext",
		ACL: volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{"bob": {}}}}
	hDenied := newExternalTestEnvFS(t, restricted, newOpsFS())
	deniedSet := filesVolumeSet{Set: hDenied.volSet, sinkHost: hDenied}
	if _, ok, err := deniedSet.ListDirView(ctx, "ext", "alice", ""); ok || err != nil {
		t.Fatalf("未授权应 (_,false,nil), got (%v,%v)", ok, err)
	}

	entries, ok, err := set.ListDirView(ctx, "ext", "alice", "")
	if err != nil || !ok {
		t.Fatalf("正常列举应 (entries,true,nil), got (%v,%v)", ok, err)
	}
	if len(entries) != 2 {
		t.Fatalf("应列出 2 个直接子项, got %d (%v)", len(entries), entries)
	}

	if _, ok, err := set.ListDirView(ctx, "ext", "alice", "../escape"); !ok {
		t.Fatal("非法子路径应已裁决 ok=true")
	} else if he, isHE := errors.AsType[*files.HTTPError](err); !isHE || he.Status != http.StatusBadRequest {
		t.Fatalf("非法子路径应映射 400, got %v", err)
	}

	fs.listErr["alice/user/broken"] = errors.New("backend list down")
	if _, ok, err := set.ListDirView(ctx, "ext", "alice", "broken"); !ok || err == nil {
		t.Fatalf("列举失败应 (_,true,error), got (%v,%v)", ok, err)
	}

	// 目录不存在（底层报 os.ErrNotExist）= 空列表，不是错误。
	fs.listErr["alice/user/gone"] = os.ErrNotExist
	if entries, ok, err := set.ListDirView(ctx, "ext", "alice", "gone"); err != nil || !ok || entries != nil {
		t.Fatalf("不存在目录应 (nil,true,nil), got (%v,%v,%v)", entries, ok, err)
	}
}

// errReadCloser 是「读取必失败」的 Reader：用于验证 FileChecksum 不吞读取错误。
type errReadCloser struct{}

func (errReadCloser) Read([]byte) (int, error) { return 0, errors.New("stream broken") }
func (errReadCloser) Close() error             { return nil }

// readErrFS 在 opsFS 之上覆盖 OpenRead（其余能力继承），模拟「打开成功但读取失败」。
type readErrFS struct{ *opsFS }

func (f readErrFS) OpenRead(context.Context, string) (io.ReadCloser, error) {
	return errReadCloser{}, nil
}

// TestFilesExternalDeleter_ErrorBranches 验证删除能力适配器的错误传播：非法 rel（键解析失败）
// 必须报错而不是删错位置；Stat/OpenRead/Delete 的底层错误必须原样上报。
func TestFilesExternalDeleter_ErrorBranches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	v := volume.Volume{Name: "ext", Type: "ops-ext"}

	fs := newOpsFS()
	fs.put("alice/user/dir/x.bin", "x")
	fs.mkdir("alice/user/dir")
	d := filesExternalDeleter{fs: fs, v: v}

	if _, _, err := d.StatSize(ctx, "ext", "alice", "../escape"); err == nil {
		t.Fatal("非法 rel 应报错（不得按错误键 stat）")
	}
	if _, err := d.FileChecksum(ctx, "ext", "alice", "../escape"); err == nil {
		t.Fatal("非法 rel 应报错")
	}
	if err := d.Delete(ctx, "ext", "alice", "../escape"); err == nil {
		t.Fatal("非法 rel 应报错（不得删错对象）")
	}

	size, exists, err := d.StatSize(ctx, "ext", "alice", "user/dir")
	if err != nil || !exists || size != 0 {
		t.Fatalf("目录应 (0,true,nil), got (%d,%v,%v)", size, exists, err)
	}

	fs.statErr["alice/user/bad.bin"] = errors.New("stat down")
	if _, _, err := d.StatSize(ctx, "ext", "alice", "user/bad.bin"); err == nil {
		t.Fatal("stat 失败必须上报")
	}

	fs.openErr["alice/user/locked.bin"] = errors.New("open down")
	if _, err := d.FileChecksum(ctx, "ext", "alice", "user/locked.bin"); err == nil {
		t.Fatal("打开失败必须上报")
	}

	fs.deleteErr["alice/user/held.bin"] = errors.New("delete down")
	if err := d.Delete(ctx, "ext", "alice", "user/held.bin"); err == nil {
		t.Fatal("删除失败必须上报")
	}

	broken := filesExternalDeleter{fs: readErrFS{opsFS: fs}, v: v}
	fs.put("alice/user/r.bin", "content")
	if _, err := broken.FileChecksum(ctx, "ext", "alice", "user/r.bin"); err == nil {
		t.Fatal("流读取失败必须上报（不得返回空校验和）")
	}
}

// TestExternalVolumeOps_Fallbacks 验证外部卷对象操作在「未装配 / 未知卷 / 未授权」下
// **一律静默未命中**（不裁决、不产生副作用），调用方据此回落既有本地路径。
func TestExternalVolumeOps_Fallbacks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	restricted := volume.Volume{Name: "ext", Type: "ops-ext",
		ACL: volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{"bob": {}}}}
	deniedFS := newOpsFS()
	deniedFS.put("alice/user/f.bin", "x")
	hDenied := newExternalTestEnvFS(t, restricted, deniedFS)
	fs := newOpsFS()
	fs.put("alice/user/f.bin", "x")
	hOK, _ := newOpsEnv(t, fs)

	cases := []struct {
		name string
		h    *Handlers
		vol  string
	}{
		{"未装配", &Handlers{logger: testLogger()}, "ext"},
		{"未知卷", hOK, "nope"},
		{"未授权", hDenied, "ext"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ops := externalVolumeOps{h: tc.h}
			if ok, err := ops.ExternalDelete(ctx, "alice", tc.vol, "f.bin", ""); ok || err != nil {
				t.Fatalf("ExternalDelete 应回落, got (%v,%v)", ok, err)
			}
			if size, cs, err := ops.ExternalStat(ctx, "alice", tc.vol, "f.bin"); size != -1 || cs != "" || err != nil {
				t.Fatalf("ExternalStat 应回落 (-1,\"\",nil), got (%d,%q,%v)", size, cs, err)
			}
			if ok, err := ops.ExternalMkdir(ctx, "alice", tc.vol, "dir"); ok || err != nil {
				t.Fatalf("ExternalMkdir 应回落, got (%v,%v)", ok, err)
			}
			if ok, err := ops.ExternalRemoveDir(ctx, "alice", tc.vol, "dir", true); ok || err != nil {
				t.Fatalf("ExternalRemoveDir 应回落, got (%v,%v)", ok, err)
			}
			if ok, err := ops.ExternalRename(ctx, "alice", tc.vol, "f.bin", "g.bin", ""); ok || err != nil {
				t.Fatalf("ExternalRename 应回落, got (%v,%v)", ok, err)
			}
			if extra, ok, err := ops.ExternalMeta(ctx, "alice", tc.vol, "f.bin"); extra != nil || ok || err != nil {
				t.Fatalf("ExternalMeta 应回落, got (%v,%v,%v)", extra, ok, err)
			}
			if extra, mt, ct, ok, err := ops.ExternalMetaInfo(ctx, "alice", tc.vol, "f.bin"); extra != nil || mt != "" || ct != "" || ok || err != nil {
				t.Fatalf("ExternalMetaInfo 应回落, got (%v,%q,%q,%v,%v)", extra, mt, ct, ok, err)
			}
			if ok, err := ops.ExternalMetaUpdate(ctx, "alice", tc.vol, "f.bin", map[string]any{"k": "v"}); ok || err != nil {
				t.Fatalf("ExternalMetaUpdate 应回落, got (%v,%v)", ok, err)
			}
			if res, err := ops.ExternalCopy(ctx, "alice", tc.vol, "f.bin", "user/f.bin", "g.bin", "user/g.bin", false); err != nil || res.Supported {
				t.Fatalf("ExternalCopy 应回落 (Supported=false,nil), got (%v,%v)", res.Supported, err)
			}
		})
	}
	if !deniedFS.has("alice/user/f.bin") {
		t.Fatal("未授权路径不得删除对象")
	}
}

// TestExternalVolumeOps_DeleteErrors 验证 ExternalDelete 的底层删除失败映射（IO ⇒ 500）
// 以及「列目录失败」时的递归删除错误（不静默成功）。
func TestExternalVolumeOps_DeleteErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := newOpsFS()
	fs.put("alice/user/io.bin", "i")
	fs.deleteErr["alice/user/io.bin"] = errors.New("io down")
	h, _ := newOpsEnv(t, fs)
	ops := externalVolumeOps{h: h}

	if ok, err := ops.ExternalDelete(ctx, "alice", "ext", "io.bin", ""); ok {
		t.Fatalf("IO 失败应 ok=false, got %v", ok)
	} else {
		wantHTTPStatus(t, err, http.StatusInternalServerError)
	}

	fs.mkdir("alice/user/tree")
	fs.put("alice/user/tree/x.bin", "x")
	fs.listErr["alice/user/tree"] = errors.New("list down")
	if ok, err := ops.ExternalRemoveDir(ctx, "alice", "ext", "tree", true); !ok {
		t.Fatalf("已裁决（列目录失败）应 ok=true, got %v", ok)
	} else {
		wantHTTPStatus(t, err, http.StatusInternalServerError)
	}
}

// TestExternalVolumeOps_RenameCopyFallbackError 验证「后端不支持原地改名」的等价路径失败时
// 如实报 500（拷贝+删源不得假成功）。
func TestExternalVolumeOps_RenameCopyFallbackError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := newOpsFS()
	fs.renameErr = syncpkg.ErrUnsupported
	h, _ := newOpsEnv(t, fs)
	h.logger = testLogger()

	// 源存在但读取失败 ⇒ 拷贝+删源失败 ⇒ 500（源不动）。
	fs.put("alice/user/src.bin", "payload")
	fs.openErr["alice/user/src.bin"] = errors.New("open down")
	if ok, err := (externalVolumeOps{h: h}).ExternalRename(ctx, "alice", "ext", "src.bin", "dst.bin", ""); !ok {
		t.Fatalf("已裁决应 ok=true, got %v", ok)
	} else {
		wantHTTPStatus(t, err, http.StatusInternalServerError)
	}
	if !fs.has("alice/user/src.bin") {
		t.Fatal("失败路径不得删除源")
	}
}

// copyErrFS 在 opsFS 之上覆盖 CopyFile（其余能力继承），用于注入复制失败/封印。
type copyErrFS struct {
	*opsFS
	err error
}

func (f copyErrFS) CopyFile(context.Context, string, string) error { return f.err }

// TestExternalCopyHelpers_Branches 验证同卷复制的辅助分支：能力探测回落、目标已存在、
// force 删除失败、CopyFile 未实现（501）与封印（409）。
func TestExternalCopyHelpers_Branches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	if _, _, _, ok := (&Handlers{logger: testLogger()}).externalCopyKeys("alice", "ext", "a", "b"); ok {
		t.Fatal("未装配 volSet 应回落 ok=false")
	}
	plain := &extFS{files: map[string]string{"alice/user/a.bin": "x"}}
	hPlain := newExternalTestEnv(t, volume.Volume{Name: "plain", Type: "ops-plain"}, plain)
	if _, _, _, ok := hPlain.externalCopyKeys("alice", "plain", "a.bin", "b.bin"); ok {
		t.Fatal("无 blob 直拷能力应回落 ok=false")
	}

	fs := newOpsFS()
	fs.put("alice/user/a.bin", "x")
	h, _ := newOpsEnv(t, fs)
	if src, dst, fsys, ok := h.externalCopyKeys("alice", "ext", "a.bin", "../escape"); !ok || fsys != nil || src != "" || dst != "" {
		t.Fatalf("非法路径应已裁决 (ok=true, 空键), got (%q,%q,%v,%v)", src, dst, fsys, ok)
	}
	src, dst, fsys, ok := h.externalCopyKeys("alice", "ext", "a.bin", "b.bin")
	if !ok || fsys == nil || src != "alice/user/a.bin" || dst != "alice/user/b.bin" {
		t.Fatalf("合法路径应返回键与 FS, got (%q,%q,%v,%v)", src, dst, fsys, ok)
	}

	if err := h.prepareExternalCopyDest(ctx, fsys, "alice/user/a.bin", false); err == nil {
		t.Fatal("目标已存在（无 force）应 409")
	} else {
		wantHTTPStatus(t, err, http.StatusConflict)
	}
	if err := h.prepareExternalCopyDest(ctx, fsys, "alice/user/missing.bin", false); err != nil {
		t.Fatalf("目标不存在应 nil, got %v", err)
	}
	fs.deleteErr["alice/user/a.bin"] = errors.New("delete down")
	if err := h.prepareExternalCopyDest(ctx, fsys, "alice/user/a.bin", true); err == nil {
		t.Fatal("force 删除失败应报错")
	} else {
		wantHTTPStatus(t, err, http.StatusInternalServerError)
	}

	// 装饰链未转发 CopyFile ⇒ 501（不静默回落写内核）。
	if err := h.copyWithinVolumeFS(ctx, "plain", "a", "b", plain, "x", "y"); err == nil {
		t.Fatal("无 CopyFile 能力应报错")
	} else {
		wantHTTPStatus(t, err, http.StatusNotImplemented)
	}
	if err := h.copyWithinVolumeFS(ctx, "ext", "a", "b", copyErrFS{opsFS: fs, err: storage.ErrSealed}, "alice/user/a.bin", "alice/user/b.bin"); err == nil {
		t.Fatal("封印复制应报错")
	} else {
		wantHTTPStatus(t, err, http.StatusConflict)
	}
	if err := h.copyWithinVolumeFS(ctx, "ext", "a", "b", copyErrFS{opsFS: fs, err: errors.New("copy down")}, "alice/user/a.bin", "alice/user/b.bin"); err == nil {
		t.Fatal("复制失败应报错")
	} else {
		wantHTTPStatus(t, err, http.StatusInternalServerError)
	}
}

// TestToFilesHTTPError_Mapping 验证装配层错误 → 领域 HTTPError 的映射（状态码与文案逐字保留），
// 非该形态错误原样返回。
func TestToFilesHTTPError_Mapping(t *testing.T) {
	t.Parallel()
	re := newRouteError(routeErrNotAllowed, http.StatusForbidden, msgVolumeNotAllowed, nil)
	got := toFilesHTTPError(re)
	he, ok := errors.AsType[*files.HTTPError](got)
	if !ok || he.Status != http.StatusForbidden || he.Message != msgVolumeNotAllowed {
		t.Fatalf("routeError 应映射为 403 HTTPError, got %T %v", got, got)
	}

	de := &downloadPathError{status: http.StatusBadRequest, message: errMsgInvalidFilename}
	got = toFilesHTTPError(de)
	he, ok = errors.AsType[*files.HTTPError](got)
	if !ok || he.Status != http.StatusBadRequest || he.Message != errMsgInvalidFilename {
		t.Fatalf("downloadPathError 应映射为 400 HTTPError, got %T %v", got, got)
	}

	plain := errors.New("boom")
	if got := toFilesHTTPError(plain); !errors.Is(got, plain) {
		t.Fatalf("普通错误应原样返回, got %v", got)
	}
}

// TestRouteUploadForTarget_Guards 验证显式卷写路由的拒绝分支：卷不在 owner 视图 / 卷名未知
// → 403 语义的 routeError；预留失败 → 契约错误（*files.HTTPError）。
//
// 已知缺陷（不在本 PR 修，见 PR body）：ACL/未知卷拒绝返回的是内部形态 `*routeError`，
// 而 s3WriteError / DAV 只识别 `*files.HTTPError` ⇒ S3 显式卷被拒时回落 500（应为 403）。
// 本用例锁定**当前**形态（routeError.status=403）作为前置事实，修复时只改映射不改语义。
func TestRouteUploadForTarget_Guards(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// volSet 未装配 → 回落到统一路由（返回错误而不是 panic）。
	if _, err := (&Handlers{logger: testLogger()}).routeUploadForTarget(ctx, "alice", "user/a.bin", "main", 1); err == nil {
		t.Fatal("未装配卷集时路由应失败（回落路径同样拒绝）")
	}

	dir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = dir
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: dir},
		{Name: "bob-only", Root: t.TempDir(), ACL: &VolumeACLConfig{Mode: VolumeACLAllow, Owners: []string{"bob"}}},
	}
	h := buildVolSetHandlers(t, cfg)

	if _, err := h.routeUploadForTarget(ctx, "alice", "user/a.bin", "bob-only", 1); err == nil {
		t.Fatal("未授权卷必须拒绝")
	} else if re, ok := errors.AsType[*routeError](err); !ok || re.status != http.StatusForbidden {
		t.Fatalf("未授权卷应返回 403 语义的 routeError, got %T %v", err, err)
	}
	if _, err := h.routeUploadForTarget(ctx, "alice", "user/a.bin", "nope", 1); err == nil {
		t.Fatal("未知卷必须拒绝")
	} else if re, ok := errors.AsType[*routeError](err); !ok || re.status != http.StatusForbidden {
		t.Fatalf("未知卷应返回 403 语义的 routeError, got %T %v", err, err)
	}

	// 合法的显式卷：返回带卷名与租户的路由（预留成功）。
	route, err := h.routeUploadForTarget(ctx, "alice", "user/ok.bin", "main", 4)
	if err != nil {
		t.Fatalf("合法卷应路由成功: %v", err)
	}
	if route.VolumeName != "main" || route.Tenant == nil {
		t.Fatalf("路由应带卷名与租户, got %+v", route)
	}
	if route.Release != nil {
		route.Release()
	}
}

// TestDownloadPathForRemote_Branches 验证远程只读面的路径解析：空名/非法路径 → 400，
// 未知卷/不存在/目录 → 404，正常文件返回带 Source 的 DownloadPath。
func TestDownloadPathForRemote_Branches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := newOpsFS()
	fs.put("alice/user/f.bin", "hello")
	fs.mkdir("alice/user/dir")
	h, _ := newOpsEnv(t, fs)

	if _, err := h.downloadPathForRemote(ctx, "alice", "ext", ""); err == nil {
		t.Fatal("空文件名必须 400")
	} else {
		wantHTTPStatus(t, err, http.StatusBadRequest)
	}
	if _, err := h.downloadPathForRemote(ctx, "alice", "ext", "../escape"); err == nil {
		t.Fatal("非法路径必须 400")
	} else {
		wantHTTPStatus(t, err, http.StatusBadRequest)
	}
	for _, name := range []string{"nope", "missing.bin", "dir"} {
		if _, err := h.downloadPathForRemote(ctx, "alice", "ext", name); err == nil {
			t.Fatalf("%q 应 404", name)
		} else {
			wantHTTPStatus(t, err, http.StatusNotFound)
		}
	}

	dp, err := h.downloadPathForRemote(ctx, "alice", "ext", "f.bin")
	if err != nil {
		t.Fatalf("正常文件应可解析: %v", err)
	}
	if dp.Source == nil || dp.VolumeName != "ext" || dp.Rel == "" {
		t.Fatalf("外部卷下载路径应带 Source/卷名/键, got %+v", dp)
	}
}

// TestExternalCopyKeys_UnknownVolumeAndUnauthorized 验证复制键解析在未知卷与未授权 owner
// 下回落（不裁决），与 ExternalCopy 的 Supported=false 语义一致。
func TestExternalCopyKeys_UnknownVolumeAndUnauthorized(t *testing.T) {
	t.Parallel()
	restricted := volume.Volume{Name: "ext", Type: "ops-ext",
		ACL: volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{"bob": {}}}}
	hDenied := newExternalTestEnvFS(t, restricted, newOpsFS())
	if _, _, _, ok := hDenied.externalCopyKeys("alice", "ext", "a", "b"); ok {
		t.Fatal("未授权 owner 应回落 ok=false")
	}

	h, _ := newOpsEnv(t, newOpsFS())
	if _, _, _, ok := h.externalCopyKeys("alice", "nope", "a", "b"); ok {
		t.Fatal("未知卷应回落 ok=false")
	}
}

// TestFilesMetaPolicy_NilHandlerGuards 验证 meta policy 在未装配 Handlers 时（warn* 分支）
// 不 panic：可观测性辅助必须容忍 nil logger。
func TestFilesMetaPolicy_NilHandlerGuards(t *testing.T) {
	t.Parallel()
	p := filesMetaPolicy{h: &Handlers{}}
	p.warnStaleMeta("user/a.bin", 1, 2)
	p.warnDegrade("读取失败", "user/a.bin", os.ErrClosed)
	p.warnDegradeDebug("sidecar 缺失", "user/a.bin", os.ErrNotExist)
}
