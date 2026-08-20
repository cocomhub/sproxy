// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudfilename

import (
	"fmt"
)

// Entry 云端下载条目。client 与 server 共用此类型，避免双端各自定义漂移。
type Entry struct {
	URL      string `json:"url"`
	Filename string `json:"filename,omitempty"`
}

// ResolveFilename 返回条目的最终保存文件名。
//   - Filename 非空：仅当 Safe 后不变（不含非法字符）才返回原文，
//     否则返回哨兵错误 ErrEntryUnsafeFilename（只校验不修改）。
//   - Filename 为空：按 URL 自动生成（DefaultFromURL，内部已 Safe）。
func ResolveFilename(e Entry) (string, error) {
	if e.Filename != "" {
		cleaned := Safe(e.Filename)
		if cleaned != e.Filename {
			return "", fmt.Errorf("%w: %q", ErrEntryUnsafeFilename, e.Filename)
		}
		return cleaned, nil
	}
	return DefaultFromURL(e.URL), nil
}
