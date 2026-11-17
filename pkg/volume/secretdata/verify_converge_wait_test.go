// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// delayedVisibleFS 模拟**百度侧的可见性/元数据收敛延迟**：上传后的一小段时间内 VerifyMany 报
// 「不可见」，过了该窗口才可见。
//
// 依据（用户 2026-11-17 实测经验）："连续两次 `BaiduPCS-Go u --policy overwrite` 上传后，
// `ls -l` 检查 md5 大概率就一致了" —— 即第二次上传（或两次之间的时间间隔）会让远端元数据收敛。
// 本用例锁定：**自动重试必须给收敛留出有界等待**，否则立即再校验必然仍不可见 ✗。
type delayedVisibleFS struct {
	verifiableFS
	mu        sync.Mutex
	uploaded  time.Time
	visibleIn time.Duration
}

func (f *delayedVisibleFS) VerifyMany(_ context.Context, items []syncpkg.VerifyItem) []syncpkg.VerifyResult {
	f.mu.Lock()
	up := f.uploaded
	f.mu.Unlock()
	out := make([]syncpkg.VerifyResult, 0, len(items))
	visible := !up.IsZero() && time.Since(up) >= f.visibleIn
	for _, it := range items {
		st := syncpkg.VerifyInvisible
		if visible {
			st = syncpkg.VerifyOK
		}
		out = append(out, syncpkg.VerifyResult{Item: it, State: st})
	}
	return out
}

// TestWriteFile_ConvergeWaitAllowsDelayedVisibility 锁定：远端「延迟可见」时，
// 覆盖写重传**之前**必须有**有界等待**（给收敛留窗口）⇒ 写入最终成功。
//
// 修复前（立即重试，无等待）⇒ 第二次校验仍在窗口内 ⇒ 失败（红）。
func TestWriteFile_ConvergeWaitAllowsDelayedVisibility(t *testing.T) {
	t.Parallel()
	base := syncpkg.NewLocalFS(t.TempDir(), nil)
	inner := &delayedVisibleFS{FS: base, visibleIn: 1500 * time.Millisecond}
	inner.uploaded = time.Now() // 首个写之前即视为"刚上传"，令两次校验都落在窗口内
	sfs, err := NewFS(inner, Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 64<<10) // 64 KiB ⇒ 单块
	start := time.Now()
	if werr := sfs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "converge.bin",
		bytes.NewReader(payload), int64(len(payload)), time.Now().Unix()); werr != nil {
		t.Fatalf("延迟可见场景应最终成功（需重试前等待收敛），got err=%v", werr)
	}
	rc, rerr := sfs.OpenRead(context.Background(), "converge.bin")
	if rerr != nil {
		t.Fatalf("OpenRead: %v", rerr)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, payload) {
		t.Fatalf("读回内容不一致：want %d bytes, got %d", len(payload), len(got))
	}
	// 有界：整体不得被等待拖到分钟级（用户纪律 2026-11-17）。
	if el := time.Since(start); el > 30*time.Second {
		t.Fatalf("延迟可见重试必须有界（应 ≤30s），实测 %v", el)
	}
}
