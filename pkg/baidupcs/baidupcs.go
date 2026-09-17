// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package baidupcs 是百度网盘（BaiduPCS）存储后端的薄适配层。
//
// 方案（R2）：本包是**独立 Go module**（见本目录 go.mod），只含自有代码——
// Adapter 双路径（二进制优先 + 库兜底）、Storage 接口、plugin 注册。
// 核心库直接引用外部 fork github.com/cocomhub/BaiduPCS-Go（经 replace 接入，
// module 声明保持 qjfoidnh/BaiduPCS-Go），不复制开源实现进本仓库，避免受
// sproxy addlicense/lint 等强校验污染。
//
// 执行策略（cocom 验证模式）：默认调 BaiduPCS-Go 二进制（命令语义稳定、子进程
// 隔离、完整传输器内置、可独立升级），二进制缺失/失败/超时时回退 fork 库实现。
package baidupcs

import (
	"fmt"
	"path"
	"strings"

	bdlib "github.com/qjfoidnh/BaiduPCS-Go/baidupcs"
	"github.com/qjfoidnh/BaiduPCS-Go/requester"
)

// defaultAppID 是 BaiduPCS 默认应用 ID（与上游一致）。
const defaultAppID = 266719

// Client 是百度网盘 API 客户端的薄封装（持有 fork 的 BaiduPCS）。
type Client struct {
	pcs *bdlib.BaiduPCS
}

// NewClient 用 BDUSS（+ 可选 SToken）构造百度网盘客户端。
// BDUSS 为空返回错误；SToken 非空时设置。
func NewClient(bduss, stoken string) (*Client, error) {
	if strings.TrimSpace(bduss) == "" {
		return nil, fmt.Errorf("baidupcs: bduss is required")
	}
	pcs := bdlib.NewPCS(defaultAppID, bduss)
	if stoken != "" {
		pcs.SetStoken(stoken)
	}
	pcs.SetHTTPS(true)
	pcs.GetClient().SetUserAgent(requester.UserAgent)
	return &Client{pcs: pcs}, nil
}

// PCS 返回底层 BaiduPCS 实例（供 Adapter 使用）。
func (c *Client) PCS() *bdlib.BaiduPCS { return c.pcs }

// sanitizeRemotePath 把用户路径归一为网盘绝对路径：
//   - 补前导斜杠（a/b → /a/b）
//   - 去尾斜杠（/a/b/ → /a/b；根 → /）
//   - 拒绝 .. 路径穿越
func sanitizeRemotePath(p string) (string, error) {
	if p == "" || p == "/" {
		return "/", nil
	}
	clean := path.Clean("/" + p)
	if clean == "/" {
		return "/", nil
	}
	// path.Clean 会把 a/../b 归为 /b，这里显式拒绝含 .. 的输入。
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("baidupcs: invalid path %q: .. not allowed", p)
		}
	}
	return clean, nil
}
