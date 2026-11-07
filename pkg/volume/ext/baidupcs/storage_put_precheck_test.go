// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"log/slog"
	"path"
	"sync"
	"testing"
	"time"
)

// hangMetaAdapter 复现实测（P7）：库 `Meta` **阻塞且不响应 ctx**（无 body 超时）；
// 列举（`ls`，含 md5）稳定可用 —— 用来验证上传路径**完全不依赖会挂起的 Meta**。
type hangMetaAdapter struct {
	mu       sync.Mutex
	present  bool // 目标是否已在"列举"中可见（上传后置 true）
	etag     string
	uploads  int
	metaCall int
}

func (a *hangMetaAdapter) Upload(_ context.Context, _, _ string, _ bool) error {
	a.mu.Lock()
	a.uploads++
	a.present = true // 上传后目标即可被列举看到
	a.mu.Unlock()
	return nil
}
func (a *hangMetaAdapter) Download(context.Context, string, string) error { return ErrUnsupported }
func (a *hangMetaAdapter) Move(context.Context, string, string) error     { return ErrUnsupported }
func (a *hangMetaAdapter) Copy(context.Context, string, string) error     { return ErrUnsupported }

// Meta：**恒挂起**（只在 ctx 结束时返回）——证明上传路径不得依赖它。
func (a *hangMetaAdapter) Meta(ctx context.Context, _ string) (*ObjectMeta, error) {
	a.mu.Lock()
	a.metaCall++
	a.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

// List：精确列举（含 md5 → ETag），实测稳定可用。
func (a *hangMetaAdapter) List(_ context.Context, dir string) ([]ObjectMeta, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.present {
		return nil, nil
	}
	return []ObjectMeta{{Key: path.Join(dir, "f.bin"), ETag: a.etag, Size: 1024}}, nil
}

var (
	_ Adapter          = (*hangMetaAdapter)(nil)
	_ metadataProvider = (*hangMetaAdapter)(nil)
)

func newHangStorage(t *testing.T, ad *hangMetaAdapter) *Storage {
	t.Helper()
	st, err := NewStorage(StorageConfig{Root: "/r", TempDir: t.TempDir(), Adapter: ad,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	return st
}

// TestStorage_Put_DoesNotUseHangingMeta 钉住 **S5'**（2026-11-08 实测 P7/P9）：
// 上传路径**不得依赖库 `Meta`**（实测其阻塞且不响应 ctx）——预检与上传后复核都走**精确列举**
// （`ls` 语义，含 md5）；**md5 校验强度不变**（仍断言 ETag == 本地整文件 md5）。
// **变异**：把"上传后复核"改回 `s.Stat` ⇒ Meta 恒挂起 ⇒ 本用例超时红。
func TestStorage_Put_DoesNotUseHangingMeta(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte("precheck"), 128)
	sum := md5.Sum(payload) //nolint:gosec
	ad := &hangMetaAdapter{etag: hex.EncodeToString(sum[:])}
	st := newHangStorage(t, ad)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	meta, perr := st.Put(ctx, "dir/f.bin", bytes.NewReader(payload))
	elapsed := time.Since(start)
	if perr != nil {
		t.Fatalf("Put 不得因 Meta 挂起而失败（应走列举），got %v（%.2fs）", perr, elapsed.Seconds())
	}
	if meta == nil || meta.ETag != hex.EncodeToString(sum[:]) {
		t.Fatalf("上传后必须拿到**权威 md5**（数据校验不得放宽），got %+v", meta)
	}
	ad.mu.Lock()
	uploads, metaCalls := ad.uploads, ad.metaCall
	ad.mu.Unlock()
	if uploads != 1 {
		t.Fatalf("应真的上传一次，got %d", uploads)
	}
	if metaCalls != 0 {
		t.Fatalf("上传路径**不得调用**会挂起的 Meta，got %d 次", metaCalls)
	}
}

// TestStorage_Put_ListingHitSkipsUpload 钉住**列举即命中 md5 ⇒ 秒传跳过上传**（预检语义保留）。
func TestStorage_Put_ListingHitSkipsUpload(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte("hit"), 64)
	sum := md5.Sum(payload) //nolint:gosec
	ad := &hangMetaAdapter{present: true, etag: hex.EncodeToString(sum[:])}
	st := newHangStorage(t, ad)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	meta, perr := st.Put(ctx, "dir/f.bin", bytes.NewReader(payload))
	if perr != nil {
		t.Fatalf("Put: %v", perr)
	}
	if meta == nil || meta.ETag != hex.EncodeToString(sum[:]) {
		t.Fatalf("应返回列举得到的权威 md5 meta，got %+v", meta)
	}
	ad.mu.Lock()
	uploads, metaCalls := ad.uploads, ad.metaCall
	ad.mu.Unlock()
	if uploads != 0 {
		t.Fatalf("列举已确认 md5 一致 ⇒ 不得再上传，got uploads=%d", uploads)
	}
	if metaCalls != 0 {
		t.Fatalf("不得调用 Meta，got %d", metaCalls)
	}
}

// TestStorage_Put_ContextTimeoutStillReported 钉住：调用方 ctx 已取消时**如实报错**（不掩盖）。
func TestStorage_Put_ContextTimeoutStillReported(t *testing.T) {
	t.Parallel()
	ad := &hangMetaAdapter{}
	st := newHangStorage(t, ad)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, perr := st.Put(ctx, "d/f.bin", bytes.NewReader(bytes.Repeat([]byte("x"), 64))); perr == nil {
		t.Fatal("调用方 ctx 已取消时必须如实报错")
	}
}
