// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

func TestECDHHandshake_Roundtrip(t *testing.T) {
	t.Parallel()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)

	errCh := make(chan error, 1)
	var keyA, keyB []byte
	ctx := context.Background()
	go func() {
		var err error
		keyA, err = performHandshake(ctx, muxA, true)
		errCh <- err
	}()
	keyB, err := performHandshake(ctx, muxB, false)
	if err != nil {
		t.Fatalf("listener handshake failed: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("dialer handshake failed: %v", err)
	}

	if len(keyA) != 32 || len(keyB) != 32 {
		t.Fatalf("expected 32-byte session keys, got %d/%d", len(keyA), len(keyB))
	}
	if !bytes.Equal(keyA, keyB) {
		t.Fatal("session keys do not match")
	}
}

func TestNewTunnelWithECDH(t *testing.T) {
	t.Parallel()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	key, _ := ParseKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	tunA := NewTunnel(muxA, key)
	tunB := NewTunnel(muxB, key)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	srvErr := make(chan error, 1)
	go func() {
		srvErr <- tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			w.Write(body)
		}))
	}()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/pfs", strings.NewReader("pfs-test"))
	resp, err := tunA.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "pfs-test" {
		t.Fatalf("expected %q, got %q", "pfs-test", string(body))
	}
	cancel()
	<-srvErr
}

func TestECDHHandshake_WrongKeyFails(t *testing.T) {
	// C-1 验收（核心）：两端静态密钥不同（keyA != keyB）时，静态密钥必须参与会话密钥
	// 派生——两端派生出不同的 sessionKey，首个加密帧 AES-GCM 解密失败 → Do 报错。
	// （修复前：匿名 ECDH 握手 + 静态密钥不参与派生 → 错误 key 也能正常往返，即 C-1 缺陷。）
	t.Parallel()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	keyB, _ := ParseKey("fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")
	keyA, _ := ParseKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// 服务端 goroutine 先运行 NewTunnel（监听端阻塞在 Accept）
	srvErr := make(chan error, 1)
	go func() {
		tunB := NewTunnel(muxB, keyB)
		srvErr <- tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			w.Write(body)
		}))
	}()
	tunA := NewTunnel(muxA, keyA)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/mismatch", strings.NewReader("mismatch-test"))
	resp, err := tunA.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("错误静态密钥的客户端应被拒绝（首个加密帧解密失败，C-1 验收）")
	}
	cancel()
	<-srvErr
}

func TestECDHHandshake_NilKeyFallback(t *testing.T) {
	// 验证 key 为 nil 时不做 ECDH 握手，隧道正常工作
	t.Parallel()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	tunA := NewTunnel(muxA, nil)
	tunB := NewTunnel(muxB, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	srvErr := make(chan error, 1)
	go func() {
		srvErr <- tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			w.Write(body)
		}))
	}()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/nil", strings.NewReader("nil-test"))
	resp, err := tunA.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "nil-test" {
		t.Fatalf("expected %q, got %q", "nil-test", string(body))
	}
	cancel()
	<-srvErr
}

// handshakeKeys 在内存管道上执行一次 ECDH 握手（含身份阶段），返回双方派生出的
// sessionKey。dialerKey/listenerKey 分别为两端传入的静态密钥（nil=纯 ECDH）。
func handshakeKeys(t *testing.T, dialerKey, listenerKey []byte) ([]byte, []byte) {
	t.Helper()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx := context.Background()
	errCh := make(chan error, 1)
	var keyA []byte
	go func() {
		var err error
		keyA, _, err = performHandshakeWithIdentity(ctx, muxA, true, nil, nil, dialerKey)
		errCh <- err
	}()
	keyB, _, err := performHandshakeWithIdentity(ctx, muxB, false, nil, nil, listenerKey)
	if err != nil {
		t.Fatalf("listener handshake: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("dialer handshake: %v", err)
	}
	return keyA, keyB
}

func TestECDHHandshake_SameKeyDerivesSameKey(t *testing.T) {
	// 两端同静态密钥 → 派生相同 sessionKey（合法对端互通）。
	t.Parallel()
	key, _ := ParseKey(testHexKey)
	keyA, keyB := handshakeKeys(t, key, key)
	if !bytes.Equal(keyA, keyB) {
		t.Fatal("同静态密钥两端应派生相同 sessionKey")
	}
}

func TestECDHHandshake_MixedKeyNilDerivationDiffers(t *testing.T) {
	// C-1：一端静态密钥参与派生、另一端 nil（纯 ECDH）时，两端 sessionKey 必须不同
	// ——否则混 key 对端与纯 ECDH 对端仍能互通，匿名 ECDH 漏洞未闭合。
	key, _ := ParseKey(testHexKey)
	for _, tc := range []struct {
		name        string
		dialerKey   []byte
		listenerKey []byte
	}{
		{"dialer keyed / listener pure", key, nil},
		{"dialer pure / listener keyed", nil, key},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			keyA, keyB := handshakeKeys(t, tc.dialerKey, tc.listenerKey)
			if bytes.Equal(keyA, keyB) {
				t.Fatal("混 key 端与纯 ECDH 端 sessionKey 应不同（C-1）")
			}
		})
	}
}

