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
	time.Sleep(50 * time.Millisecond)

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
	time.Sleep(50 * time.Millisecond)

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
	time.Sleep(50 * time.Millisecond)

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
	time.Sleep(50 * time.Millisecond)

	tunA := NewTunnel(muxA, key)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/mixed", strings.NewReader("mixed-test"))
	resp, err := tunA.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("keyed dialer + nil listener 数据面应失败（C-1：sessionKey 不一致）")
	}
	cancel()
	<-srvErr
}
