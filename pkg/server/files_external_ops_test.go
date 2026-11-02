// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// files_external_ops_test.go 钉住外部卷对象操作接缝（pkg/files.ExternalVolumeOps 的装配实现）：
// 每个方法都有「非本接缝可处理 ⇒ 回落（false/未命中）」与「已裁决 ⇒ 明确 HTTP 状态」两态，
// 错误映射（封印 409 / IO 500 / 非法路径 400 / 已存在 409 / 校验和不一致 409）不得漂移。
//
// 夹具 opsFS 是**可控能力 + 可控故障**的内存卷：可开关 CopyFile / meta.Provider /
// MetaExtraUpdater，可按键注入 Stat/ListDir/Delete/MakeDir/Rename/OpenRead 错误——
// 否则这些错误分支只能靠真实后端故障复现（不可测）。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// opsFS 是外部卷测试夹具：内存对象 + 能力开关 + 按方法注入的故障。
type opsFS struct {
	mu    sync.Mutex
	files map[string][]byte
	dirs  map[string]bool

	statErr   map[string]error
	listErr   map[string]error
	openErr   map[string]error
	deleteErr map[string]error
	mkdirErr  error
	renameErr error

	// meta 相关（CopyFile 能力断言与 meta 读取共用同一实现）。
	metas     map[string]*meta.FileMeta
	metaErr   map[string]error
	updateErr error
	updates   map[string]map[string]any
}

func newOpsFS() *opsFS {
	return &opsFS{
		files:     map[string][]byte{},
		dirs:      map[string]bool{},
		statErr:   map[string]error{},
		listErr:   map[string]error{},
		openErr:   map[string]error{},
		deleteErr: map[string]error{},
		metas:     map[string]*meta.FileMeta{},
		metaErr:   map[string]error{},
		updates:   map[string]map[string]any{},
	}
}

// opsBackend 把任意 sync.FS 适配为 registry.ExternalBackend（能力由 FS 自身表达）。
type opsBackend struct{ fs syncpkg.FS }

func (b *opsBackend) FS() syncpkg.FS { return b.fs }
func (b *opsBackend) Close() error   { return nil }

var _ syncpkg.FS = (*opsFS)(nil)

