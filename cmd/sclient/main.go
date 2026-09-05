// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
)

var (
	Version = "dev"
	BuildAt = "unknown"
)

func main() {
	if err := Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "sclient:", err)
		os.Exit(1)
	}
}
