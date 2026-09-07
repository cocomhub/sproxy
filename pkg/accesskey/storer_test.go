// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

import (
	"bytes"
	"testing"
)

// TestPlainStorer_Roundtrip 验证 PlainStorer 明文默认实现的往返保证：
// Encrypt 与 Decrypt 均应当逐字节原样返回输入（未开启加密时装配的兜底实现）。
func TestPlainStorer_Roundtrip(t *testing.T) {
	var s PlainStorer
	payload := []byte("credentials-snapshot-plaintext-bytes")
	enc, err := s.Encrypt(payload)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !bytes.Equal(enc, payload) {
		t.Fatalf("Encrypt 应原样返回: got %q, want %q", enc, payload)
	}
	dec, err := s.Decrypt(enc)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(dec, payload) {
		t.Fatalf("Decrypt 应还原明文: got %q, want %q", dec, payload)
	}
	// 空输入往返也保持原样（nil/空语义）。
	empty, err := s.Encrypt(nil)
	if err != nil {
		t.Fatalf("Encrypt(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("Encrypt(nil) 应返回空: got %d bytes", len(empty))
	}
	empty2, err := s.Decrypt(nil)
	if err != nil {
		t.Fatalf("Decrypt(nil): %v", err)
	}
	if len(empty2) != 0 {
		t.Fatalf("Decrypt(nil) 应返回空: got %d bytes", len(empty2))
	}
}

// sentinelStorer 是测试专用探针类型，用于验证注册表按目标类型命中
// （区别于 PlainStorer——两者都满足 SecureStorer，但类型不同不应串味）。
type sentinelStorer struct{}

func (sentinelStorer) Encrypt(p []byte) ([]byte, error) { return p, nil }
func (sentinelStorer) Decrypt(c []byte) ([]byte, error) { return c, nil }

// TestStorerRegistry_RegisterGetUnregister 验证 storer 注册表的契约：
//   - RegisterStorer 后 GetStorer[T] 按目标类型命中且类型正确；
//   - 重名 RegisterStorer 返回错误（非 nil）；
//   - UnregisterStorer 后 GetStorer 不命中（ok==false）；
//   - 类型不匹配（存 A 取 B）不命中且返回零值。
func TestStorerRegistry_RegisterGetUnregister(t *testing.T) {
	const name = "test-storer-4c1"

	// 首次注册成功。
	if err := RegisterStorer(name, PlainStorer{}); err != nil {
		t.Fatalf("RegisterStorer 首次注册应成功: %v", err)
	}

	// GetStorer[PlainStorer] 命中（PlainStorer 满足 SecureStorer）。
	if got, ok := GetStorer[PlainStorer](name); !ok {
		t.Fatalf("GetStorer[PlainStorer] 应命中注册项")
	} else if _, err := got.Encrypt([]byte("x")); err != nil {
		t.Fatalf("命中项调用应可用: %v", err)
	}
	// 显式类型断言：命中项可直接以接口引用（编译期证明类型正确）。
	if _, ok := GetStorer[SecureStorer](name); !ok {
		t.Fatalf("GetStorer[SecureStorer] 应命中（PlainStorer 满足接口）")
	}

	// 重名注册 → 错误。
	if err := RegisterStorer(name, sentinelStorer{}); err == nil {
		t.Fatalf("重名 RegisterStorer 应返回错误")
	}

	// 类型不匹配：以 sentinelStorer 注册后，GetStorer[PlainStorer] 不应命中。
	UnregisterStorer(name)
	if err := RegisterStorer(name, sentinelStorer{}); err != nil {
		t.Fatalf("RegisterStorer(sentinel) 首次注册应成功: %v", err)
	}
	if got, ok := GetStorer[PlainStorer](name); ok {
		t.Fatalf("类型不匹配不应命中: got %+v", got)
	}
	if _, ok := GetStorer[sentinelStorer](name); !ok {
		t.Fatalf("sentinelStorer 本身应命中")
	}

	// 反注册后不命中。
	UnregisterStorer(name)
	if _, ok := GetStorer[sentinelStorer](name); ok {
		t.Fatalf("UnregisterStorer 后不应命中")
	}

	// 未注册名不命中。
	if _, ok := GetStorer[PlainStorer]("no-such-storer"); ok {
		t.Fatalf("未注册名不应命中")
	}
}

// typedNilPtrStorer 是 typed-nil 探针：*typedNilPtrStorer 满足 SecureStorer，
// 零值即 nil 指针。
type typedNilPtrStorer struct{}

func (*typedNilPtrStorer) Encrypt(p []byte) ([]byte, error) { return p, nil }
func (*typedNilPtrStorer) Decrypt(c []byte) ([]byte, error) { return c, nil }

// TestStorerRegistry_RejectTypedNil 验证 RegisterStorer 对 typed-nil 值的防御：
//   - RegisterStorer(name, (*typedNilPtrStorer)(nil)) → 错误（拒绝入库）；
//   - 拒绝后 GetStorer 不命中（ok==false），不 panic；
//   - 字面 nil 同样拒绝。
func TestStorerRegistry_RejectTypedNil(t *testing.T) {
	const ln = "reject-typed-nil"

	if err := RegisterStorer(ln, (*typedNilPtrStorer)(nil)); err == nil {
		t.Fatalf("RegisterStorer typed-nil 应返回错误（拒绝入库）")
	}
	if _, ok := GetStorer[*typedNilPtrStorer](ln); ok {
		t.Fatalf("typed-nil 拒绝后不应命中注册表")
	}

	if err := RegisterStorer("reject-literal-nil", nil); err == nil {
		t.Fatalf("RegisterStorer 字面 nil 应返回错误")
	}
	if _, ok := GetStorer[PlainStorer]("reject-literal-nil"); ok {
		t.Fatalf("字面 nil 拒绝后不应命中注册表")
	}
}