func (f *opsFS) ListDir(_ context.Context, p string) ([]syncpkg.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.listErr[p]; err != nil {
		return nil, err
	}
	prefix := strings.TrimSuffix(p, "/")
	if prefix != "" {
		prefix += "/"
	}
	seen := map[string]bool{}
	var out []syncpkg.Entry
	for name, data := range f.files {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		child := strings.TrimPrefix(name, prefix)
		if i := strings.Index(child, "/"); i >= 0 {
			child = child[:i]
			if seen[child] {
				continue
			}
			seen[child] = true
			out = append(out, syncpkg.Entry{Name: child, Path: prefix + child, IsDir: true})
			continue
		}
		out = append(out, syncpkg.Entry{Name: child, Path: name, Size: int64(len(data))})
	}
	for name := range f.dirs {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		child := strings.TrimPrefix(name, prefix)
		if i := strings.Index(child, "/"); i >= 0 {
			child = child[:i]
			if seen[child] {
				continue
			}
			seen[child] = true
			out = append(out, syncpkg.Entry{Name: child, Path: prefix + child, IsDir: true})
			continue
		}
		if !seen[child] {
			seen[child] = true
			out = append(out, syncpkg.Entry{Name: child, Path: name, IsDir: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *opsFS) Stat(_ context.Context, p string) (*syncpkg.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.statErr[p]; err != nil {
		return nil, err
	}
	if data, ok := f.files[p]; ok {
		return &syncpkg.Entry{Name: p, Path: p, Size: int64(len(data))}, nil
	}
	if f.dirs[p] {
		return &syncpkg.Entry{Name: p, Path: p, IsDir: true}, nil
	}
	return nil, nil
}

func (f *opsFS) OpenRead(_ context.Context, p string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.openErr[p]; err != nil {
		return nil, err
	}
	data, ok := f.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(string(data))), nil
}

func (f *opsFS) WriteFile(_ context.Context, _ syncpkg.LogicalKey, p string, r io.Reader, _ int64, _ int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = data
	return nil
}

func (f *opsFS) Rename(_ context.Context, from, to string) error {
	if f.renameErr != nil {
		return f.renameErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[from]
	if !ok {
		return os.ErrNotExist
	}
	delete(f.files, from)
	f.files[to] = data
	return nil
}

func (f *opsFS) Delete(_ context.Context, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.deleteErr[p]; err != nil {
		return err
	}
	delete(f.files, p)
	delete(f.dirs, p)
	return nil
}

func (f *opsFS) MakeDir(_ context.Context, p string) error {
	if f.mkdirErr != nil {
		return f.mkdirErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[p] = true
	return nil
}

// CopyFile 实现 blob 直拷能力（copyFile=false 时仍存在，能力开关由 opsPlainFS 表达）。
func (f *opsFS) CopyFile(_ context.Context, from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[from]
	if !ok {
		return os.ErrNotExist
	}
	f.files[to] = append([]byte(nil), data...)
	return nil
}

// FileMeta 实现 meta.Provider（metaErr 注入读取失败）。
func (f *opsFS) FileMeta(_ context.Context, p string) (*meta.FileMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.metaErr[p]; err != nil {
		return nil, err
	}
	if fm, ok := f.metas[p]; ok {
		return fm, nil
	}
	return nil, os.ErrNotExist
}

// UpdateMetaExtra 实现 syncpkg.MetaExtraUpdater（穿透装饰器后命中本实现）。
func (f *opsFS) UpdateMetaExtra(_ context.Context, p string, extra map[string]any) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates[p] = extra
	return nil
}

func (f *opsFS) put(p, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = []byte(content)
}

func (f *opsFS) has(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.files[p]
	return ok
}

func (f *opsFS) mkdir(p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[p] = true
}

// newOpsEnv 构造「本地默认卷 main + 外部卷 ext」的 Handlers（与生产装配同形：
// 外部卷经 AddExternalVolume 注入，外部对象键恒 `<owner>/user/<rel>`）。
func newOpsEnv(t *testing.T, fs syncpkg.FS) (*Handlers, volume.Volume) {
	t.Helper()
	v := volume.Volume{Name: "ext", Type: "ops-ext"}
	h := newExternalTestEnvFS(t, v, fs)
	return h, v
}

// newExternalTestEnvFS 与 newExternalTestEnv 同构，但接受任意 sync.FS（能力可裁）。
func newExternalTestEnvFS(t *testing.T, v volume.Volume, fs syncpkg.FS) *Handlers {
	t.Helper()
	root, err := storage.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	vs := registry.NewSet(
		[]volume.Volume{{Name: "main"}},
		map[string]*storage.Root{"main": root},
		nil, nil, "main",
	)
	if aerr := vs.AddExternalVolume(v, &opsBackend{fs: fs}); aerr != nil {
		t.Fatalf("AddExternalVolume: %v", aerr)
	}
	tenants := storage.NewTenantCache(root)
	t.Cleanup(func() { _ = tenants.Close() })
	t.Cleanup(func() { _ = root.Close() })
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(&Config{})
	return &Handlers{volSet: vs, logger: testLogger(), tenants: tenants, cfgPtr: &cfgPtr}
}

// wantHTTPStatus 断言 err 是带指定状态码的 *files.HTTPError。
func wantHTTPStatus(t *testing.T, err error, status int) {
	t.Helper()
	he, ok := errors.AsType[*files.HTTPError](err)
	if !ok {
		t.Fatalf("期望 *files.HTTPError, got %T (%v)", err, err)
	}
	if he.Status != status {
		t.Fatalf("HTTP 状态=%d want %d (%s)", he.Status, status, he.Message)
	}
}

// TestExternalOps_Mkdir 钉住建目录：成功 / 非法路径 400 / 封印 409 / IO 500 / 未授权与未装配回落。
func TestExternalOps_Mkdir(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := newOpsFS()
	h, _ := newOpsEnv(t, fs)
	ops := externalVolumeOps{h: h}

	ok, err := ops.ExternalMkdir(ctx, "alice", "ext", "dir/sub")
	if !ok || err != nil {
		t.Fatalf("正常建目录应成功: ok=%v err=%v", ok, err)
	}
	if !fs.dirs["alice/user/dir/sub"] {
		t.Fatal("目录键应为 <owner>/user/<rel>")
	}

	fs.mkdirErr = storage.ErrSealed
	if _, err := ops.ExternalMkdir(ctx, "alice", "ext", "sealed"); err == nil {
		t.Fatal("封印卷必须拒绝建目录")
	} else {
		wantHTTPStatus(t, err, http.StatusConflict)
	}
	fs.mkdirErr = errors.New("io down")
	if _, err := ops.ExternalMkdir(ctx, "alice", "ext", "io"); err == nil {
		t.Fatal("IO 失败必须报错（不静默成功）")
	} else {
		wantHTTPStatus(t, err, http.StatusInternalServerError)
	}
	fs.mkdirErr = nil

	// 非法路径：已裁决 (true, 400)——调用方不得继续回落本地路径。
	if ok, err := ops.ExternalMkdir(ctx, "alice", "ext", "../escape"); !ok {
		t.Fatal("非法路径应 ok=true（已裁决）")
	} else {
		wantHTTPStatus(t, err, http.StatusBadRequest)
	}

	// 未授权 owner：不命中（false, nil）——不泄卷存在性。
	restricted := volume.Volume{Name: "ext", Type: "ops-ext",
		ACL: volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{"bob": {}}}}
	hDenied := newExternalTestEnvFS(t, restricted, newOpsFS())
	if ok, err := (externalVolumeOps{h: hDenied}).ExternalMkdir(ctx, "alice", "ext", "x"); ok || err != nil {
		t.Fatalf("未授权应回落 (false,nil), got (%v,%v)", ok, err)
	}

	// 未装配卷集 / 未知卷。
	if ok, err := (externalVolumeOps{h: &Handlers{logger: testLogger()}}).ExternalMkdir(ctx, "alice", "ext", "x"); ok || err != nil {
		t.Fatalf("未装配 volSet 应回落 (false,nil), got (%v,%v)", ok, err)
	}
	if ok, err := ops.ExternalMkdir(ctx, "alice", "nope", "x"); ok || err != nil {
		t.Fatalf("未知卷应回落 (false,nil), got (%v,%v)", ok, err)
	}
}

// TestExternalOps_RemoveDir 钉住删目录四态：不存在 404 / 非目录 400 / 未确认 force 400 /
// 封印 409 / 递归成功。
func TestExternalOps_RemoveDir(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := newOpsFS()
	h, _ := newOpsEnv(t, fs)
	ops := externalVolumeOps{h: h}

	if ok, err := ops.ExternalRemoveDir(ctx, "alice", "ext", "missing", true); !ok {
		t.Fatal("已裁决（不存在）应 ok=true")
	} else {
		wantHTTPStatus(t, err, http.StatusNotFound)
	}

	fs.put("alice/user/plain.bin", "x")
	if ok, err := ops.ExternalRemoveDir(ctx, "alice", "ext", "plain.bin", true); !ok {
		t.Fatal("已裁决（非目录）应 ok=true")
	} else {
		wantHTTPStatus(t, err, http.StatusBadRequest)
	}

	fs.mkdir("alice/user/tree")
	fs.put("alice/user/tree/a.bin", "a")
	fs.mkdir("alice/user/tree/sub")
	fs.put("alice/user/tree/sub/b.bin", "b")
	if ok, err := ops.ExternalRemoveDir(ctx, "alice", "ext", "tree", false); !ok {
		t.Fatal("已裁决（未确认 force）应 ok=true")
	} else {
		wantHTTPStatus(t, err, http.StatusBadRequest)
	}
	if !fs.has("alice/user/tree/a.bin") {
		t.Fatal("未确认 force 时不得删除内容")
	}

	if ok, err := ops.ExternalRemoveDir(ctx, "alice", "ext", "tree", true); !ok || err != nil {
		t.Fatalf("递归删除应成功: ok=%v err=%v", ok, err)
	}
	if fs.has("alice/user/tree/a.bin") || fs.has("alice/user/tree/sub/b.bin") {
		t.Fatal("递归删除后子文件应全部消失")
	}

	fs.mkdir("alice/user/sealed")
	fs.put("alice/user/sealed/x.bin", "x")
	fs.deleteErr["alice/user/sealed/x.bin"] = storage.ErrSealed
	if ok, err := ops.ExternalRemoveDir(ctx, "alice", "ext", "sealed", true); !ok {
		t.Fatal("已裁决（封印）应 ok=true")
	} else {
		wantHTTPStatus(t, err, http.StatusConflict)
	}

	// 非法路径。
	if ok, err := ops.ExternalRemoveDir(ctx, "alice", "ext", "../x", true); !ok {
		t.Fatal("非法路径应 ok=true")
	} else {
		wantHTTPStatus(t, err, http.StatusBadRequest)
	}
}

// TestExternalOps_Rename 钉住改名：源不存在未命中 / 目标已存在 409 / 成功 / 封印 409 /
// 后端不支持原地改名 ⇒ 拷贝+删源 / 其它错误 500。
func TestExternalOps_Rename(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := newOpsFS()
	h, _ := newOpsEnv(t, fs)
	ops := externalVolumeOps{h: h}

	if ok, err := ops.ExternalRename(ctx, "alice", "ext", "missing.bin", "b.bin", ""); ok || err != nil {
		t.Fatalf("源不存在应回落 (false,nil), got (%v,%v)", ok, err)
	}

	fs.put("alice/user/a.bin", "payload")
	fs.put("alice/user/b.bin", "exists")
	if ok, err := ops.ExternalRename(ctx, "alice", "ext", "a.bin", "b.bin", ""); !ok {
		t.Fatal("已裁决（目标已存在）应 ok=true")
	} else {
		wantHTTPStatus(t, err, http.StatusConflict)
	}
	if got := string(fs.files["alice/user/a.bin"]); got != "payload" {
		t.Fatal("拒绝覆盖时源必须原样保留")
	}

	if ok, err := ops.ExternalRename(ctx, "alice", "ext", "a.bin", "c.bin", ""); !ok || err != nil {
		t.Fatalf("正常改名应成功: ok=%v err=%v", ok, err)
	}
	if !fs.has("alice/user/c.bin") || fs.has("alice/user/a.bin") {
		t.Fatal("改名后源消失、目标存在")
	}

	// checksum 交叉比对：meta.Provider 记录值与客户端声明不一致 ⇒ 409（不重算内容）。
	fs.put("alice/user/ck.bin", "v1")
	fs.metas["alice/user/ck.bin"] = &meta.FileMeta{TotalSHA256: "deadbeef"}
	if ok, err := ops.ExternalRename(ctx, "alice", "ext", "ck.bin", "ck2.bin", "cafebabe"); !ok {
		t.Fatal("已裁决（校验和不一致）应 ok=true")
	} else {
		wantHTTPStatus(t, err, http.StatusConflict)
	}
	// 一致 ⇒ 继续改名。
	if ok, err := ops.ExternalRename(ctx, "alice", "ext", "ck.bin", "ck2.bin", "deadbeef"); !ok || err != nil {
		t.Fatalf("校验和一致应改名成功: ok=%v err=%v", ok, err)
	}

	// 封印 ⇒ 409。
	fs.put("alice/user/sealed.bin", "s")
	fs.renameErr = storage.ErrSealed
	if ok, err := ops.ExternalRename(ctx, "alice", "ext", "sealed.bin", "s2.bin", ""); !ok {
		t.Fatal("已裁决（封印）应 ok=true")
	} else {
		wantHTTPStatus(t, err, http.StatusConflict)
	}

	// 后端明确不支持原地改名（契约哨兵）⇒ 读源→写目标→删源（不丢数据）。
	fs.renameErr = syncpkg.ErrUnsupported
	if ok, err := ops.ExternalRename(ctx, "alice", "ext", "sealed.bin", "moved.bin", ""); !ok || err != nil {
		t.Fatalf("不支持原地改名时应拷贝+删源成功: ok=%v err=%v", ok, err)
	}
	if !fs.has("alice/user/moved.bin") || fs.has("alice/user/sealed.bin") {
		t.Fatal("拷贝+删源后目标应存在、源应消失")
	}

	// 其它底层错误 ⇒ 500（如实失败）。
	fs.put("alice/user/io.bin", "io")
	fs.renameErr = errors.New("backend io error")
	if ok, err := ops.ExternalRename(ctx, "alice", "ext", "io.bin", "io2.bin", ""); !ok {
		t.Fatal("已裁决（IO 失败）应 ok=true")
	} else {
		wantHTTPStatus(t, err, http.StatusInternalServerError)
	}
}

// TestExternalOps_Meta 钉住 meta 读：文件不存在未命中 / sidecar 缺失 ⇒ 空 map /
// sidecar 有 Extra ⇒ 原样返回 / 读取真错误 ⇒ 错误。
func TestExternalOps_Meta(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := newOpsFS()
	h, _ := newOpsEnv(t, fs)
	ops := externalVolumeOps{h: h}

	if extra, ok, err := ops.ExternalMeta(ctx, "alice", "ext", "missing.bin"); ok || err != nil || extra != nil {
		t.Fatalf("不存在应 (nil,false,nil), got (%v,%v,%v)", extra, ok, err)
	}

	fs.put("alice/user/no-meta.bin", "x")
	extra, ok, err := ops.ExternalMeta(ctx, "alice", "ext", "no-meta.bin")
	if !ok || err != nil {
		t.Fatalf("无 sidecar 应 (空 map,true,nil), got (%v,%v)", ok, err)
	}
	if len(extra) != 0 {
		t.Fatalf("无 sidecar 应返回空 map, got %v", extra)
	}

	fs.put("alice/user/with-meta.bin", "y")
	fs.metas["alice/user/with-meta.bin"] = &meta.FileMeta{
		Extra: map[string]any{"k": "v"}, MTime: "2026-01-01T00:00:00Z", CTime: "2025-12-31T00:00:00Z",
	}
	extra, mtime, ctime, ok, err := ops.ExternalMetaInfo(ctx, "alice", "ext", "with-meta.bin")
	if !ok || err != nil {
		t.Fatalf("sidecar 存在应 (map,true,nil), got (%v,%v)", ok, err)
	}
	if extra["k"] != "v" || mtime == "" || ctime == "" {
		t.Fatalf("meta 内容/时间未透传: extra=%v mtime=%q ctime=%q", extra, mtime, ctime)
	}

	// 读 meta 的真错误 ⇒ fail-closed（不得当作「无 extra」）。
	fs.put("alice/user/bad.bin", "z")
	fs.metaErr["alice/user/bad.bin"] = errors.New("meta store down")
	if _, ok, err := ops.ExternalMeta(ctx, "alice", "ext", "bad.bin"); !ok || err == nil {
		t.Fatalf("meta 读取失败应 (_,true,error), got (%v,%v)", ok, err)
	}

	// 非法路径 ⇒ 400。
	if _, ok, err := ops.ExternalMeta(ctx, "alice", "ext", "../x"); !ok {
		t.Fatal("非法路径应 ok=true")
	} else {
		wantHTTPStatus(t, err, http.StatusBadRequest)
	}
}

// TestExternalOps_MetaUpdate 钉住 meta 写：不存在未命中 / 无能力 ⇒ ErrMetaUnavailable
// （fail-closed，不假成功）/ 底层失败 ⇒ 包装错误 / 成功写透。
func TestExternalOps_MetaUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := newOpsFS()
	h, _ := newOpsEnv(t, fs)
	ops := externalVolumeOps{h: h}

	if ok, err := ops.ExternalMetaUpdate(ctx, "alice", "ext", "missing.bin", map[string]any{"a": 1}); ok || err != nil {
		t.Fatalf("不存在应 (false,nil), got (%v,%v)", ok, err)
	}

	fs.put("alice/user/f.bin", "x")
	extra := map[string]any{"source_integrity": "damaged"}
	if ok, err := ops.ExternalMetaUpdate(ctx, "alice", "ext", "f.bin", extra); !ok || err != nil {
		t.Fatalf("应更新成功: ok=%v err=%v", ok, err)
	}
	if got := fs.updates["alice/user/f.bin"]["source_integrity"]; got != "damaged" {
		t.Fatalf("extra 未写入底层: %v", fs.updates["alice/user/f.bin"])
	}

	fs.updateErr = errors.New("disk full")
	if ok, err := ops.ExternalMetaUpdate(ctx, "alice", "ext", "f.bin", extra); !ok || err == nil {
		t.Fatalf("底层失败应 (true,error), got (%v,%v)", ok, err)
	}

	// 无 MetaExtraUpdater 能力（裸卷未装配 meta 装饰）：必须 fail-closed 返回哨兵。
	plain := &extFS{files: map[string]string{"alice/user/p.bin": "x"}}
	h2 := newExternalTestEnv(t, volume.Volume{Name: "plain", Type: "ops-plain"}, plain)
	ops2 := externalVolumeOps{h: h2}
	if ok, err := ops2.ExternalMetaUpdate(ctx, "alice", "plain", "p.bin", extra); !ok || !errors.Is(err, files.ErrMetaUnavailable) {
		t.Fatalf("无 meta 能力应 ErrMetaUnavailable, got (%v,%v)", ok, err)
	}
}

// TestExternalOps_Copy 钉住同卷复制：无 blob 直拷能力 ⇒ 回落（Supported=false）；
// 支持 ⇒ Supported=true 且透传 meta 的 checksum/size；目标已存在 409（force 覆盖）。
func TestExternalOps_Copy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// ① 不支持：裸卷 FS 不暴露 CopyFile 能力 ⇒ 回落写内核（重新加密）。
	plain := &extFS{files: map[string]string{"alice/user/a.bin": "x"}}
	h1 := newExternalTestEnv(t, volume.Volume{Name: "plain", Type: "ops-plain"}, plain)
	if res, err := (externalVolumeOps{h: h1}).ExternalCopy(ctx, "alice", "plain", "a.bin", "user/a.bin", "b.bin", "user/b.bin", false); err != nil || res.Supported {
		t.Fatalf("无此能力应回落 (Supported=false,nil), got (%v,%v)", res.Supported, err)
	}

	// ② 支持：blob 直拷 + meta 透传。
	fs := newOpsFS()
	h2, _ := newOpsEnv(t, fs)
	ops := externalVolumeOps{h: h2}
	fs.put("alice/user/a.bin", "payload")
	fs.metas["alice/user/a.bin"] = &meta.FileMeta{TotalSHA256: "abc123", Size: 7}
	res, err := ops.ExternalCopy(ctx, "alice", "ext", "a.bin", "user/a.bin", "b.bin", "user/b.bin", false)
	if err != nil || !res.Supported {
		t.Fatalf("支持时应 Supported=true: %v %v", res.Supported, err)
	}
	if res.Checksum != "abc123" || res.Size != 7 {
		t.Fatalf("应透传 meta 的 checksum/size, got (%q,%d)", res.Checksum, res.Size)
	}
	if !fs.has("alice/user/b.bin") {
		t.Fatal("复制后目标应存在")
	}

	// ③ 目标已存在：默认 409；force ⇒ 覆盖。
	fs.put("alice/user/exists.bin", "old")
	if _, err := ops.ExternalCopy(ctx, "alice", "ext", "a.bin", "user/a.bin", "exists.bin", "user/exists.bin", false); err == nil {
		t.Fatal("目标已存在应 409")
	} else {
		wantHTTPStatus(t, err, http.StatusConflict)
	}
	if res, err := ops.ExternalCopy(ctx, "alice", "ext", "a.bin", "user/a.bin", "exists.bin", "user/exists.bin", true); err != nil || !res.Supported {
		t.Fatalf("force 覆盖应成功: %v %v", res.Supported, err)
	}

	// ④ 底层复制失败 ⇒ 500（不得假成功）。
	fs.put("alice/user/src2.bin", "s2")
	fs.put("alice/user/dst2.bin", "d2")
	fs.metas["alice/user/src2.bin"] = &meta.FileMeta{TotalSHA256: "s2", Size: 2}
	// 目的已存在 + force 先删，再让 CopyFile 因源不存在失败。
	if _, err := ops.ExternalCopy(ctx, "alice", "ext", "gone.bin", "user/gone.bin", "dst2.bin", "user/dst2.bin", true); err == nil {
		t.Fatal("源不存在时 blob 直拷必须失败")
	}
}

// TestExternalOps_DeleteAndStat 钉住对象删除/元信息：未命中 404 语义 / 校验和不一致 409 /
// 封印 409 / 成功删除 / Stat 返回 size+checksum。
func TestExternalOps_DeleteAndStat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := newOpsFS()
	h, _ := newOpsEnv(t, fs)
	ops := externalVolumeOps{h: h}

	// Stat：不存在 / 目录 / 成功（含 meta checksum）。
	fs.put("alice/user/f.bin", "hello")
	fs.metas["alice/user/f.bin"] = &meta.FileMeta{TotalSHA256: "sum1"}
	size, cs, err := ops.ExternalStat(ctx, "alice", "ext", "f.bin")
	if err != nil || size != 5 || cs != "sum1" {
		t.Fatalf("ExternalStat=(%d,%q,%v) want (5,sum1,nil)", size, cs, err)
	}
	if size, cs, err := ops.ExternalStat(ctx, "alice", "ext", "missing.bin"); size != -1 || cs != "" || err != nil {
		t.Fatalf("不存在应 (-1,\"\",nil), got (%d,%q,%v)", size, cs, err)
	}
	fs.mkdir("alice/user/dir")
	if size, _, err := ops.ExternalStat(ctx, "alice", "ext", "dir"); size != -1 || err != nil {
		t.Fatalf("目录应未命中 (-1), got (%d,%v)", size, err)
	}
	if _, _, err := ops.ExternalStat(ctx, "alice", "ext", "../x"); err == nil {
		t.Fatal("非法路径应报错")
	} else {
		wantHTTPStatus(t, err, http.StatusBadRequest)
	}

	// Delete：校验和不一致 ⇒ 409 且不删。
	if ok, err := ops.ExternalDelete(ctx, "alice", "ext", "f.bin", "wrong"); ok {
		t.Fatal("校验和不一致不得删除")
	} else {
		wantHTTPStatus(t, err, http.StatusConflict)
	}
	if !fs.has("alice/user/f.bin") {
		t.Fatal("被拒绝的删除不得改动对象")
	}
	// 一致 ⇒ 删除成功。
	if ok, err := ops.ExternalDelete(ctx, "alice", "ext", "f.bin", "sum1"); !ok || err != nil {
		t.Fatalf("删除应成功: ok=%v err=%v", ok, err)
	}
	if fs.has("alice/user/f.bin") {
		t.Fatal("删除后对象应消失")
	}
	// 不存在 / 目录 ⇒ 未命中（false,nil）。
	if ok, err := ops.ExternalDelete(ctx, "alice", "ext", "missing.bin", ""); ok || err != nil {
		t.Fatalf("不存在应 (false,nil), got (%v,%v)", ok, err)
	}
	// 封印 ⇒ 409（接缝当前契约：返回 (false, 409)——调用方据错误状态回包）。
	fs.put("alice/user/sealed.bin", "s")
	fs.deleteErr["alice/user/sealed.bin"] = storage.ErrSealed
	if ok, err := ops.ExternalDelete(ctx, "alice", "ext", "sealed.bin", ""); ok {
		t.Fatal("封印删除应返回错误（ok=false）")
	} else {
		wantHTTPStatus(t, err, http.StatusConflict)
	}
	// IO 失败 ⇒ 500。
	fs.put("alice/user/io.bin", "i")
	fs.deleteErr["alice/user/io.bin"] = errors.New("io")
	if ok, err := ops.ExternalDelete(ctx, "alice", "ext", "io.bin", ""); ok {
		t.Fatal("IO 失败应返回错误（ok=false）")
	} else {
		wantHTTPStatus(t, err, http.StatusInternalServerError)
	}
}

