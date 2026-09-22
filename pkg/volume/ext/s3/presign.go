// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

// PresignedURL 生成对象的预签名 URL（SigV4）：PUT 直传 / GET 下载。
// 用于服务端签发 → 客户端/浏览器直传 S3（roadmap 3.3 签名 v4 直传）。
// expires ≤ 0 时用默认 1h；对象路径经 keyFor 归一（卷根前缀）。
func (f *S3FS) PresignedURL(ctx context.Context, relPath, method string, expires time.Duration) (string, error) {
	if expires <= 0 {
		expires = time.Hour
	}
	key := f.keyFor(relPath)
	switch method {
	case "PUT", "GET":
		// Presign 是纯本地签名（不探测 region——PresignedPutObject 会 getBucketLocation 发请求）。
		u, err := f.client.Presign(ctx, method, f.bucket, key, expires, url.Values{})
		if err != nil {
			return "", fmt.Errorf("s3: presign %s %q: %w", method, relPath, err)
		}
		return u.String(), nil
	default:
		return "", fmt.Errorf("s3: 不支持的 presign method %q（PUT/GET）", method)
	}
}
