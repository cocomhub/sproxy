// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

// IdlePaddingInterval 是空闲填充的默认发送周期（roadmap §5.3 P1 被动伪装层）。
const IdlePaddingInterval = 10 * time.Second

// MuxIdlePaddingOptions 把 cfg.IdlePadding 转成 mux Option 列表：
// 开启时返回 WithIdlePadding(10s)（与 30s 心跳独立共存）；关闭返回空（零回归）。
// 装配层（xfer/remote listener）统一调用，避免各创建点重复判断。
func MuxIdlePaddingOptions(cfg *Config) []mux.Option {
	if cfg == nil || !cfg.IdlePadding {
		return nil
	}
	return []mux.Option{mux.WithIdlePadding(IdlePaddingInterval)}
}
