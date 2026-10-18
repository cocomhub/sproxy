// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
)

// errReadCloser 模拟「读取期才报错」的 reader（minio 的 GetObject 是**惰性**的：
// 缺失键的错误在首次 Read 才出现，而非 OpenRead 调用时）。
type errReadCloser struct{ err error }

func (e *errReadCloser) Read([]byte) (int, error) { return 0, e.err }
func (e *errReadCloser) Close() error             { return nil }

// TestNotExistMappedReader_ReadTimeAbsentWrapsErrNotExist 读期暴露的「不存在」必须映射为
// `os.ErrNotExist`（**项目 not-exist 约定**，见 pkg/volume/enclosure/region.go）。
//
// 为何是硬要求：卷内账本（pkg/volume/capacity）据「不存在 vs 真错误」区分「空账本」与
// 「损坏 ⇒ fail-closed 拒写」。S3 的缺失键错误在读期才出现且**不满足** os.ErrNotExist
// ⇒ 被判真错误 ⇒ **该 S3 卷全部写入被拒** ✗。
func TestNotExistMappedReader_ReadTimeAbsentWrapsErrNotExist(t *testing.T) {
	t.Parallel()
	inner := &errReadCloser{err: &minio.ErrorResponse{Code: "NoSuchKey", Message: "The specified key does not exist."}}
	r := &notExistMappedReader{ReadCloser: inner, rel: "meta/quota/shards"}
	_, err := io.ReadAll(r)
	if err == nil {
		t.Fatal("读期「不存在」必须返回错误")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("读期「不存在」必须满足 errors.Is(err, os.ErrNotExist)（项目约定）: %v", err)
	}
}

// TestNotExistMappedReader_RealErrorStaysUndetermined **对照组**：真错误必须原样上抛，
// **不得**被映射为不存在——否则账本把「读失败」当空账本 ⇒ 限额 fail-open（最危险形态）✗。
func TestNotExistMappedReader_RealErrorStaysUndetermined(t *testing.T) {
	t.Parallel()
	inner := &errReadCloser{err: errors.New("connection reset by peer")}
	r := &notExistMappedReader{ReadCloser: inner, rel: "meta/quota/shards"}
	_, err := io.ReadAll(r)
	if err == nil {
		t.Fatal("真错误必须上抛")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("真错误**不得**被映射为不存在（fail-open ✗）: %v", err)
	}
}

// TestNotExistMappedReader_EOFPassthrough 正常读与 EOF 不得被改写（透传零回归）。
func TestNotExistMappedReader_EOFPassthrough(t *testing.T) {
	t.Parallel()
	r := &notExistMappedReader{ReadCloser: io.NopCloser(strings.NewReader("ok")), rel: "x"}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("正常读不得报错: %v", err)
	}
	if string(b) != "ok" {
		t.Fatalf("读到 %q, want %q", b, "ok")
	}
}
