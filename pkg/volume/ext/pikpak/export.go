// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

// export.go 便捷导出（cmd 装配层用）。

// ParseShareID 从 mypikpak.com/s/<share_id> URL 提取 share id。
func ParseShareID(raw string) (string, error) { return parseShareID(raw) }

// PickLargestVideo 从分享文件里挑最大的视频（全长主视频）。
func PickLargestVideo(files []FileMeta) *FileMeta { return pickLargestVideo(files) }

// Path 返回 CLI 可执行路径。
func (c *Cli) Path() string { return c.bin }
