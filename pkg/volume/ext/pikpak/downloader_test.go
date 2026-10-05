// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"io"
	"log/slog"
	"testing"

	"github.com/cocomhub/sproxy/pkg/downloader"
)

func TestPikpakDownloaderIntegrityMode(t *testing.T) {
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

// TestFinalizeDownloadResultIntegrity 验证 finalizeDownload 产出的 Result 声明 local_only 完整性。
func TestFinalizeDownloadResultIntegrity(t *testing.T) {
	lease := &RestoreLease{}
	d := &PikpakDownloader{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r, err := d.finalizeDownload(t.Context(), 1024, "checksum", "", false, nil, lease)
	if err != nil {
		t.Fatalf("finalizeDownload error: %v", err)
	}
	if r.Integrity != downloader.ModeLocalOnly {
		t.Fatalf("Result.Integrity 应 local_only，got %v", r.Integrity)
	}
}
