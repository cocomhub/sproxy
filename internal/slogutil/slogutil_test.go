// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package slogutil

import (
	"log/slog"
	"testing"
)

func TestDefaultLogger_Nil(t *testing.T) {
	logger := Default(nil)
	if logger == nil {
		t.Fatal("Default(nil) returned nil")
	}
}

func TestDefaultLogger_NonNil(t *testing.T) {
	l := slog.Default()
	logger := Default(l)
	if logger != l {
		t.Error("Default 应原样返回非 nil 入参（同一实例）")
	}
}
