// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !race

package testutil

// RaceEnabled 表示当前二进制由 `-race` 构建（此处为否）。语义见 race_on.go。
const RaceEnabled = false
