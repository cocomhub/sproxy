// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"errors"
	"testing"
)

// testAK / testSK 是认证测试用的合法 AK/SK（SK 为 64 hex 字符 = 32 字节）。
const (
	testAK = "sk-test-access-key"
	testSK = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// testRegisterProof 用 testSK 计算指定 nodeID 的注册证明。
func testRegisterProof(t *testing.T, nodeID string) string {
	t.Helper()
	proof, err := ComputeRegisterProof(testSK, nodeID)
	if err != nil {
		t.Fatalf("ComputeRegisterProof: %v", err)
	}
	return proof
}

func TestAuthenticator(t *testing.T) {
	// 正确 AK/SK：proof 匹配 → 通过
	a := NewAuthenticator([]AccessKey{{Key: testAK, Secret: testSK}})
	if err := a.Authenticate(testAK, testRegisterProof(t, "node-a"), "node-a"); err != nil {
		t.Fatal("expected success for matching AK/proof")
	}

	// 错误 proof → ErrInvalidAccessKeyProof
	if err := a.Authenticate(testAK, "deadbeef", "node-a"); !errors.Is(err, ErrInvalidAccessKeyProof) {
		t.Fatalf("expected ErrInvalidAccessKeyProof for wrong proof, got %v", err)
	}

	// 未知 AK → ErrInvalidAccessKey
	if err := a.Authenticate("unknown-ak", testRegisterProof(t, "node-a"), "node-a"); !errors.Is(err, ErrInvalidAccessKey) {
		t.Fatalf("expected ErrInvalidAccessKey for unknown AK, got %v", err)
	}

	// 空 accessKeys = fail-closed：拒绝所有注册（C2 纵深加固）
	a2 := NewAuthenticator(nil)
	if err := a2.Authenticate(testAK, testRegisterProof(t, "node-a"), "node-a"); !errors.Is(err, ErrInvalidAccessKey) {
		t.Fatalf("expected ErrInvalidAccessKey when accessKeys empty (fail-closed), got %v", err)
	}

	// proof 绑定 nodeID：同一 SK 对另一 nodeID 的 proof 不匹配 → 拒绝（防串用/重放）
	if err := a.Authenticate(testAK, testRegisterProof(t, "node-b"), "node-a"); !errors.Is(err, ErrInvalidAccessKeyProof) {
		t.Fatalf("expected ErrInvalidAccessKeyProof when proof bound to different nodeID, got %v", err)
	}

	// 多对 AK/SK：命中第二对 → 通过
	multi := NewAuthenticator([]AccessKey{
		{Key: "first-ak", Secret: testSK},
		{Key: testAK, Secret: testSK},
	})
	if err := multi.Authenticate(testAK, testRegisterProof(t, "node-a"), "node-a"); err != nil {
		t.Fatalf("expected success when matching second AK, got %v", err)
	}
}

func TestComputeRegisterProof_Validation(t *testing.T) {
	// 非 hex SK → 错误
	if _, err := ComputeRegisterProof("not-hex", "node-a"); err == nil {
		t.Fatal("expected error for non-hex SK")
	}
	// 长度不足 32 字节（<64 hex 字符）→ 错误
	if _, err := ComputeRegisterProof("abcd", "node-a"); err == nil {
		t.Fatal("expected error for short SK")
	}
	// 合法 SK → 64 hex 字符输出，且两次一致（确定性）
	p1, err := ComputeRegisterProof(testSK, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := ComputeRegisterProof(testSK, "node-a")
	if len(p1) != 64 || p1 != p2 {
		t.Fatalf("expected deterministic 64-hex proof, got %q vs %q", p1, p2)
	}
}
