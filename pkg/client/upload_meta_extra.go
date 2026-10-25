// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

// upload_meta_extra.go 客户端侧上传元数据（方案①）：
//   - UploadOption.WithMetaExtra(map) → 上传头 X-File-Meta-Extra（base64(JSON)）
//   - UpdateFileMeta(ctx, filename, extra) → PUT /api/files/meta（方案②）
//
// 字段白名单/校验在服务端 pkg/files/metaextra；客户端只编码传输（不重复校验）。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"

	"github.com/cocomhub/sproxy/pkg/files/metaextra"
)

// uploadOptions 是上传的可选项集合。
type uploadOptions struct {
	MetaExtra map[string]any
}

// UploadOption 是上传选项（函数式）。
type UploadOption func(*uploadOptions)

// WithMetaExtra 指定随上传写入 .meta sidecar 的自定义元数据（A/B/C 档，服务端校验）。
func WithMetaExtra(extra map[string]any) UploadOption {
	return func(o *uploadOptions) { o.MetaExtra = extra }
}

// headerFileMetaExtra 与 pkg/files 一致的头名。
const headerFileMetaExtra = "X-File-Meta-Extra"

// encodeMetaExtraHeader 序列化 extra → base64(JSON)（HTTP 头仅可见 ASCII）。
// 空/无字段 → 空串（不设头）。
func encodeMetaExtraHeader(extra map[string]any) (string, error) {
	extra = metaextra.Trim(extra)
	if len(extra) == 0 {
		return "", nil
	}
	b, err := json.Marshal(extra)
	if err != nil {
		return "", fmt.Errorf("meta-extra 序列化失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// UpdateFileMeta 补录已上传文件的元数据（方案②：PUT /api/files/meta）。
// extra 字段须过白名单（服务端校验；本地 fail-fast 校验同样执行，减少无效请求）。
func (c *FileClient) UpdateFileMeta(ctx context.Context, filename string, extra map[string]any) error {
	extra = metaextra.Trim(extra)
	if err := metaextra.Validate(extra); err != nil {
		return err
	}
	body := fmt.Sprintf(`{"extra":%s}`, mustJSON(extra))
	hdr := make(http.Header)
	hdr.Set("Content-Type", "application/json")
	resp, err := c.doRequest(ctx, http.MethodPut, "/api/files/meta?filename="+urlQueryEscape(filename), strings.NewReader(body), hdr)
	if err != nil {
		return fmt.Errorf("更新 meta 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("更新 meta 失败 (HTTP %d)", resp.StatusCode)
	}
	return nil
}

// mustJSON 把对象序列化为 JSON（已过 Validate，序列化必成功）。
func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// urlQueryEscape 转义 query 参数。
func urlQueryEscape(s string) string { return neturl.QueryEscape(s) }
