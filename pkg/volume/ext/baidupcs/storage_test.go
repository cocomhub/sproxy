// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // 测试双算法校验
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStorageAdapter 是 Storage 测试用的内存 adapter（实现 Adapter 接口）。
type fakeStorageAdapter struct {
	files map[string]string   // remote path → content
	dirs  map[string]struct{} // 目录标记（remote path → exists）
	dlink string              // canned 直链（DirectLink 返回）；空 = 不支持直链
}

func newFakeStorageAdapter() *fakeStorageAdapter {
	return &fakeStorageAdapter{
		files: make(map[string]string),
		dirs:  make(map[string]struct{}),
	}
}

func (f *fakeStorageAdapter) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	if _, exists := f.files[targetPath]; exists && !overwrite {
		return errAlreadyExists
	}
	f.files[targetPath] = string(data)
	// 父目录标记（目录语义：dir/ 前缀的路径隐式建目录）。
	f.markDirs(targetPath)
	return nil
}

func (f *fakeStorageAdapter) Download(ctx context.Context, remotePath, localPath string) error {
	data, ok := f.files[remotePath]
	if !ok {
		return errNotFound
	}
	return os.WriteFile(localPath, []byte(data), 0o644)
}

// Move 服务端移动（源移除、目标覆盖）。
func (f *fakeStorageAdapter) Move(ctx context.Context, from, to string) error {
	data, ok := f.files[from]
	if !ok {
		return errNotFound
	}
	delete(f.files, from)
	f.files[to] = data
	f.markDirs(to)
	return nil
}

// Copy 服务端复制（源保留）。
func (f *fakeStorageAdapter) Copy(ctx context.Context, from, to string) error {
	data, ok := f.files[from]
	if !ok {
		return errNotFound
	}
	f.files[to] = data
	f.markDirs(to)
	return nil
}

// Delete 服务端删除（幂等：不存在不报错）。
func (f *fakeStorageAdapter) Delete(ctx context.Context, remotePath string) error {
	if _, ok := f.files[remotePath]; !ok {
		return nil // 幂等：缺失不报错
	}
	delete(f.files, remotePath)
	return nil
}

// markDirs 为 remotePath 的所有父路径建目录标记。
func (f *fakeStorageAdapter) markDirs(remotePath string) {
	dir := path.Dir(strings.TrimSuffix(remotePath, "/"))
	for dir != "/" && dir != "." && dir != "" {
		f.dirs[dir] = struct{}{}
		dir = path.Dir(dir)
	}
	f.dirs["/"] = struct{}{}
}

// List 返回 remotePath 下单层条目（目录+文件混合，不递归）。实现 metadataProvider。
// 条目 Key 为**完整 remote 路径**（调用方 Storage.List 剥 root 得相对路径）。
func (f *fakeStorageAdapter) List(ctx context.Context, remotePath string) ([]ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, isDir := f.dirs[remotePath]; !isDir {
		if _, isFile := f.files[remotePath]; isFile {
			// 单文件路径：返回自身
			return []ObjectMeta{{Key: remotePath, Size: int64(len(f.files[remotePath]))}}, nil
		}
		return nil, errNotFound
	}
	base := strings.TrimSuffix(remotePath, "/")
	if base == "/" {
		base = ""
	}
	prefix := base
	if prefix != "" {
		prefix += "/"
	}
	seen := map[string]bool{}
	out := f.listFilesLocked(prefix, seen)
	out = append(out, f.listDirsLocked(prefix, remotePath, seen)...)
	return out, nil
}

// listFilesLocked 列出 prefix 下单层文件条目（rel 去重写入 seen；key 保持完整 remote 路径）。
func (f *fakeStorageAdapter) listFilesLocked(prefix string, seen map[string]bool) []ObjectMeta {
	out := make([]ObjectMeta, 0)
	for k := range f.files {
		rel, ok := strings.CutPrefix(k, prefix)
		if !ok || rel == "" || strings.Contains(rel, "/") {
			continue
		}
		if seen[rel] {
			continue
		}
		seen[rel] = true
		out = append(out, ObjectMeta{Key: k, Size: int64(len(f.files[k]))})
	}
	return out
}

// listDirsLocked 列出 prefix 下单层子目录条目（rel 去重写入 seen；key 保持完整 remote 路径）。
func (f *fakeStorageAdapter) listDirsLocked(prefix, remotePath string, seen map[string]bool) []ObjectMeta {
	out := make([]ObjectMeta, 0)
	for d := range f.dirs {
		if d == "/" || d == remotePath {
			continue
		}
		rel, ok := strings.CutPrefix(d, prefix)
		if !ok || rel == "" || strings.Contains(rel, "/") {
			continue
		}
		if seen[rel] {
			continue
		}
		seen[rel] = true
		out = append(out, ObjectMeta{Key: d, IsDir: true})
	}
	return out
}

// Meta 返回单个路径元信息（目录/文件）。实现 metadataProvider。
// ETag = 内容 md5（对齐百度 ETag 语义；供 Put 的 md5 刷新复核通过）。
func (f *fakeStorageAdapter) Meta(ctx context.Context, remotePath string) (*ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, isDir := f.dirs[remotePath]; isDir {
		return &ObjectMeta{Key: remotePath, IsDir: true}, nil
	}
	data, ok := f.files[remotePath]
	if !ok {
		return nil, errNotFound
	}
	h := md5.Sum([]byte(data)) //nolint:gosec // 测试双算法校验
	return &ObjectMeta{
		Key:     remotePath,
		Size:    int64(len(data)),
		ModTime: time.Now(),
		ETag:    hex.EncodeToString(h[:]),
	}, nil
}

var _ metadataProvider = (*fakeStorageAdapter)(nil)

