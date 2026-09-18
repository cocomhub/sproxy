// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// ---- trust login（TOTP 登录）----

// errLoginNotConfirmed 是 trust login 在确认输入为空（stdin EOF / 管道空）时
// 返回的非零错误：无输入即中止，绝不把「未确认」当成功（M4——仿
// errDeleteAKNotConfirmed 语义）。
var errLoginNotConfirmed = errors.New("登录已中止：未收到确认输入")

// errLoginOverwriteDenied 是 trust login 在用户**拒绝覆盖现有凭据**（D4 覆盖确认
// 输入非 y/yes）时返回的非零错误，与 errLoginNotConfirmed 并列（语义区分：不是
// "没输入"，而是"用户明确拒绝覆盖"）。文案含动作指引：可用 --overwrite 跳过确认。
var errLoginOverwriteDenied = errors.New("已取消：拒绝覆盖现有凭据（如需新会话可用 --overwrite 跳过确认）")

// errLoginNoCreds 是 trust login 在既无本地 access_key 配置、也未指定
// 用户名/--ak 时返回的非零错误：登录命令不再隐式注册新账号（注册由独立子命令
// trust register 承担）——无凭据即中止并指引注册，绝不静默新建账号（M4 语义）。
var errLoginNoCreds = errors.New("未检测到凭据：请先运行 trust register 注册（或指定用户名/--ak）")

// newCmdTrustLogin 创建 trust login 命令：GA 密钥录入→登录→解 session SK 回填
// access_key 三件套。
//
// 流程（薄逻辑：flag + IO + 回填；领域逻辑全在 pkg/client TOTP 领域 API）：
//  1. 目标登录身份：位置参数 <username>（推荐，owner 反查 AK 免记 AK）> 配置
//     access_key > --ak <AK>。三者都无 → 报错指引 `trust register`（不静默注册
//     新账号，注册由独立子命令承担）。
//  2. 提示输入动态码 → RequestTOTPNonce → LoginTOTP(ak/owner, nonce, code, "cli")
//     （D3 cli 态）→ 解 session SK。
//  3. 回填前覆盖确认（D4）：cfg.AccessKeySecret 非空（已有凭据，可能来自 renew 的
//     长命 SK）→ 打印提示「将覆盖现有凭据；长期运行 daemon 建议 `trust renew`」并
//     交互确认（y/N），--overwrite 跳过确认；未确认 → 非零退出（M4 语义）。
//  4. 回填 config set access_key <ak> / access_key_secret <hex(sessionSK)> /
//     access_key_id <session_skeyID>（沿用 trust renew 的 SaveConfig 模式）。
//
// RPC 用**显式无凭据客户端**（M14）：RequestTOTPNonce/LoginTOTP 都是公开端点，
// 发送前必须清空 access_key/access_key_secret/access_key_id 三字段——若工厂按配置
// 带了凭据签名，会把可能过期的签名头带到登录端点（authMiddleware 401 拒绝，整个
// 登录流程不可用）。因此这里不透过 factory.NewClient 构造，而是新建零凭据 FileClient
// + WithSendNoAuth（doRequest 层短路签名，防构造遗漏带签名）。
func newCmdTrustLogin(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider, cfgFile *string) *cobra.Command {
	var (
		username  string
		overwrite bool
		manualAK  string
	)
	cmd := &cobra.Command{
		Use:   "login [username]",
		Short: "TOTP 登录（位置参数=用户名，免记 AK；回填 access_key 三件套）",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				username = args[0]
			}
			return runTrustLogin(cmd.Context(), cmd, ios, cfgSvc, cfgFile, runTrustLoginOpts{
				username:  username,
				overwrite: overwrite,
				manualAK:  manualAK,
			})
		},
	}
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "跳过覆盖确认直接回填（已有凭据时）")
	cmd.Flags().StringVar(&manualAK, "ak", "", "已注册但本地无配置 AK 时手动指定（与用户名互斥）")
	return cmd
}

// runTrustLoginOpts 是 trust login 的参数字段集合（测试透传便利）。
type runTrustLoginOpts struct {
	username  string
	overwrite bool
	manualAK  string
}

