// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// close_checksum_test.go 锁「Handlers.Close 必须关闭 per-owner 校验台账」。
//
// 真实缺陷（CI windows 抓到）：台账落盘走 debounce 合并器（窗口默认 1s）。
// 此前 Close() 只停止调度器/后台服务，**没有关闭台账实例** ⇒ Close 返回后仍有迟到的
// 落盘 goroutine 往租户 `meta/` 目录写文件：
//   - 测试侧：`t.TempDir` 清理报 "directory is not empty"（Test (windows) 实测）；
//   - 生产侧：优雅停服可能丢掉最后一次台账写（重启后校验台账少条目）。
//
// 与 SetStateStore 的既有做法一致（切换后端前先 Close 旧实例），Close 也必须做同样的事。

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/checksum"
)

// fakeChecksumStore 记录 Close 是否被调用（满足 ChecksumStoreIface + checksum.Closer）。
type fakeChecksumStore struct {
	closed  bool
	flushed int
}

func (f *fakeChecksumStore) Get(string) (string, bool) { return "", false }
func (f *fakeChecksumStore) Set(string, string)        {}
func (f *fakeChecksumStore) Delete(string)             {}
func (f *fakeChecksumStore) Rename(string, string)     {}
func (f *fakeChecksumStore) DeletePrefix(string)       {}
func (f *fakeChecksumStore) GetAll() map[string]string { return nil }
func (f *fakeChecksumStore) Close() error              { f.closed = true; return nil }
func (f *fakeChecksumStore) Flush() error              { f.flushed++; return nil }

var _ checksum.ChecksumStoreIface = (*fakeChecksumStore)(nil)
var _ checksum.Closer = (*fakeChecksumStore)(nil)

// TestHandlersClose_ClosesCachedChecksumStores 断言 Close 会关闭并清空缓存的台账实例。
func TestHandlersClose_ClosesCachedChecksumStores(t *testing.T) {
	t.Parallel()
	h, _ := newSecretsHandlers(t, nil)

	fake := &fakeChecksumStore{}
	h.tenantMu.Lock()
	h.checksumStores["alice"] = fake
	h.tenantMu.Unlock()

	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !fake.closed {
		t.Fatal("Close 未关闭缓存的校验台账实例（debounce 合并器会在 Close 后继续落盘）")
	}
	h.tenantMu.Lock()
	n := len(h.checksumStores)
	h.tenantMu.Unlock()
	if n != 0 {
		t.Fatalf("Close 后缓存仍有 %d 个台账实例（应清空，防重复关闭/迟到写）", n)
	}
}
