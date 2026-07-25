// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package state_test

import (
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
)

func TestState_ResolveRemotePath_Absolute(t *testing.T) {
	s := &state.State{CurrentDir: "subdir"}
	got, err := s.ResolveRemotePath("/abs/path")
	if err != nil {
		t.Fatalf("ResolveRemotePath failed: %v", err)
	}
	if got != "abs/path" {
		t.Errorf("got %q, want %q", got, "abs/path")
	}
}

func TestState_ResolveRemotePath_Relative(t *testing.T) {
	s := &state.State{CurrentDir: "base"}
	got, err := s.ResolveRemotePath("file.txt")
	if err != nil {
		t.Fatalf("ResolveRemotePath failed: %v", err)
	}
	if got != "base/file.txt" {
		t.Errorf("got %q, want %q", got, "base/file.txt")
	}
}

func TestState_ResolveRemotePath_EmptyCurrentDir(t *testing.T) {
	s := &state.State{CurrentDir: ""}
	got, err := s.ResolveRemotePath("file.txt")
	if err != nil {
		t.Fatalf("ResolveRemotePath failed: %v", err)
	}
	if got != "file.txt" {
		t.Errorf("got %q, want %q", got, "file.txt")
	}
}

func TestState_ResolveRemotePath_ParentRef(t *testing.T) {
	s := &state.State{CurrentDir: "base"}
	_, err := s.ResolveRemotePath("../file.txt")
	if err == nil {
		t.Error("expected error for parent reference")
	}
}

func TestState_ResolveRemotePathOrErr(t *testing.T) {
	s := &state.State{CurrentDir: ""}
	got, err := s.ResolveRemotePathOrErr("test.txt")
	if err != nil {
		t.Fatalf("ResolveRemotePathOrErr failed: %v", err)
	}
	if got != "test.txt" {
		t.Errorf("got %q, want %q", got, "test.txt")
	}
}

func TestState_ResolveRemotePathOrErr_Invalid(t *testing.T) {
	s := &state.State{CurrentDir: "base"}
	_, err := s.ResolveRemotePathOrErr("../file.txt")
	if err == nil {
		t.Error("expected error for invalid path")
	}
}

func TestState_ResolveRemotePath_CurrentDirWithParentRef(t *testing.T) {
	s := &state.State{CurrentDir: "a/../b"}
	_, err := s.ResolveRemotePath("file.txt")
	if err == nil {
		t.Error("expected error when CurrentDir contains parent ref")
	}
}

func TestState_ResolveRemotePath_PathEndsWithParentRef(t *testing.T) {
	s := &state.State{CurrentDir: "base"}
	_, err := s.ResolveRemotePath("a/..")
	if err == nil {
		t.Error("expected error for path ending with ..")
	}
}

func TestState_ResolveRemotePath_PathMidParentRef(t *testing.T) {
	s := &state.State{CurrentDir: "base"}
	_, err := s.ResolveRemotePath("a/../b/file.txt")
	if err == nil {
		t.Error("expected error for path with mid ..")
	}
}

func TestState_ResolveRemotePath_NotParentRef(t *testing.T) {
	s := &state.State{CurrentDir: ""}
	got, err := s.ResolveRemotePath("...file")
	if err != nil {
		t.Fatalf("should not error for '...file': %v", err)
	}
	if got != "...file" {
		t.Errorf("got %q, want %q", got, "...file")
	}
}
