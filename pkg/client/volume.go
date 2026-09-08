// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// volume.go 提供 FileClient 的卷上下文支持（多卷服务端；单卷/无卷服务端零回归）。
//
// 设计（零值 = auto）：
//   - FileClient.Volume 是可选的「卷上下文」，零值（空串）表示 auto——请求不携带 volume 参数，
//     服务端按默认路由 / 跨卷定位（与旧版行为完全一致）；
//   - 非空时，upload/download/list/stat/delete/rename/分块路径在请求中附加 volume 参数，
//     把操作限定到指定卷（显式深入能力，向后兼容）。
//   - Volumes() / MoveVolume() 分别对应服务端 GET /api/volumes 与 POST /api/volumes/move。
package client

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// VolumeInfo 是 GET /api/volumes 返回的单个卷描述（仅 owner 允许卷；allowed 恒 true）。
type VolumeInfo struct {
	Name     string `json:"name"`
	Mode     string `json:"mode"`     // 卷 ACL 模式（allow|deny）
	Capacity int64  `json:"capacity"` // 卷容量上限（0 = 不限）
	Usage    int64  `json:"usage"`    // 卷容量池当前已用字节
	Allowed  bool   `json:"allowed"`  // owner 是否允许使用
}

// WithVolume 设置 FileClient 的默认卷上下文（空 = auto，请求不携带 volume 参数）。
// 多卷服务端下可用它把后续操作限定到指定卷；单卷服务端忽略该上下文（无卷语义）。
func WithVolume(name string) Option {
	return func(c *FileClient) {
		c.volume = name
	}
}

// Volume 返回当前卷上下文（空 = auto）。
func (c *FileClient) Volume() string { return c.volume }

// SetVolume 修改 FileClient 的卷上下文（空 = auto）。
//
// 适用场景：同一客户端在多次请求间切换卷（如 mv 跨卷两步走）；调用方须自行保证
// 不在并发请求中途修改该值。缺省用法（NewFileClient + WithVolume 构造期设定）无需本方法。
func (c *FileClient) SetVolume(name string) { c.volume = name }

// appendVolumeQuery 把卷上下文作为 volume 参数写入 q（空上下文不写入——auto）。
func (c *FileClient) appendVolumeQuery(q url.Values) {
	if c.volume != "" {
		q.Set("volume", c.volume)
	}
}

// volumeQueryPart 返回可拼接到 URL 末尾的卷参数段（&volume=<name>）；空上下文返回 ""。
func (c *FileClient) volumeQueryPart() string {
	if c.volume == "" {
		return ""
	}
	return "&volume=" + url.QueryEscape(c.volume)
}

// Volumes 获取当前 owner 可见的卷列表（GET /api/volumes）。
// 服务端按 owner ACL 过滤，列表内卷 owner 均有权限；卷未装配（无卷语义）返回空列表。
func (c *FileClient) Volumes(ctx context.Context) ([]VolumeInfo, error) {
	var out struct {
		Volumes []VolumeInfo `json:"volumes"`
	}
	if err := c.doJSON(ctx, "GET", "/api/volumes", nil, &out); err != nil {
		return nil, fmt.Errorf("获取卷列表失败: %w", err)
	}
	return out.Volumes, nil
}

// MoveVolume 把 filename（同 owner 同相对路径）从 fromVol 跨卷迁移到 toVol
// （POST /api/volumes/move）。fromVol == toVol 时服务端按无操作成功（幂等）。
// 目标卷已存在同 rel、卷不在 owner 视图、配额不足等由服务端返回相应错误。
func (c *FileClient) MoveVolume(ctx context.Context, fromVol, toVol, filename string) error {
	if fromVol == "" || toVol == "" || filename == "" {
		return fmt.Errorf("from_volume、to_volume、filename 均不能为空")
	}
	q := url.Values{}
	q.Set("from_volume", fromVol)
	q.Set("to_volume", toVol)
	q.Set("filename", filename)
	var result UploadResult
	if err := c.doJSON(ctx, "POST", "/api/volumes/move?"+q.Encode(), nil, &result); err != nil {
		return fmt.Errorf("跨卷移动失败: %w", err)
	}
	return nil
}

// VolumeOf 返回 filename 当前所在卷名（多卷聚合视图；owner 可见卷内定位）。
// 判定方式：列出 filename 的父目录，按精确文件名匹配条目并读取其 Volume 字段。
// 找不到条目返回空串与 ErrNotFound；单卷/旧装配路径条目无 Volume 字段时返回空串（无卷语义）。
//
// 用途：mv --to-volume 未显式给源卷时确定文件 home 卷（跨卷移动前写前定位）。
func (c *FileClient) VolumeOf(ctx context.Context, filename string) (string, error) {
	if filename == "" {
		return "", fmt.Errorf("filename 不能为空")
	}
	if containsPathTraversal(filename) {
		return "", fmt.Errorf("文件名不能包含路径穿越符 '..'")
	}
	clean := path.Clean("/" + strings.ReplaceAll(filename, "\\", "/"))
	dir := path.Dir(clean)
	base := path.Base(clean)
	if base == "." || base == "/" || base == "" {
		return "", fmt.Errorf("文件名非法: %s", filename)
	}
	var subdirs []string
	if rel := strings.TrimPrefix(dir, "/"); rel != "" {
		subdirs = strings.Split(rel, "/")
	}
	files, err := c.List(ctx, subdirs...)
	if err != nil {
		return "", err
	}
	for i := range files {
		if !files[i].IsDir && files[i].Name == base {
			return files[i].Volume, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrNotFound, filename)
}
