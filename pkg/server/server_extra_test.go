// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strings"
	"testing"
)

func TestUploadStore_SessionDir(t *testing.T) {
	us := NewUploadStore(t.TempDir(), 0, nil)
	dir := us.SessionDir("test-upload-id")
	if dir == "" {
		t.Fatal("expected non-empty session dir")
	}
	if !strings.Contains(dir, "test-upload-id") {
		t.Errorf("expected session dir to contain upload ID, got: %s", dir)
	}
}
