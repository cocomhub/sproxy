// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

// backend_test.go 是 S3 backend 插件（V3 可插拔）的单元测试。
// 覆盖：Extra 配置解析（endpoint/bucket/access_key/secret_key）→ S3FS 构造、
// 缺字段 fail-closed、registry.RegisterBackend 分派。
//
// 不依赖 MinIO 容器（S3FS 构造只校验配置，不连网）——容器集成见 s3_fs_integration_test.go。

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// TestNewS3Backend_FromVolumeExtra 验证 Extra 配置解析 → S3FS 构造。
func TestNewS3Backend_FromVolumeExtra(t *testing.T) {
	t.Parallel()
	v := volume.Volume{
		Name: "s3-disk1",
		Type: "s3",
		Extra: map[string]any{
			"endpoint":   "127.0.0.1:9000",
			"bucket":     "mybucket",
			"access_key": "test-ak",
			"secret_key": "test-sk",
			"region":     "us-east-1",
		},
	}
	be, err := newS3Backend(context.Background(), v)
	if err != nil {
		t.Fatalf("newS3Backend: %v", err)
	}
	if be == nil {
		t.Fatal("newS3Backend 返回 nil")
	}
	if be.FS() == nil {
		t.Fatal("ExternalBackend.FS() 为 nil")
	}
	var _ = be.FS() // FS 必须是 sync.FS
	// Close 幂等安全。
	if err := be.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestNewS3Backend_MissingFields 验证缺必填字段 → 明确错误（fail-closed）。
func TestNewS3Backend_MissingFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		extra   map[string]any
		wantErr string // 空 = 期望通过
	}{
		{"完整配置", map[string]any{"endpoint": "127.0.0.1:9000", "bucket": "b", "access_key": "ak", "secret_key": "sk"}, ""},
		{"缺 endpoint", map[string]any{"bucket": "b", "access_key": "ak", "secret_key": "sk"}, "endpoint"},
		{"缺 bucket", map[string]any{"endpoint": "127.0.0.1:9000", "access_key": "ak", "secret_key": "sk"}, "bucket"},
		{"缺 access_key", map[string]any{"endpoint": "127.0.0.1:9000", "bucket": "b", "secret_key": "sk"}, "access_key"},
		{"缺 secret_key", map[string]any{"endpoint": "127.0.0.1:9000", "bucket": "b", "access_key": "ak"}, "secret_key"},
		{"空 endpoint", map[string]any{"endpoint": "  ", "bucket": "b", "access_key": "ak", "secret_key": "sk"}, "endpoint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := volume.Volume{Name: "v", Type: "s3", Extra: tc.extra}
			_, err := newS3Backend(context.Background(), v)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("应通过, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应报错（含 %q）, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误应含 %q, got %q", tc.wantErr, err.Error())
			}
		})
	}
}

// TestNewS3Backend_NotExternalType 验证非外部类型防御拒绝。
func TestNewS3Backend_NotExternalType(t *testing.T) {
	t.Parallel()
	v := volume.Volume{Name: "v", Type: volume.TypeLocal, Extra: map[string]any{"endpoint": "x"}}
	if _, err := newS3Backend(context.Background(), v); err == nil {
		t.Fatal("本地类型应被拒绝")
	}
}

// TestRegisterS3Backend 验证 registry.NewBackend 分派（唯一类型名避免与生产注册冲突）。
func TestRegisterS3Backend(t *testing.T) {
	t.Parallel()
	typ := "s3-test-unique-" + randSuffix()
	registerS3BackendWithFactory(typ)
	be, err := registry.NewBackend(context.Background(), volume.Volume{
		Name: "v", Type: typ,
		Extra: map[string]any{"endpoint": "127.0.0.1:9000", "bucket": "b", "access_key": "ak", "secret_key": "sk"},
	})
	if err != nil {
		t.Fatalf("registry.NewBackend: %v", err)
	}
	if be == nil || be.FS() == nil {
		t.Fatalf("分派返回空 backend/FS")
	}
	_ = be.Close()
}

// TestRegisterS3Backend_Production 验证生产注册（sync.Once 幂等）。
func TestRegisterS3Backend_Production(t *testing.T) {
	t.Parallel()
	RegisterS3Backend()
	RegisterS3Backend() // 第二次调用应幂等（sync.Once）
	types := registry.BackendTypes()
	found := slices.Contains(types, "s3")
	if !found {
		t.Fatalf("BackendTypes 应含 s3, got %v", types)
	}
}

// randSuffix 生成唯一后缀（测试类型名避免冲突）。
func randSuffix() string {
	return fmt.Sprintf("%d", suffixCounter.Add(1))
}

var suffixCounter atomic.Int64

// TestNormalizeEndpoint 验证 endpoint scheme 剥离（http:// → host:port + useSSL=false）。
func TestNormalizeEndpoint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		useSSL  bool
		wantEP  string
		wantSSL bool
	}{
		{"127.0.0.1:9000", false, "127.0.0.1:9000", false},
		{"http://127.0.0.1:9000", false, "127.0.0.1:9000", false},
		{"https://s3.example.com", false, "s3.example.com", true},
		{"http://x", true, "x", false}, // scheme 优先于 useSSL
	}
	for _, tc := range cases {
		ep, ssl := normalizeEndpoint(tc.in, tc.useSSL)
		if ep != tc.wantEP || ssl != tc.wantSSL {
			t.Errorf("normalizeEndpoint(%q, %v) = (%q, %v), want (%q, %v)", tc.in, tc.useSSL, ep, ssl, tc.wantEP, tc.wantSSL)
		}
	}
}

// TestNewS3Backend_MultipartConfig 验证 Extra 的 multipart 配置接线到 S3FS.cfg。
func TestNewS3Backend_MultipartConfig(t *testing.T) {
	t.Parallel()
	v := volume.Volume{
		Name: "s3-mp",
		Type: "s3",
		Extra: map[string]any{
			"endpoint":            "127.0.0.1:9000",
			"bucket":              "b",
			"access_key":          "ak",
			"secret_key":          "sk",
			"multipart_threshold": "8388608", // 8MiB
			"multipart_part_size": "4194304", // 4MiB（<5MiB 会被钳制）
			"upload_retries":      "5",
		},
	}
	be, err := newS3Backend(context.Background(), v)
	if err != nil {
		t.Fatalf("newS3Backend: %v", err)
	}
	fs := be.(*s3ExternalBackend).fs
	if fs.cfg.MultipartThreshold != 8<<20 {
		t.Errorf("MultipartThreshold = %d, want %d", fs.cfg.MultipartThreshold, 8<<20)
	}
	if fs.cfg.MultipartPartSize != 4<<20 {
		t.Errorf("MultipartPartSize = %d, want %d", fs.cfg.MultipartPartSize, 4<<20)
	}
	if fs.cfg.UploadRetries != 5 {
		t.Errorf("UploadRetries = %d, want 5", fs.cfg.UploadRetries)
	}
}
