// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
)

func TestResolveRemotePath(t *testing.T) {
	cases := []struct {
		name       string
		currentDir string
		input      string
		want       string
		wantErr    bool
	}{
		{"empty_input_root", "", "", "", false},
		{"empty_input_with_currentdir", "sub", "", "sub", false},
		{"relative_root", "", "file.txt", "file.txt", false},
		{"relative_with_currentdir", "sub", "file.txt", "sub/file.txt", false},
		{"absolute_skips_currentdir", "sub", "/abs/file.txt", "abs/file.txt", false},
		{"trailing_slash_cleaned", "sub", "file.txt/", "sub/file.txt", false},
		{"dot_no_op", "sub", ".", "sub", false},
		{"reject_parent_top", "", "..", "", true},
		{"reject_parent_relative", "", "../etc/passwd", "", true},
		{"reject_parent_after_currentdir_resolve", "sub", "../../etc", "", true},
		{"parent_cancels_currentdir_one_level", "sub", "../foo", "", true},
		{"internal_parent_cleaned_to_safe", "", "a/../b", "", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &state.State{CurrentDir: c.currentDir}
			got, err := s.ResolveRemotePath(c.input)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q (currentDir=%q), got nil (returned %q)", c.input, c.currentDir, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", c.input, err)
			}
			if got != c.want {
				t.Fatalf("want %q, got %q", c.want, got)
			}
		})
	}
}

func TestResolveRemotePath_ParentRefMessage(t *testing.T) {
	s := &state.State{CurrentDir: ""}
	_, err := s.ResolveRemotePath("../escape.txt")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "父级引用") {
		t.Fatalf("error message should mention parent ref: %v", err)
	}
}
