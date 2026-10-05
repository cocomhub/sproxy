// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package downloader_test

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/downloader"
)

func TestIntegrityModeConstants(t *testing.T) {
	if downloader.ModeLocalOnly == downloader.ModeAuthority {
		t.Fatal("模式须互斥")
	}
	if downloader.ModeUnknown != 0 {
		t.Fatalf("ModeUnknown 应为 0（零值），got %d", downloader.ModeUnknown)
	}
}

func TestResultIntegrityField(t *testing.T) {
	r := downloader.Result{Integrity: downloader.ModeLocalOnly}
	if r.Integrity != downloader.ModeLocalOnly {
		t.Fatalf("Result 应含 Integrity 字段，got %v", r.Integrity)
	}
	r.AuthorityHash = "gcid-hex"
	if r.AuthorityHash == "" {
		t.Fatal("Result 应含 AuthorityHash 字段")
	}
}

func TestHTTPDownloaderMode(t *testing.T) {
	d := downloader.NewHTTPDownloader()
	// HTTPDownloader 是具体类型（非接口），先经 any 再做接口断言。
	if ip, ok := any(d).(downloader.IntegrityProvider); ok {
		if got := ip.IntegrityMode(); got != downloader.ModeSelfVerified {
			t.Fatalf("HTTP 应 self_verified，got %v", got)
		}
	} else {
		t.Fatal("HTTP 应实现 IntegrityProvider")
	}
}
