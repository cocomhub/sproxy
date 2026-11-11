// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

// volume_inspect.go 是「真实存储审查」的客户端封装（GET /api/volumes/{name}/trace|explain）。
//
// 返回类型直接复用 pkg/volume 的对外模型：服务端与客户端**共用一份结构定义**，
// 避免两处 JSON 字段漂移（R54 同族考虑：契约类型只有一份）。

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// VolumeTrace 返回逻辑路径 rel 在指定卷上的真实落点（disk=true 时对比磁盘）。
func (c *FileClient) VolumeTrace(ctx context.Context, volName, rel string, disk bool) (*syncpkg.PathTrace, error) {
	if volName == "" {
		return nil, fmt.Errorf("卷名不能为空")
	}
	q := url.Values{}
	q.Set("path", rel)
	if disk {
		q.Set("disk", "1")
	}
	var out struct {
		Trace *syncpkg.PathTrace `json:"trace"`
	}
	endpoint := "/api/volumes/" + url.PathEscape(volName) + "/trace?" + q.Encode()
	if err := c.doJSON(ctx, "GET", endpoint, nil, &out); err != nil {
		return nil, fmt.Errorf("真实存储落点查询失败: %w", err)
	}
	if out.Trace == nil {
		return nil, fmt.Errorf("服务端返回空结果（该卷可能不支持审查）")
	}
	return out.Trace, nil
}

// VolumeTraceTree 返回逻辑路径的**递归落点树**（depth 层；depth≤1 等价 VolumeTrace）。
//
// 子项由服务端按索引展开（不逐节点 disk 探测）；超出上限会被截断并带 truncated 异常。
func (c *FileClient) VolumeTraceTree(ctx context.Context, volName, rel string, depth int, disk bool) (*syncpkg.PathTrace, error) {
	if volName == "" {
		return nil, fmt.Errorf("卷名不能为空")
	}
	q := url.Values{}
	q.Set("path", rel)
	if depth > 1 {
		q.Set("depth", strconv.Itoa(depth))
	}
	if disk {
		q.Set("disk", "1")
	}
	var out struct {
		Trace *syncpkg.PathTrace `json:"trace"`
	}
	endpoint := "/api/volumes/" + url.PathEscape(volName) + "/trace?" + q.Encode()
	if err := c.doJSON(ctx, "GET", endpoint, nil, &out); err != nil {
		return nil, fmt.Errorf("真实存储落点树查询失败: %w", err)
	}
	if out.Trace == nil {
		return nil, fmt.Errorf("服务端返回空结果（该卷可能不支持审查）")
	}
	return out.Trace, nil
}

// VolumeExplain 解释真实路径 realRel（卷根相对）的用途与逻辑归属。
func (c *FileClient) VolumeExplain(ctx context.Context, volName, realRel string, disk bool) (*syncpkg.RealExplanation, error) {
	if volName == "" {
		return nil, fmt.Errorf("卷名不能为空")
	}
	q := url.Values{}
	q.Set("real", realRel)
	if disk {
		q.Set("disk", "1")
	}
	var out struct {
		Explain *syncpkg.RealExplanation `json:"explain"`
	}
	endpoint := "/api/volumes/" + url.PathEscape(volName) + "/explain?" + q.Encode()
	if err := c.doJSON(ctx, "GET", endpoint, nil, &out); err != nil {
		return nil, fmt.Errorf("真实路径解释失败: %w", err)
	}
	if out.Explain == nil {
		return nil, fmt.Errorf("服务端返回空结果（该卷可能不支持审查）")
	}
	return out.Explain, nil
}
