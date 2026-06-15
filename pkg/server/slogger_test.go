// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"testing"
)

func TestDefaultLogger_Nil(t *testing.T) {
	logger := defaultLogger(nil)
	if logger == nil {
		t.Fatal("defaultLogger(nil) returned nil")
	}
}

func TestDefaultLogger_NonNil(t *testing.T) {
	l := slog.Default()
	logger := defaultLogger(l)
	if logger != l {
		t.Error("defaultLogger should return the same instance")
	}
}
