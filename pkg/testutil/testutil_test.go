// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestTestKey_Length(t *testing.T) {
	key := TestKey()
	if len(key) != 64 {
		t.Errorf("TestKey() = %q (len %d), want 64 hex chars", key, len(key))
	}
}

func TestTestKey_Hex(t *testing.T) {
	key := TestKey()
	for _, c := range key {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) { //nolint:staticcheck // readability
			t.Errorf("TestKey() contains non-hex char %c", c)
			break
		}
	}
}

func TestTestKey_Repeatable(t *testing.T) {
	k1, k2 := TestKey(), TestKey()
	if k1 != k2 {
		t.Error("TestKey() should be deterministic")
	}
}

func TestDiscardLogger_Type(t *testing.T) {
	logger := DiscardLogger()
	if logger == nil {
		t.Fatal("DiscardLogger() returned nil")
	}
	logger.Info("test")
}

func TestSHA256Hex_Empty(t *testing.T) {
	h := SHA256Hex(nil)
	if len(h) != 64 {
		t.Errorf("SHA256Hex(nil) = %q (len %d), want 64", h, len(h))
	}
}

func TestSHA256Hex_Known(t *testing.T) {
	h := SHA256Hex([]byte("hello"))
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if h != want {
		t.Errorf("SHA256Hex(\"hello\") = %q, want %q", h, want)
	}
}

func TestCaptureStdout(t *testing.T) {
	out := CaptureStdout(func() {
		fmt.Printf("hello, world")
	})
	if !strings.Contains(out, "hello, world") {
		t.Errorf("CaptureStdout = %q, want containing %q", out, "hello, world")
	}
}

func TestCaptureStderr(t *testing.T) {
	out := CaptureStderr(func() {
		fmt.Fprintf(os.Stderr, "error output")
	})
	if !strings.Contains(out, "error output") {
		t.Errorf("CaptureStderr = %q, want containing %q", out, "error output")
	}
}

func TestCaptureStdout_Restores(t *testing.T) {
	// Verify stdout is restorable after capturing
	out := CaptureStdout(func() {
		fmt.Print("inside")
	})

	// After CaptureStdout, stdout should be restored.
	if !strings.Contains(out, "inside") {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestCapture_FunctionsDoNotPanic(t *testing.T) {
	t.Run("stdout", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("CaptureStdout panicked: %v", r)
			}
		}()
		CaptureStdout(func() {})
	})
	t.Run("stderr", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("CaptureStderr panicked: %v", r)
			}
		}()
		CaptureStderr(func() {})
	})
}
