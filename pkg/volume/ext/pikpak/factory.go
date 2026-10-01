// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

// factory.go 提供便捷导出（给 cmd 装配层用）：ParseShareID / PickLargestVideo / Cli.Path。

// Path 返回 CLI 可执行路径。
func (c *Cli) Path() string { return c.bin }
