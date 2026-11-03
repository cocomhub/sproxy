// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// credentials_local_face_test.go 锁「隧道内层凭据入口真的可服务」。
//
// 真实缺陷（表化过程中发现）：localMux 的三条公开凭据端点
// （register/nonce/login）在注册时用的是**尚未初始化的限流器字段**——
// 注册发生在 `h.registerLimiter = ...` 之前，方法值捕获到的是 nil 指针，
// 请求到达时 `rl.mu` 解引用直接 panic（net/http 吞掉 panic 后断连，客户端只看到
// EOF，不是 500）。本测试用真实的 localMux 链打一次，锁住「不 panic 且有响应」。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCredentialLocalMuxEndpointsServe 断言隧道内层凭据端点在**未携带合法请求体**时
// 返回正常业务响应（4xx），而不是 panic/断连。
func TestCredentialLocalMuxEndpointsServe(t *testing.T) {
	t.Parallel()
	h, _ := newSecretsHandlers(t, nil)
	if h.LocalHandler() == nil {
		t.Fatal("localHandler 未装配")
	}
	for _, tc := range []struct {
		path string
		body string
	}{
		{"/api/credentials/register", `{}`},
		{"/api/credentials/nonce", `{}`},
		{"/api/credentials/login", `{}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			// panic 在测试里不会被 net/http recover ⇒ 用例直接失败（这就是我们要的信号）。
			h.LocalHandler().ServeHTTP(rec, req)
			if rec.Code == 0 {
				t.Fatalf("%s 无响应（可能 panic 断连）", tc.path)
			}
			if rec.Code >= 500 {
				t.Fatalf("%s status = %d, want 4xx（缺参数属客户端错误）: %s", tc.path, rec.Code, rec.Body.String())
			}
		})
	}
}
