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

// errLoginNotConfirmed 是 trust login 在确认输入为空（stdin EOF / 管道空）或覆盖
// 确认被拒绝时返回的非零错误：无输入即中止，绝不把「未确认」当成功（M4——仿
// errDeleteAKNotConfirmed 语义）。
var errLoginNotConfirmed = errors.New("登录已中止：未收到确认输入")

// newCmdTrustLogin 创建 trust login 命令：GA 密钥录入→注册/登录→解 session SK 回填
// access_key 三件套。
//
// 流程（薄逻辑：flag + IO + 回填；领域逻辑全在 pkg/client TOTP 领域 API）：
//  1. 无 access_key 配置（或 --register）→ 调 RegisterTOTP：打印 ak、base32_secret
//     （唯一展示，不落日志）、otpauth_uri；提示「已加入 Authenticator 后输入 6 位
//     动态码」；响应 admin:true → 额外提示「您是首个注册用户，将成为 admin」（S2）。
//  2. 已注册 → 直接提示输入动态码；本地无配置 AK 时可用 --ak <AK> 手动指定（M6）。
//  3. RequestTOTPNonce → LoginTOTP(ak, nonce, code, "cli")（D3 cli 态）→ 解 session SK。
//  4. 回填前覆盖确认（D4）：cfg.AccessKeySecret 非空（已有凭据，可能来自 renew 的
//     长命 SK）→ 打印提示「将覆盖现有凭据；长期运行 daemon 建议 `trust renew`」并
//     交互确认（y/N），--overwrite 跳过确认；未确认 → 非零退出（M4 语义）。
//  5. 回填 config set access_key <ak> / access_key_secret <hex(sessionSK)> /
//     access_key_id <session_skeyID>（沿用 trust renew 的 SaveConfig 模式）。
//
// RPC 用**显式无凭据客户端**（M14）：RegisterTOTP/RequestTOTPNonce/LoginTOTP 都是
// 公开端点，发送前必须清空 access_key/access_key_secret/access_key_id 三字段——
// 若工厂按配置带了凭据签名，会把可能过期的签名头带到登录端点（authMiddleware 401
// 拒绝，整个登录流程不可用）。因此这里不透过 factory.NewClient 构造，而是新建
// 零凭据 FileClient + WithSendNoAuth（doRequest 层短路签名，防构造遗漏带签名）。
func newCmdTrustLogin(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider, cfgFile *string) *cobra.Command {
	var (
		owner     string
		register  bool
		overwrite bool
		manualAK  string
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "TOTP 登录（录入 GA 密钥注册/登录，回填 access_key 三件套）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTrustLogin(cmd.Context(), ios, cfgSvc, cfgFile, runTrustLoginOpts{
				owner:     owner,
				register:  register,
				overwrite: overwrite,
				manualAK:  manualAK,
			})
		},
	}
	cmd.Flags().StringVar(&owner, "owner", "", "owner 影响文件桶归属，默认=AK（S1）")
	cmd.Flags().BoolVar(&register, "register", false, "强制走注册分支（忽略本地 access_key 配置）")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "跳过覆盖确认直接回填（已有凭据时）")
	cmd.Flags().StringVar(&manualAK, "ak", "", "已注册但本地无配置 AK 时手动指定（M6）")
	return cmd
}

// runTrustLoginOpts 是 trust login 的参数字段集合（测试透传便利）。
type runTrustLoginOpts struct {
	owner     string
	register  bool
	overwrite bool
	manualAK  string
}

// runTrustLogin 执行 trust login 全流程。返回 error 时命令非零退出（M4：未确认 /
// 中止都不得静默成功）；其余内部错误写 stderr 并返回 nil（与 trust 既有命令一致）。
func runTrustLogin(ctx context.Context, ios cli.IOStreams, cfgSvc ConfigProvider, cfgFile *string, opts runTrustLoginOpts) error {
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
	noAuth := client.NewFileClient(cfg.ServerURL, client.WithSendNoAuth(true))

	// 目标 AK：--ak > 注册结果 > 配置 access_key。
	ak := opts.manualAK

	// 1. 注册分支：无 access_key 配置（或 --register）→ RegisterTOTP。
	if opts.register || ak == "" {
		res, rerr := noAuth.RegisterTOTP(ctx, opts.owner)
		if rerr != nil {
			ios.WriteErrLine("注册失败: %v", rerr)
			return fmt.Errorf("注册失败: %w", rerr)
		}
		ak = res.AK
		fmt.Fprintf(ios.Out, "注册成功: ak=%s owner=%s\n", res.AK, res.Owner)
		// base32_secret 属凭据（S49）：唯一一次展示，不落日志。
		fmt.Fprintf(ios.Out, "请在 Authenticator 中录入以下密钥（只展示这一次）:\n  base32: %s\n  otpauth: %s\n",
			res.Base32Secret, res.OTPAuthURI)
		if res.Admin {
			// S2：首个注册用户（服务端原子授予 admin）。
			fmt.Fprintln(ios.Out, "您是首个注册用户，将成为 admin")
		}
	} else if opts.manualAK == "" && cfg.AccessKey != "" {
		// 已注册（配置已有 access_key）→ 直接提示输入动态码。
		ak = cfg.AccessKey
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
	loginRes, lerr := noAuth.LoginTOTP(ctx, ak, nonceObj.Nonce, code, "cli")
	if lerr != nil {
		ios.WriteErrLine("登录失败: %v", lerr)
		return fmt.Errorf("登录失败: %w", lerr)
	}
	fmt.Fprintf(ios.Out, "登录成功: ak=%s session_expires_at=%s\n",
		loginRes.AK, loginRes.SessionExpiresAt.Format("2006-01-02 15:04"))

	// 4. 覆盖确认（D4）：已有凭据（可能来自 renew 的长命 SK）→ 提示确认后回填。
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
			return errLoginNotConfirmed
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
