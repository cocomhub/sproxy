// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"crypto/md5" //nolint:gosec // 测试双算法校验
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
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
	if e.multipart && !e.rapidHit && m != nil {
		// 分片语义：真实 md5 算好前 Stat 返回"片组合"（伪造值，≠ 整文件 md5）。
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
