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
	// 空输入往返也保持原样（nil↔nil：结果必须为 nil，而非非 nil 空切片）。
	empty, err := s.Encrypt(nil)
	if err != nil {
		t.Fatalf("Encrypt(nil): %v", err)
	}
	if empty != nil {
		t.Fatalf("Encrypt(nil) 应返回 nil, got %#v (len=%d)", empty, len(empty))
	}
	empty2, err := s.Decrypt(nil)
	if err != nil {
		t.Fatalf("Decrypt(nil): %v", err)
	}
	if empty2 != nil {
		t.Fatalf("Decrypt(nil) 应返回 nil, got %#v (len=%d)", empty2, len(empty2))
	}
}

// TestPlainStorer_DeepCopy 钉死 PlainStorer 的深拷贝契约（审查 M3）：若实现被
// 「简化」为直接返回入参切片（`return p, nil`），调用方 mutate 返回切片会反过来
// 污染入参缓冲——对凭据快照是静默数据损坏。双向断言：mutate Encrypt 返回值 → 入参
// 原样；mutate Decrypt 返回值 → 密文原样（拷贝语义不共享底层数组）。
func TestPlainStorer_DeepCopy(t *testing.T) {
	var s PlainStorer

	// Encrypt：mutate 返回切片，入参必须不受影响。
	pt := []byte("plaintext-payload")
	enc, err := s.Encrypt(pt)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	for i := range enc {
		enc[i] ^= 0xFF
	}
	if !bytes.Equal(pt, []byte("plaintext-payload")) {
		t.Fatalf("Encrypt 返回切片与入参共享底层数组（非深拷贝）: 入参被污染为 %q", pt)
	}

	// Decrypt：mutate 返回切片，密文必须不受影响。
	dec, err := s.Decrypt(enc)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	before := append([]byte(nil), enc...)
	for i := range dec {
		dec[i] = 0x00
	}
	if !bytes.Equal(enc, before) {
		t.Fatalf("Decrypt 返回切片与密文共享底层数组（非深拷贝）: 密文被污染")
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

// typedNilPtrStorer 是 typed-nil 指针探针：*typedNilPtrStorer 满足 SecureStorer，
// 零值即 nil 指针。
type typedNilPtrStorer struct{}

func (*typedNilPtrStorer) Encrypt(p []byte) ([]byte, error) { return p, nil }
func (*typedNilPtrStorer) Decrypt(c []byte) ([]byte, error) { return c, nil }

// typedNilMapStorer 是 typed-nil 非指针 nil-able 探针：map 派生类型，零值即 nil map。
type typedNilMapStorer map[string]struct{}

func (typedNilMapStorer) Encrypt(p []byte) ([]byte, error) { return p, nil }
func (typedNilMapStorer) Decrypt(c []byte) ([]byte, error) { return c, nil }

// TestStorerRegistry_RejectTypedNil 验证 RegisterStorer 对 nil 值的防御（审查 M2）：
//   - 字面 nil → 错误；
//   - typed-nil 指针（(*typedNilPtrStorer)(nil)）→ 错误（拒绝入库）；
//   - typed-nil 非指针 nil-able（nil map 派生类型）→ 错误（守卫覆盖 Pointer 以外
//     的 Chan/Func/Map/Slice/Interface）；
//   - 拒绝后 GetStorer 不命中（ok==false），不 panic。
func TestStorerRegistry_RejectTypedNil(t *testing.T) {
	if err := RegisterStorer("reject-literal-nil", nil); err == nil {
		t.Fatalf("RegisterStorer 字面 nil 应返回错误")
	}
	if _, ok := GetStorer[PlainStorer]("reject-literal-nil"); ok {
		t.Fatalf("字面 nil 拒绝后不应命中注册表")
	}

	const ln = "reject-typed-nil"
	if err := RegisterStorer(ln, (*typedNilPtrStorer)(nil)); err == nil {
		t.Fatalf("RegisterStorer typed-nil 指针应返回错误（拒绝入库）")
	}
	if _, ok := GetStorer[*typedNilPtrStorer](ln); ok {
		t.Fatalf("typed-nil 指针拒绝后不应命中注册表")
	}

	const lnMap = "reject-typed-nil-map"
	var nilMap typedNilMapStorer // nil map
	if err := RegisterStorer(lnMap, nilMap); err == nil {
		t.Fatalf("RegisterStorer typed-nil map 应返回错误（拒绝入库）")
	}
	if _, ok := GetStorer[typedNilMapStorer](lnMap); ok {
		t.Fatalf("typed-nil map 拒绝后不应命中注册表")
	}
}
