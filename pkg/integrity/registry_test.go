// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integrity_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/integrity"
)

// fakeChecker 是最小 Checker 实现：Matches 按 image 扩展名族判定，Check 恒 OK。
// 测试装配用，经本地 Registry 注入、不触碰包级默认全局（R18 并行安全、互不污染）。
type fakeChecker struct{}

func (fakeChecker) Kind() string { return "image/*" }
func (fakeChecker) Matches(name string) bool {
	lower := strings.ToLower(name)
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}
func (fakeChecker) Check(context.Context, string, int64) (*integrity.Report, error) {
	return &integrity.Report{OK: true}, nil
}

// tarChecker：第二个校验器（异 Kind、同机制），Matches 断言 tar 扩展名族。
type tarChecker struct{}

func (tarChecker) Kind() string { return "archive/tar" }
func (tarChecker) Matches(name string) bool {
	lower := strings.ToLower(name)
	for _, ext := range []string{".tar", ".tar.gz", ".tgz", ".tar.zst", ".tar.br"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}
func (tarChecker) Check(context.Context, string, int64) (*integrity.Report, error) {
	return &integrity.Report{OK: true}, nil
}

// TestCheckerInterface 编译期断言 fakeChecker 满足 Checker（锁定接口契约）。
func TestCheckerInterface(t *testing.T) {
	t.Parallel()
	var _ integrity.Checker = fakeChecker{}
}

// TestRegisterAndLookup_DispatchByMatches：注册 image/* 后同名扩展命中；未知扩展
// 返回 nil（未知类型仅字节级校验，不误报——Review Focus 1）。
func TestRegisterAndLookup_DispatchByMatches(t *testing.T) {
	t.Parallel()
	reg := integrity.NewRegistry()
	reg.Register("image/*", func() integrity.Checker { return fakeChecker{} })

	c := reg.Lookup("a.png")
	if c == nil {
		t.Fatal("png 应命中 image/*")
	}
	if reg.Lookup("x.xyz") != nil {
		t.Fatal("未知扩展名应返回 nil")
	}
}

// TestRegisterDuplicatePanics：重复注册同一 Kind → fail-fast panic（shardseal 模式，
// 哨兵 ErrDuplicateKind）。
func TestRegisterDuplicatePanics(t *testing.T) {
	t.Parallel()
	reg := integrity.NewRegistry()
	fac := func() integrity.Checker { return fakeChecker{} }
	reg.Register("image/*", fac)
	defer func() {
		rv := recover()
		if rv == nil {
			t.Fatal("重复注册应 panic")
		}
		err, ok := rv.(error)
		if !ok || !errors.Is(err, integrity.ErrDuplicateKind) {
			t.Fatalf("panic 应为 ErrDuplicateKind，got %v", rv)
		}
	}()
	reg.Register("image/*", fac)
}

// TestRegisterEmptyKindPanics：空 Kind 注册 → panic（装配错误即失败，不静默）。
func TestRegisterEmptyKindPanics(t *testing.T) {
	t.Parallel()
	reg := integrity.NewRegistry()
	defer func() {
		if recover() == nil {
			t.Fatal("空 Kind 注册应 panic")
		}
	}()
	reg.Register("", func() integrity.Checker { return fakeChecker{} })
}

// TestRegisterNilFactoryPanics：nil 工厂注册 → panic。
func TestRegisterNilFactoryPanics(t *testing.T) {
	t.Parallel()
	reg := integrity.NewRegistry()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("nil 工厂注册应 panic")
		}
	}()
	reg.Register("image/*", nil)
}

// TestLookup_MultiKindDispatch：image/* 与 archive/tar 各自分发到对应 Kind。
func TestLookup_MultiKindDispatch(t *testing.T) {
	t.Parallel()
	reg := integrity.NewRegistry()
	reg.Register("image/*", func() integrity.Checker { return fakeChecker{} })
	reg.Register("archive/tar", func() integrity.Checker { return tarChecker{} })

	if c := reg.Lookup("a.png"); c == nil || c.Kind() != "image/*" {
		t.Fatalf("png 应分发到 image/*，got %+v", c)
	}
	if c := reg.Lookup("b.tar"); c == nil || c.Kind() != "archive/tar" {
		t.Fatalf("b.tar 应分发到 archive/tar，got %+v", c)
	}
}

// TestDefaultRegistry_LookupUnregisteredNil：包级默认 Lookup 对未注册名返回 nil
// （并行只读冒烟，不触碰全局）。
func TestDefaultRegistry_LookupUnregisteredNil(t *testing.T) {
	t.Parallel()
	if integrity.Lookup("whatever.xyz") != nil {
		t.Fatal("未注册名在默认注册表应返回 nil")
	}
}
