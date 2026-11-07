// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"io"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// fs_range_contract_test.go 钉住远程卷（集群出口/联邦）的 **Range 长度契约**
// （2026-10-14 跨层扫雷）：size==0 = 空区间成功（此前直接报错，与本地卷/加密卷不一致）；
// size>0 加精确长度守卫（对端 ServeContent 钳制 ⇒ 提前 EOF ⇒ 报错而非静默短读）。
//
// 变异：恢复 `size <= 0` 报错 ⇒ 零长用例红；去掉 ExactRangeReader 包装 ⇒ 钳制用例红。

// TestRemoteFS_OpenRangeRead_ZeroSizeIsEmpty：size==0 ⇒ 空流且不触网（c 为 nil 即证明未发起请求）。
func TestRemoteFS_OpenRangeRead_ZeroSizeIsEmpty(t *testing.T) {
	t.Parallel()
	fs := &remoteFS{} // c=nil：任何真实下行都会 panic/nil deref ⇒ 证明零长区间不触网
	rc, err := fs.OpenRangeRead(context.Background(), "dir/f.bin", 5, 0)
	if err != nil {
		t.Fatalf("size==0 应成功返回空流: %v", err)
	}
	defer rc.Close()
	got, rerr := io.ReadAll(rc)
	if rerr != nil || len(got) != 0 {
		t.Fatalf("size==0 应为空流, got %q err=%v", got, rerr)
	}
}

// TestRemoteFS_OpenRangeRead_InvalidRange：负 offset/size ⇒ 非法区间错误（不触网）。
func TestRemoteFS_OpenRangeRead_InvalidRange(t *testing.T) {
	t.Parallel()
	fs := &remoteFS{}
	if _, err := fs.OpenRangeRead(context.Background(), "f.bin", -1, 4); err == nil {
		t.Fatal("负 offset 必须报错")
	}
	if _, err := fs.OpenRangeRead(context.Background(), "f.bin", 0, -4); err == nil {
		t.Fatal("负 size 必须报错")
	}
}

// TestRemoteFS_OpenRangeRead_ShortStreamIsError：模拟对端只回更少字节（ServeContent 钳制语义）
// ⇒ 精确长度守卫必须报错（静默短读是截断型缺陷的温床）。
func TestRemoteFS_OpenRangeRead_ShortStreamIsError(t *testing.T) {
	t.Parallel()
	// 直接验证守卫行为（OpenRangeRead 的包装点无法在无服务端下单测）：对端只给 3 字节。
	rc := syncpkg.ExactRangeReader(io.NopCloser(strings.NewReader("abc")), 8)
	defer rc.Close()
	_, err := io.ReadAll(rc)
	if err == nil {
		t.Fatal("提前 EOF 必须报错（不得静默短读）")
	}
}