// TestFilesExternalDeleter_KeyMapping 钉住域 rel → 卷键映射与删除/校验和读取：
// "user" 桶自身映射到桶内空路径（防 "user/user" 残留）、目录 size=0、流式 SHA-256。
func TestFilesExternalDeleter_KeyMapping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := newOpsFS()
	fs.put("alice/user/a.bin", "content")
	d := filesExternalDeleter{fs: fs, v: volume.Volume{Name: "ext", Type: "ops-ext"}}

	if key, err := d.ownerKey("alice", "user"); err != nil || key != "alice/user" {
		t.Fatalf("user 桶自身应映射 alice/user, got (%q,%v)", key, err)
	}
	if key, err := d.ownerKey("alice", "user/a.bin"); err != nil || key != "alice/user/a.bin" {
		t.Fatalf("rel 映射错误: (%q,%v)", key, err)
	}
	if _, err := d.ownerKey("alice", "../escape"); err == nil {
		t.Fatal("非法路径必须返回错误")
	}

	size, exists, err := d.StatSize(ctx, "ext", "alice", "user/a.bin")
	if err != nil || !exists || size != 7 {
		t.Fatalf("StatSize=(%d,%v,%v) want (7,true,nil)", size, exists, err)
	}
	if _, exists2, merr := d.StatSize(ctx, "ext", "alice", "user/missing.bin"); merr != nil || exists2 {
		t.Fatalf("不存在应 (0,false,nil), got (%v,%v)", exists2, merr)
	}

	sum, err := d.FileChecksum(ctx, "ext", "alice", "user/a.bin")
	if err != nil {
		t.Fatalf("FileChecksum: %v", err)
	}
	if sum != "ed7002b439e9ac845f22357d822bac1444730fbdb6016d3ec9432297b9ec9f73" {
		t.Fatalf("明文 SHA-256 不符: %s", sum)
	}
	if _, err := d.FileChecksum(ctx, "ext", "alice", "user/missing.bin"); err == nil {
		t.Fatal("不存在时 FileChecksum 必须报错")
	}

	if err := d.Delete(ctx, "ext", "alice", "user/a.bin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if fs.has("alice/user/a.bin") {
		t.Fatal("Delete 后对象应消失")
	}
}

