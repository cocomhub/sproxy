// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// nilReadFS 模拟**契约合规**的卷 FS：不存在的文件按**项目约定**以 `(nil, nil)` 表达
// （`pkg/volume/enclosure/region.go` 的 regionAbsent 注释明写：「不存在的两种契约：
// 返回 os/fs.ErrNotExist，**或**按 sync.FS 契约返回 (nil, nil)」）。
type nilReadFS struct {
	syncpkg.FS
}

func (f *nilReadFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	rc, err := f.FS.OpenRead(ctx, p)
	if err != nil && errors.Is(err, os.ErrNotExist) {
		return nil, nil // 契约二：不存在以 (nil, nil) 表达
	}
	return rc, err
}

// TestLedger_AbsentAsNilNilIsNotCorrupt 契约二（`(nil,nil)` = 不存在）必须按「不存在」处理，
// **不得**判损坏 fail-closed —— 否则任何按该约定实现的卷 FS 上账本恒拒写（该卷不可用 ✗）。
//
// 与 TestLedger_ReadErrorIsUndetermined 的区别（两者都必须成立）：那里是**真错误**
// （非 not-exist）⇒ 不可确定 ⇒ fail-closed ✓；这里是**契约内的不存在** ⇒ 空账本 ✓。
func TestLedger_AbsentAsNilNilIsNotCorrupt(t *testing.T) {
	t.Parallel()
	inner := syncpkg.NewLocalFS(t.TempDir(), nil)
	fsys := &nilReadFS{FS: inner}
	led, err := OpenLedgerFS(fsys, LedgerDirName, "node")
	if err != nil {
		t.Fatal(err)
	}
	if aerr := led.Add(100); aerr != nil {
		t.Fatalf("(nil,nil)=不存在 ⇒ Add 必须成功（未修必红：被误判为损坏 ⇒ fail-closed）: %v", aerr)
	}
	if got, ok := led.Total(); !ok || got != 100 {
		t.Fatalf("Total=%d ok=%v, want 100/true", got, ok)
	}
}
