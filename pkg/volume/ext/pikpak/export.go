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

// ParseShareTarget 从分享 URL 提取 spx_target 目标文件名（未配置空串）。
func ParseShareTarget(raw string) string { return parseShareTarget(raw) }

// PickVideoByTarget 按目标文件名挑选分享内视频；target 空回落最大视频。
func PickVideoByTarget(files []FileMeta, target string) *FileMeta {
	return pickVideoByTarget(files, target)
}