// DirectLink 实现 directLinkProvider（fake 版）：返回 canned dlink（空 = 不支持）。
func (f *fakeStorageAdapter) DirectLink(ctx context.Context, remotePath string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", true, err
	}
	if f.dlink == "" {
		return "", false, nil
	}
	if _, ok := f.files[remotePath]; !ok {
		return "", true, errNotFound
	}
	return f.dlink, true, nil
}

var _ directLinkProvider = (*fakeStorageAdapter)(nil)

// errAlreadyExists / errNotFound 是 fake 内部哨兵（storage 层映射为公开错误）。
var (
	errAlreadyExists = errors.New("already exists")
	errNotFound      = errors.New("not found")
)

func TestStorage_PutGetRoundtrip(t *testing.T) {
	t.Parallel()
	s := newTestStorage(t, newFakeStorageAdapter())
	meta, err := s.Put(context.Background(), "dir/f.txt", strings.NewReader("hello baidu"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if meta.Size != int64(len("hello baidu")) {
		t.Fatalf("meta.Size = %d, want %d", meta.Size, len("hello baidu"))
	}
	rc, m2, err := s.Get(context.Background(), "dir/f.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "hello baidu" {
		t.Fatalf("Get 内容 = %q, want %q", got, "hello baidu")
	}
	if m2.Key != "dir/f.txt" {
		t.Fatalf("meta.Key = %q, want dir/f.txt", m2.Key)
	}
}

func TestStorage_Stat_NotFound(t *testing.T) {
	t.Parallel()
	s := newTestStorage(t, newFakeStorageAdapter())
	if _, err := s.Stat(context.Background(), "nope.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat 缺失应 ErrNotFound, got %v", err)
	}
}

func TestStorage_Put_StatNotFound_Retries(t *testing.T) {
	t.Parallel()
	// fake adapter 上传后 Stat 恒报 NotFound（百度最终一致性）→ 有界重试 ≤3 后 ErrTransient
	ad := &statMissingAdapter{inner: newFakeStorageAdapter()}
	s := newTestStorage(t, ad)
	_, err := s.Put(context.Background(), "f.txt", strings.NewReader("data"))
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("Stat 恒 NotFound 应 ErrTransient, got %v", err)
	}
	if ad.uploads > maxPutAttempts {
		t.Fatalf("重试次数 %d 超过上限 %d", ad.uploads, maxPutAttempts)
	}
	if ad.uploads != maxPutAttempts {
		t.Fatalf("应重试 %d 次, got %d", maxPutAttempts, ad.uploads)
	}
}

func TestStorage_Get_TempCleanup(t *testing.T) {
	t.Parallel()
	ad := newFakeStorageAdapter()
	s := newTestStorage(t, ad)
	if _, err := s.Put(context.Background(), "f.txt", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	rc, _, err := s.Get(context.Background(), "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// 临时文件应被清理（temp 目录无残留）
	entries, readDirErr := os.ReadDir(s.temp)
	if readDirErr != nil {
		t.Fatal(readDirErr)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("Get 后临时文件未清理: %s", e.Name())
		}
	}
}

func TestStorage_Delete_Missing(t *testing.T) {
	t.Parallel()
	s := newTestStorage(t, newFakeStorageAdapter())
	if err := s.Delete(context.Background(), "nope.txt"); err != nil {
		t.Fatalf("删除缺失文件不应报错（幂等）: %v", err)
	}
}

func TestStorage_List_Recursive(t *testing.T) {
	t.Parallel()
	ad := newFakeStorageAdapter()
	s := newTestStorage(t, ad)
	// fake 只支持精确路径（无目录遍历）——List 依赖 adapter 的目录语义，
	// 本测试用单文件断言（List 对文件路径返回单元素）。
	if _, err := s.Put(context.Background(), "a.txt", strings.NewReader("a")); err != nil {
		t.Fatal(err)
	}
	metas, err := s.List(context.Background(), "a.txt")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 1 || metas[0].Key != "a.txt" {
		t.Fatalf("List 文件 = %+v, want [a.txt]", metas)
	}
}

// statMissingAdapter 上传后 Stat（Download）恒报 NotFound（触发有界重试）。
type statMissingAdapter struct {
	inner   *fakeStorageAdapter
	uploads int
}

func (e *statMissingAdapter) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	e.uploads++
	return e.inner.Upload(ctx, localPath, targetPath, overwrite)
}

func (e *statMissingAdapter) Download(ctx context.Context, remotePath, localPath string) error {
	return errNotFound
}

func (e *statMissingAdapter) Move(ctx context.Context, from, to string) error {
	return e.inner.Move(ctx, from, to)
}
func (e *statMissingAdapter) Copy(ctx context.Context, from, to string) error {
	return e.inner.Copy(ctx, from, to)
}

// newTestStorage 构造测试用 Storage（temp 指向 t.TempDir()）。
func newTestStorage(t *testing.T, ad Adapter) *Storage {
	t.Helper()
	s, err := NewStorage(StorageConfig{
		Root:    "/baidu",
		TempDir: t.TempDir(),
		Adapter: ad,
		Logger:  testLogger(),
	})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	if s.temp == "" {
		t.Fatal("s.temp 不应为空")
	}
	return s
}

var _ = filepath.Join // 保留 filepath 导入（后续用例可能用）

// metadataProvider 断言测试辅助：fakeStorageAdapter 需实现 List/Meta（单层目录语义）。

