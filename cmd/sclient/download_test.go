// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
)

func TestDownloadCmd_Use(t *testing.T) {
	t.Parallel()
	cmd := NewCmdDownload(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard}, &state.State{})
	if !strings.HasPrefix(cmd.Use, "download") {
		t.Errorf("expected Use to start with 'download', got %q", cmd.Use)
	}
}

func TestDownloadCmd_Args(t *testing.T) {
	t.Parallel()
	cmd := NewCmdDownload(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard}, &state.State{})
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Error("expected error for 0 args")
	}
	if err := cmd.Args(cmd, []string{"file.txt"}); err != nil {
		t.Errorf("expected no error for 1 arg, got: %v", err)
	}
	if err := cmd.Args(cmd, []string{"file.txt", "out.txt"}); err != nil {
		t.Errorf("expected no error for 2 args, got: %v", err)
	}
}

func TestDownloadCmd_Flags(t *testing.T) {
	t.Parallel()
	cmd := NewCmdDownload(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard}, &state.State{})
	for _, name := range []string{"chunked", "chunk-size", "concurrency", "resume"} {
		if f := cmd.Flags().Lookup(name); f == nil {
			t.Errorf("missing flag: %s", name)
		}
	}
}
