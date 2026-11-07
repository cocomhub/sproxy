// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// pkg/cli/iostreams_test.go
package cli_test

import (
	"bytes"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cli"
)

func TestSystemIOStreams_NotNil(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	ios := cli.SystemIOStreams()
	if ios.In == nil || ios.Out == nil || ios.ErrOut == nil {
		t.Error("SystemIOStreams should return non-nil streams")
	}
}

func TestIOStreams_WriteToOut(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	var buf bytes.Buffer
	ios := cli.IOStreams{Out: &buf, ErrOut: io.Discard}
	_, err := ios.Out.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if buf.String() != "hello" {
		t.Errorf("got %q, want %q", buf.String(), "hello")
	}
}

func TestIOStreams_WriteToErrOut(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	var buf bytes.Buffer
	ios := cli.IOStreams{ErrOut: &buf, Out: io.Discard}
	ios.WriteErrLine("error: %s", "test")
	if !strings.Contains(buf.String(), "error: test") {
		t.Errorf("expected error output, got %q", buf.String())
	}
}

func TestIOStreams_WriteOutLine(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	var buf bytes.Buffer
	ios := cli.IOStreams{Out: &buf, ErrOut: io.Discard}
	ios.WriteOutLine("result: %d", 42)
	if !strings.Contains(buf.String(), "result: 42") {
		t.Errorf("expected 'result: 42', got %q", buf.String())
	}
}

// TestIOStreams_WriteErrLineSignalsReporter 验证：WriteErrLine 置位上报器（main 据此去重）。
func TestIOStreams_WriteErrLineSignalsReporter(t *testing.T) {
	t.Parallel()
	var reported atomic.Bool
	var buf bytes.Buffer
	ios := cli.IOStreams{Out: io.Discard, ErrOut: &buf}.WithErrReporter(&reported)
	ios.WriteErrLine("错误: %s", "x")
	if !reported.Load() {
		t.Fatal("WriteErrLine 应置位 reporter")
	}
	if got := buf.String(); got != "错误: x\n" {
		t.Fatalf("stderr=%q", got)
	}
}

// TestIOStreams_WriteWarnLineDoesNotSignalReporter 验证：WriteWarnLine 不置位（警告后命令
// 仍可能返回真正的错误，不能被当作「已打印错误」吞掉）。
func TestIOStreams_WriteWarnLineDoesNotSignalReporter(t *testing.T) {
	t.Parallel()
	var reported atomic.Bool
	var buf bytes.Buffer
	ios := cli.IOStreams{Out: io.Discard, ErrOut: &buf}.WithErrReporter(&reported)
	ios.WriteWarnLine("警告: %s", "y")
	if reported.Load() {
		t.Fatal("WriteWarnLine 不应置位 reporter")
	}
	if got := buf.String(); got != "警告: y\n" {
		t.Fatalf("stderr=%q", got)
	}
}
