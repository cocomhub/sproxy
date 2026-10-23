// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"net/url"
	"path"
	"strings"
)

// ShareTargetParam 是分享 URL 内「目标文件名」的查询参数名。
//
// 语义：云端下载需要「分享内指定文件」时，调用方在该分享 URL 上加
// `?spx_target=<文件名>`；下载器据此选择文件（分卷 A/B/C 各提交一次）。
// 为什么用 URL 参数而非新增 API 字段（设计决策 2026-10-23）：
//   - 免改 downloader/cloud 接口与任务结构（改动面最小、无接口扩散）；
//   - 同分享不同目标 ⇒ URL 不同 ⇒ 云端任务去重（findByURL）不会误吸收；
//   - 参数只被 PikPak 下载器消费，其它下载器忽略（不影响通用 http 路径）。
const ShareTargetParam = "spx_target"

// parseShareTarget 从分享 URL 提取目标文件名（未配置返回空串）。
func parseShareTarget(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Query().Get(ShareTargetParam))
}

// stripShareTarget 移除 URL 上的 spx_target 参数（解析分享 ID / 下载直链前归一）。
func stripShareTarget(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Query().Get(ShareTargetParam) == "" {
		return raw
	}
	q := u.Query()
	q.Del(ShareTargetParam)
	u.RawQuery = q.Encode()
	return u.String()
}

// matchTargetFile 判断文件名是否匹配目标（忽略大小写；允许省略扩展名）。
// 空 target 恒不匹配（调用方据此回落「挑最大」）。
func matchTargetFile(name, target string) bool {
	if target == "" {
		return false
	}
	n := strings.ToLower(strings.TrimSpace(name))
	t := strings.ToLower(strings.TrimSpace(target))
	if n == t {
		return true
	}
	if strings.TrimSuffix(n, path.Ext(n)) == t {
		return true
	}
	return false
}

// pickVideoByTarget 按目标文件名挑选视频文件；target 为空时回落「挑最大视频」
// （既有行为零回归）。target 非空但无匹配 ⇒ 返回 nil（不静默改选其它文件）。
func pickVideoByTarget(files []FileMeta, target string) *FileMeta {
	if target == "" {
		return pickLargestVideo(files)
	}
	for i := range files {
		f := &files[i]
		if f.Kind != driveKindFileMeta {
			continue
		}
		if matchTargetFile(f.Name, target) {
			return f
		}
	}
	return nil
}

// driveKindFileMeta 是分享/网盘列表里文件条目的 Kind 值（FileMeta 口径）。
const driveKindFileMeta = "drive#file"

// pickShareFileByTarget 按目标文件名挑选匿名分享文件（[]ShareFile 口径）；
// target 为空时回落既有的「最大视频 → 最大文件」行为。
func pickShareFileByTarget(files []ShareFile, target string) *ShareFile {
	if target == "" {
		return pickLargestShareFile(files)
	}
	for i := range files {
		if matchTargetFile(files[i].Name, target) {
			return &files[i]
		}
	}
	return nil
}
