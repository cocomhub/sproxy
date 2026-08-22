// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

// sdpWriteFlags Windows 无 O_NOFOLLOW；O_EXCL 已防覆盖，返回 0。
func sdpWriteFlags() int { return 0 }