// runTrustLogin 执行 trust login 全流程。返回 error 时命令非零退出（M4：未确认 /
// 中止都不得静默成功）；其余内部错误写 stderr 并返回 nil（与 trust 既有命令一致）。
// cmd 用于读取 --ca-file/--insecure（直连面 TLS 安全 flag；传 nil 时仅测试透传路径）。
func runTrustLogin(ctx context.Context, cmd *cobra.Command, ios cli.IOStreams, cfgSvc ConfigProvider, cfgFile *string, opts runTrustLoginOpts) error {
	// 载入配置（决定是否需要注册分支 / 覆盖确认；ServerURL 供无凭据客户端定位）。
	cfg, err := loadTrustLoginConfig(cfgSvc)
	if err != nil {
		ios.WriteErrLine("加载配置失败: %v", err)
		return fmt.Errorf("加载配置失败: %w", err)
	}

	// 显式无凭据客户端（M14）：零凭据构造 + doRequest 层短路签名。ServerURL 未配置
	// 报错（无凭据客户端的 server URL 没有回落语义；--server 在此由调用方填充）。
	if cfg.ServerURL == "" {
		ios.WriteErrLine("未配置 server_url（无法连接 TOTP 服务端）")
		return fmt.Errorf("未配置 server_url（无法连接 TOTP 服务端）")
	}
	noAuthOpts := []client.Option{client.WithSendNoAuth(true)}
	// 直连面安全 flag：--ca-file（严格校验自签/私有 CA）与 --insecure（跳过校验，
	// 仅限 loopback）二选一。两者互斥与「--insecure 仅限 loopback」由调用方（root
	// flag）语义保证；此处把 flag 值接进无凭据客户端，否则自签 hub 下注册/登录
	// 必然 TLS 握手失败（与 factory 直连面 CA 装配同语义）。
	caFlag, insecureOn2 := trustLoginTLSFlags(cmd)
	if caFlag != "" && insecureOn2 {
		return fmt.Errorf("--ca-file 与 --insecure 互斥，不能同时使用")
	}
	if caFlag != "" {
		noAuthOpts = append(noAuthOpts, client.WithCAFile(caFlag))
	} else if insecureOn2 {
		noAuthOpts = append(noAuthOpts, client.WithInsecureTLS())
	}
	noAuth := client.NewFileClient(cfg.ServerURL, noAuthOpts...)

	// 目标登录身份：位置参数 <username>（推荐，owner 反查 AK 免记 AK）> 配置
	// access_key > --ak <AK>。三者都无 → 报错指引 `trust register`（不静默注册
	// 新账号——注册由独立子命令 trust register 承担，登录命令只负责登录）。
	ak := opts.manualAK
	if opts.username == "" && ak == "" {
		ak = cfg.AccessKey
	}
	if opts.username == "" && ak == "" {
		ios.WriteErrLine("未检测到本地 access_key 凭据，也未指定用户名/--ak")
		ios.WriteErrLine("首次使用请先运行 `trust register [用户名]` 注册，然后 `trust login [用户名]` 免记 AK 登录")
		return errLoginNoCreds
	}

	// 2. 提示输入 6 位动态码（stdin）。
	fmt.Fprintf(ios.ErrOut, "请输入 6 位动态码（已加入 Authenticator 后输入）: ")
	reader := bufio.NewReader(ios.In)
	line, err := reader.ReadString('\n')
	if errors.Is(err, io.EOF) && strings.TrimSpace(line) == "" {
		ios.WriteErrLine("未确认（无输入），已中止")
		return errLoginNotConfirmed
	}
	code := strings.TrimSpace(line)
	if code == "" {
		ios.WriteErrLine("未确认（空输入），已中止")
		return errLoginNotConfirmed
	}

	// 3. nonce → login（D3：login_type=cli 回填）。
	nonceObj, nerr := noAuth.RequestTOTPNonce(ctx)
	if nerr != nil {
		ios.WriteErrLine("获取登录 nonce 失败: %v", nerr)
		return fmt.Errorf("获取登录 nonce 失败: %w", nerr)
	}
	var loginRes *client.TOTPLoginResult
	var lerr error
	if opts.username != "" {
		loginRes, lerr = noAuth.LoginTOTPByOwner(ctx, opts.username, nonceObj.Nonce, code, "cli")
	} else {
		loginRes, lerr = noAuth.LoginTOTP(ctx, ak, nonceObj.Nonce, code, "cli")
	}
	if lerr != nil {
		ios.WriteErrLine("登录失败: %v", lerr)
		return fmt.Errorf("登录失败: %w", lerr)
	}
	fmt.Fprintf(ios.Out, "登录成功: ak=%s session_expires_at=%s\n",
		loginRes.AK, loginRes.SessionExpiresAt.Format("2006-01-02 15:04"))

	// 4. 覆盖确认（D4）：已有凭据（可能来自 renew 的长命 SK）→ 提示确认后回填。
	// 拒绝覆盖返回独立哨兵 errLoginOverwriteDenied（与动态码阶段的
	// errLoginNotConfirmed 语义区分）。
	if cfg.AccessKeySecret != "" && !opts.overwrite {
		fmt.Fprint(ios.ErrOut, "将覆盖现有 access_key_secret 凭据；长期运行 daemon 建议 `trust renew`，确认覆盖? (y/N): ")
		cLine, cerr := reader.ReadString('\n')
		if errors.Is(cerr, io.EOF) && strings.TrimSpace(cLine) == "" {
			ios.WriteErrLine("未确认（无输入），已中止")
			return errLoginNotConfirmed
		}
		cAns := strings.ToLower(strings.TrimSpace(cLine))
		if cAns == "" {
			ios.WriteErrLine("未确认（空输入），已中止")
			return errLoginNotConfirmed
		}
		if cAns != "y" && cAns != "yes" {
			ios.WriteErrLine("已取消（拒绝覆盖现有凭据）")
			return errLoginOverwriteDenied
		}
	}

	// 5. 回填三件套（沿用 trust renew 的 SaveConfig 模式）。
	if *cfgFile == "" {
		// 防御：config 写入路径必须存在（生产由 root.go 生成默认路径）。
		ios.WriteErrLine("配置文件路径为空，无法回填登录凭据")
		return fmt.Errorf("配置文件路径为空，无法回填登录凭据")
	}
	reloaded, cerr2 := loadTrustLoginConfig(cfgSvc)
	if cerr2 != nil {
		ios.WriteErrLine("重新加载配置失败: %v", cerr2)
		return fmt.Errorf("重新加载配置失败: %w", cerr2)
	}
	reloaded.AccessKey = loginRes.AK
	reloaded.AccessKeySecret = hex.EncodeToString(loginRes.SessionSK)
	reloaded.AccessKeyID = loginRes.SessionSkeyID
	if err := client.SaveConfig(reloaded, *cfgFile); err != nil {
		ios.WriteErrLine("保存配置失败: %v", err)
		return fmt.Errorf("保存配置失败: %w", err)
	}
	fmt.Fprintf(ios.Out, "凭据已回填: access_key=%s access_key_id=%s（access_key_secret 已写入配置）\n",
		loginRes.AK, loginRes.SessionSkeyID)
	return nil
}

// loadTrustLoginConfig 载入 sclient 配置（nil cfgSvc / 空配置视为无配置）。
func loadTrustLoginConfig(cfgSvc ConfigProvider) (*client.Config, error) {
	if cfgSvc == nil {
		return &client.Config{}, nil
	}
	cfg, err := cfgSvc.LoadConfig()
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return &client.Config{}, nil
	}
	return cfg, nil
}

// trustLoginTLSFlags 读取 --ca-file / --insecure 直连面 TLS 安全 flag。
// cmd 为 nil（纯测试透传 runTrustLoginOpts 路径）时返回空（不启用 CA/insecure）。
func trustLoginTLSFlags(cmd *cobra.Command) (caFile string, insecure bool) {
	if cmd == nil {
		return "", false
	}
	if f := cmd.Flags().Lookup("ca-file"); f != nil {
		caFile, _ = cmd.Flags().GetString("ca-file")
	}
	if f := cmd.Flags().Lookup("insecure"); f != nil {
		insecure, _ = cmd.Flags().GetBool("insecure")
	}
	return caFile, insecure
}
