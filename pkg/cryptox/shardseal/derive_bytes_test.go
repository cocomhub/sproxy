// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"bytes"
	"testing"
)

// TestDeriveBytes_DeterministicAndVolumeBound 钉住 meta 盐/nonce 的**确定性派生**
// （设计 docs/designs/2026-11-16-deterministic-blob-convergent-encryption.md §9 第 7 项、§10）：
//   - 同 (卷密钥, 域标签, 内容) ⇒ **逐字节相同**（同内容不再各自取随机盐 ⇒ 不产生重复 meta）；
//   - 不同域标签 / 不同内容 ⇒ 不同（域分离 + 内容敏感）；
//   - **不同卷密钥 ⇒ 必不相等**（跨卷不可关联，硬验收 §10.2）。
func TestDeriveBytes_DeterministicAndVolumeBound(t *testing.T) {
	t.Parallel()
	secretA := []byte("volume-key-A-0123456789")
	secretB := []byte("volume-key-B-0123456789")
	content := []byte(`{"type":"dir","dir_id":"EXAMPLEID","name":"x"}`)

	a1, err := DeriveBytes(secretA, "dir-meta", content, SaltLen)
	if err != nil {
		t.Fatalf("DeriveBytes: %v", err)
	}
	if len(a1) != SaltLen {
		t.Fatalf("派生长度应为 %d，got %d", SaltLen, len(a1))
	}
	a2, err := DeriveBytes(secretA, "dir-meta", content, SaltLen)
	if err != nil {
		t.Fatalf("DeriveBytes(第二次): %v", err)
	}
	if !bytes.Equal(a1, a2) {
		t.Fatalf("同输入两次派生必须逐字节相同（确定性），got %x vs %x", a1, a2)
	}

	// 域分离：同密钥同内容、不同标签 ⇒ 不同结果。
	otherLabel, err := DeriveBytes(secretA, "metacache-nonce", content, SaltLen)
	if err != nil {
		t.Fatalf("DeriveBytes(其它标签): %v", err)
	}
	if bytes.Equal(a1, otherLabel) {
		t.Fatal("不同域标签必须派生不同结果（域分离）")
	}

	// 内容敏感：同密钥同标签、不同内容 ⇒ 不同结果。
	otherContent, err := DeriveBytes(secretA, "dir-meta", []byte("different-content"), SaltLen)
	if err != nil {
		t.Fatalf("DeriveBytes(其它内容): %v", err)
	}
	if bytes.Equal(a1, otherContent) {
		t.Fatal("不同内容必须派生不同结果")
	}

	// **跨卷不等（硬验收 §10.2）**：同标签同内容、不同卷密钥 ⇒ 必不相等。
	b1, err := DeriveBytes(secretB, "dir-meta", content, SaltLen)
	if err != nil {
		t.Fatalf("DeriveBytes(另一卷): %v", err)
	}
	if bytes.Equal(a1, b1) {
		t.Fatalf("不同卷密钥必须派生不同结果（否则跨卷可关联）: %x", a1)
	}
}

// TestDeriveBytes_ShortLength 支持派生更短字节（如紧凑缓存 nonce）。
func TestDeriveBytes_ShortLength(t *testing.T) {
	t.Parallel()
	secret := []byte("volume-key-A-0123456789")
	n1, err := DeriveBytes(secret, "metacache-nonce", []byte("plain"), 12)
	if err != nil {
		t.Fatalf("DeriveBytes: %v", err)
	}
	if len(n1) != 12 {
		t.Fatalf("派生长度应为 12，got %d", len(n1))
	}
	n2, err := DeriveBytes(secret, "metacache-nonce", []byte("plain"), 12)
	if err != nil {
		t.Fatalf("DeriveBytes(第二次): %v", err)
	}
	if !bytes.Equal(n1, n2) {
		t.Fatal("同输入两次派生必须相同")
	}
}

// TestDeriveBytes_EmptySecretFails 空密钥必须 fail-closed（否则退化为跨卷可关联的常量派生）。
func TestDeriveBytes_EmptySecretFails(t *testing.T) {
	t.Parallel()
	if _, err := DeriveBytes(nil, "dir-meta", []byte("x"), SaltLen); err == nil {
		t.Fatal("空密钥必须报错（fail-closed）")
	}
}

// TestDeriveBytes_InvalidLengthFails 非正长度/超长必须 fail-closed（HMAC-SHA256 上限 32）。
func TestDeriveBytes_InvalidLengthFails(t *testing.T) {
	t.Parallel()
	secret := []byte("volume-key-A-0123456789")
	if _, err := DeriveBytes(secret, "dir-meta", []byte("x"), 0); err == nil {
		t.Fatal("长度 0 必须报错")
	}
	if _, err := DeriveBytes(secret, "dir-meta", []byte("x"), -1); err == nil {
		t.Fatal("负长度必须报错")
	}
	if _, err := DeriveBytes(secret, "dir-meta", []byte("x"), SaltLen+1); err == nil {
		t.Fatal("超过 HMAC-SHA256 输出长度必须报错")
	}
}
