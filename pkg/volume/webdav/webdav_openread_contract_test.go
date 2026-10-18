// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webdav

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestWebDAVFS_OpenReadMissingWrapsErrNotExist 缺失路径的错误必须满足
// `errors.Is(err, os.ErrNotExist)`（**项目 not-exist 约定**，见 pkg/volume/enclosure/region.go）。
//
// 为何是硬要求：卷内账本（pkg/volume/capacity）据「不存在 vs 真错误」区分「空账本」与
// 「损坏 ⇒ fail-closed 拒写」。裸错误会被判为真错误 ⇒ **该 WebDAV 卷全部写入被拒** ✗。
func TestWebDAVFS_OpenReadMissingWrapsErrNotExist(t *testing.T) {
	t.Parallel()
	srv := newFakeWebDAVServer()
	ts := newTestServer(t, srv)
	fs := newClient(t, ts.URL)

	_, err := fs.OpenRead(context.Background(), "no/such/file.txt")
	if err == nil {
		t.Fatal("缺失路径必须返回错误")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("缺失路径错误必须满足 errors.Is(err, os.ErrNotExist)（项目约定）: %v", err)
	}
}
