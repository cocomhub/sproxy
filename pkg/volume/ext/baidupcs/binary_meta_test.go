// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// 以下夹具为 WSL 实测（BaiduPCS-Go v4.0.1，2026-10-10）的真实输出。
const metaOutputFile = `[0] - [/downList.tgz] --------------

  类型              文件                              
  文件路径          /downList.tgz                     
  文件名称          downList.tgz                      
  文件大小          4712617, 4.494302MB               
  md5 (可能不正确)  b69d50218b268594bae4161264c45252  
  app_id            250528                            
  fs_id             775970150298282                   
  创建日期          2026-08-21 15:06:04               
  修改日期          2026-08-21 15:06:04               
`

const metaOutputDir = `[0] - [/我的资源] --------------

  类型            目录                 
  目录路径        /我的资源            
  目录名称        我的资源             
  app_id          250528               
  fs_id           799581621227860      
  创建日期        2017-10-05 16:06:27  
  修改日期        2026-01-04 10:51:37  
  是否含有子目录  true                 
`

const metaOutputTwo = `[0] - [/downList.tgz] --------------

  类型              文件                              
  文件大小          4712617, 4.494302MB               
  md5 (可能不正确)  b69d50218b268594bae4161264c45252  
  修改日期          2026-08-21 15:06:04               

[1] - [/test.cocoma] --------------

  类型              文件                              
  文件大小          4362320, 4.160233MB               
  md5 (截图请打码)  66eb57fb6e8a6f4f5cd2c094990a23ba  
  修改日期          2026-04-16 10:19:34               
`

const listOutput = `
当前目录: /
----
  #     文件大小         修改日期                                     文件(目录)                                
   0             -  2026-04-20 21:01:08  01 [金田一少年事件簿]/  
  14      353.75MB  2026-04-23 16:49:04  465014.cocoma                                                          
  15        4.49MB  2026-08-21 15:06:04  downList.tgz                                                           
      总: 362.40MB                       文件总数: 2, 目录总数: 1                                              
----
`

func TestParseBinaryMeta_File(t *testing.T) {
	t.Parallel()
	m, err := parseBinaryMeta([]byte(metaOutputFile), "/downList.tgz")
	if err != nil {
		t.Fatalf("parseBinaryMeta: %v", err)
	}
	if m.Size != 4712617 {
		t.Fatalf("Size = %d, want 4712617", m.Size)
	}
	if m.ETag != "b69d50218b268594bae4161264c45252" {
		t.Fatalf("ETag = %q", m.ETag)
	}
	if m.IsDir {
		t.Fatal("文件不应 IsDir")
	}
	want := time.Date(2026, 8, 21, 15, 6, 4, 0, time.Local)
	if !m.ModTime.Equal(want) {
		t.Fatalf("ModTime = %v, want %v", m.ModTime, want)
	}
}

func TestParseBinaryMeta_Dir(t *testing.T) {
	t.Parallel()
	m, err := parseBinaryMeta([]byte(metaOutputDir), "/我的资源")
	if err != nil {
		t.Fatalf("parseBinaryMeta: %v", err)
	}
	if !m.IsDir {
		t.Fatal("目录应 IsDir=true")
	}
}

func TestParseBinaryMetaBlocks_Two(t *testing.T) {
	t.Parallel()
	blocks := parseBinaryMetaBlocks([]byte(metaOutputTwo))
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(blocks))
	}
	if m := blocks["/downList.tgz"]; m == nil || m.Size != 4712617 {
		t.Fatalf("downList.tgz 块不符: %+v", m)
	}
	// md5 标签带截图提示时也应取到 hex。
	if m := blocks["/test.cocoma"]; m == nil || m.ETag != "66eb57fb6e8a6f4f5cd2c094990a23ba" {
		t.Fatalf("test.cocoma 块不符: %+v", m)
	}
}

func TestParseBinaryList(t *testing.T) {
	t.Parallel()
	entries, err := parseBinaryList([]byte(listOutput))
	if err != nil {
		t.Fatalf("parseBinaryList: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3 (%+v)", len(entries), entries)
	}
	if !entries[0].IsDir || entries[0].Key != "01 [金田一少年事件簿]" {
		t.Fatalf("目录条目不符: %+v", entries[0])
	}
	if entries[1].IsDir || entries[1].Key != "465014.cocoma" {
		t.Fatalf("文件条目不符: %+v", entries[1])
	}
	mib := 1 << 20
	if entries[1].Size != int64(353.75*float64(mib)) { // 近似值（精确值由 meta 覆盖）
		t.Fatalf("近似 Size = %d", entries[1].Size)
	}
}

func TestParseHumanSize(t *testing.T) {
	t.Parallel()
	mib := 1 << 20 // 变量（非编译期常量），避免 float 常量转 int64 报错
	cases := map[string]int64{
		"-":        0,
		"123B":     123,
		"1KB":      1 << 10,
		"4.49MB":   int64(4.49 * float64(mib)),
		"353.75MB": int64(353.75 * float64(mib)),
		"2GB":      2 << 30,
	}
	for in, want := range cases {
		if got := parseHumanSize(in); got != want {
			t.Errorf("parseHumanSize(%q) = %d, want %d", in, got, want)
		}
	}
}

