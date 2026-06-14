// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
)

func TestPrintFileList(t *testing.T) {
	tests := []struct {
		name     string
		files    []client.FileInfo
		contains []string // substrings that must appear in output
		not      []string // substrings that must NOT appear
	}{
		{
			name: "regular files",
			files: []client.FileInfo{
				{Name: "report.pdf", Size: 1024, Checksum: "abc123def456"},
				{Name: "notes.txt", Size: 512, Checksum: "xyz789"},
			},
			contains: []string{"report.pdf", "notes.txt", "1024 B", "512 B"},
			not:      []string{"[DIR]"},
		},
		{
			name: "directory entry",
			files: []client.FileInfo{
				{Name: "mydir", IsDir: true, Size: 0, Checksum: ""},
			},
			contains: []string{"[DIR]", "mydir/"},
		},
		{
			name: "empty checksum",
			files: []client.FileInfo{
				{Name: "no_checksum.bin", Size: 0, Checksum: ""},
			},
			contains: []string{"no_checksum.bin", "-"},
		},
		{
			name: "truncated long checksum",
			files: []client.FileInfo{
				{Name: "long_hash.bin", Size: 999, Checksum: "abcdef1234567890extra"},
			},
			contains: []string{"long_hash.bin", "abcdef1234567890"},
			not:      []string{"extra"},
		},
		{
			name:     "empty file list",
			files:    []client.FileInfo{},
			contains: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf strings.Builder
			printFileList(tt.files, &buf)
			output := buf.String()

			for _, want := range tt.contains {
				if !strings.Contains(output, want) {
					t.Errorf("expected output to contain %q, got:\n%s", want, output)
				}
			}
			for _, not := range tt.not {
				if strings.Contains(output, not) {
					t.Errorf("expected output NOT to contain %q, got:\n%s", not, output)
				}
			}
		})
	}
}
