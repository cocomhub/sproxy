// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// testAK / testSK 是认证测试用的合法 AK/SK（SK 为 64 hex 字符 = 32 字节）。
const (
	testAK = "sk-test-access-key"
	testSK = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// testRing 构造含给定 AK/SK 对的鉴权 Ring（每条 AK 一条 plain alive 条目）。
// 无参时回落默认 testAK/testSK 单对（与既有测试语义一致）。
func testRing(pairs ...accesskey.KeyPair) *accesskey.Ring {
	if len(pairs) == 0 {
		pairs = []accesskey.KeyPair{{Key: testAK, Secret: testSK}}
	}
	return accesskey.NewRingFromKeyPairs(pairs)
}

// testRegCred 用 testSK 为指定 nodeID 生成一次注册凭据（proof + ts + nonce）。
// 每次调用使用唯一 nonce（NewRegisterNonce），满足 Authenticate 的 nonce 去重。
func testRegCred(t *testing.T, nodeID string) (proof string, ts int64, nonce string) {
	t.Helper()
	ts = time.Now().UnixMilli()
	nonce = NewRegisterNonce()
	var err error
	proof, err = ComputeRegisterProof(testSK, nodeID, ts, nonce)
	if err != nil {
		t.Fatalf("ComputeRegisterProof: %v", err)
	}
	return proof, ts, nonce
}

// testRegCredAt 用指定 ts 生成注册凭据（供过期/重放测试）。
func testRegCredAt(t *testing.T, nodeID string, ts int64) (proof string, nonce string) {
	t.Helper()
	nonce = NewRegisterNonce()
	var err error
	proof, err = ComputeRegisterProof(testSK, nodeID, ts, nonce)
	if err != nil {
		t.Fatalf("ComputeRegisterProof: %v", err)
	}
	return proof, nonce
}

func TestAuthenticator(t *testing.T) {
	// 正确 AK/SK：proof 匹配 → 通过
	a := NewAuthenticator(testRing())
	proof, ts, nonce := testRegCred(t, "node-a")
	if err := a.Authenticate(testAK, proof, "node-a", ts, nonce); err != nil {
		t.Fatal("expected success for matching AK/proof")
	}

	// 错误 proof → ErrInvalidAccessKeyProof
	_, ts2, nonce2 := testRegCred(t, "node-a")
	if err := a.Authenticate(testAK, "deadbeef", "node-a", ts2, nonce2); !errors.Is(err, ErrInvalidAccessKeyProof) {
		t.Fatalf("expected ErrInvalidAccessKeyProof for wrong proof, got %v", err)
	}

	// 未知 AK → ErrInvalidAccessKey
	proof3, ts3, nonce3 := testRegCred(t, "node-a")
	if err := a.Authenticate("unknown-ak", proof3, "node-a", ts3, nonce3); !errors.Is(err, ErrInvalidAccessKey) {
		t.Fatalf("expected ErrInvalidAccessKey for unknown AK, got %v", err)
	}

	// 空 ring = fail-closed：拒绝所有注册（C2 纵深加固）
	a2 := NewAuthenticator(accesskey.NewRing())
	proof4, ts4, nonce4 := testRegCred(t, "node-a")
	if err := a2.Authenticate(testAK, proof4, "node-a", ts4, nonce4); !errors.Is(err, ErrInvalidAccessKey) {
		t.Fatalf("expected ErrInvalidAccessKey when ring empty (fail-closed), got %v", err)
	}

	// proof 绑定 nodeID：同一 SK 对另一 nodeID 的 proof 不匹配 → 拒绝（防串用/重放）
	proof5, ts5, nonce5 := testRegCred(t, "node-b")
	if err := a.Authenticate(testAK, proof5, "node-a", ts5, nonce5); !errors.Is(err, ErrInvalidAccessKeyProof) {
		t.Fatalf("expected ErrInvalidAccessKeyProof when proof bound to different nodeID, got %v", err)
	}

	// 多对 AK/SK：命中第二对 → 通过
	multi := NewAuthenticator(testRing(
		accesskey.KeyPair{Key: "first-ak", Secret: testSK},
		accesskey.KeyPair{Key: testAK, Secret: testSK},
	))
	proof6, ts6, nonce6 := testRegCred(t, "node-a")
	if err := multi.Authenticate(testAK, proof6, "node-a", ts6, nonce6); err != nil {
		t.Fatalf("expected success when matching second AK, got %v", err)
	}
}

func TestAuthenticator_StaleTS(t *testing.T) {
	a := NewAuthenticator(testRing())
	// ts 超出新鲜度窗口（+2×窗口）→ ErrStaleRegisterProof
	old := time.Now().Add(-2 * registerProofMaxAge).UnixMilli()
	proof, nonce := testRegCredAt(t, "node-a", old)
	if err := a.Authenticate(testAK, proof, "node-a", old, nonce); !errors.Is(err, ErrStaleRegisterProof) {
		t.Fatalf("expected ErrStaleRegisterProof for stale ts, got %v", err)
	}
	// 未来 ts（+2×窗口）→ ErrStaleRegisterProof
	future := time.Now().Add(2 * registerProofMaxAge).UnixMilli()
	proof2, nonce2 := testRegCredAt(t, "node-a", future)
	if err := a.Authenticate(testAK, proof2, "node-a", future, nonce2); !errors.Is(err, ErrStaleRegisterProof) {
		t.Fatalf("expected ErrStaleRegisterProof for future ts, got %v", err)
	}
}

func TestAuthenticator_NonceReplay(t *testing.T) {
	a := NewAuthenticator(testRing())
	// 同一 nonce 二次使用 → ErrReplayRegisterNonce（防重放）
	ts := time.Now().UnixMilli()
	nonce := NewRegisterNonce()
	proof, err := ComputeRegisterProof(testSK, "node-a", ts, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Authenticate(testAK, proof, "node-a", ts, nonce); err != nil {
		t.Fatalf("first use should pass: %v", err)
	}
	if err := a.Authenticate(testAK, proof, "node-a", ts, nonce); !errors.Is(err, ErrReplayRegisterNonce) {
		t.Fatalf("expected ErrReplayRegisterNonce for replayed nonce, got %v", err)
	}
}

// TestAuthenticator_SharedRing（任务 4 新增）：共享同一 ring 实例的两个 Authenticator
// 行为一致（无需 SetAccessKeys）——rotate 后在共享 ring 上动态生效。
func TestAuthenticator_SharedRing(t *testing.T) {
	ring := testRing()
	a1 := NewAuthenticator(ring)
	a2 := NewAuthenticator(ring)

	proof, ts, nonce := testRegCred(t, "node-a")
	if err := a1.Authenticate(testAK, proof, "node-a", ts, nonce); err != nil {
		t.Fatalf("a1 should pass for ring AK: %v", err)
	}
	// ring 追加新 SK → a2 立即可见（共享实例动态生效）。
	skHex, err := hex.DecodeString(testSK)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ring.AddKey(testAK, skHex, accesskey.WithMeta(accesskey.Meta{Type: "rotated"}))
	if err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	proof2, ts2, nonce2 := testRegCred(t, "node-a")
	if err := a2.Authenticate(testAK, proof2, "node-a", ts2, nonce2); err != nil {
		t.Fatalf("a2 should see rotated entry via shared ring: %v", err)
	}
	// ring 全删 AK → 两个实例都 fail-closed。
	if err := ring.DeleteAK(testAK); err != nil {
		t.Fatalf("DeleteAK: %v", err)
	}
	proof3, ts3, nonce3 := testRegCred(t, "node-a")
	if err := a1.Authenticate(testAK, proof3, "node-a", ts3, nonce3); !errors.Is(err, ErrInvalidAccessKey) {
		t.Fatalf("expected ErrInvalidAccessKey after DeleteAK, got %v", err)
	}
}

func TestComputeRegisterProof_Validation(t *testing.T) {
	// 非 hex SK → 错误
	if _, err := ComputeRegisterProof("not-hex", "node-a", 0, "x"); err == nil {
		t.Fatal("expected error for non-hex SK")
	}
	// 长度不足 32 字节（<64 hex 字符）→ 错误
	if _, err := ComputeRegisterProof("abcd", "node-a", 0, "x"); err == nil {
		t.Fatal("expected error for short SK")
	}
	// 合法 SK → 64 hex 字符输出，且参数确定时结果确定
	p1, err := ComputeRegisterProof(testSK, "node-a", 12345, "nonce-x")
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := ComputeRegisterProof(testSK, "node-a", 12345, "nonce-x")
	if len(p1) != 64 || p1 != p2 {
		t.Fatalf("expected deterministic 64-hex proof, got %q vs %q", p1, p2)
	}
	// 相同 nodeID/ts 但 nonce 不同 → proof 不同（nonce 参与签名）
	p3, _ := ComputeRegisterProof(testSK, "node-a", 12345, "nonce-y")
	if p1 == p3 {
		t.Fatal("expected different proof for different nonce")
	}
}
