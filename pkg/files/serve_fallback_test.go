// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// errAfterReader 先返回 n 字节再回错（模拟校验失败/底层损坏）。
type errAfterReader struct {
	data []byte
	pos  int64
	err  error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.pos >= int64(len(r.data)) {
		return 0, r.err
	}
	n := copy(p, r.data[r.pos:])
	r.pos += int64(n)
	return n, nil
}

func (r *errAfterReader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.pos = off
	case io.SeekCurrent:
		r.pos += off
	case io.SeekEnd:
		// 谎报更大尺寸：使 ServeContent 试图读取超过实际可提供的数据 → 触发 Read 错误
		// （否则 CopyN 恰好读满、错误永不被观察，与真实损坏场景不符）。
		r.pos = int64(len(r.data)) + 10 + off
	}
	return r.pos, nil
}

// TestServeVerifiedFallback_AbortsOnReadError transform 回退分支必须与主链同口径：
// 读到错误时**中断连接**（panic http.ErrAbortHandler），不得静默截断 200。
// 变异：回退分支改回裸 http.ServeContent → 不再 panic → 红。
func TestServeVerifiedFallback_AbortsOnReadError(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/download?filename=a.bin", nil)
	dp := DownloadPath{Filename: "a.bin", VolumeName: "main"}
	seeker := &errAfterReader{data: []byte("partial-content"), err: errors.New("校验失败：分块 sha256 不一致")}

	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("读到错误时必须中断连接（panic http.ErrAbortHandler）——裸 ServeContent 会静默截断 200")
		}
		if !strings.Contains(errText(v), "abort") && v != http.ErrAbortHandler {
			t.Fatalf("应 panic http.ErrAbortHandler, got %v", v)
		}
	}()
	env.svc.serveVerifiedFallback(rec, req, dp, seeker, "a.bin", time.Now())
}

func errText(v any) string {
	if err, ok := v.(error); ok {
		return err.Error()
	}
	return ""
}

// TestServeVerifiedFallback_SuccessNoAbort 正常内容不得中断，且内容完整。
func TestServeVerifiedFallback_SuccessNoAbort(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/download?filename=a.bin", nil)
	dp := DownloadPath{Filename: "a.bin", VolumeName: "main"}
	body := []byte("complete-content")
	env.svc.serveVerifiedFallback(rec, req, dp, bytes.NewReader(body), "a.bin", time.Now())
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码应为 200, got %d", rec.Code)
	}
	if rec.Body.String() != string(body) {
		t.Fatalf("内容应完整, got %q", rec.Body.String())
	}
}
