// Copyright 2026 The Cocomhub Authors. All rights reserved.

// SPDX-License-Identifier: Apache-2.0

// Package s3 提供 S3 对象存储后端客户端：把任意 S3 兼容服务（AWS S3 / MinIO /

// COS / OSS，ListObjectsV2/GetObject/PutObject/CopyObject/RemoveObject）适配为

// `pkg/sync.FS`（7 方法），作为 sproxy 外部卷（V3 通用卷模型，RegisterBackend("s3")

// 接入系统盘/用户卷）。

//

// 独立 Go module（go.mod 隔离）：minio-go 第三方 SDK 依赖隔离在本 module，主仓

// 不直接依赖（仿 pkg/baidupcs 模式，用户明示 2026-09-18）。

//

// S3 无目录纯概念：目录 = 前缀（key + "/"）。MakeDir 创建零字节占位对象；ListDir

// 用 delimiter="/" 单层列举（CommonPrefixes 目录 + Contents 文件）；Stat 目录探测

// 占位对象。

package s3

import (
	"bytes"

	"context"

	"fmt"

	"io"

	"path"

	"strings"

	"time"

	"github.com/cocomhub/sproxy/pkg/sync"

	"github.com/minio/minio-go/v7"

	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ClientConfig 是 S3 客户端配置。

type ClientConfig struct {
	// Endpoint 是 S3 服务地址（host[:port]，如 "127.0.0.1:9000"；必填）。

	Endpoint string

	// AccessKey / SecretKey 是凭据（必填）。

	AccessKey string

	SecretKey string

	// Region 是区域（AWS 需要；MinIO 可空）。

	Region string

	// UseSSL 是否启用 TLS（默认 false）。

	UseSSL bool

	// Bucket 是桶名（必填）。

	Bucket string

	// Prefix 是卷根前缀（对象键前缀；空 = 桶根）。

	Prefix string

	// MultipartThreshold 是走分片上传的大文件阈值（roadmap 3.3 P1 s3 分片；
	// <=0 = 禁用分片恒单 PutObject；默认 64MiB）。
	MultipartThreshold int64

	// MultipartPartSize 是分片大小（默认 16MiB；minio 5MiB 下限钳制）。
	MultipartPartSize int64

	// UploadRetries 是 PutObject 失败重试次数（默认 3；退避重试）。
	UploadRetries int
}

// S3FS 是 S3 对象存储后端的 sync.FS 实现。

//

// 并发安全：minio.Client 自身并发安全（每实例独立连接池，仓库硬规则 17：

// 禁共享 DefaultTransport）；本结构无可变共享状态（client/bucket/prefix 只读）。

type S3FS struct {
	client *minio.Client

	putter putter // PutObject 最小接口（生产 = client；测试注入失败用）

	bucket string

	prefix string // 卷根前缀（去尾斜杠；空 = 桶根）

	cfg ClientConfig // 完整配置（分片参数经 multipartOptsFromConfig 读取）
}

// NewS3FS 构造 S3FS。bucket 必填；prefix 去首尾斜杠归一（空 = 桶根）。

func NewS3FS(cfg ClientConfig) (*S3FS, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("s3: endpoint 必填")
	}

	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3: bucket 必填")
	}

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds: credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),

		Secure: cfg.UseSSL,

		Region: cfg.Region,
	})

	if err != nil {
		return nil, fmt.Errorf("s3: 创建客户端失败: %w", err)
	}

	return &S3FS{
		client: client,

		putter: client,

		bucket: cfg.Bucket,

		prefix: normalizePrefix(cfg.Prefix),

		cfg: cfg,
	}, nil
}

// Close 关闭客户端（minio.Client 无显式 Close——幂等 no-op，接口兼容）。

func (f *S3FS) Close() error { return nil }

// normalizePrefix 把卷根前缀归一为「去首尾斜杠」形式（"" = 桶根）。

func normalizePrefix(p string) string {
	return strings.Trim(p, "/")
}

// keyFor 把 FS 相对路径映射为对象键（拼接 prefix）。

