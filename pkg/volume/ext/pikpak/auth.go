// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// AuthStatus 是 pikpak auth status 的解析结果。
type AuthStatus struct {
	LoggedIn bool   `json:"logged_in"`
	UserID   string `json:"user_id"`
	Name     string `json:"name"`
	Email    string `json:"email"`
}

// DeviceCode 是 OAuth device 授权流程的响应（auth login -F json）。
type DeviceCode struct {
	Flow            string `json:"flow"`
	DeviceCode      string `json:"device_code"`
	VerificationURI string `json:"verification_uri"`
	UserCode        string `json:"user_code"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// Auth 管理 CLI 登录态（OAuth device 授权）。
type Auth struct {
	cli *Cli
	log *slog.Logger
}

// NewAuth 创建 Auth。
func NewAuth(cli *Cli) *Auth {
	return &Auth{cli: cli, log: cli.log}
}

// Status 查询当前登录状态。
func (a *Auth) Status(ctx context.Context) (*AuthStatus, error) {
	var st AuthStatus
	if err := a.cli.RunJSON(ctx, &st, "auth", "status"); err != nil {
		return nil, err
	}
	return &st, nil
}

// LoggedIn 判断是否已登录。
func (a *Auth) LoggedIn(ctx context.Context) (bool, error) {
	st, err := a.Status(ctx)
	if err != nil {
		return false, err
	}
	return st.LoggedIn, nil
}

// DeviceLoginStart 发起 OAuth device 授权，返回授权链接 + user_code。
// 调用方需展示链接/码让用户授权；随后调用 DeviceLoginPoll 轮询完成。
func (a *Auth) DeviceLoginStart(ctx context.Context, label string) (*DeviceCode, error) {
	args := []string{"auth", "login", "-m", "device", "--no-launch-browser"}
	if label != "" {
		args = append(args, "-l", label)
	}
	var dc DeviceCode
	if err := a.cli.RunJSON(ctx, &dc, args...); err != nil {
		return nil, err
	}
	a.log.Info("pikpak device login started",
		"verification_uri", dc.VerificationURI, "user_code", dc.UserCode, "expires_in", dc.ExpiresIn)
	return &dc, nil
}

// DeviceLoginPoll 轮询 device 授权结果直到成功/超时。
func (a *Auth) DeviceLoginPoll(ctx context.Context, dc *DeviceCode, timeout time.Duration) error {
	// CLI 的 auth login 本身在授权完成后返回；这里用「轮询 auth status」实现：
	// 发起一次新的 device login（会阻塞直到用户授权或过期），
	// 或采用轻量轮询：间隔 interval 秒调 auth status 直到 logged_in。
	deadline := time.Now().Add(timeout)
	interval := time.Duration(dc.Interval) * time.Second
	if interval < time.Second {
		interval = 2 * time.Second
	}
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("pikpak device login timed out after %s", timeout)
		}
		ok, err := a.LoggedIn(ctx)
		if err == nil && ok {
			return nil
		}
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
