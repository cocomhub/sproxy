// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

// upload_meta_extra.go 客户端侧上传元数据（方案①）：
//   - UploadOption.WithMetaExtra(map) → 上传头 X-File-Meta-Extra（base64(JSON)）
//   - UpdateFileMeta(ctx, filename, extra) → PUT /api/files/meta（方案②）
//
// 字段白名单/校验在服务端 pkg/files/meta（metaextra 已并入）；客户端只编码传输（不重复校验）。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"

	"github.com/cocomhub/sproxy/pkg/files/meta"
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
	extra = meta.TrimExtra(extra)
	if len(extra) == 0 {
		return "", nil
	}
	b, err := json.Marshal(extra)
	if err != nil {
		return "", fmt.Errorf("meta-extra 序列化失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// MetaUpdateMode 是 meta extra 更新模式（用户裁定 2026-10-26：默认**新增而非覆盖**）。
type MetaUpdateMode string

const (
	// MetaUpdateMerge 合并（同名键更新，其余保留）——**默认**。
	MetaUpdateMerge MetaUpdateMode = "merge"
	// MetaUpdateReplace 整表替换（丢弃旧键）——显式覆盖。
	MetaUpdateReplace MetaUpdateMode = "replace"
	// MetaUpdateDelete 删除指定键（extra 的 key 集合，value 忽略）。
	MetaUpdateDelete MetaUpdateMode = "delete"
)

// MetaUpdateOptions 是 UpdateFileMetaWithMode 的可选项。
type MetaUpdateOptions struct {
	// Mode 是更新模式（空 = MetaUpdateMerge）。
	Mode MetaUpdateMode
}

// UpdateFileMeta 补录已上传文件的元数据（**默认合并**：新增而非覆盖）。
// extra 字段须过白名单（服务端校验；本地 fail-fast 校验同样执行，减少无效请求）。
func (c *FileClient) UpdateFileMeta(ctx context.Context, filename string, extra map[string]any) error {
	return c.UpdateFileMetaWithMode(ctx, filename, extra, MetaUpdateOptions{})
}

// UpdateFileMetaWithMode 按指定模式补录（merge 默认 / replace 整表替换 / delete 删键）。
func (c *FileClient) UpdateFileMetaWithMode(ctx context.Context, filename string, extra map[string]any, opts MetaUpdateOptions) error {
	extra = meta.TrimExtra(extra)
	mode := opts.Mode
	if mode == "" {
		mode = MetaUpdateMerge
	}
	// delete 模式只取 key 集合（value 忽略）；merge/replace 走完整白名单校验。
	if mode != MetaUpdateDelete {
		if err := meta.ValidateExtra(extra); err != nil {
			return err
		}
	}
	if len(extra) == 0 {
		return fmt.Errorf("extra 不能为空")
	}
	payload := fmt.Sprintf(`{"extra":%s,"mode":%q}`, mustJSON(extra), string(mode))
	hdr := make(http.Header)
	hdr.Set("Content-Type", "application/json")
	q := neturl.Values{"filename": {filename}}
	c.appendVolumeQuery(q)
	resp, err := c.doRequest(ctx, http.MethodPut, "/api/files/meta?"+q.Encode(), strings.NewReader(payload), hdr)
	if err != nil {
		return fmt.Errorf("更新 meta 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("更新 meta 失败 (HTTP %d)", resp.StatusCode)
	}
	return nil
}

// GetFileMeta 读取已上传文件的元数据 extra（GET /api/files/meta）。
// 文件存在但无 extra → 空 map（非错误）；文件不存在 → ErrNotFound。
func (c *FileClient) GetFileMeta(ctx context.Context, filename string) (map[string]any, error) {
	q := neturl.Values{"filename": {filename}}
	c.appendVolumeQuery(q)
	resp, err := c.doRequest(ctx, http.MethodGet, "/api/files/meta?"+q.Encode(), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("读取 meta 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, filename)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("读取 meta 失败 (HTTP %d)", resp.StatusCode)
	}
	var out struct {
		Extra map[string]any `json:"extra"`
	}
	if derr := json.NewDecoder(resp.Body).Decode(&out); derr != nil {
		return nil, fmt.Errorf("解析 meta 响应失败: %w", derr)
	}
	if out.Extra == nil {
		return map[string]any{}, nil
	}
	return out.Extra, nil
}

// mustJSON 把对象序列化为 JSON（已过 Validate，序列化必成功）。
func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
