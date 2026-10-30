// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// errReader 在读取时返回非超限错误（模拟客户端断连/传输错误）。
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }
func (errReader) Close() error             { return nil }

// TestS3ReadBody_ReadErrorRejected 回归：读失败必须 400，不得返回 (nil,true) 让验签回落到
// 客户端声明的 x-amz-content-sha256（否则空 body 可绕过并落 0 字节对象）。
func TestS3ReadBody_ReadErrorRejected(t *testing.T) {
	t.Parallel()
	s := &Service{}
	req := httptest.NewRequest(http.MethodPut, "/s3/b/k", errReader{})
	req.Header.Set("x-amz-content-sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	rec := httptest.NewRecorder()

	body, ok := s.s3ReadBody(rec, req)
	if ok {
		t.Fatalf("读失败必须返回 ok=false（否则会以空 body 继续验签），got body=%v", body)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d，want 400", rec.Code)
	}
}

// TestS3ReadBody_NilBodySkipped 无 body 的直调路径保持零回归（跳过读取）。
func TestS3ReadBody_NilBodySkipped(t *testing.T) {
	t.Parallel()
	s := &Service{}
	req := httptest.NewRequest(http.MethodDelete, "/s3/b/k", nil)
	req.Body = nil
	rec := httptest.NewRecorder()
	if body, ok := s.s3ReadBody(rec, req); !ok || body != nil {
		t.Fatalf("nil body 应返回 (nil,true)，got (%v,%v)", body, ok)
	}
}

var _ io.Reader = errReader{}