func TestDeriveSessionKey_Binding(t *testing.T) {
	// deriveSessionKey 单元级断言：两输入（ECDH 共享密钥 + 静态密钥）都参与派生，
	// nil 保持旧纯 ECDH 派生字节级一致（向后兼容）。
	key, _ := ParseKey(testHexKey)
	key2, _ := ParseKey("fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")
	sharedSecret := bytes.Repeat([]byte{0xAB}, 32)
	otherSecret := bytes.Repeat([]byte{0xCD}, 32)

	// 相同输入 → 确定性相同输出。
	k1, err := deriveSessionKey(sharedSecret, key)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := deriveSessionKey(sharedSecret, key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("相同输入应派生相同 sessionKey")
	}

	// 不同静态密钥 → 不同 sessionKey（C-1 核心：任何 key 变化都改变 sessionKey）。
	k3, err := deriveSessionKey(sharedSecret, key2)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(k1, k3) {
		t.Fatal("不同静态密钥应派生不同 sessionKey（C-1）")
	}

	// nil（纯 ECDH）与混 key → 不同。
	kNil, err := deriveSessionKey(sharedSecret, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(k1, kNil) {
		t.Fatal("staticKey=nil（纯 ECDH）与混 key 派生应不同")
	}

	// nil 派生与旧纯 ECDH 派生字节级一致（向后兼容）。
	oldKey, err := hkdf.Key(sha256.New, sharedSecret, []byte(ecdhSalt), ecdhInfo, sessionKeyLen)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(kNil, oldKey) {
		t.Fatal("nil staticKey 派生必须与旧纯 ECDH 派生字节级一致（向后兼容）")
	}

	// 不同 ECDH 共享密钥 → 不同 sessionKey（共享密钥也必须参与）。
	k4, err := deriveSessionKey(otherSecret, key)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(k1, k4) {
		t.Fatal("不同 ECDH 共享密钥应派生不同 sessionKey")
	}

	// 输出恒为 32 字节（AES-256）。
	for _, k := range [][]byte{k1, k3, kNil, k4} {
		if len(k) != 32 {
			t.Fatalf("sessionKey 长度应为 32，实际 %d", len(k))
		}
	}
}

// TestECDHHandshake_KeyedDialerNilListenerFails 验证 keyed dialer + nil listener
// 数据面必须失败。
//
// 根因（flake #599 windows Test Sub-Modules 5m 超时）：dialer 握手阶段（阶段 2 身份
// 交换）在 listener 侧读到 EOF（无身份扩展）后返回；但 **dialer 侧对端（nil listener）
// 的 Serve 仍在 accept 循环中**，本测试的 Do 在 sendRequestBody 后阻塞于
// readResponseMeta 的 io.ReadFull（对端永远不回响应），且 req ctx 取消不中断 mux
// 流的 Read——阻塞测试线程直到 5m 超时（defer muxA.Close 不执行，因为卡在 Do 内）。
// 修法：cancel() 后**显式 muxA.Close() + muxB.Close()**（Close 关闭流 done，Read 即
// 返回）→ Do 返回错误，断言才可达。
func TestECDHHandshake_KeyedDialerNilListenerFails(t *testing.T) {
	// C-1 同步发布协议变更：keyed dialer + nil listener（无密钥模式）→ 数据面必须失败
	// （fail-closed，两端 sessionKey 不一致）。同版本 sclient/sproxy 才可互通。
	t.Parallel()
	key, _ := ParseKey(testHexKey)
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	srvErr := make(chan error, 1)
	go func() {
		tunB := NewTunnel(muxB, nil)
		srvErr <- tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			w.Write(body)
		}))
	}()
	tunA := NewTunnel(muxA, key)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/mixed", strings.NewReader("mixed-test"))
	resp, err := tunA.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("keyed dialer + nil listener 数据面应失败（C-1：sessionKey 不一致）")
	}
	cancel()
	// 解除 Do 内部对 readResponseMeta 的流读阻塞（req ctx 取消不中断 mux 流 Read）：
	// 显式关闭两端 mux，流 done 关闭后阻塞读立即返回，确保本测试不会卡 5m 超时。
	_ = muxA.Close()
	_ = muxB.Close()
	<-srvErr
}

