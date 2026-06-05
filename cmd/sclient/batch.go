// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "fmt"

// batchOperationResult holds result of a single batch operation.
type batchOperationResult struct {
	Name    string
	Success bool
	Message string
}

// runBatchOperation runs a function for each item, collecting results.
func runBatchOperation(items []string, op func(item string) error) []batchOperationResult {
	results := make([]batchOperationResult, 0, len(items))
	for _, item := range items {
		result := batchOperationResult{Name: item}
		if err := op(item); err != nil {
			result.Message = err.Error()
		} else {
			result.Success = true
			result.Message = "OK"
		}
		results = append(results, result)
	}
	return results
}

// printBatchResults prints batch operation results to stdout.
func printBatchResults(results []batchOperationResult) {
	for _, r := range results {
		status := "OK"
		if !r.Success {
			status = "FAIL"
		}
		fmt.Printf("[%s] %s: %s\n", status, r.Name, r.Message)
	}
}

// countBatchSuccess counts the number of successful operations.
func countBatchSuccess(results []batchOperationResult) int {
	count := 0
	for _, r := range results {
		if r.Success {
			count++
		}
	}
	return count
}