func (f *S3FS) keyFor(rel string) string {
	rel = strings.Trim(rel, "/")

	if rel == "" {
		return f.prefix
	}

	if f.prefix == "" {
		return rel
	}

	if f.prefix == "" {
		return rel
	}
	return f.prefix + "/" + rel
}

// ListDir 列出 path 的直接子条目（ListObjectsV2 delimiter="/" 单层不递归）。

func (f *S3FS) ListDir(ctx context.Context, relPath string) ([]sync.Entry, error) {
	prefix := f.keyFor(relPath)

	if prefix != "" {
		prefix += "/"
	}

	var out []sync.Entry

	seen := map[string]bool{}

	for obj := range f.client.ListObjects(ctx, f.bucket, minio.ListObjectsOptions{
		Prefix: prefix,

		Recursive: false, // delimiter="/" 单层（CommonPrefixes 目录 + Contents 文件）
	}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("s3: ListObjectsV2 %q: %w", relPath, obj.Err)
		}

		// 目录（CommonPrefixes）：key = prefix + name + "/"。

		if obj.Key != "" && strings.HasSuffix(obj.Key, "/") {
			name := strings.TrimSuffix(strings.TrimPrefix(obj.Key, prefix), "/")

			if name == "" {
				continue // 自身目录
			}

			if seen[name] {
				continue
			}

			seen[name] = true

			out = append(out, sync.Entry{
				Name: name,

				Path: joinRel(relPath, name),

				IsDir: true,
			})

			continue
		}

		// 文件（Contents）：key = prefix + name。

		name := strings.TrimPrefix(obj.Key, prefix)

		if name == "" || seen[name] {
			continue
		}

		seen[name] = true

		out = append(out, sync.Entry{
			Name: name,

			Path: joinRel(relPath, name),

			Size: obj.Size,

			MTime: obj.LastModified.UnixNano(),

			IsDir: false,
		})
	}

	return out, nil
}

// Stat 返回条目信息；不存在返回 (nil, nil)。目录 = 占位对象（key+"/"）探测。

func (f *S3FS) Stat(ctx context.Context, relPath string) (*sync.Entry, error) {
	clean := strings.Trim(relPath, "/")

	if clean == "" {
		// 根路径：恒为目录（卷根）。

		return &sync.Entry{Name: "", Path: "", IsDir: true}, nil
	}

	// 先试文件（对象键 = key）。

	info, err := f.client.StatObject(ctx, f.bucket, f.keyFor(clean), minio.StatObjectOptions{})

	if err == nil {
		return &sync.Entry{
			Name: path.Base(clean),

			Path: clean,

			Size: info.Size,

			MTime: info.LastModified.UnixNano(),

			IsDir: false,
		}, nil
	}

	if !isNotFound(err) {
		return nil, fmt.Errorf("s3: StatObject %q: %w", relPath, err)
	}

	// 再试目录占位对象（key+"/"）。

	dirInfo, dirErr := f.client.StatObject(ctx, f.bucket, f.keyFor(clean)+"/", minio.StatObjectOptions{})

	if dirErr == nil {
		return &sync.Entry{
			Name: path.Base(clean),

			Path: clean,

			MTime: dirInfo.LastModified.UnixNano(),

			IsDir: true,
		}, nil
	}

	if isNotFound(dirErr) {
		return nil, nil // 不存在
	}

	return nil, fmt.Errorf("s3: StatObject 目录 %q: %w", relPath, dirErr)
}

// OpenRead 打开对象读流（GetObject）。

func (f *S3FS) OpenRead(ctx context.Context, relPath string) (io.ReadCloser, error) {
	rc, err := f.client.GetObject(ctx, f.bucket, f.keyFor(relPath), minio.GetObjectOptions{})

	if err != nil {
		return nil, fmt.Errorf("s3: GetObject %q: %w", relPath, err)
	}

	return rc, nil
}