func TestECDHHandshake_KeyedListenerNilDialerFails(t *testing.T) {
	// 反向（审查 Minor #2）：keyed listener + nil dialer（无密钥模式）。keyed listener
	// 握手失败即 fail-closed 拒绝整个隧道（Important #1 修复后 Serve 返回 error）——
	// 不回退静态密钥，避免固定密钥加密丧失前向保密。同时断言服务端 handler 未执行
	// （客户端请求被拒、零访问）。
	t.Parallel()
	key, _ := ParseKey(testHexKey)
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	handled := make(chan struct{}, 1)
	srvErr := make(chan error, 1)
	go func() {
		// keyed listener：握手失败（nil dialer 不参与 ECDH，读流即失败）→ Serve fail-closed。
		tunB := NewTunnel(muxB, key)
		srvErr <- tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handled <- struct{}{}
			body, _ := io.ReadAll(r.Body)
			w.Write(body)
		}))
	}()
	// nil dialer：无密钥，不握手，直接发请求流（被 keyed listener 当握手流消费 → 握手失败）。
	tunA := NewTunnel(muxA, nil)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/mixed", strings.NewReader("mixed-test"))
	if resp, err := tunA.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("nil dialer + keyed listener 请求应失败（keyed listener fail-closed）")
	}

	// 服务端 handler 不得执行（客户端零访问）。
	select {
	case <-handled:
		t.Fatal("keyed listener 拒绝握手后服务端 handler 不应执行")
	case <-time.After(500 * time.Millisecond):
	}

	cancel()
	if err := <-srvErr; err == nil {
		t.Fatal("keyed listener Serve 应返回 error（握手失败 fail-closed），而非 nil")
	}
}