// fakeMetaFallback 实现 libraryFallback + metadataProvider（binary 失败回退断言用）。
type fakeMetaFallback struct {
	meta    *ObjectMeta
	list    []ObjectMeta
	metaErr error
}

func (f *fakeMetaFallback) Upload(context.Context, string, string, bool) error { return nil }
func (f *fakeMetaFallback) Download(context.Context, string, string) error     { return nil }
func (f *fakeMetaFallback) Move(context.Context, string, string) error         { return nil }
func (f *fakeMetaFallback) Copy(context.Context, string, string) error         { return nil }
func (f *fakeMetaFallback) Delete(context.Context, string) error               { return nil }
func (f *fakeMetaFallback) Meta(context.Context, string) (*ObjectMeta, error) {
	return f.meta, f.metaErr
}
func (f *fakeMetaFallback) List(context.Context, string) ([]ObjectMeta, error) { return f.list, nil }

// TestBinaryAdapter_Meta_Fallback binary 执行失败 → 回退库 metadata（不落回全量下载）。
func TestBinaryAdapter_Meta_Fallback(t *testing.T) {
	t.Parallel()
	fb := &fakeMetaFallback{meta: &ObjectMeta{Key: "/x", Size: 42, ETag: "e"}}
	a := newBinaryAdapter(AdapterConfig{BinaryPath: "definitely-not-a-real-binary-xyz", Fallback: fb})
	m, err := a.Meta(context.Background(), "/x")
	if err != nil {
		t.Fatalf("应回退库 Meta: %v", err)
	}
	if m == nil || m.Size != 42 {
		t.Fatalf("回退值不符: %+v", m)
	}
}

// TestBinaryAdapter_Meta_NoFallback binary 失败且无兜底 → 返回错误（fail-closed）。
func TestBinaryAdapter_Meta_NoFallback(t *testing.T) {
	t.Parallel()
	a := newBinaryAdapter(AdapterConfig{BinaryPath: "definitely-not-a-real-binary-xyz"})
	if _, err := a.Meta(context.Background(), "/x"); err == nil {
		t.Fatal("无兜底应返回错误（不得静默落回全量下载）")
	}
	if _, err := a.List(context.Background(), "/x"); err == nil {
		t.Fatal("List 无兜底应返回错误")
	}
}

// TestBinaryAdapter_List_Fallback binary 失败 → 回退库 List。
func TestBinaryAdapter_List_Fallback(t *testing.T) {
	t.Parallel()
	fb := &fakeMetaFallback{list: []ObjectMeta{{Key: "/a", Size: 1}}}
	a := newBinaryAdapter(AdapterConfig{BinaryPath: "definitely-not-a-real-binary-xyz", Fallback: fb})
	got, err := a.List(context.Background(), "/d")
	if err != nil {
		t.Fatalf("应回退库 List: %v", err)
	}
	if len(got) != 1 || got[0].Size != 1 {
		t.Fatalf("回退列表不符: %+v", got)
	}
}

// ---- P0-2 回归：BaiduPCS-Go 失败时退出码恒 0、错误进 stdout，且 meta 块头先打印 ----

const metaOutputCLIError = `[0] - [/不存在] --------------

文件不存在
remote path is not absolute
`

const listOutputCLIError = `帐号未登录
`

const listOutputEmptyDir = `
当前目录: /empty
----
  #     文件大小         修改日期                                     文件(目录)
----
`

// TestParseBinaryMeta_CLIErrorNoFabrication CLI 失败不得伪造「存在」的元信息。
func TestParseBinaryMeta_CLIErrorNoFabrication(t *testing.T) {
	t.Parallel()
	m, err := parseBinaryMeta([]byte(metaOutputCLIError), "/不存在")
	if err == nil {
		t.Fatalf("CLI 失败应返回 error（不得伪造 ObjectMeta：%+v）", m)
	}
}

// TestParseBinaryList_CLIErrorReturnsError 无目录头也无条目 → 视为 CLI 失败（非空目录）。
func TestParseBinaryList_CLIErrorReturnsError(t *testing.T) {
	t.Parallel()
	if _, err := parseBinaryList([]byte(listOutputCLIError)); err == nil {
		t.Fatal("CLI 失败输出应返回 error（不得当空目录）")
	}
}

// TestParseBinaryList_EmptyDirOK 真空目录（含目录头）仍应成功返回 0 条目。
func TestParseBinaryList_EmptyDirOK(t *testing.T) {
	t.Parallel()
	es, err := parseBinaryList([]byte(listOutputEmptyDir))
	if err != nil || len(es) != 0 {
		t.Fatalf("空目录应 0 条目且无错: n=%d err=%v", len(es), err)
	}
}

