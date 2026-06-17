// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package size

import "testing"

func TestSizeConstants(t *testing.T) {
	if KiB != 1024 {
		t.Errorf("KiB = %d, want 1024", KiB)
	}
	if MiB != 1024*1024 {
		t.Errorf("MiB = %d, want %d", MiB, 1024*1024)
	}
	if GiB != 1024*1024*1024 {
		t.Errorf("GiB = %d, want %d", GiB, 1024*1024*1024)
	}

	if MiB != KiB*1024 {
		t.Errorf("MiB (%d) != KiB*1024 (%d)", MiB, KiB*1024)
	}
	if GiB != MiB*1024 {
		t.Errorf("GiB (%d) != MiB*1024 (%d)", GiB, MiB*1024)
	}
}

func TestHardLimitRelations(t *testing.T) {
	if DefaultChunkBodyLimit > UploadBodyLimit {
		t.Errorf("DefaultChunkBodyLimit (%d) > UploadBodyLimit (%d)", DefaultChunkBodyLimit, UploadBodyLimit)
	}
	if MaxChunkHashBuf != DefaultChunkBodyLimit {
		t.Errorf("MaxChunkHashBuf (%d) != DefaultChunkBodyLimit (%d)", MaxChunkHashBuf, DefaultChunkBodyLimit)
	}
	if CompleteBodyLimit > DefaultChunkBodyLimit {
		t.Errorf("CompleteBodyLimit (%d) > DefaultChunkBodyLimit (%d)", CompleteBodyLimit, DefaultChunkBodyLimit)
	}
}

func TestDefaultValueRelations(t *testing.T) {
	if DefaultChunkSize >= DefaultMaxChunkSize {
		t.Errorf("DefaultChunkSize (%d) >= DefaultMaxChunkSize (%d)", DefaultChunkSize, DefaultMaxChunkSize)
	}
	if AutoChunkThreshold <= DefaultChunkSize {
		t.Errorf("AutoChunkThreshold (%d) <= DefaultChunkSize (%d)", AutoChunkThreshold, DefaultChunkSize)
	}
	// AutoChunkThreshold (100 MiB) > DefaultMaxChunkSize (64 MiB) by design:
	// auto-chunk is triggered at 100 MiB, at which point the chunk size
	// scales up adaptively from 4 MiB onward.
	if MultipartBufSize <= 0 || MultipartBufSize > DefaultChunkSize {
		t.Errorf("MultipartBufSize (%d) out of reasonable range", MultipartBufSize)
	}
}

func TestDefaultVsHardLimit(t *testing.T) {
	if DefaultChunkSize > DefaultChunkBodyLimit {
		t.Errorf("DefaultChunkSize (%d) > DefaultChunkBodyLimit (%d)", DefaultChunkSize, DefaultChunkBodyLimit)
	}
	if DefaultMaxChunkSize > DefaultChunkBodyLimit {
		t.Errorf("DefaultMaxChunkSize (%d) > DefaultChunkBodyLimit (%d)", DefaultMaxChunkSize, DefaultChunkBodyLimit)
	}
}

func TestDefaultChunkBodyLimitPrecise(t *testing.T) {
	want := int64(64 * 1024 * 1024)
	if DefaultChunkBodyLimit != want {
		t.Errorf("DefaultChunkBodyLimit = %d, want %d", DefaultChunkBodyLimit, want)
	}
}

func TestUploadBodyLimitPrecise(t *testing.T) {
	want := int64(1024 * 1024 * 1024)
	if UploadBodyLimit != want {
		t.Errorf("UploadBodyLimit = %d, want %d", UploadBodyLimit, want)
	}
}
