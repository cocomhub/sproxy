// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// mount_container_listdir_test.go 钉住「容器级列目录失败必须拒绝挂载」（独立审核 P2，2026-10-18）。
//
// 缺陷：挂载时某容器 ListDir 失败被**静默吞掉**（`if err != nil { return }`，零日志）⇒ 该容器
// **全部文件**不进索引（用户看到"文件消失"）；更危险的是 GC 复用内存索引（gcMarkIndex）⇒ 会把
// 缺失的 meta 当孤儿**清扫**（数据丢失），且 BaseVersion 只基于残余索引 ⇒ 重启后
// resolveSameRelConflict 可能把新上传内容换回旧内容（内容回滚）。
// 修法：与本文件既有原则一致（「挂载失败比静默空视图安全」，见 loadIndex 首层 ListDir 注释）
// ⇒ 容器级失败 fail-closed（拒绝不完整视图挂载）；仅 os.ErrNotExist（并发删除）视为不存在。

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// flakyListDirFS：对指定路径的**第 failAfter+1 次起** ListDir 失败——精确复现审核所指窗口
// 「容器树 walk 成功之后的瞬时故障」（walk 已确认容器存在 ⇒ 只有后续扫描期失败才是本缺陷面；
// 若一开始就失败，更早的 dirContainers walk 就会拒绝挂载）。
type flakyListDirFS struct {
	*syncpkg.LocalFS
	path      string
	failAfter int
	calls     atomic.Int64
}

func (f *flakyListDirFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	if p == f.path && int(f.calls.Add(1)) > f.failAfter {
		return nil, errors.New("injected: transient listdir failure after walk")
	}
	return f.LocalFS.ListDir(ctx, p)
}

func TestNewFS_ContainerListDirFailureFailsMount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inner := syncpkg.NewLocalFS(t.TempDir(), nil)
	opts := Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo}
	fs, err := NewFS(inner, opts)
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if merr := fs.MakeDir(ctx, "a"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	container := fs.dirSegs["a"]
	if container == "" {
		t.Fatal("前置：容器未建立")
	}
	// 重新挂载：容器树 walk 的首次列目录成功，其后的扫描期列目录瞬时失败。
	failing := &flakyListDirFS{LocalFS: inner, path: container, failAfter: 1}
	if _, rerr := NewFS(failing, opts); rerr == nil {
		t.Fatal("容器级列目录失败必须**拒绝挂载**（未修必红：静默跳过 ⇒ 该容器文件全部不可见且零日志；" +
			"GC 复用内存索引 ⇒ 把缺失 meta 当孤儿清扫 = 数据丢失）")
	}
	// 纵深防御：Phase 1（目录 meta 扫描）通过、Phase 3（文件 meta 登记）期才失败，同样必须拒绝。
	// （failAfter=2：walk 1 次 + Phase 1 1 次成功，Phase 3 起失败。）
	late := &flakyListDirFS{LocalFS: inner, path: container, failAfter: 2}
	if _, rerr := NewFS(late, opts); rerr == nil {
		t.Fatal("Phase 3 容器级列目录失败同样必须拒绝挂载（否则索引不完整 ⇒ GC 误清扫/BaseVersion 回退）")
	}
}
