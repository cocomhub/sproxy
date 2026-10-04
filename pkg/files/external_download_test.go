// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeSource 是 DownloadSource 的测试实现：内存 seeker（模拟服务端读取源，
// secretdata 加密卷解密转发 / 私密外部卷整流同构）。
type fakeSource struct {
	data []byte
}

func (f *fakeSource) Stat(context.Context) (fs.FileInfo, error) {
	return fakeFileInfo{name: "external.bin", size: int64(len(f.data)), modTime: time.Time{}}, nil
}

func (f *fakeSource) Open(context.Context) (SeekReadCloser, error) {
	return &fakeSeeker{data: f.data}, nil
}

// fakeSeeker 是内存 io.ReadSeeker+Closer（bytes.Reader 包装）。
type fakeSeeker struct {
	data []byte
	off  int64
}

func (f *fakeSeeker) Read(p []byte) (int, error) {
	if f.off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.off:])
	f.off += int64(n)
	return n, nil
}

func (f *fakeSeeker) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = f.off + offset
	case io.SeekEnd:
		next = int64(len(f.data)) + offset
	default:
		return f.off, errFakeSeekWhence
	}
	if next < 0 {
		return f.off, errFakeSeekNegative
	}
	f.off = next
	return f.off, nil
}

var (
	errFakeSeekWhence   = errors.New("fake seek: 非法 whence")
	errFakeSeekNegative = errors.New("fake seek: 负偏移")
)

func (f *fakeSeeker) Close() error { return nil }

// fakeFileInfo 是最小 os.FileInfo 实现（dirs_test 内同类模式；此处独立定义不耦合）。
type fakeFileInfo struct {
	name    string
	size    int64
	modTime time.Time
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return f.size }
func (f fakeFileInfo) Mode() fs.FileMode  { return 0o600 }
func (f fakeFileInfo) ModTime() time.Time { return f.modTime }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

// TestService_Download_RedirectURL302：装配层判定明文外部卷未私密 → RedirectURL 非空
// → Download 直接 302（流量不经服务端），不 OpenPath。
func TestService_Download_RedirectURL302(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.resolveDownloadPath = func(r *http.Request) (DownloadPath, error) {
		return DownloadPath{
			Filename:    "f.bin",
			VolumeName:  "baidu",
			RedirectURL: "https://d.pcs.baidu.com/file/f.bin?sign=test123",
		}, nil
	}
	env.rebuild()

	req := readReq("alice", "GET", "/download?filename=f.bin")
	rr := httptest.NewRecorder()
	env.svc.Download(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("302 直链应返回 302, got %d: %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "https://d.pcs.baidu.com/file/f.bin?sign=test123" {
		t.Fatalf("Location=%q want 百度直链", loc)
	}
}

// TestService_Download_SourceRange206：外部卷服务端读取源（A/C 态）→ OpenPath 经
// Source 打开，Range 请求返回 206 + Content-Range + 内容一致（解密转发顶点）。
func TestService_Download_SourceRange206(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	const body = "0123456789abcdef"
	env.resolveDownloadPath = func(r *http.Request) (DownloadPath, error) {
		return DownloadPath{
			Filename:   "movie.bin",
			VolumeName: "secret",
			Source:     &fakeSource{data: []byte(body)},
		}, nil
	}
	env.rebuild()

	req := readReq("alice", "GET", "/download?filename=movie.bin")
	req.Header.Set("Range", "bytes=2-7")
	rr := httptest.NewRecorder()
	env.svc.Download(rr, req)

	if rr.Code != http.StatusPartialContent {
		t.Fatalf("外部卷 Range 应 206, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != body[2:8] {
		t.Fatalf("Range 内容=%q want %q", got, body[2:8])
	}
	if cr := rr.Header().Get("Content-Range"); cr != "bytes 2-7/16" {
		t.Fatalf("Content-Range=%q want bytes 2-7/16", cr)
	}
}

// TestService_Download_SourceFull200：外部卷服务端读取源无 Range → 200 全量整流。
func TestService_Download_SourceFull200(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	const body = "hello external volume"
	env.resolveDownloadPath = func(r *http.Request) (DownloadPath, error) {
		return DownloadPath{
			Filename:   "f.bin",
			VolumeName: "sftp",
			Source:     &fakeSource{data: []byte(body)},
		}, nil
	}
	env.rebuild()

	req := readReq("alice", "GET", "/download?filename=f.bin")
	rr := httptest.NewRecorder()
	env.svc.Download(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("整流下载应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !bytes.Equal(rr.Body.Bytes(), []byte(body)) {
		t.Fatalf("整流内容 != 原文：got %q", rr.Body.String())
	}
}

// TestService_Stat_SourceExternal：外部卷 Stat 经 Source 返回元信息（不 Redirect）。
func TestService_Stat_SourceExternal(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	const body = "stat me"
	env.resolveDownloadPath = func(r *http.Request) (DownloadPath, error) {
		return DownloadPath{
			Filename:   "f.bin",
			VolumeName: "secret",
			Source:     &fakeSource{data: []byte(body)},
		}, nil
	}
	env.rebuild()

	req := readReq("alice", "GET", "/download?filename=f.bin")
	rr := httptest.NewRecorder()
	env.svc.Stat(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("外部卷 Stat 应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if size := rr.Header().Get("X-File-Size"); size != "7" {
		t.Fatalf("X-File-Size=%q want 7", size)
	}
}

var _ = strings.TrimSpace
