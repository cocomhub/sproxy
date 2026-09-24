// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"errors"
	"log/slog"
	"testing"
)

// fakeState 是注册表测试用的最小 StateStore 实现。
type fakeState struct{}

func (fakeState) Get(context.Context, string) ([]byte, error)    { return nil, ErrKeyNotFound }
func (fakeState) Put(context.Context, string, []byte) error      { return nil }
func (fakeState) Delete(context.Context, string) error           { return nil }
func (fakeState) List(context.Context, string) ([]string, error) { return nil, nil }
func (fakeState) CAS(context.Context, string, []byte, []byte) error {
	return nil
}

// TestRegistry_RegisterStateStore 钉住注册表行为：重复注册 panic / 空名 panic /
// 未注册 NewStateStore 报错（fail-closed）/ StateStoreTypes 副本。
func TestRegistry_RegisterStateStore(t *testing.T) {
	t.Parallel()
	defer UnregisterStateStoreForTest("fake-reg")

	RegisterStateStore("fake-reg", func(cfg StateStoreConfig, logger *slog.Logger) (StateStore, error) {
		return fakeState{}, nil
	})
	// 重复注册 → panic。
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("重复注册应 panic")
			}
		}()
		RegisterStateStore("fake-reg", func(cfg StateStoreConfig, logger *slog.Logger) (StateStore, error) {
			return fakeState{}, nil
		})
	}()
	// 空名 → panic。
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("空名注册应 panic")
			}
		}()
		RegisterStateStore("", func(cfg StateStoreConfig, logger *slog.Logger) (StateStore, error) {
			return fakeState{}, nil
		})
	}()
	// StateStoreTypes 含已注册类型（副本）。
	types := StateStoreTypes()
	found := false
	for _, tp := range types {
		if tp == "fake-reg" {
			found = true
		}
	}
	if !found {
		t.Fatalf("StateStoreTypes() 应含 fake-reg, got %v", types)
	}
	// 未注册类型 → fail-closed 报错。
	if _, err := NewStateStore("no-such", StateStoreConfig{}, testLogger()); err == nil {
		t.Fatal("未注册类型应报错（fail-closed，不回落 local）")
	} else if !errors.Is(err, ErrStateStoreNotRegistered) {
		t.Fatalf("未注册错误应为 ErrStateStoreNotRegistered, got %v", err)
	}
}

// TestNewStateStore_Local 验证内置 local 类型经注册表可构造且返回真实本地实现。
func TestNewStateStore_Local(t *testing.T) {
	t.Parallel()
	st, err := NewStateStore("local", StateStoreConfig{Dir: t.TempDir()}, testLogger())
	if err != nil {
		t.Fatalf("NewStateStore(local): %v", err)
	}
	ctx := context.Background()
	if err := st.Put(ctx, "credential/anonymous/ring", []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got, err := st.Get(ctx, "credential/anonymous/ring"); err != nil || string(got) != "v" {
		t.Fatalf("Get = %q, %v", got, err)
	}
}
