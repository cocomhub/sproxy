// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/contextcfg"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// ---- trust register（TOTP 注册）----

// newCmdTrustRegister 创建 trust register 命令：向服务端注册新 TOTP 账号，打印
// AK 与 TOTP 密钥（base32 / otpauth_uri），提示录入 Authenticator 后用
// `trust login <用户名>` 完成绑定。不读动态码、不登录、不回填 session 凭据。
//
// 与 trust login 的边界：register 只负责「创建账号 + 展示密钥」（无 SK 条目，
// DEC-B pending 确认制）；login 只负责「用动态码登录拿 session SK 回填」。用户先
// register 拿到密钥录入 GA，再 login 完成绑定——登录命令不再隐式注册。
//
// context 语义（设计 §4.2）：register 作用于当前 context 的 environment（从
// config.yaml 解析 server_url），注册成功后**自动切当前 context 的 user 到新用户**
// 并回填 access_key（无 session SK 故 secret/id 待登录后回填）。
//
// RPC 用显式无凭据客户端（M14，同 trust login）：公开端点直达，不带签名头。
func newCmdTrustRegister(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider, cfgFile *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "register [username]",
		Short: "注册新 TOTP 账号（打印 AK 与 TOTP 密钥；随后 trust login 完成绑定）",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			username := ""
			if len(args) == 1 {
				username = args[0]
			}
			return runTrustRegister(cmd.Context(), cmd, ios, cfgSvc, cfgFile, username)
		},
	}
	return cmd
}

// runTrustRegister 执行 trust register 全流程。
//
// 双模式：config.yaml（context 模型）存在且解析成功 → 用当前 context 的 env 注册 +
// 自动切 user + 回填到 Users 段；config.yaml 不存在 → 回落旧平铺路径（既有语义，
// 零破坏——回填 access_key 到平铺配置）。
func runTrustRegister(ctx context.Context, cmd *cobra.Command, ios cli.IOStreams, cfgSvc ConfigProvider, cfgFile *string, owner string) error {
	serverURL, contextMode := trustContextServerURL(cfgFile, ios)
	if contextMode {
		// context 模式：server_url 来自当前 context 的 environment。
	} else {
		cfg, err := loadTrustLoginConfig(cfgSvc)
		if err != nil {
			ios.WriteErrLine("加载配置失败: %v", err)
			return fmt.Errorf("加载配置失败: %w", err)
		}
		if cfg.ServerURL == "" {
			ios.WriteErrLine("未配置 server_url（无法连接 TOTP 服务端）")
			return fmt.Errorf("未配置 server_url（无法连接 TOTP 服务端）")
		}
		serverURL = cfg.ServerURL
	}

	noAuthOpts := []client.Option{client.WithSendNoAuth(true)}
	caFlag, insecureOn2 := trustLoginTLSFlags(cmd)
	if caFlag != "" && insecureOn2 {
		return fmt.Errorf("--ca-file 与 --insecure 互斥，不能同时使用")
	}
	if caFlag != "" {
		noAuthOpts = append(noAuthOpts, client.WithCAFile(caFlag))
	} else if insecureOn2 {
		noAuthOpts = append(noAuthOpts, client.WithInsecureTLS())
	}
	noAuth := client.NewFileClient(serverURL, noAuthOpts...)

	res, rerr := noAuth.RegisterTOTP(ctx, owner)
	if rerr != nil {
		ios.WriteErrLine("注册失败: %v", rerr)
		return fmt.Errorf("注册失败: %w", rerr)
	}
	fmt.Fprintf(ios.Out, "注册成功: ak=%s owner=%s\n", res.AK, res.Owner)
	// base32_secret 属凭据（S49）：唯一一次展示，不落日志。
	fmt.Fprintf(ios.Out, "请在 Authenticator 中录入以下密钥（只展示这一次）:\n  base32: %s\n  otpauth: %s\n",
		res.Base32Secret, res.OTPAuthURI)
	if res.Admin {
		// S2：首个注册用户（服务端原子授予 admin）。
		fmt.Fprintln(ios.Out, "您是首个注册用户，将成为 admin")
	}
	fmt.Fprintln(ios.Out, "录入完成后请运行 `trust login "+res.Owner+"`（或 --ak "+res.AK+"）完成登录绑定")

	if contextMode {
		// context 模式回填：Users 增/更新新用户（名=owner，ak 回填、secret/id 待登录）
		// + 当前 context 的 user 切到该用户（自动切换，设计 §4.2）→ Save config.yaml。
		if *cfgFile == "" {
			return fmt.Errorf("配置文件路径为空，无法回填注册的 access_key")
		}
		cfg, cerr := contextcfg.Load(*cfgFile)
		if cerr != nil {
			ios.WriteErrLine("加载 context 配置失败: %v", cerr)
			return fmt.Errorf("加载 context 配置失败: %w", cerr)
		}
		userName := res.Owner
		if userName == "" {
			userName = res.AK
		}
		u := cfg.FindUser(userName)
		if u == nil {
			cfg.Users = append(cfg.Users, &contextcfg.User{Name: userName})
			u = cfg.FindUser(userName)
		}
		u.AccessKey = res.AK
		u.Owner = res.Owner
		// 当前 context（current-context）的 user 自动切到新用户。
		cur := cfg.CurrentContext
		if cur != "" {
			if ctx := cfg.FindContext(cur); ctx != nil {
				ctx.User = userName
			}
		}
		if err := contextcfg.Save(cfg, *cfgFile); err != nil {
			ios.WriteErrLine("保存 context 配置失败: %v", err)
			return fmt.Errorf("保存 context 配置失败: %w", err)
		}
		fmt.Fprintf(ios.Out, "已回填 access_key=%s（context 用户已切换为 %s；待登录成功后回填 access_key_secret）\n",
			res.AK, userName)
		return nil
	}

	// 平铺模式回填：回填 access_key（供后续 `trust login` 默认使用；无 session SK
	// 故 secret/id 不回填——登录成功后才写入）。沿用 trust renew 的 SaveConfig 模式。
	if *cfgFile == "" {
		// 防御：config 写入路径必须存在（生产由 root.go 生成默认路径）。
		return fmt.Errorf("配置文件路径为空，无法回填注册的 access_key")
	}
	cfg, cerr := loadTrustLoginConfig(cfgSvc)
	if cerr != nil {
		ios.WriteErrLine("加载配置失败: %v", cerr)
		return fmt.Errorf("加载配置失败: %w", cerr)
	}
	cfg.AccessKey = res.AK
	if err := client.SaveConfig(cfg, *cfgFile); err != nil {
		ios.WriteErrLine("保存配置失败: %v", err)
		return fmt.Errorf("保存配置失败: %w", err)
	}
	fmt.Fprintf(ios.Out, "已回填 access_key=%s（待登录成功后回填 access_key_secret）\n", res.AK)
	return nil
}