// TestSetSaltPrefix_Custom 验证自定义前缀后：
// 1. 全部 5 个盐带前缀（无 "sproxy" 串）；
// 2. 相同自定义前缀的派生会话密钥一致（两端协商）；
// 3. 不同前缀派生不同（防串用）。
// sproxy:serial: 共享全局盐变量（SetSaltPrefix 全局副作用），不能并行
func TestSetSaltPrefix_Custom(t *testing.T) {
	// 保存默认值，测试后复位（t.Cleanup 防污染其它并行测试）。
	origSalt, origInfo := ecdhSalt, ecdhInfo
	origStaticSalt, origInfoStatic := ecdhStaticSalt, ecdhInfoStatic
	origSigDomain := identitySigDomain
	t.Cleanup(func() {
		SetSaltPrefix("")
		ecdhSalt, ecdhInfo = origSalt, origInfo
		ecdhStaticSalt, ecdhInfoStatic = origStaticSalt, origInfoStatic
		identitySigDomain = origSigDomain
	})

	SetSaltPrefix("nf-2026")
	for _, v := range []string{ecdhSalt, ecdhInfo, ecdhStaticSalt, ecdhInfoStatic, identitySigDomain} {
		if strings.Contains(v, "sproxy") {
			t.Errorf("自定义前缀后仍含 sproxy: %q", v)
		}
		if !strings.HasPrefix(v, "nf-2026-") {
			t.Errorf("自定义前缀缺失: %q", v)
		}
	}

	// 相同自定义前缀 → 派生一致（两端协商）。
	shared := make([]byte, 32)
	for i := range shared {
		shared[i] = byte(i)
	}
	k1, err := deriveSessionKey(shared, nil)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := deriveSessionKey(shared, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("同前缀派生不一致（握手会失败）")
	}

	// 不同前缀 → 派生不同（防串用）。
	SetSaltPrefix("other-2026")
	k3, err := deriveSessionKey(shared, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(k1, k3) {
		t.Fatal("不同前缀派生相同（应不同）")
	}
}

// TestSetSaltPrefix_DefaultReset 验证 SetSaltPrefix("") 复位默认前缀。
// sproxy:serial: 共享全局盐变量（SetSaltPrefix 全局副作用），不能并行
func TestSetSaltPrefix_DefaultReset(t *testing.T) {
	SetSaltPrefix("tmp-x")
	if ecdhSalt != "tmp-x-ecdh-salt-v1" {
		t.Fatalf("自定义未生效: %q", ecdhSalt)
	}
	SetSaltPrefix("")
	if ecdhSalt != "sproxy-ecdh-salt-v1" {
		t.Fatalf("复位失败: %q", ecdhSalt)
	}
	if identitySigDomain != "sproxy-identity-v1" {
		t.Fatalf("identitySigDomain 复位失败: %q", identitySigDomain)
	}
}

// TestDeriveProtocolSalts_Deterministic 验证：
// 1. 同 key → 同盐（两端协商一致）；
// 2. 不同 key → 不同盐（防串用）；
// 3. 派生盐无语义（不含 sproxy / 非可打印语义串）；
// 4. 默认 sproxy 前缀不在派生盐中（去协议指纹）。
func TestDeriveProtocolSalts_Deterministic(t *testing.T) {
	// sproxy:serial: 共享全局盐变量（SetProtocolSalts 全局副作用），不能并行
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	s1 := DeriveProtocolSalts(key)
	s2 := DeriveProtocolSalts(key)
	if s1 != s2 {
		t.Fatalf("同 key 派生不一致（握手会失败）: %+v vs %+v", s1, s2)
	}
	// 无语义（全 hex，不含 sproxy 或字母语义串）。
	for name, v := range map[string]string{
		"ECDH": s1.ECDH, "Info": s1.Info, "Static": s1.Static,
		"InfoStatic": s1.InfoStatic, "IdentitySig": s1.IdentitySig,
	} {
		if strings.Contains(v, "sproxy") {
			t.Errorf("%s 含 sproxy: %q", name, v)
		}
		if len(v) != 32 { // 16B hex = 32 字符
			t.Errorf("%s 长度异常: %q (len=%d)", name, v, len(v))
		}
	}
	// 不同 key → 不同盐。
	key2 := append([]byte(nil), key...)
	key2[0] ^= 0xff
	s3 := DeriveProtocolSalts(key2)
	if s3 == s1 {
		t.Fatal("不同 key 派生相同（应不同）")
	}
	// 短 key → 默认 sproxy 盐（零回归）。
	s4 := DeriveProtocolSalts([]byte("short"))
	if s4.ECDH != "sproxy-ecdh-salt-v1" {
		t.Fatalf("短 key 应回落默认盐: %q", s4.ECDH)
	}
	// 黄金向量：派生常数/算法变化会改变此值（防"假绿"）。
	gold := DeriveProtocolSalts(key)
	if gold.ECDH != "a8b6d240a46136cb35b77c99c4f9e62d" {
		t.Fatalf("黄金向量漂移（派生算法变化?）: %q", gold.ECDH)
	}
}

// TestSetProtocolSalts_Apply 验证 SetProtocolSalts 应用/复位。
func TestSetProtocolSalts_Apply(t *testing.T) {
	// sproxy:serial: 共享全局盐变量（SetProtocolSalts 全局副作用），不能并行
	orig := defaultSalts()
	t.Cleanup(func() { SetProtocolSalts(orig) })
	key := make([]byte, 32)
	copy(key, "0123456789abcdef0123456789abcdef")
	der := DeriveProtocolSalts(key)
	SetProtocolSalts(der)
	if ecdhSalt != der.ECDH {
		t.Fatalf("ecdhSalt 未应用: %q vs %q", ecdhSalt, der.ECDH)
	}
	if identitySigDomain != der.IdentitySig {
		t.Fatalf("identitySigDomain 未应用: %q vs %q", identitySigDomain, der.IdentitySig)
	}
	// 复位：空盐集 → 默认。
	SetProtocolSalts(ProtocolSalts{})
	if ecdhSalt != "sproxy-ecdh-salt-v1" {
		t.Fatalf("复位失败: %q", ecdhSalt)
	}
}
