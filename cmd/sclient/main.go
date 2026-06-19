// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "os"

var (
	Version = "dev"
	BuildAt = "unknown"
)

func main() {
	if err := Execute(); err != nil {
		os.Exit(1)
	}
}