// TestFilesVolumeSet_ExternalCapabilityRouting 钉住按卷名解析外部能力：
// 本地卷同样可删（U2 统一键空间），未知卷/未装配 ⇒ nil；ExternalSink 仅外部卷非 nil。
func TestFilesVolumeSet_ExternalCapabilityRouting(t *testing.T) {
	t.Parallel()
	fs := newOpsFS()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	h := buildVolSetHandlers(t, cfg)
	if err := h.volSet.AddExternalVolume(volume.Volume{Name: "ext", Type: "ops-ext"}, &opsBackend{fs: fs}); err != nil {
		t.Fatalf("AddExternalVolume: %v", err)
	}
	a := filesVolumeSet{Set: h.volSet, sinkHost: h}

	if a.ExternalDeleter("ext") == nil {
		t.Fatal("外部卷应解析出删除能力")
	}
	if a.ExternalDeleter("main") == nil {
		t.Fatal("本地卷同样应有统一删除能力（U2）")
	}
	if a.ExternalDeleter("nope") != nil {
		t.Fatal("未知卷应返回 nil")
	}
	if got := (filesVolumeSet{Set: h.volSet}).ExternalDeleter("ext"); got == nil {
		t.Fatal("无 sinkHost 不影响删除能力解析")
	}
	if a.ExternalSink("alice", "ext") == nil {
		t.Fatal("外部卷应解析出写入面")
	}
	if a.ExternalSink("alice", "main") != nil {
		t.Fatal("本地卷不得走外部写入面（会丢原子写/事件语义）")
	}
	if got := (filesVolumeSet{}).ExternalSink("alice", "ext"); got != nil {
		t.Fatal("无 sinkHost 应返回 nil（fail-closed）")
	}
}
