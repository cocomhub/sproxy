// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

// Carrier is an abstraction for extracting/injecting tracing information into or
// out of a context. An HTTP Header is a natural implementation.
type Carrier interface {
	// Get returns the value for key, or "" if absent.
	Get(key string) string
	// Set writes value for key.
	Set(key, value string)
}
