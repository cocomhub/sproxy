// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import "net/http"

// withTunnelKey 是认证驱动模式下的测试中间件：把密钥放入请求 ctx，再交给内部 handler。
// 等价于服务端 authMiddleware 验签成功后调用 SetTunnelKey 再进入 Handler。
func withTunnelKey(key []byte, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(SetTunnelKey(r.Context(), key)))
	})
}