// WriteFile 写入对象（PutObject；S3 无 mtime 直接设置——服务端 LastModified 决定）。
//
// 分片上传（roadmap 3.3 P1）：size >= MultipartThreshold 时走 multipart（minio PutObject
// 内建自动分片 + 失败自动 Abort 防孤儿；PartSize 透传可配分片大小）+ 显式失败重试
// （UploadRetries 次退避）；小文件（< 阈值）单 PutObject 零回归。
func (f *S3FS) WriteFile(ctx context.Context, relPath string, r io.Reader, size, mtime int64) error {
	if size < 0 {
		size = 0
	}

	mo := multipartOptsFromConfig(f.cfg)
	key := f.keyFor(relPath)
	if shouldMultipart(size, mo.threshold) {
		// 大文件 multipart：透传 PartSize（已归一 >=5MiB）；minio 自动分片 + 失败 Abort。
		if err := putObjectWithRetry(ctx, f.putter, f.bucket, key, r, size,
			minio.PutObjectOptions{PartSize: uint64(mo.partSize)}, mo.retries); err != nil {
			return fmt.Errorf("s3: PutObject(分片) %q: %w", relPath, err)
		}
		return nil
	}

	// 小文件：单 PutObject（minio <16MiB 单原子 PUT）零回归。
	if err := putObjectWithRetry(ctx, f.putter, f.bucket, key, r, size,
		minio.PutObjectOptions{}, mo.retries); err != nil {
		return fmt.Errorf("s3: PutObject %q: %w", relPath, err)
	}
	return nil
}

// Rename 重命名/移动（S3 无原子 MOVE → CopyObject + RemoveObject 两步）。

func (f *S3FS) Rename(ctx context.Context, from, to string) error {
	src := f.keyFor(from)

	dst := f.keyFor(to)

	if _, err := f.client.CopyObject(ctx, minio.CopyDestOptions{
		Bucket: f.bucket, Object: dst,
	}, minio.CopySrcOptions{
		Bucket: f.bucket, Object: src,
	}); err != nil {
		return fmt.Errorf("s3: CopyObject %q → %q: %w", from, to, err)
	}

	if err := f.client.RemoveObject(ctx, f.bucket, src, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("s3: RemoveObject %q（copy 后删源）: %w", from, err)
	}

	return nil
}

// Delete 删除对象（RemoveObject 幂等：不存在不报错）。

func (f *S3FS) Delete(ctx context.Context, relPath string) error {
	if err := f.client.RemoveObject(ctx, f.bucket, f.keyFor(relPath), minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("s3: RemoveObject %q: %w", relPath, err)
	}

	return nil
}

// MakeDir 创建目录（零字节占位对象 key+"/"；已存在 → PutObject 覆盖幂等）。

func (f *S3FS) MakeDir(ctx context.Context, relPath string) error {
	key := f.keyFor(relPath) + "/"

	_, err := f.client.PutObject(ctx, f.bucket, key, bytes.NewReader(nil), 0, minio.PutObjectOptions{})

	if err != nil {
		return fmt.Errorf("s3: MakeDir %q（占位对象）: %w", relPath, err)
	}

	return nil
}

// joinRel 拼接目录相对路径与子名（正斜杠）。

func joinRel(dir, name string) string {
	if dir == "" || dir == "/" {
		return name
	}

	return strings.TrimSuffix(dir, "/") + "/" + name
}

// isNotFound 判定 S3 错误是否为不存在（对象/桶不存在）。

func isNotFound(err error) bool {
	if err == nil {
		return false
	}

	var respErr minio.ErrorResponse

	if asErr, ok := err.(minio.ErrorResponse); ok {
		respErr = asErr
	} else if ok2, ok3 := err.(*minio.ErrorResponse); ok2 != nil && ok3 {
		respErr = *ok2
	}

	if respErr.Code == "NoSuchKey" || respErr.Code == "NoSuchBucket" || respErr.Code == "NotFound" {
		return true
	}

	// minio 对 StatObject 不存在常返回 "The specified key does not exist."（NoSuchKey）。

	return strings.Contains(err.Error(), "NoSuchKey") || strings.Contains(err.Error(), "NoSuchBucket")
}

// _ 编译期断言：S3FS 实现 sync.FS。

var _ sync.FS = (*S3FS)(nil)

var _ = time.Now // 保留 time import（MTime 用 UnixNano 已用；防未来裁剪）
