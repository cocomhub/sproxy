// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

func main() {
	if err := rootCmd.Execute(); err != nil {
		osExitErr(err)
	}
}
