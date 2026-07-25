// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// pkg/cli/iostreams_test.go
package cli_test

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cli"
)

func TestSystemIOStreams_NotNil(t *testing.T) {
	ios := cli.SystemIOStreams()
	if ios.In == nil || ios.Out == nil || ios.ErrOut == nil {
		t.Error("SystemIOStreams should return non-nil streams")
	}
}

func TestIOStreams_WriteToOut(t *testing.T) {
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
	var buf bytes.Buffer
	ios := cli.IOStreams{ErrOut: &buf, Out: io.Discard}
	ios.WriteErrLine("error: %s", "test")
	if !strings.Contains(buf.String(), "error: test") {
		t.Errorf("expected error output, got %q", buf.String())
	}
}

func TestIOStreams_WriteOutLine(t *testing.T) {
	var buf bytes.Buffer
	ios := cli.IOStreams{Out: &buf, ErrOut: io.Discard}
	ios.WriteOutLine("result: %d", 42)
	if !strings.Contains(buf.String(), "result: 42") {
		t.Errorf("expected 'result: 42', got %q", buf.String())
	}
}
