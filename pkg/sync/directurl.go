// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
)

// DirectURLProvider 是可选能力接口：FS 支持给单个文件生成 302 直链时实现
// （当前 baidupcs 明文直链：LocateDownload 返回用户自己的下载直链，流量不经服务端）。
// 未实现的 FS（本地 / secretdata 加密卷等）由调用方回落服务端转发，零回归——
// 严格复用 RangeReader 的 optional-interface 范式（见 range.go）。
//
// 语义边界（安全）：直链只在「明文外部卷且未开启私密」时下发——加密卷内容密文
// 必须服务端解密，绝不外出直链（调用方 volumePrivate 判定）。err 表示定位失败，
// 调用方必须回落服务端转发，绝不暴露半截 URL。
type DirectURLProvider interface {
	// DirectURL 返回文件 relPath 的直链（自包含签名、短时有效、支持 Range）。
	// ok=false = 后端不支持直链（调用方回落服务端转发）；err = 定位失败
	// （调用方 fail——闭环转发，绝不暴露半截 URL）。
	DirectURL(ctx context.Context, relPath string) (string, bool, error)
}

// AssertDirectURL 断言 FS 实现 DirectURLProvider；未实现返回 nil（调用方服务端转发）。
func AssertDirectURL(fs FS) DirectURLProvider {
	if d, ok := fs.(DirectURLProvider); ok {
		return d
	}
	return nil
}
