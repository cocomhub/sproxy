// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// testKey 返回一个 64 字符 hex 密钥（32 字节）给测试使用。
// 安全警告：这是一个弱密钥（全 a），仅用于测试，不可用于生产环境。
// 生产环境应使用 sclient genkey 或 crypto/rand 生成密钥。
func testKey() string {
	return strings.Repeat("a", 64)
}

// testLogger 返回一个丢弃所有日志的 slog.Logger 供测试使用。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// defaultNoAuthRegOpts 是「无凭据 + AllowInsecureLoopback 兜底放行」的注册选项。
// 凭据 store 化后，ring 为空（无任何凭据）→ authMiddleware 走 allow_insecure_loopback
// 兜底（默认生产为 401）；测试大量未认证集成用例（直接 GET/POST 断言业务行为）依赖
// 等价旧 `--allow-no-auth` 的全放行语义，故默认注入【空 Ring + AllowInsecureLoopback=true】
// （回环来源任意方法放行）。显式认证契约的测试自行注入带凭据的 Ring。
func defaultNoAuthRegOpts() RegisterRoutesOpts {
	ring := accesskey.NewRing() // 空 Ring：无任何凭据
	return RegisterRoutesOpts{
		CredentialRing:        ring,
		CredentialStore:       nil, // 注入空 Ring 时不持久化，纯内存场景
		AllowInsecureLoopback: true,
	}
}

// withHeader 为 *http.Request 添加 header，返回自身便于链式调用。
// 该函数当前无调用者（由 server_auth_test.go 旧代码引用），但保留作为测试公共辅助模式参考。
//
//lint:file-ignore U1000 保留以备未来 auth 测试使用
func withHeader(r *http.Request, key, value string) *http.Request {
	r.Header.Set(key, value)
	return r
}

// testCredPair 是测试凭据的 AK/SK 对（SK 为 64-hex 表示）。
// 可选 skeyID 字段：缺省用 testEntryID(ak) 确定性生成（与 signRequest 精确匹配）。
type testCredPair struct {
	ak, sk string
	// skeyID 显式指定条目 ID（与 ring 注入条目一致；缺省 testEntryID(ak)）。
	skeyID string
}

// ringForTestCreds 构造含给定 AK/SK 对（每对一条 plain alive 条目）的 Ring。
// 无参数时回落默认 testAccessKey/testAccessSecret 单对（与 withTestCreds 一致）。
// 条目 ID 用 testEntryID(ak) 确定性生成（与 signRequest/signTunnelRequest 精确匹配，
// 保证 v2 必传 skey-id 的测试签名能被服务端 (ak, skeyID) 定位）。
func ringForTestCreds(creds ...testCredPair) *accesskey.Ring {
	if len(creds) == 0 {
		creds = []testCredPair{{ak: testAccessKey, sk: testAccessSecret}}
	}
	ring := accesskey.NewRing()
	for _, c := range creds {
		sk, err := hex.DecodeString(c.sk)
		if err != nil || len(sk) != 32 {
			continue
		}
		id := c.skeyID
		if id == "" {
			id = testEntryID(c.ak)
		}
		_ = ring.UpsertAK(c.ak, "test")
		_, _ = ring.AddKey(c.ak, sk, accesskey.WithID(id), accesskey.WithMeta(accesskey.Meta{Type: "initial"}))
	}
	return ring
}

// emptyTestRing 返回空 Ring（无任何凭据），供无认证兜底测试使用。
func emptyTestRing() *accesskey.Ring {
	return accesskey.NewRing()
}

// testCredOpts 返回注入给定凭据 Ring 的注册选项（配合 newTestServer 等的
// RegisterRoutesOpts 基座；AllowInsecureLoopback 保持 false——凭据非空时认证接管）。
func testCredOpts(creds ...testCredPair) RegisterRoutesOpts {
	return RegisterRoutesOpts{
		CredentialRing:  ringForTestCreds(creds...),
		CredentialStore: nil, // 测试 ring 不持久化
	}
}

// withTestCreds 把凭据注入 opts（可变参数：缺省用 testAccessKey/testAccessSecret）。
func withTestCreds(opts *RegisterRoutesOpts, creds ...testCredPair) {
	if len(creds) == 0 {
		creds = []testCredPair{{ak: testAccessKey, sk: testAccessSecret}}
	}
	opts.CredentialRing = ringForTestCreds(creds...)
	opts.CredentialStore = nil
}

// withTunnelKeyCtx 把派生密钥放入请求 ctx（模拟 authMiddleware 对 /tunnel 验签后
// SetTunnelKey 的行为），供认证驱动隧道的 handler 测试使用。
func withTunnelKeyCtx(key []byte, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(tunnel.SetTunnelKey(r.Context(), key)))
	})
}
