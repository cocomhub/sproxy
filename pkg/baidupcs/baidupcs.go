// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package baidupcs 提供百度网盘存储后端（独立 go.mod 隔离依赖）。
//
// 核心库从 BaiduPCS-Go（Apache-2.0，https://github.com/qjfoidnh/BaiduPCS-Go）
// fork 裁剪到 internal/（仅 API 客户端/上传/下载，裁 CLI/配置/更新/展示层），
// 依赖不污染 sproxy 主 module。
//
// 执行策略（参考 cocom 的接入经验）：**二进制优先 + 库兜底**——上传/下载优先调用
// BaiduPCS-Go 二进制（命令语义稳定、内置完整传输器），二进制缺失/失败时回退到
// internal 库的裸 API（PrepareUpload/DownloadFile）。
package baidupcs

import (
	"fmt"
	"path"
	"strings"

	pcsapi "github.com/cocomhub/sproxy/pkg/baidupcs/internal"
)

// ClientConfig 是百度网盘客户端配置。
type ClientConfig struct {
	// BDUSS 是百度登录态 cookie（必填）。
	BDUSS string
	// SToken 是网盘页面的 STOKEN（可选，部分接口需要）。
	SToken string
	// AppID 是百度网盘应用 ID；0 = 默认 266719。
	AppID int
	// UID 是百度用户 UID（可选，注入后 locatedownload 签名可用；
	// 不注入时 locatedownload 会 fail-closed 报错，直链下载不受影响）。
	UID uint64
}

// Client 是百度网盘 API 客户端薄封装。
type Client struct {
	cfg ClientConfig
	pcs *pcsapi.BaiduPCS
}

// NewClient 创建百度网盘客户端（BDUSS 必填）。
func NewClient(cfg ClientConfig) (*Client, error) {
	if strings.TrimSpace(cfg.BDUSS) == "" {
		return nil, fmt.Errorf("baidupcs: bduss is required")
	}
	appID := cfg.AppID
	if appID == 0 {
		appID = 266719
	}
	pcs := pcsapi.NewPCS(appID, cfg.BDUSS)
	if cfg.SToken != "" {
		pcs.SetStoken(cfg.SToken)
	}
	if cfg.UID != 0 {
		pcs.SetUID(cfg.UID)
	}
	return &Client{cfg: cfg, pcs: pcs}, nil
}

// PCS 返回内部 API 客户端（供 Adapter 使用）。
func (c *Client) PCS() *pcsapi.BaiduPCS {
	return c.pcs
}

// sanitizeRemotePath 归一化网盘路径：空 → "/"；不以 / 开头则补；结尾 / 去除；
// 拒绝 .. 穿越段、反斜杠、Windows 盘符形状（fail-closed）。
func sanitizeRemotePath(p string) (string, error) {
	if p == "" {
		return "/", nil
	}
	if strings.ContainsAny(p, `\`) {
		return "", fmt.Errorf("baidupcs: 路径含反斜杠: %q", p)
	}
	if strings.Contains(p, ":") && len(p) >= 2 && p[1] == ':' {
		return "", fmt.Errorf("baidupcs: 拒绝盘符形状路径: %q", p)
	}
	clean := path.Clean(p)
	// path.Clean 会消解 .. 段（/a/../b → /b），但这正是我们要拒绝的穿越形态：
	// 先检查原始路径是否含 .. 段（fail-closed，拒绝任何 .. 输入）。
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("baidupcs: 拒绝路径穿越段: %q", p)
		}
	}
	if !strings.HasPrefix(clean, "/") {
		clean = "/" + clean
	}
	return clean, nil
}
