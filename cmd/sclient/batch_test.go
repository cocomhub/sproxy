// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"testing"
)

func TestRunBatchOperation(t *testing.T) {
	tests := []struct {
		name     string
		items    []string
		op       func(item string) error
		wantSucc int
		wantFail int
	}{
		{
			name:     "all succeed",
			items:    []string{"a", "b", "c"},
			op:       func(item string) error { return nil },
			wantSucc: 3,
			wantFail: 0,
		},
		{
			name:     "all fail",
			items:    []string{"a", "b"},
			op:       func(item string) error { return errors.New("fail") },
			wantSucc: 0,
			wantFail: 2,
		},
		{
			name:     "mixed results",
			items:    []string{"good", "bad", "ok"},
			op:       func(item string) error { if item == "bad" { return errors.New("err") }; return nil },
			wantSucc: 2,
			wantFail: 1,
		},
		{
			name:     "empty input",
			items:    []string{},
			op:       func(item string) error { return nil },
			wantSucc: 0,
			wantFail: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := runBatchOperation(tt.items, tt.op)
			if len(results) != len(tt.items) {
				t.Fatalf("expected %d results, got %d", len(tt.items), len(results))
			}
			succ := countBatchSuccess(results)
			fail := len(results) - succ
			if succ != tt.wantSucc {
				t.Errorf("expected %d successes, got %d", tt.wantSucc, succ)
			}
			if fail != tt.wantFail {
				t.Errorf("expected %d failures, got %d", tt.wantFail, fail)
			}
		})
	}
}

func TestPrintBatchResults(t *testing.T) {
	tests := []struct {
		name    string
		results []batchOperationResult
	}{
		{
			name: "all OK",
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
			captureStdout(func() {
				printBatchResults(tt.results)
			})
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
