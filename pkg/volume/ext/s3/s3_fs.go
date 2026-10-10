// Copyright 2026 The Cocomhub Authors. All rights reserved.

// SPDX-License-Identifier: Apache-2.0

// Package s3 提供 S3 对象存储后端客户端：把任意 S3 兼容服务（AWS S3 / MinIO /

// COS / OSS，ListObjectsV2/GetObject/PutObject/CopyObject/RemoveObject）适配为

// `pkg/sync.FS`（7 方法），作为 sproxy 外部卷（V3 通用卷模型，RegisterBackend("s3")

// 接入系统盘/用户卷）。

//

// 独立 Go module（go.mod 隔离）：minio-go 第三方 SDK 依赖隔离在本 module，主仓

// 不直接依赖（仿 pkg/volume/ext/baidupcs 模式，用户明示 2026-09-18）。

//

// S3 无目录纯概念：目录 = 前缀（key + "/"）。MakeDir 创建零字节占位对象；ListDir

// 用 delimiter="/" 单层列举（CommonPrefixes 目录 + Contents 文件）；Stat 目录探测

// 占位对象。

package s3

import (
	"bytes"

	"context"

	"errors"

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

		if e, ok := listEntryFromObject(prefix, relPath, obj, seen); ok {
			out = append(out, e)
		}
	}

	return out, nil
}

// listEntryFromObject 把单个 ListObjects 对象规整为 sync.Entry（目录占位对象 / 文件）。
// 返回 (entry, ok)；ok=false 表示跳过（自身目录 / 重复名）。
func listEntryFromObject(prefix, relPath string, obj minio.ObjectInfo, seen map[string]bool) (sync.Entry, bool) {
	// 目录（CommonPrefixes）：key = prefix + name + "/"。

	if obj.Key != "" && strings.HasSuffix(obj.Key, "/") {
		name := strings.TrimSuffix(strings.TrimPrefix(obj.Key, prefix), "/")

		if name == "" || seen[name] {
			return sync.Entry{}, false // 自身目录 / 重复
		}

		seen[name] = true

		return sync.Entry{
			Name: name,

			Path: joinRel(relPath, name),

			IsDir: true,
		}, true
	}

	// 文件（Contents）：key = prefix + name。

	name := strings.TrimPrefix(obj.Key, prefix)

	if name == "" || seen[name] {
		return sync.Entry{}, false
	}

	seen[name] = true

	return sync.Entry{
		Name: name,

		Path: joinRel(relPath, name),

		Size: obj.Size,

		MTime: obj.LastModified.UnixNano(),

		IsDir: false,
	}, true
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
		e := &sync.Entry{
			Name: path.Base(clean),

			Path: clean,

			Size: info.Size,

			MTime: info.LastModified.UnixNano(),

			IsDir: false,
		}
		// S3 校验和信息（用户裁定：每个 entry 提供已知的所有校验和数据）：
		// ETag 是服务端对象标识（单 PUT = 内容 MD5；分片 = 片 MD5 组合，非权威整文件
		// 哈希）——作为 "etag" 算法值提供，供 Equal 按可用算法比对；无 sha256（S3
		// 不返回整文件内容哈希）。
		if info.ETag != "" {
			e.Checksum = info.ETag
			e.ChecksumType = "etag"
			e.Checksums = map[string]string{"etag": info.ETag}
		}
		return e, nil
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
	// size<0（未知长度）：透传给 minio（其内部走 putObjectMultipartStreamNoLength 流式读
	// 到 EOF）。**不得钳为 0**——那会声明 0 字节却带非空 body（数据丢失/语义冲突）。

	mo := multipartOptsFromConfig(f.cfg)
	key := f.keyFor(relPath)
	if shouldMultipart(size, mo.threshold) {
		// 大文件 multipart：透传 PartSize（已归一 >=5MiB）；minio 自动分片 + 失败 Abort。
		if err := putObjectWithRetry(ctx, f.putter, uploadParams{
			bucket:  f.bucket,
			key:     key,
			opts:    minio.PutObjectOptions{PartSize: uint64(mo.partSize)},
			retries: mo.retries,
		}, r, size); err != nil {
			return fmt.Errorf("s3: PutObject(分片) %q: %w", relPath, err)
		}
		return nil
	}

	// 小文件：单 PutObject（minio <16MiB 单原子 PUT）零回归。
	if err := putObjectWithRetry(ctx, f.putter, uploadParams{
		bucket:  f.bucket,
		key:     key,
		opts:    minio.PutObjectOptions{},
		retries: mo.retries,
	}, r, size); err != nil {
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
// m8 修复：errors.As 沿包装链找 *minio.ErrorResponse（任一层 %w 包装仍可判定）；
// 未命中时回退 minio.ToErrorResponse（minio 内部从 resp 解析，处理错误链语义），最后
// 字符串兜底。原实现用精确类型断言（err.(*minio.ErrorResponse)），任一 %w 包装后
// 断言失败 → 把「不存在」误判为真失败（Stat 重试循环烧请求）。
func isNotFound(err error) bool {
	if err == nil {
		return false
	}

	var target *minio.ErrorResponse
	if errors.As(err, &target) && target != nil {
		return isNotFoundCode(target.Code) || notFoundText(err)
	}
	// 非 *ErrorResponse 链：minio.ToErrorResponse 提取（包装内部错误时仍能解出 Code）。
	parsed := minio.ToErrorResponse(err)
	return isNotFoundCode(parsed.Code) || notFoundText(err)
}

// isNotFoundCode 判定错误码是否为「不存在」族。
func isNotFoundCode(code string) bool {
	return code == "NoSuchKey" || code == "NoSuchBucket" || code == "NotFound"
}

// notFoundText 字符串兜底（minio 对 StatObject 不存在常返回
// "The specified key does not exist."，Code 解析可能为空）。
// MINOR 修复：补 minio 字面 "The specified key does not exist." 与 "does not exist"——
// 原只查 NoSuchKey/NoSuchBucket 子串，minio 纯文本错误（无 Code）时兜底失效。
func notFoundText(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "NoSuchKey") || strings.Contains(msg, "NoSuchBucket") ||
		strings.Contains(msg, "The specified key does not exist.")
}

// _ 编译期断言：S3FS 实现 sync.FS。
var _ sync.FS = (*S3FS)(nil)

// ExemptStagingQuota 报告 s3 无需本地 staging 配额（显式豁免——用户裁定 2026-10-10：
// 不需要 staging 或只占部分空间的卷须显式实现接口；s3 为流式直传：PutObject 单请求
// 直发、multipart 由 minio 内存/HTTP 直传，**无本地中间态落盘**——装配层探测到本接口
// 跳过 StagingQuotaGateFS 包装，不预留本地磁盘）。
func (f *S3FS) ExemptStagingQuota() bool { return true }

var _ = time.Now // 保留 time import（MTime 用 UnixNano 已用；防未来裁剪）
