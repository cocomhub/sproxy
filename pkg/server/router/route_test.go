// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClass_Valid(t *testing.T) {
	t.Parallel()
	for _, c := range []Class{ClassPublic, ClassAuth, ClassUser, ClassReader, ClassNode, ClassAdmin, ClassTunnel} {
		if !c.Valid() {
			t.Fatalf("%q 应合法", c)
		}
	}
	for _, c := range []Class{ClassNone, "bogus", "Auth"} {
		if c.Valid() {
			t.Fatalf("%q 不应合法", c)
		}
	}
}

func TestRegister_BothMuxesWithDistinctWrappers(t *testing.T) {
	t.Parallel()
	srv, local := http.NewServeMux(), http.NewServeMux()
	routes := []Route[string]{
		{Method: "GET", Path: "/tasks", Server: ClassAuth, Local: ClassTunnel, H: "list"},
	}

	var srvWrapped, localWrapped int
	wrap := func(c Class) func(http.HandlerFunc) http.HandlerFunc {
		return func(h http.HandlerFunc) http.HandlerFunc {
			switch c {
			case ClassAuth:
				srvWrapped++
			case ClassTunnel:
				localWrapped++
			}
			return h
		}
	}
	n := Register(srv, local, "/api/cloud", routes, func(k string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(k)) }
	}, wrap)

	if n != 2 {
		t.Fatalf("注册条数 = %d, want 2（双 mux）", n)
	}
	if srvWrapped != 1 || localWrapped != 1 {
		t.Fatalf("包装器调用 srv=%d local=%d, want 1/1", srvWrapped, localWrapped)
	}
	for _, tc := range []struct {
		mux  *http.ServeMux
		path string
	}{{srv, "/api/cloud/tasks"}, {local, "/api/cloud/tasks"}} {
		rec := httptest.NewRecorder()
		tc.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK || rec.Body.String() != "list" {
			t.Fatalf("%s 响应 = (%d,%q)", tc.path, rec.Code, rec.Body.String())
		}
	}
}

func TestRegister_OneSidedRoute(t *testing.T) {
	t.Parallel()
	srv, local := http.NewServeMux(), http.NewServeMux()
	routes := []Route[int]{
		{Method: "POST", Path: "/only-server", Server: ClassAuth, Local: ClassNone, H: 1},
		{Method: "POST", Path: "/only-local", Server: ClassNone, Local: ClassTunnel, H: 2},
	}
	n := Register(srv, local, "/api/x", routes, func(int) http.HandlerFunc {
		return func(http.ResponseWriter, *http.Request) {}
	}, nil)
	if n != 2 {
		t.Fatalf("注册条数 = %d, want 2（两条单向）", n)
	}
}

func TestRegister_PanicsOnRelativePath(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("相对路径缺少前导 / 应 panic（fail-fast）")
		}
	}()
	Register(http.NewServeMux(), http.NewServeMux(), "/api/x",
		[]Route[string]{{Method: "GET", Path: "bad", Server: ClassAuth, Local: ClassNone, H: "h"}},
		func(string) http.HandlerFunc { return func(http.ResponseWriter, *http.Request) {} }, nil)
}

func TestRegister_PanicsOnBadPrefix(t *testing.T) {
	t.Parallel()
	for _, prefix := range []string{"api/x", "/api/x/", ""} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("前缀 %q 应 panic", prefix)
				}
			}()
			Register[string](http.NewServeMux(), http.NewServeMux(), prefix, nil, nil, nil)
		}()
	}
}

func TestRegister_PanicsOnUnboundHandler(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("未绑定 handler 应 panic（不允许静默跳过）")
		}
	}()
	Register(http.NewServeMux(), http.NewServeMux(), "/api/x",
		[]Route[string]{{Method: "GET", Path: "/a", Server: ClassAuth, Local: ClassNone, H: "h"}},
		func(string) http.HandlerFunc { return nil }, nil)
}

func TestRegister_PanicsOnUnknownClass(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("未知分类应 panic（fail-closed）")
		}
		if !strings.Contains(r.(string), "未知分类") {
			t.Fatalf("panic 文案 = %v", r)
		}
	}()
	Register(http.NewServeMux(), http.NewServeMux(), "/api/x",
		[]Route[string]{{Method: "GET", Path: "/a", Server: Class("bogus"), Local: ClassNone, H: "h"}},
		func(string) http.HandlerFunc { return func(http.ResponseWriter, *http.Request) {} }, nil)
}
