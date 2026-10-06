// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/integrity"
)

func TestPikpakDownloaderIntegrityMode(t *testing.T) {
	t.Parallel()
	d, err := NewPikpakDownloader(DownloaderConfig{
		Cli:        &Cli{},
		API:        &API{},
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		TempSuffix: ".download",
	})
	if err != nil {
		t.Fatalf("NewPikpakDownloader error: %v", err)
	}
	// 编译期已由 var _ downloader.IntegrityProvider 断言，运行时再核一次声明值。
	ip, ok := any(d).(downloader.IntegrityProvider)
	if !ok {
		t.Fatal("PikpakDownloader 应实现 downloader.IntegrityProvider")
	}
	if got := ip.IntegrityMode(); got != downloader.ModeLocalOnly {
		t.Fatalf("PikpakDownloader 应 local_only，got %v", got)
	}
}

// TestFinalizeDownloadResultIntegrity 验证 finalizeDownload 产出的 Result 声明 local_only 完整性
// （无权威 hash → ② 态）。
func TestFinalizeDownloadResultIntegrity(t *testing.T) {
	t.Parallel()
	lease := &RestoreLease{}
	d := &PikpakDownloader{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r, err := d.finalizeDownload(t.Context(), 1024, "checksum", "", false, nil, nil, "", lease)
	if err != nil {
		t.Fatalf("finalizeDownload error: %v", err)
	}
	if r.Integrity != downloader.ModeLocalOnly {
		t.Fatalf("Result.Integrity 应 local_only，got %v", r.Integrity)
	}
}

// TestFinalizeDownload_GCIDAuthorityHit：候选分块复算命中官方 hash → ModeAuthority + AuthorityHash。
func TestFinalizeDownload_GCIDAuthorityHit(t *testing.T) {
	t.Parallel()
	// 256KB 整数倍文件，官方 hash 设为 256KB 复算值 → 权威命中。
	block := int64(262144)
	data := make([]byte, block*2)
	for i := range data {
		data[i] = byte(i*3 + 1)
	}
	path := t.TempDir() + "/video.mp4"
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	gcid, ok, err := integrity.RecomputeGCIDFile(path, integrity.GCIDCandidates)
	if err != nil || !ok {
		t.Fatalf("RecomputeGCIDFile: ok=%v err=%v", ok, err)
	}
	d := &PikpakDownloader{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r, err := d.finalizeDownload(t.Context(), int64(len(data)), "sha256", "", false, nil, &FileMeta{Hash: gcid}, path, &RestoreLease{})
	if err != nil {
		t.Fatalf("finalizeDownload error: %v", err)
	}
	if r.Integrity != downloader.ModeAuthority {
		t.Fatalf("命中官方 hash 应为 ModeAuthority，got %v", r.Integrity)
	}
	if r.AuthorityHash != gcid {
		t.Fatalf("AuthorityHash = %q, want %q", r.AuthorityHash, gcid)
	}
}

// TestFinalizeDownload_GCIDMissFallback：复算值不命中官方 hash → ModeLocalOnly（② 态），
// 不为不存在权威误报。
func TestFinalizeDownload_GCIDMissFallback(t *testing.T) {
	t.Parallel()
	block := int64(262144)
	data := make([]byte, block*2)
	for i := range data {
		data[i] = byte(i*3 + 1)
	}
	path := t.TempDir() + "/video.mp4"
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	d := &PikpakDownloader{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	// 官方 hash 用与文件内容无关的值（肯定不命中）。
	r, err := d.finalizeCheck(t.Context(), int64(len(data)), "sha256", &FileMeta{Hash: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}, path, &RestoreLease{})
	if err != nil {
		t.Fatalf("finalizeDownload error: %v", err)
	}
	if r.Integrity != downloader.ModeLocalOnly {
		t.Fatalf("未命中官方 hash 应 local_only，got %v", r.Integrity)
	}
}

// finalizeCheck 包装 finalizeDownload（GCID 变体），避免测试签名连写。
func (d *PikpakDownloader) finalizeCheck(ctx context.Context, size int64, checksum string, target *FileMeta, path string, lease *RestoreLease) (*Result, error) {
	return d.finalizeDownload(ctx, size, checksum, "", false, nil, target, path, lease)
}
