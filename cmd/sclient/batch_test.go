// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"testing"
)

func TestPrintBatchResults(t *testing.T) {
	tests := []struct {
		name    string
		results []batchOperationResult
	}{
		{
			name: "all_ok",
			results: []batchOperationResult{
				{Name: "file1.txt", Success: true, Message: "OK"},
				{Name: "file2.txt", Success: true, Message: "OK"},
			},
		},
		{
			name: "mixed",
			results: []batchOperationResult{
				{Name: "good.txt", Success: true, Message: "OK"},
				{Name: "bad.txt", Success: false, Message: "not found"},
			},
		},
		{
			name:    "empty",
			results: []batchOperationResult{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			printBatchResults(tt.results, io.Discard)
		})
	}
}

func TestCountBatchSuccess(t *testing.T) {
	tests := []struct {
		name  string
		input []batchOperationResult
		want  int
	}{
		{"all success", []batchOperationResult{{Success: true}, {Success: true}, {Success: true}}, 3},
		{"none success", []batchOperationResult{{Success: false}, {Success: false}}, 0},
		{"mixed", []batchOperationResult{{Success: true}, {Success: false}, {Success: true}}, 2},
		{"empty", []batchOperationResult{}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countBatchSuccess(tt.input); got != tt.want {
				t.Errorf("countBatchSuccess() = %d, want %d", got, tt.want)
			}
		})
	}
}
