// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package totp 是服务端渲染 QR 等三方 TOTP/QR Go 依赖的唯一落点
// （独立子 module，主 go.mod 零新增，spec §10/D7）。
//
// Go 标准库 TOTP（pkg/otp，4B-2）在主包，不进本 module。本包为骨架：未接入任何三方
// 依赖，默认实现返回哨兵错误 ErrNotConfigured（fail-closed，非占位/panic）；未来接入
// 三方库（如 skip2/go-qrcode）只加本 module 的 go.mod，并在 init 替换注册的 Provider。
package totp

import (
	"errors"

	accesskey "github.com/cocomhub/sproxy/pkg/accesskey"
)

// ErrNotConfigured 表示未配置三方 TOTP/QR 依赖（骨架默认态，接入前不可用）。
// 作为哨兵错误供调用方 errors.Is 判定（fail-closed）。
var ErrNotConfigured = errors.New("totp: 未配置三方 TOTP/QR 依赖（ext/auth/totp 为骨架）")

// Provider 是服务端 QR 渲染提供者接口（宿主经 accesskey 注册表按名装配；三方接入后
// 以真实实现替换默认骨架）。
type Provider interface {
	// Render 把 content（如 otpauth:// 密钥 URI）渲染成二维码图片字节（PNG）。
	Render(content string) ([]byte, error)
}

// 编译期断言：defaultProvider 满足 Provider。
var _ Provider = defaultProvider{}

// defaultProvider 是未接入三方依赖的默认实现：fail-closed 返回 ErrNotConfigured。
type defaultProvider struct{}

// Render 返回 ErrNotConfigured（骨架未接入三方库）。
func (defaultProvider) Render(string) ([]byte, error) {
	return nil, ErrNotConfigured
}

// ServerSideQR 渲染服务端 QR 图片。骨架未接入三方库 → 返回 ErrNotConfigured。
// 接入三方库后的调用方应经注册表取值（Provider），本函数保留为无装配直调的兜底入口。
func ServerSideQR(_ string) ([]byte, error) {
	return nil, ErrNotConfigured
}

// init 注册默认（未配置）骨架到 accesskey 注册表（名字 "totp-qr"）：host 空导入即
// 装配，经 GetStorer[Provider]("totp-qr") 探测/调用。重名注册仅在同进程重复空导入时
// 发生——不可能，故忽略 error（骨架非 panic）。
func init() {
	_ = accesskey.RegisterStorer("totp-qr", defaultProvider{})
}