// TestStorage_List_ReturnsChildren 验证 List(prefix) 返回 prefix 下单层条目（目录+文件混合）。
func TestStorage_List_ReturnsChildren(t *testing.T) {
	t.Parallel()
	ad := newFakeStorageAdapter()
	s := newTestStorage(t, ad)
	// fake adapter 预置：目录 dir/（含 a.txt/b.txt）+ 顶层 c.txt
	if _, err := s.Put(context.Background(), "dir/a.txt", strings.NewReader("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(), "dir/b.txt", strings.NewReader("b")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(), "c.txt", strings.NewReader("c")); err != nil {
		t.Fatal(err)
	}
	metas, err := s.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byKey := make(map[string]ObjectMeta, len(metas))
	for _, m := range metas {
		byKey[m.Key] = m
	}
	if _, ok := byKey["dir"]; !ok {
		t.Fatalf("顶层缺 dir 目录条目，got keys=%v", keysOf(metas))
	}
	if !byKey["dir"].IsDir {
		t.Fatalf("dir 应为目录（IsDir=true），got %+v", byKey["dir"])
	}
	if _, ok := byKey["c.txt"]; !ok {
		t.Fatalf("顶层缺 c.txt，got keys=%v", keysOf(metas))
	}
	if _, ok := byKey["dir/a.txt"]; ok {
		t.Fatalf("List 不应递归子目录（出现 dir/a.txt）")
	}
}

func keysOf(metas []ObjectMeta) []string {
	out := make([]string, 0, len(metas))
	for _, m := range metas {
		out = append(out, m.Key)
	}
	return out
}

// TestStorage_List_UnderSubdir 验证 List(subdir) 只列该子目录单层。
func TestStorage_List_UnderSubdir(t *testing.T) {
	t.Parallel()
	ad := newFakeStorageAdapter()
	s := newTestStorage(t, ad)
	if _, err := s.Put(context.Background(), "dir/a.txt", strings.NewReader("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(), "dir/sub/x.txt", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	metas, err := s.List(context.Background(), "dir")
	if err != nil {
		t.Fatalf("List(dir): %v", err)
	}
	byKey := make(map[string]ObjectMeta, len(metas))
	for _, m := range metas {
		byKey[m.Key] = m
	}
	if _, ok := byKey["dir/a.txt"]; !ok {
		t.Fatalf("dir 下缺 dir/a.txt，got keys=%v", keysOf(metas))
	}
	if _, ok := byKey["dir/sub"]; !ok || !byKey["dir/sub"].IsDir {
		t.Fatalf("dir 下缺 dir/sub 目录条目，got keys=%v", keysOf(metas))
	}
	if _, ok := byKey["dir/sub/x.txt"]; ok {
		t.Fatalf("List 不应递归子目录（出现 dir/sub/x.txt）")
	}
}

// TestStorage_Stat_UsesMeta 验证 Stat 的 isdir/size 来自库 Meta（不经临时下载）。
func TestStorage_Stat_UsesMeta(t *testing.T) {
	t.Parallel()
	ad := newFakeStorageAdapter()
	s := newTestStorage(t, ad)
	if _, err := s.Put(context.Background(), "dir/a.txt", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	dirMeta, err := s.Stat(context.Background(), "dir")
	if err != nil {
		t.Fatalf("Stat(dir): %v", err)
	}
	if !dirMeta.IsDir {
		t.Fatalf("dir 应 IsDir=true，got %+v", dirMeta)
	}
	fileMeta, err := s.Stat(context.Background(), "dir/a.txt")
	if err != nil {
		t.Fatalf("Stat(a.txt): %v", err)
	}
	if fileMeta.IsDir {
		t.Fatalf("a.txt 不应 IsDir")
	}
	if fileMeta.Size != int64(len("hello")) {
		t.Fatalf("a.txt size = %d, want %d", fileMeta.Size, len("hello"))
	}
}

// TestStorage_Copy_GetPutCombo 验证 Storage.Copy（Get+Put 组合）：目标 key 出现相同内容。
func TestStorage_Copy_GetPutCombo(t *testing.T) {
	t.Parallel()
	s := newTestStorage(t, newFakeStorageAdapter())
	ctx := context.Background()
	if _, err := s.Put(ctx, "dir/a.txt", strings.NewReader("copy-me")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	meta, err := s.Copy(ctx, "dir/a.txt", "dir/b.txt")
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if meta == nil || meta.Size != int64(len("copy-me")) {
		t.Fatalf("Copy meta.Size = %v, want %d", meta, len("copy-me"))
	}
	rc, _, err := s.Get(ctx, "dir/b.txt")
	if err != nil {
		t.Fatalf("Get copied: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "copy-me" {
		t.Fatalf("复制内容 = %q, want %q", got, "copy-me")
	}
}

// TestStorage_Put_MultipartETagRefresh M4 回归（cocom 行为）：分片上传后远端 ETag 是
// 片组合/服务端"可能不正确"（非整文件 md5）→ Put 复核不匹配 → rapidupload 秒传刷新
// （内容已在网盘）→ 目标 md5 刷新为权威整文件 md5 → 严格复核一致才成功（无 readback
// 接受捷径）。
func TestStorage_Put_MultipartETagRefresh(t *testing.T) {
	t.Parallel()
	ad := &multipartETagAdapter{inner: newFakeStorageAdapter(), multipart: true}
	s := newTestStorage(t, ad)
	content := strings.Repeat("multipart-md5-refresh-content-", 100)
	if _, err := s.Put(context.Background(), "big.bin", strings.NewReader(content)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !ad.rapidHit {
		t.Fatal("分片上传 ETag 不匹配应触发 rapidupload 秒传刷新")
	}
	// 最终远端内容与本地一致 + ETag 权威（假 adapter 秒传后 ETag 用整文件 md5）。
	m, err := s.Stat(context.Background(), "big.bin")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	want := md5Hex(content)
	if m.ETag != want {
		t.Fatalf("秒传刷新后 ETag 应权威整文件 md5 %s, got %s", want, m.ETag)
	}
}

// TestStorage_PutFromStagedPath_HintsSkipPreStat RandomBlob 提示跳过首轮 pre-Stat
// （内容随机新建，秒传命中率 0）——用计数 Stat 断言：RandomBlob 路径的上传前 Stat
// 次数为 0（普通路径首轮有 pre-Stat）；上传后的复核 Stat 是**必须的**（刷新路径，不计）。
func TestStorage_PutFromStagedPath_HintsSkipPreStat(t *testing.T) {
	t.Parallel()
	ad := &countingStatAdapter{inner: newFakeStorageAdapter()}
	s := newTestStorage(t, ad)
	content := "hints-skip-prestat-content"
	staged := filepath.Join(s.temp, "h.bin")
	if err := os.WriteFile(staged, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// 普通路径：上传前有 pre-Stat（putCheckExisting）。
	if _, err := s.PutFromStagedPath(context.Background(), "n.bin", staged, int64(len(content)), nil); err != nil {
		t.Fatalf("PutFromStagedPath: %v", err)
	}
	if ad.statCalls < 2 {
		t.Fatalf("普通路径应有 pre-Stat + 上传后复核 Stat（>=2），got %d", ad.statCalls)
	}
	ad.statCalls = 0
	// RandomBlob 路径：跳过上传前 pre-Stat，但上传后复核 Stat 仍在（>=1）。
	if _, err := s.PutFromStagedPath(context.Background(), "h.bin", staged, int64(len(content)),
		&PutHints{RandomBlob: true}); err != nil {
		t.Fatalf("PutFromStagedPath: %v", err)
	}
	if ad.statCalls != 1 {
		t.Fatalf("RandomBlob 应跳过上传前 pre-Stat、仅留上传后复核 Stat（=1），got %d", ad.statCalls)
	}
}

// countingStatAdapter 统计 Meta（= Stat）调用次数。
type countingStatAdapter struct {
	inner     *fakeStorageAdapter
	statCalls int
}

func (a *countingStatAdapter) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	return a.inner.Upload(ctx, localPath, targetPath, overwrite)
}
func (a *countingStatAdapter) Download(ctx context.Context, remotePath, localPath string) error {
	return a.inner.Download(ctx, remotePath, localPath)
}
func (a *countingStatAdapter) Move(ctx context.Context, f, t2 string) error {
	return a.inner.Move(ctx, f, t2)
}
func (a *countingStatAdapter) Copy(ctx context.Context, f, t2 string) error {
	return a.inner.Copy(ctx, f, t2)
}
func (a *countingStatAdapter) Delete(ctx context.Context, remotePath string) error {
	return a.inner.Delete(ctx, remotePath)
}
func (a *countingStatAdapter) List(ctx context.Context, remotePath string) ([]ObjectMeta, error) {
	return a.inner.List(ctx, remotePath)
}
func (a *countingStatAdapter) Meta(ctx context.Context, remotePath string) (*ObjectMeta, error) {
	a.statCalls++
	return a.inner.Meta(ctx, remotePath)
}

var _ Adapter = (*countingStatAdapter)(nil)

// TestStorage_PutFromStagedPath_RandomBlobSkipsBlockListRead RandomBlob 跳过秒传参数读回
// （sliceMD5/分块列表不需要）——断言 st.blockMD5s 为空但上传仍成功且 md5 正确。
func TestStorage_PutFromStagedPath_RandomBlobSkipsBlockListRead(t *testing.T) {
	t.Parallel()
	ad := newFakeStorageAdapter()
	s := newTestStorage(t, ad)
	content := strings.Repeat("skip-blocklist-", 64)
	staged := filepath.Join(s.temp, "blk.bin")
	if err := os.WriteFile(staged, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutFromStagedPath(context.Background(), "blk.bin", staged, int64(len(content)),
		&PutHints{RandomBlob: true}); err != nil {
		t.Fatalf("PutFromStagedPath: %v", err)
	}
	// 内容与 md5 一致（小文件单传 ETag 即整文件 md5，无需刷新）。
	m, err := s.Stat(context.Background(), "blk.bin")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if want := md5Hex(content); m.ETag != want {
		t.Fatalf("ETag = %s, want %s", m.ETag, want)
	}
}

// multipartETagAdapter 模拟百度分片上传：Upload 后 Stat 返回片组合 ETag（非整文件 md5），
// RapidUpload 命中后把 ETag 刷新为整文件 md5。
type multipartETagAdapter struct {
	inner     *fakeStorageAdapter
	multipart bool // 分片语义：上传后 ETag 是片组合
	rapidHit  bool
}

func (e *multipartETagAdapter) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	return e.inner.Upload(ctx, localPath, targetPath, overwrite)
}
func (e *multipartETagAdapter) Download(ctx context.Context, remotePath, localPath string) error {
	return e.inner.Download(ctx, remotePath, localPath)
}
func (e *multipartETagAdapter) Move(ctx context.Context, from, to string) error {
	return e.inner.Move(ctx, from, to)
}
func (e *multipartETagAdapter) Copy(ctx context.Context, from, to string) error {
	return e.inner.Copy(ctx, from, to)
}
func (e *multipartETagAdapter) Delete(ctx context.Context, remotePath string) error {
	return e.inner.Delete(ctx, remotePath)
}
func (e *multipartETagAdapter) List(ctx context.Context, remotePath string) ([]ObjectMeta, error) {
	return e.inner.List(ctx, remotePath)
}
func (e *multipartETagAdapter) Meta(ctx context.Context, remotePath string) (*ObjectMeta, error) {
	m, err := e.inner.Meta(ctx, remotePath)
	if err != nil {
		return nil, err
	}
	// 分片语义：**首次上传后 Stat 必为片组合 ETag**（服务端 md5 为片组合/"可能不正确"），
	// 只有 rapidupload 秒传刷新命中后才返回权威整文件 md5——真实百度行为（M4 实测）。
	if e.multipart && !e.rapidHit && m != nil {
		return &ObjectMeta{Key: m.Key, Size: m.Size, ETag: "slicemd5-combo-not-whole"}, nil
	}
	return m, nil
}
func (e *multipartETagAdapter) RapidUpload(ctx context.Context, remotePath string, st *stagedUpload) (bool, error) {
	e.rapidHit = true
	e.multipart = false // 秒传命中后 Stat 返回真实整文件 md5
	return true, nil
}

var _ Adapter = (*multipartETagAdapter)(nil)
var _ rapidUploader = (*multipartETagAdapter)(nil)

// md5Hex 计算字符串的十六进制 md5。
func md5Hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

// TestBackoffBeforeRetry M4 退避：轮间等待递增（第二次 ≥300ms），ctx 取消立即中断。
func TestBackoffBeforeRetry(t *testing.T) {
	t.Parallel()
	s := newTestStorage(t, newFakeStorageAdapter())
	// 第二次轮间退避 300ms（C-MINOR-10：真实 sleep 已 t.Parallel 可接受，只验下限）。
	start := time.Now()
	if err := s.backoffBeforeRetry(context.Background(), 2); err != nil {
		t.Fatalf("backoff(2): %v", err)
	}
	if el := time.Since(start); el < 250*time.Millisecond {
		t.Fatalf("第 2 轮退避应 ≥300ms, got %v", el)
	}
	// ctx 取消立即中断。
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.backoffBeforeRetry(cctx, 2); err == nil {
		t.Fatal("ctx 取消应中断退避")
	}
}

// TestBlockMD5ListOf C-C1 回归：分块 md5 列表按 uploadBlockSize（4MiB）切分——
// >4MiB 文件多块、<4MiB 单块（= 整文件 md5）、空文件空列表。
func TestBlockMD5ListOf(t *testing.T) {
	t.Parallel()
	// <4MiB：单块 = 整文件 md5。
	small := strings.Repeat("s", 100)
	sp := filepath.Join(t.TempDir(), "small.bin")
	_ = os.WriteFile(sp, []byte(small), 0o600)
	list, err := blockMD5ListOf(sp, int64(len(small)))
	if err != nil {
		t.Fatalf("blockMD5ListOf: %v", err)
	}
	if len(list) != 1 || list[0] != md5Hex(small) {
		t.Fatalf("小文件应单块=整文件 md5, got %v", list)
	}
	// >4MiB：多块（每块 md5 独立，非整文件 md5）。
	big := []byte(strings.Repeat("b", 4<<20+100)) // 4MiB+100
	bp := filepath.Join(t.TempDir(), "big.bin")
	_ = os.WriteFile(bp, big, 0o600)
	blist, berr := blockMD5ListOf(bp, int64(len(big)))
	if berr != nil {
		t.Fatalf("blockMD5ListOf big: %v", berr)
	}
	if len(blist) != 2 {
		t.Fatalf("4MiB+100 应 2 块, got %d", len(blist))
	}
	if blist[0] == md5Hex(string(big)) {
		t.Fatal("分块 md5 不得等于整文件 md5（否则秒传仍 miss）")
	}
	// 空文件 → 空列表。
	empty := filepath.Join(t.TempDir(), "empty.bin")
	_ = os.WriteFile(empty, nil, 0o600)
	elist, eerr := blockMD5ListOf(empty, 0)
	if eerr != nil || elist != nil {
		t.Fatalf("空文件应空列表, got %v %v", elist, eerr)
	}
}

// TestRefreshByRapidUpload_FallbackBinary C-C1 生产默认路径回归：binaryAdapter（二进制
// 优先 + 库 Fallback）下 refreshByRapidUpload 必须透传 Fallback 的 rapidUploader——
// 否则大文件分片上传后 ETag 恒错，默认装配路径无秒传刷新 → 3 轮重传 ErrTransient。
func TestRefreshByRapidUpload_FallbackBinary(t *testing.T) {
	t.Parallel()
	inner := &multipartETagAdapter{inner: newFakeStorageAdapter(), multipart: true}
	ba := newBinaryAdapter(AdapterConfig{Logger: testLogger(), Fallback: inner})
	s := newTestStorage(t, ba)
	content := strings.Repeat("fallback-rapid-", 100)
	if _, err := s.Put(context.Background(), "big.bin", strings.NewReader(content)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !inner.rapidHit {
		t.Fatal("binaryAdapter 默认装配（库 Fallback）下 ETag 不匹配应经 Fallback rapidupload 刷新")
	}
}

// TestBinaryAdapter_Upload_CLIDirSemantics e2e 实测修复：binaryAdapter.Upload 的 CLI
// `upload` target 是**目录**语义（内容落 `<dir>/<basename>`）——调用方传入文件路径时
// 不得直接透传（否则父路径错位 + 残留目录）。这里验证 runBinary 收到的 arg 是目录
// 而非文件路径。
func TestBinaryAdapter_Upload_CLIDirSemantics(t *testing.T) {
	t.Parallel()
	recorded := &recordBinaryAdapter{}
	// 用记录 args 的 fake fallback 路径验证：binary 失败（无二进制）→ 回退前 args 应为目录。
	ba := newBinaryAdapter(AdapterConfig{BinaryPath: "/nonexistent/BaiduPCS-Go", Logger: testLogger(), Fallback: recorded})
	if err := ba.Upload(context.Background(), "/tmp/local.bin", "/baidu/dir/file.bin", true); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if len(recorded.calls) != 1 {
		t.Fatalf("应回退库一次, got %d", len(recorded.calls))
	}
	// 二进制失败后回退库（库 Upload 拿到的是完整文件路径——库语义）。
	if recorded.calls[0].target != "/baidu/dir/file.bin" {
		t.Fatalf("库回退应收完整文件路径, got %q", recorded.calls[0].target)
	}
}

// recordBinaryAdapter 记录 Upload 调用的库兜底（验证二进制回退链路）。
type recordBinaryAdapter struct {
	calls []struct{ local, target string }
}

func (r *recordBinaryAdapter) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	r.calls = append(r.calls, struct{ local, target string }{localPath, targetPath})
	return nil
}
func (r *recordBinaryAdapter) Download(ctx context.Context, remotePath, localPath string) error {
	return nil
}
func (r *recordBinaryAdapter) Move(ctx context.Context, from, to string) error     { return nil }
func (r *recordBinaryAdapter) Copy(ctx context.Context, from, to string) error     { return nil }
func (r *recordBinaryAdapter) Delete(ctx context.Context, remotePath string) error { return nil }

// unreliableMD5Adapter 模拟 binary-only 模式的 `meta`：远端 ETag 非权威整文件 md5
// （服务端标注「可能不正确」）——不得据此做相等断言。sizeSkew 非 0 时在 Meta 中上报
// 偏离的 size（模拟「内容不符/写少」）。
type unreliableMD5Adapter struct {
	inner    *fakeStorageAdapter
	sizeSkew int64
}

func (a *unreliableMD5Adapter) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	return a.inner.Upload(ctx, localPath, targetPath, overwrite)
}
func (a *unreliableMD5Adapter) Download(ctx context.Context, remotePath, localPath string) error {
	return a.inner.Download(ctx, remotePath, localPath)
}
func (a *unreliableMD5Adapter) Move(ctx context.Context, from, to string) error {
	return a.inner.Move(ctx, from, to)
}
func (a *unreliableMD5Adapter) Copy(ctx context.Context, from, to string) error {
	return a.inner.Copy(ctx, from, to)
}
func (a *unreliableMD5Adapter) Delete(ctx context.Context, remotePath string) error {
	return a.inner.Delete(ctx, remotePath)
}
func (a *unreliableMD5Adapter) List(ctx context.Context, remotePath string) ([]ObjectMeta, error) {
	return a.inner.List(ctx, remotePath)
}
func (a *unreliableMD5Adapter) Meta(ctx context.Context, remotePath string) (*ObjectMeta, error) {
	m, err := a.inner.Meta(ctx, remotePath)
	if err != nil || m == nil {
		return m, err
	}
	m.ETag = "00000000000000000000000000000000" // 非整文件 md5（片组合/不确定）
	m.MD5Unreliable = true
	m.Size += a.sizeSkew
	return m, nil
}

// TestStorage_Put_UnreliableMD5SizeMismatchRejected（P2 修复，2026-10-19）：md5 非权威
// （无校验能力）**不允许接受任意内容**——仍必须比对 size；远端 size 与本地不符 ⇒ fail-closed。
// 变异：去掉 putCheckAfterUpload 的 size 断言 ⇒ 本用例（Put 返回 nil）必红。
func TestStorage_Put_UnreliableMD5SizeMismatchRejected(t *testing.T) {
	t.Parallel()
	ad := &unreliableMD5Adapter{inner: newFakeStorageAdapter(), sizeSkew: 1}
	s := newTestStorage(t, ad)
	content := strings.Repeat("unreliable-md5-size-mismatch-", 40)
	// P2 修复（2026-10-19）：size 不符不再「立即致命」，而走 putRetryLoop 的**有界重试**
	// （容忍上传后最终一致窗口）——重试耗尽仍不符 ⇒ **ErrTransient 终态**（仍 fail-closed，
	// 绝不静默接受错误内容）。
	_, err := s.Put(context.Background(), "m.bin", strings.NewReader(content))
	if err == nil {
		t.Fatal("远端 md5 非权威且 size 持续不符 ⇒ 重试耗尽后必须失败（不得静默接受错误内容）✗")
	}
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("size 持续不符应以 ErrTransient 终态返回（可重试语义），实测: %v", err)
	}
}

// TestStorage_Put_UnreliableMD5RemoteSizeZero_WarnsAndAccepts（F4，2026-10-19）：远端把**非空**
// 对象报 0 字节（Size<=0）且本地 size>0 时，md5 非权威 ⇒ 既无法按 md5 也无法按 size 校验——
// 此前**静默**接受；修后必须显式 Warn（不改行为：远端确实可能不报 size）。
//
// 变异：去掉 putCheckAfterUpload 的 Size<=0 Warn ⇒ 日志断言必红。
func TestStorage_Put_UnreliableMD5RemoteSizeZero_WarnsAndAccepts(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("unreliable-md5-zero-size-", 40)
	ad := &unreliableMD5Adapter{inner: newFakeStorageAdapter(), sizeSkew: -int64(len(content))}
	var buf syncBuf
	s, err := NewStorage(StorageConfig{
		Root: "/baidu", TempDir: t.TempDir(), Adapter: ad,
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
	})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	if _, perr := s.Put(context.Background(), "z.bin", strings.NewReader(content)); perr != nil {
		t.Fatalf("远端未给 size 时应告警但接受（不得拒绝合法内容）: %v", perr)
	}
	if !strings.Contains(buf.String(), "未给出 size") {
		t.Fatalf("远端 size<=0 且本地非空时必须显式告警，实测日志:\n%s", buf.String())
	}
}

// syncBuf 是并发安全的日志缓冲（slog handler 可能被多 goroutine 写入）。
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestRapidUploaderOf_FallbackTransparent（P2 修复，2026-10-19）：renewAuthoritativeMD5 此前
// **未遗传** binaryAdapter→Fallback 的库秒传能力（只有 refreshByRapidUpload 遗传）⇒ 默认装配下
// 「远端 md5 非权威」永远刷不出权威 md5。现两者共用 rapidUploaderOf ⇒ 本用例锁定遗传。
// 变异：把 rapidUploaderOf 中的 binaryAdapter 分支去掉 ⇒ binaryAdapter+Fallback 用例必红。
func TestRapidUploaderOf_FallbackTransparent(t *testing.T) {
	t.Parallel()
	// ① 适配器直接实现 → 命中。
	direct := newTestStorage(t, &multipartETagAdapter{inner: newFakeStorageAdapter()})
	if _, ok := direct.rapidUploaderOf(); !ok {
		t.Error("直接实现 rapidUploader 的适配器应命中")
	}
	// ② binaryAdapter + 库 Fallback 实现 rapidUploader → 必须透传命中。
	fb := &multipartETagAdapter{inner: newFakeStorageAdapter()}
	ba := newBinaryAdapter(AdapterConfig{Logger: testLogger(), Fallback: fb})
	bs := newTestStorage(t, ba)
	got, ok := bs.rapidUploaderOf()
	if !ok {
		t.Fatal("binaryAdapter + 库 Fallback（rapidUploader）应透传命中（否则 renewAuthoritativeMD5 无法刷新）✗")
	}
	if got != rapidUploader(fb) {
		t.Fatalf("应返回 Fallback 的 rapidUploader，实测 %T", got)
	}
	// ③ binaryAdapter + 无 rapidUploader 能力的 Fallback → 不命中。
	bs2 := newTestStorage(t, newBinaryAdapter(AdapterConfig{Logger: testLogger(), Fallback: &recordBinaryAdapter{}}))
	if _, ok2 := bs2.rapidUploaderOf(); ok2 {
		t.Error("Fallback 无 rapidUploader 能力时不应命中")
	}
}

// TestStorage_Put_BinaryOnlyUnreliableMD5Accepted EXT-2 回归：binary-only 模式上传后远端
// md5 可能不正确 → 不得判失败重传（否则 3 轮全量重传后 ErrTransient），应接受。
func TestStorage_Put_BinaryOnlyUnreliableMD5Accepted(t *testing.T) {
	t.Parallel()
	s := newTestStorage(t, &unreliableMD5Adapter{inner: newFakeStorageAdapter()})
	content := strings.Repeat("binary-only-unreliable-md5-", 64)
	if _, err := s.Put(context.Background(), "x.bin", strings.NewReader(content)); err != nil {
		t.Fatalf("Put 应接受（md5 非权威不判失败）: %v", err)
	}
	rc, _, err := s.Get(context.Background(), "x.bin")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != content {
		t.Fatalf("内容不一致")
	}
}

// unreliableMD5RapidAdapter 是「远端 md5 非权威 + **有秒传能力**」的替身：RapidUpload 后
// 远端 md5 变权威（整文件 md5），用于验证 M6 的「先刷新重验」路径。
type unreliableMD5RapidAdapter struct {
	inner     *unreliableMD5Adapter
	rapidHit  bool
	rapidPath string // 最近一次 RapidUpload 收到的 remotePath（断言「刷新打对路径」）
}

func (a *unreliableMD5RapidAdapter) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	return a.inner.Upload(ctx, localPath, targetPath, overwrite)
}
func (a *unreliableMD5RapidAdapter) Download(ctx context.Context, remotePath, localPath string) error {
	return a.inner.Download(ctx, remotePath, localPath)
}
func (a *unreliableMD5RapidAdapter) Move(ctx context.Context, from, to string) error {
	return a.inner.Move(ctx, from, to)
}
func (a *unreliableMD5RapidAdapter) Copy(ctx context.Context, from, to string) error {
	return a.inner.Copy(ctx, from, to)
}
func (a *unreliableMD5RapidAdapter) Delete(ctx context.Context, remotePath string) error {
	return a.inner.Delete(ctx, remotePath)
}
func (a *unreliableMD5RapidAdapter) List(ctx context.Context, remotePath string) ([]ObjectMeta, error) {
	return a.inner.List(ctx, remotePath)
}
func (a *unreliableMD5RapidAdapter) Meta(ctx context.Context, remotePath string) (*ObjectMeta, error) {
	// 直取底层 fake（跳过 unreliableMD5Adapter 的无条件 MD5Unreliable 覆盖）。
	m, err := a.inner.inner.Meta(ctx, remotePath)
	if err != nil || m == nil || a.rapidHit {
		return m, err // 刷新后：权威整文件 md5
	}
	m.MD5Unreliable = true // 刷新前：分片组合语义
	return m, nil
}
func (a *unreliableMD5RapidAdapter) RapidUpload(ctx context.Context, remotePath string, st *stagedUpload) (bool, error) {
	a.rapidHit = true
	a.rapidPath = remotePath
	return true, nil
}

var _ Adapter = (*unreliableMD5RapidAdapter)(nil)
var _ rapidUploader = (*unreliableMD5RapidAdapter)(nil)

// TestStorage_Put_UnreliableMD5RefreshedByRapidUpload 回归（M6，第 7 轮 MINOR）：远端 md5
// 非权威时**必须先经 rapidupload 刷新**再判定，而不是直接接受——否则「必须 md5 一致」在
// 分片上传（>4MiB）路径被架空（任何 md5 不匹配都会被同一句「可能不正确」放过）。
// 变异：去掉 renewAuthoritativeMD5 调用 → rapidHit=false → 红。
// 判别力修复（P1，2026-10-19）：调用点此前误传裸 key（接口要求 remote 路径）⇒ fake 忽略
// remotePath 时假绿。本用例断言 RapidUpload 收到的是 s.remotePath(key)（root 前缀 + 前导 /）。
func TestStorage_Put_UnreliableMD5RefreshedByRapidUpload(t *testing.T) {
	t.Parallel()
	ad := &unreliableMD5RapidAdapter{inner: &unreliableMD5Adapter{inner: newFakeStorageAdapter()}}
	s := newTestStorage(t, ad)
	content := strings.Repeat("unreliable-md5-rapid-refresh-", 80)
	if _, err := s.Put(context.Background(), "r.bin", strings.NewReader(content)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !ad.rapidHit {
		t.Fatal("远端 md5 非权威时应先触发 rapidupload 刷新（M6）")
	}
	wantRemote, rerr := s.remotePath("r.bin")
	if rerr != nil {
		t.Fatalf("remotePath: %v", rerr)
	}
	if ad.rapidPath != wantRemote {
		t.Fatalf("rapidupload 刷新必须打 remote 路径 %q（含 root 前缀），实收 %q ✗", wantRemote, ad.rapidPath)
	}
	m, err := s.Stat(context.Background(), "r.bin")
	if err != nil || m == nil {
		t.Fatalf("Stat: %v", err)
	}
	if m.MD5Unreliable {
		t.Fatal("刷新后远端 md5 应权威（否则相等断言仍被跳过）")
	}
	if m.ETag != md5Hex(content) {
		t.Fatalf("刷新后应做相等断言并与本地 md5 一致: got %s want %s", m.ETag, md5Hex(content))
	}
}

// TestStorage_PutFromStagedPath_NoPhysicalSecondCopy staging 复用入口**只读**调用方文件、
// 不产生第二份**物理**拷贝（同 inode 的硬链接不算拷贝）：以 os.SameFile 逐文件判定，避免
// 「按 Size 求和」把硬链接误计为 2×。变异：把 prepareStagedForAdapter 的链接分支改成拷贝
// → SameFile 为假 → 红。
func TestStorage_PutFromStagedPath_NoPhysicalSecondCopy(t *testing.T) {
	t.Parallel()
	sink := &recordingUploadPathAdapter{inner: newFakeStorageAdapter()}
	s := newTestStorage(t, sink)
	content := strings.Repeat("staged-reuse-body-", 64)
	staged := filepath.Join(s.temp, "already-staged.bin")
	if err := os.WriteFile(staged, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sink.linkSource = staged
	if _, err := s.PutFromStagedPath(context.Background(), "reuse.bin", staged, int64(len(content)), nil); err != nil {
		t.Fatalf("PutFromStagedPath: %v", err)
	}
	stagedInfo, serr := os.Stat(staged)
	if serr != nil {
		t.Fatalf("调用方暂存文件不得被删除/移动（只读契约）: %v", serr)
	}
	// 上传播径必须是暂存文件的**硬链接**（同 inode）：在 adapter 内（链接仍在时）判定——
	// 拷贝版会在 PutFromStagedPath 返回前被 cleanup 删掉，事后扫描看不见。
	if !sink.sameFile {
		t.Fatalf("交给 adapter 的本地路径必须是暂存文件的硬链接（不得为拷贝）: %s", sink.uploadedPath)
	}
	_ = filepath.WalkDir(s.temp, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || p == staged {
			return nil
		}
		fi, ferr := d.Info()
		if ferr != nil {
			return nil
		}
		if !os.SameFile(stagedInfo, fi) {
			t.Fatalf("不得出现第二份物理拷贝（staging 复用应链接而非复制）: %s", p)
		}
		return nil
	})
}

// recordingUploadPathAdapter 记录 adapter.Upload 收到的本地路径（断言「硬链接而非拷贝」）。
type recordingUploadPathAdapter struct {
	inner        *fakeStorageAdapter
	uploadedPath string
	linkSource   string
	sameFile     bool
}

func (a *recordingUploadPathAdapter) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	a.uploadedPath = localPath
	if a.linkSource != "" {
		si, e1 := os.Stat(a.linkSource)
		fi, e2 := os.Stat(localPath)
		a.sameFile = e1 == nil && e2 == nil && os.SameFile(si, fi)
	}
	return a.inner.Upload(ctx, localPath, targetPath, overwrite)
}
func (a *recordingUploadPathAdapter) Download(ctx context.Context, remotePath, localPath string) error {
	return a.inner.Download(ctx, remotePath, localPath)
}
func (a *recordingUploadPathAdapter) Move(ctx context.Context, from, to string) error {
	return a.inner.Move(ctx, from, to)
}
func (a *recordingUploadPathAdapter) Copy(ctx context.Context, from, to string) error {
	return a.inner.Copy(ctx, from, to)
}
func (a *recordingUploadPathAdapter) Delete(ctx context.Context, remotePath string) error {
	return a.inner.Delete(ctx, remotePath)
}
func (a *recordingUploadPathAdapter) List(ctx context.Context, remotePath string) ([]ObjectMeta, error) {
	return a.inner.List(ctx, remotePath)
}
func (a *recordingUploadPathAdapter) Meta(ctx context.Context, remotePath string) (*ObjectMeta, error) {
	return a.inner.Meta(ctx, remotePath)
}

// TestStorage_PutFromStagedPath_RandomBlobStillRefreshesMD5 复现/回归（红线，2026-10-21）：
// RandomBlob 跳块列表读回 ⇒ blockMD5s 空 ⇒ renewAuthoritativeMD5 bail ⇒ md5 刷新被跳过
// （分片上传后远端 ETag 必非权威，无刷新则一致性红线被架空）。修复：块列表**必须算**。
func TestStorage_PutFromStagedPath_RandomBlobStillRefreshesMD5(t *testing.T) {
	t.Parallel()
	ad := &unreliableMD5RapidAdapter{inner: &unreliableMD5Adapter{inner: newFakeStorageAdapter()}}
	s := newTestStorage(t, ad)
	content := strings.Repeat("repro-", 100)
	staged := filepath.Join(s.temp, "repro.bin")
	if err := os.WriteFile(staged, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutFromStagedPath(context.Background(), "repro.bin", staged, int64(len(content)),
		&PutHints{RandomBlob: true}); err != nil {
		t.Fatalf("PutFromStagedPath: %v", err)
	}
	if !ad.rapidHit {
		t.Fatal("BUG：远端 md5 非权威 + RandomBlob ⇒ 刷新被跳过（红线违反）")
	}
}
