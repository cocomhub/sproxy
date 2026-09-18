// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// backend.go 是 S3 存储后端的 V3 plugin 接入：把 S3FS（sync.FS 客户端）包装为
// registry.ExternalBackend，经 RegisterBackend("s3") 注册——系统盘（volumes[]
// type=s3）与用户卷（POST /api/volumes/user type=s3）统一走 V3 装配分派。
//
// 与 webdav backend（pkg/volume/webdav/backend.go）、baidupcs backend 同构：
// Extra 读类型特有配置 → 构造客户端 → ExternalBackend 包装；装配层（cmd/sproxy/root.go）
// 调用 Register 注册。
package s3

import (
	"context"
	"fmt"
	"strings"
	"sync"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// s3ExternalBackend 是 S3 卷的 ExternalBackend 实现：持有 S3FS 同步视图，
// Close 幂等（S3FS.Close no-op，minio.Client 无显式关闭）。
type s3ExternalBackend struct {
	fs *S3FS
}

func (b *s3ExternalBackend) FS() syncpkg.FS { return b.fs }

func (b *s3ExternalBackend) Close() error { return b.fs.Close() }

var _ registry.ExternalBackend = (*s3ExternalBackend)(nil)

// newS3Backend 按卷描述构造 S3 外部后端（V3 可插拔；用户确认放 pkg/volume/ext/s3）。
//
// 从 v.Extra 读类型特有配置（map[string]any，值须为 string）：
//   - "endpoint"：S3 服务地址（必填，host[:port]，如 "127.0.0.1:9000"；也可带 scheme
//     http(s)://——use_ssl 决定协议时 endpoint 无 scheme）；
//   - "bucket"：桶名（必填）；
//   - "access_key"/"secret_key"：凭据（必填）；
//   - "region"：区域（可选；AWS 需要，MinIO 可空）；
//   - "use_ssl"：是否 TLS（可选 bool/string，默认 false）；
//   - "local_root"：本地中间态基目录（预留：与 baidupcs/webdav 一致的中间态约束；
//     当前 S3FS 无 staging——WriteFile 直接 PutObject，OpenRead GetObject 直连）。
//
// 凭据（fail-closed）：endpoint/bucket/access_key/secret_key 必填；endpoint 支持
// http(s):// scheme（解析 host:port 传给 minio.New，use_ssl 决定协议）。
func newS3Backend(ctx context.Context, v volume.Volume) (registry.ExternalBackend, error) {
	if v.Type == "" || v.Type == volume.TypeLocal {
		return nil, fmt.Errorf("s3 backend: 卷 %q 类型 %q 不是外部 s3 卷", v.Name, v.Type)
	}
	rawEndpoint, _ := v.Extra["endpoint"].(string)
	rawEndpoint = strings.TrimSpace(rawEndpoint)
	if rawEndpoint == "" {
		return nil, fmt.Errorf("s3 backend: 卷 %q 需配置 extra.endpoint（S3 服务地址 host[:port]，如 127.0.0.1:9000）", v.Name)
	}
	bucket, _ := v.Extra["bucket"].(string)
	if strings.TrimSpace(bucket) == "" {
		return nil, fmt.Errorf("s3 backend: 卷 %q 需配置 extra.bucket（桶名）", v.Name)
	}
	accessKey, _ := v.Extra["access_key"].(string)
	if strings.TrimSpace(accessKey) == "" {
		return nil, fmt.Errorf("s3 backend: 卷 %q 需配置 extra.access_key", v.Name)
	}
	secretKey, _ := v.Extra["secret_key"].(string)
	if strings.TrimSpace(secretKey) == "" {
		return nil, fmt.Errorf("s3 backend: 卷 %q 需配置 extra.secret_key", v.Name)
	}
	region, _ := v.Extra["region"].(string)
	useSSL := parseUseSSL(v.Extra["use_ssl"])
	// local_root 预留（当前 S3FS 无 staging；保留字段供未来中间态扩展）。
	// _ = v.Extra["local_root"]

	// endpoint 带 scheme（http(s)://）时剥离，scheme 决定 useSSL 缺省。
	endpoint, useSSL := normalizeEndpoint(rawEndpoint, useSSL)

	fs, err := NewS3FS(ClientConfig{
		Endpoint:  endpoint,
		AccessKey: accessKey,
		SecretKey: secretKey,
		Region:    region,
		UseSSL:    useSSL,
		Bucket:    bucket,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 backend: 卷 %q 客户端构造失败: %w", v.Name, err)
	}
	return &s3ExternalBackend{fs: fs}, nil
}

// parseUseSSL 解析 use_ssl（bool 或 string "true"/"false"；缺省 false）。
func parseUseSSL(raw any) bool {
	switch val := raw.(type) {
	case bool:
		return val
	case string:
		return strings.EqualFold(strings.TrimSpace(val), "true")
	}
	return false
}

// normalizeEndpoint 剥离 endpoint 的 http(s):// scheme（若有），scheme 决定 useSSL。
// 返回 (host[:port], useSSL)。
func normalizeEndpoint(raw string, useSSL bool) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	lower := strings.ToLower(trimmed)
	switch {
	case strings.HasPrefix(lower, "https://"):
		return strings.TrimPrefix(trimmed, "https://"), true
	case strings.HasPrefix(lower, "http://"):
		return strings.TrimPrefix(trimmed, "http://"), false
	}
	return trimmed, useSSL
}

// registerS3BackendWithFactory 注册 s3 后端类型构造器（测试可用唯一类型名注册，
// 避免与生产 "s3" 重复 panic）。重复注册 → registry panic（编程错误）。
func registerS3BackendWithFactory(typ string) {
	registry.RegisterBackend(typ, newS3Backend)
}

// RegisterS3Backend 注册 s3 后端（装配层 root.go 调用）。
// 用 sync.Once 保证只注册一次：多装配/多测试并发调 runServer 时避免重复注册 panic。
var registerS3Once sync.Once

func RegisterS3Backend() {
	registerS3Once.Do(func() {
		registerS3BackendWithFactory("s3")
	})
}
