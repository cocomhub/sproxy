// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webdav

import (
	"context"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// TestNewWebDAVBackend_FromVolumeExtra 钉住 backend 构造：v.Extra 读 url/username/password
// → 构造 WebDAVFS 并包装 ExternalBackend（FS() 非 nil；类型断言 sync.FS）。
func TestNewWebDAVBackend_FromVolumeExtra(t *testing.T) {
	t.Parallel()
	v := volume.Volume{
		Name: "webdav-vol",
		Type: "webdav",
		Extra: map[string]any{
			"url":      "http://127.0.0.1:8080/webdav",
			"username": "user1",
			"password": "pass1",
		},
	}
	be, err := newWebDAVBackend(context.Background(), v)
	if err != nil {
		t.Fatalf("newWebDAVBackend: %v", err)
	}
	if be.FS() == nil {
		t.Fatal("FS() 不应为 nil")
	}
	// Close 幂等（关 idle 连接）。
	if err := be.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestNewWebDAVBackend_TokenAuth 钉住 Bearer token 认证路径（token 优先于 Basic）。
func TestNewWebDAVBackend_TokenAuth(t *testing.T) {
	t.Parallel()
	v := volume.Volume{
		Name: "webdav-vol",
		Type: "webdav",
		Extra: map[string]any{
			"url":   "https://example.com/webdav",
			"token": "tok-123",
		},
	}
	be, err := newWebDAVBackend(context.Background(), v)
	if err != nil {
		t.Fatalf("newWebDAVBackend(token): %v", err)
	}
	if be.FS() == nil {
		t.Fatal("FS() 不应为 nil")
	}
}

// TestNewWebDAVBackend_MissingURL 钉住 fail-closed：Extra 缺 url → 明确错误。
func TestNewWebDAVBackend_MissingURL(t *testing.T) {
	t.Parallel()
	v := volume.Volume{
		Name: "webdav-vol",
		Type: "webdav",
		Extra: map[string]any{
			"username": "user1",
			"password": "pass1",
		},
	}
	if _, err := newWebDAVBackend(context.Background(), v); err == nil {
		t.Fatal("缺 url 应报错")
	} else if !strings.Contains(err.Error(), "需配置 extra.url") {
		t.Fatalf("错误应提及需配置 extra.url, got %v", err)
	}
}

// TestNewWebDAVBackend_BadURLScheme 钉住 url scheme 校验（仅 http/https）。
func TestNewWebDAVBackend_BadURLScheme(t *testing.T) {
	t.Parallel()
	v := volume.Volume{
		Name: "webdav-vol",
		Type: "webdav",
		Extra: map[string]any{
			"url":      "ftp://host/webdav",
			"username": "user1",
			"password": "pass1",
		},
	}
	if _, err := newWebDAVBackend(context.Background(), v); err == nil {
		t.Fatal("ftp scheme 应报错")
	} else if !strings.Contains(err.Error(), "extra.url 非法") {
		t.Fatalf("错误应提及 extra.url 非法, got %v", err)
	}
}

// TestNewWebDAVBackend_MissingAuth 钉住 fail-closed：url 有但 username/password 与 token
// 全空 → 明确错误（WebDAV 无匿名目标，认证必需）。
func TestNewWebDAVBackend_MissingAuth(t *testing.T) {
	t.Parallel()
	v := volume.Volume{
		Name: "webdav-vol",
		Type: "webdav",
		Extra: map[string]any{
			"url": "http://127.0.0.1:8080/webdav",
		},
	}
	if _, err := newWebDAVBackend(context.Background(), v); err == nil {
		t.Fatal("缺认证应报错")
	} else if !strings.Contains(err.Error(), "auth") && !strings.Contains(err.Error(), "username") {
		t.Fatalf("错误应提及认证, got %v", err)
	}
}

// TestRegisterWebDAVBackend 钉住注册 + registry 分派：用唯一类型名注册后，
// registry.NewBackend 经分派构造出 backend（V3 可插拔承诺）。
func TestRegisterWebDAVBackend(t *testing.T) {
	t.Parallel()
	typ := "webdav-test-reg"
	registerWebDAVBackendWithFactory(typ)

	v := volume.Volume{
		Name: "webdav-vol",
		Type: typ,
		Extra: map[string]any{
			"url":      "http://127.0.0.1:8080/webdav",
			"username": "user1",
			"password": "pass1",
		},
	}
	be, err := registry.NewBackend(context.Background(), v)
	if err != nil {
		t.Fatalf("registry.NewBackend 分派失败: %v", err)
	}
	if be.FS() == nil {
		t.Fatal("分派构造的 backend FS() 不应为 nil")
	}
}
