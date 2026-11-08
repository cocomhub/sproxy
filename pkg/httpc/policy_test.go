// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package httpc

// policy_test.go —— 锁定「可替换命名策略」与「按用途收敛 + 共享池默认」的语义。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/pipe"
)

// TestPolicyFor_Builtins 预置策略与"未注册 ⇒ standard"的兜底。
func TestPolicyFor_Builtins(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     PolicyName
		attempts int
		base     time.Duration
		cap      time.Duration
	}{
		{PolicyNone, 1, 0, 0},
		{PolicyFast, 3, 200 * time.Millisecond, 2 * time.Second},
		{PolicyStandard, 4, time.Second, 8 * time.Second},
		{PolicySlow, 5, 5 * time.Second, 60 * time.Second},
	}
	for _, tc := range cases {
		got := PolicyFor(tc.name)
		if got.Attempts != tc.attempts || got.Base != tc.base || got.Cap != tc.cap {
			t.Fatalf("PolicyFor(%s)=%+v want attempts=%d base=%v cap=%v", tc.name, got, tc.attempts, tc.base, tc.cap)
		}
	}
	// 未注册名与空名 ⇒ standard（"忘了声明"也有安全默认）。
	for _, n := range []PolicyName{"", "not-registered"} {
		if got := PolicyFor(n); got.Attempts != PolicyFor(PolicyStandard).Attempts {
			t.Fatalf("PolicyFor(%q) 应回落 standard，got %+v", n, got)
		}
	}
}

// TestRegisterPolicy_OverridableAndResettable 策略可替换（装配层定制口径）且可复位。
func TestRegisterPolicy_OverridableAndResettable(t *testing.T) {
	// 不用 t.Parallel：本用例修改进程级注册表（与其它并行用例隔离靠 Restore）。
	t.Cleanup(ResetPoliciesForTest)
	RegisterPolicy(PolicyFast, pipe.Policy{Attempts: 9, Base: time.Millisecond})
	if got := PolicyFor(PolicyFast); got.Attempts != 9 {
		t.Fatalf("覆盖后应生效，got %+v", got)
	}
	// Attempts<=0 视为 1（不重试）——避免"注册出无限重试"。
	RegisterPolicy("zero", pipe.Policy{Attempts: 0})
	if got := PolicyFor("zero"); got.Attempts != 1 {
		t.Fatalf("Attempts<=0 应归一为 1，got %+v", got)
	}
	ResetPoliciesForTest()
	if got := PolicyFor(PolicyFast); got.Attempts != 3 {
		t.Fatalf("Reset 后应恢复预置，got %+v", got)
	}
}

// TestPolicyNone_NoRetryButIdempotentOverride PolicyNone 不重试；显式 Idempotent+策略仍生效。
func TestPolicyNone_NoRetryButIdempotentOverride(t *testing.T) {
	t.Parallel()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := New(Options{Retry: PolicyFor(PolicyNone), Env: pipe.Env{}})
	resp, err := c.Get(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("500 应报错")
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	if n.Load() != 1 {
		t.Fatalf("PolicyNone 不得重试，got %d 次", n.Load())
	}
}

// TestShared_ReusesPerPurpose 同用途复用同一实例；不同用途相互独立。
func TestShared_ReusesPerPurpose(t *testing.T) {
	// 修改进程级注册表 ⇒ 串行（R18 依赖函数体内标记）。
	// sproxy:serial: 断言进程级共享注册表（同 purpose 必须同实例）——共享状态不可并行
	t.Cleanup(ResetSharedForTest)
	a := Shared(PurposeLLM, Options{})
	b := Shared(PurposeLLM, Options{Retry: PolicyFor(PolicySlow)}) // 第二次 opt 被忽略
	if a != b {
		t.Fatal("同 purpose 必须复用同一实例（第二次 opt 生效会导致行为不一致）")
	}
	c := Shared(PurposeEmbed, Options{})
	if c == a {
		t.Fatal("不同 purpose 必须相互独立")
	}
	if SharedCount() != 2 {
		t.Fatalf("应注册 2 个共享客户端，got %d", SharedCount())
	}
	ResetSharedForTest()
	if SharedCount() != 0 {
		t.Fatal("Reset 后应为空")
	}
}

// TestDefaultTransport_SharedVsIsolated 默认**共享池**（同进程同实例）；显式 Isolated 才独立。
func TestDefaultTransport_SharedVsIsolated(t *testing.T) {
	t.Parallel()
	c1 := NewHTTPClient(Options{})
	c2 := NewHTTPClient(Options{})
	if c1.Transport != c2.Transport {
		t.Fatal("默认必须使用进程级共享池（连接/TLS 复用）")
	}
	i1 := NewHTTPClient(Options{Isolated: true})
	i2 := NewHTTPClient(Options{Isolated: true})
	if i1.Transport == i2.Transport {
		t.Fatal("Isolated 必须每次独立池")
	}
	if i1.Transport == c1.Transport {
		t.Fatal("Isolated 不得复用共享池")
	}
}

// TestClient_CloseIdleConnections 独立池可安全关闭；nil 安全（共享池场景禁止调用，由文档约束）。
func TestClient_CloseIdleConnections(t *testing.T) {
	t.Parallel()
	c := New(Options{Isolated: true})
	c.CloseIdleConnections() // 必须不 panic
	var nilClient *Client
	nilClient.CloseIdleConnections() // nil 安全
}
