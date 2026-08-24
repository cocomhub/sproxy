// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import "context"

type tunnelKeyCtx struct{}

// SetTunnelKey 将隧道密钥写入 context，供下游请求处理链路读取。
func SetTunnelKey(ctx context.Context, key []byte) context.Context {
	return context.WithValue(ctx, tunnelKeyCtx{}, key)
}

// GetTunnelKey 从 context 读取隧道密钥；未设置时返回 nil。
func GetTunnelKey(ctx context.Context) []byte {
	if v, _ := ctx.Value(tunnelKeyCtx{}).([]byte); v != nil {
		return v
	}
	return nil
}
