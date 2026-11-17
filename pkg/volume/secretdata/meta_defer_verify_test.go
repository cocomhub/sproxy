// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"io"
	"path"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// hintCapturingFS 记录每次 WriteFile 收到的 UploadHints（可判定 DeferVerify 是否置位）。
type hintCapturingFS struct {
	syncpkg.FS
	mu    sync.Mutex
	hints []bool // 每次 WriteFile 的 DeferVerify
	keys  []string
}

func (f *hintCapturingFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, rel string, r io.Reader, size, mtime int64) error {
	uh, _ := syncpkg.UploadHintsFrom(ctx)
	f.mu.Lock()
	f.hints = append(f.hints, uh != nil && uh.DeferVerify)
	f.keys = append(f.keys, rel)
	f.mu.Unlock()
	return f.FS.WriteFile(ctx, key, rel, r, size, mtime)
}

func (f *hintCapturingFS) snapshot() ([]bool, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.hints...), append([]string(nil), f.keys...)
}

// TestUploadBlob_MetaCarriesDeferVerify 锁定（2026-11-17 实测：`上传 meta 失败: …上传后内容复核
// 不匹配超过 3 次（远端 ETag=）` ⇒ 整个写入 500）：
//
//	**meta blob 的上传也必须带 DeferVerify**（与分块路径同口径）—— 百度新目录树远端可见性延迟
//	5–10 分钟 ⇒ 即时 ETag/Stat 复核必然拿不到 ⇒ 硬重试 3 次即 ErrTransient ⇒ 写入失败 ✗
//
// 修复前：meta 的 hints 无 DeferVerify（红）。
func TestUploadBlob_MetaCarriesDeferVerify(t *testing.T) {
	t.Parallel()
	base := syncpkg.NewLocalFS(t.TempDir(), nil)
	inner := &hintCapturingFS{FS: base}
	sfs, err := NewFS(inner, Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("m"), 16<<10)
	if werr := sfs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "meta-hint.bin",
		bytes.NewReader(payload), int64(len(payload)), time.Now().Unix()); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	hints, keys := inner.snapshot()
	if len(hints) == 0 {
		t.Fatal("夹具未捕获到任何 WriteFile")
	}
	// 至少存在一次 meta blob（KindFileMeta/KindDirMeta）上传，且其 DeferVerify 必须为 true。
	metaSeen := false
	for i, k := range keys {
		if kd := shardseal.ClassifyName(path.Base(k)); kd == shardseal.KindFileMeta || kd == shardseal.KindDirMeta {
			metaSeen = true
			if !hints[i] {
				t.Fatalf("meta blob %q 的上传必须带 DeferVerify（否则新目录树可见性滞后 ⇒ 写入 500）", k)
			}
		}
	}
	if !metaSeen {
		t.Fatalf("未捕获到 meta blob 上传（keys=%v）", keys)
	}
	// 分块路径同样必须带（既有语义，防回归）。
	for i, k := range keys {
		if kd := shardseal.ClassifyName(path.Base(k)); kd != shardseal.KindFileMeta && kd != shardseal.KindDirMeta && !hints[i] {
			t.Fatalf("分块 %q 的 DeferVerify 丢失（回归）", k)
		}
	}
}