// TestParseBinaryMeta_SingleBlockDifferentPathRejected 唯一块键与请求不同（CLI glob 展开成
// 别的对象）时不得改写采用。
func TestParseBinaryMeta_SingleBlockDifferentPathRejected(t *testing.T) {
	t.Parallel()
	if _, err := parseBinaryMeta([]byte(metaOutputFile), "/other.tgz"); err == nil {
		t.Fatal("块键不匹配时不应采用（防取到别的对象元信息）")
	}
}

// TestParseBinaryMeta_MD5Unreliable 带括号注记的 md5 行标记为非权威（EXT-2）。
func TestParseBinaryMeta_MD5Unreliable(t *testing.T) {
	t.Parallel()
	m, err := parseBinaryMeta([]byte(metaOutputFile), "/downList.tgz")
	if err != nil {
		t.Fatalf("parseBinaryMeta: %v", err)
	}
	if !m.MD5Unreliable {
		t.Fatal("`md5 (可能不正确)` 应标记 MD5Unreliable")
	}
}

// TestSanitizeRemotePath_RejectsGlob EXT-3：CLI glob 元字符 `*`/`?` 被 BaiduPCS-Go 展开，
// 可能把别的对象当目标 → fail-closed 拒绝（`[` 由 CLI escaper 转义，不拒）。
func TestSanitizeRemotePath_RejectsGlob(t *testing.T) {
	t.Parallel()
	for _, p := range []string{"/dir/a*b.txt", "/dir/a?b.txt", "a*b"} {
		if _, err := sanitizeRemotePath(p); err == nil {
			t.Fatalf("路径 %q 含 glob 元字符应拒绝", p)
		}
	}
	// `[` 合法（escaper 处理）：不拒。
	if _, err := sanitizeRemotePath("/dir/01 [金田一少年事件簿]/x"); err != nil {
		t.Fatalf("含 `[` 的合法路径不应拒绝: %v", err)
	}
}

// TestParseBinaryList_InternalSpacesPreserved EXT-4：文件名内部连续空格必须保留
// （strings.Fields+Join 会归一为单空格 → 后续寻址错对象）。
func TestParseBinaryList_InternalSpacesPreserved(t *testing.T) {
	t.Parallel()
	const out = `
当前目录: /
----
  #     文件大小         修改日期                                     文件(目录)
   7        1.00MB  2026-04-23 16:49:04  a  b.txt
----
`
	es, err := parseBinaryList([]byte(out))
	if err != nil || len(es) != 1 {
		t.Fatalf("parseBinaryList: n=%d err=%v", len(es), err)
	}
	if es[0].Key != "a  b.txt" {
		t.Fatalf("内部空格应保留, got %q", es[0].Key)
	}
}

// TestIsBinaryNotFound 第 4 轮对抗评审 P1：CLI 输出须归类「不存在」，否则 binary-only
// 模式下任意新文件上传在写前失败（putCheckExisting 需 errors.Is(ErrNotFound)）。
func TestIsBinaryNotFound(t *testing.T) {
	t.Parallel()
	cases := []struct {
		out  string
		want bool
	}{
		{metaOutputCLIError, true},
		{"文件不存在", true},
		{"目录不存在", true},
		{"not found", true},
		{"No such file or directory", true},
		{listOutputCLIError, false}, // 帐号未登录
		{metaOutputFile, false},     // 正常输出
		{"", false},
	}
	for _, tc := range cases {
		if got := isBinaryNotFound([]byte(tc.out)); got != tc.want {
			t.Errorf("isBinaryNotFound(%.30q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}

// TestBinaryAdapter_Meta_CLINotFoundIsErrNotFound P2 回归（第 5 轮对抗评审）：
// `runBinaryOutput` 失败时 out 恒 nil，原实现 `isBinaryNotFound(out)` 是死代码 →
// CLI 以非零退出报告「路径不存在」时不会归类 ErrNotFound → binary-only 模式下任意
// 新文件上传在 putCheckExisting 写前失败。修复：stderr 文本并入错误串，判定改看它。
func TestBinaryAdapter_Meta_CLINotFoundIsErrNotFound(t *testing.T) {
	t.Parallel()
	bin := writeNotFoundScript(t)
	a := newBinaryAdapter(AdapterConfig{BinaryPath: bin})
	_, err := a.Meta(context.Background(), "/no/such/file")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("CLI 报告不存在应归类 ErrNotFound，得到 %v", err)
	}
}

// writeNotFoundScript 写一个「向 stderr 打 not-found 文本并非零退出」的可执行脚本（跨平台；
// 用 ASCII 短语避免 Windows 控制台代码页把 UTF-8 中文弄乱）。
func writeNotFoundScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		p := filepath.Join(dir, "cli.bat")
		if err := os.WriteFile(p, []byte("@echo off\r\necho No such file or directory 1>&2\r\nexit /b 1\r\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := filepath.Join(dir, "cli.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho 'No such file or directory' 1>&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}
