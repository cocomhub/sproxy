// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// valueStorer 是仅用于 TestNormalizeStorer 的非指针值类型 CredentialStorer 实现
// （验证 normalizeStorer 对值类型入参原样返回，不误归一为 nil）。
type valueStorer struct{ tag string }

func (valueStorer) Load() ([]accesskey.Key, error) { return nil, nil }
func (valueStorer) Save([]accesskey.Key) error     { return nil }

// TestNormalizeStorer 验证 normalizeStorer 的 nil 归一语义（审查 M1）：
//   - 字面 nil → nil；
//   - typed-nil `(*accesskey.CredentialStore)(nil)` 装箱进接口 → nil（消除 persistCredentials
//     对 nil 接收者调 Save 的 panic 根因）；
//   - 真指针 → 原样同一值；
//   - 非指针值类型实现（valueStorer 满足 CredentialStorer）→ 原样同一接口值。
func TestNormalizeStorer(t *testing.T) {
	real := accesskey.NewCredentialStore(filepath.Join(t.TempDir(), "tenant", "meta"))

	cases := []struct {
		name    string
		in      accesskey.CredentialStorer
		wantNil bool
	}{
		{name: "字面nil", in: nil, wantNil: true},
		{name: "typed-nil指针", in: (*accesskey.CredentialStore)(nil), wantNil: true},
		{name: "真指针非nil", in: real},
		{name: "非指针值类型", in: valueStorer{tag: "v"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeStorer(tc.in)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("应归一为 nil, got %#v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("非 nil 输入被误归一为 nil")
			}
			// 同一性：归一结果与入参指向/等效同一实例。
			switch v := tc.in.(type) {
			case *accesskey.CredentialStore:
				if got.(*accesskey.CredentialStore) != v {
					t.Fatalf("真指针应原样返回，got 另一个实例")
				}
			case valueStorer:
				stored, ok := got.(valueStorer)
				if !ok || stored != v {
					t.Fatalf("值类型应原样返回: got %#v, want %#v", got, v)
				}
			default:
				t.Fatalf("未预期的输入类型单向断言: %T", tc.in)
			}
		})
	}
}
