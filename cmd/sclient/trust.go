// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// 凭据信任管理命令族（trust）。
//
// 领域逻辑全部在 pkg/client（RenewAccessKey / ListAccessKeys / DeleteSK / ExpireSK /
// AddAK / DeleteAK / ListAKs）；本文件只做 flag 解析 + 调用 + IO 展示（cmd 薄逻辑）。
//
// 权限分档（与服务端一致）：
//   - renew / sk 管理：认证通过即视为本人（能签名即持有某 SK）。
//   - ak list/add/delete：admin-only（4B 注册产生的 admin 条目；4A 无 admin → 403）。

// NewCmdTrust 创建 trust 命令族。
func NewCmdTrust(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider, cfgFile *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trust",
		Short: "凭据信任管理（renew 轮换 SK / SK 条目管理 / AK 管理）",
	}
	cmd.AddCommand(newCmdTrustRenew(factory, ios, cfgSvc, cfgFile))
	cmd.AddCommand(newCmdTrustSK(factory, ios, cfgSvc))
	cmd.AddCommand(newCmdTrustAK(factory, ios, cfgSvc))
	return cmd
}

// newTrustDirectClient 为凭据管理命令构建「直连模式」客户端。
//
// 凭据管理端点（renew / sk 列表/删除/过期 / ak 增删）的「本人判定」依赖
// authMiddleware 在验签成功后注入的 actor（ActorFrom(ctx)）。隧道内层（localMux）
// 不注入 actor → 这些端点在内层按 404 处理，**隧道模式下无法工作**。因此 trust
// 一切命令必须走直连模式（外层 SproxySig 验签 → actor 来自签名头上的 AK）。
//
// 实现：若用户未显式 --server，则用配置 server_url 填充该 flag——factory 的
// serverFlagNotSet 据此判「未显式指定」为 false，从而跳过 WithTunnel、走
// WithAccessKey 直连签名路径。用户显式给了 --server 则原样透传。
func newTrustDirectClient(cmd *cobra.Command, factory clientfactory.Factory, cfgSvc ConfigProvider) (*client.FileClient, error) {
	if s, _ := cmd.Flags().GetString("server"); s == "" && cfgSvc != nil {
		if cfg, err := cfgSvc.LoadConfig(); err == nil && cfg.ServerURL != "" {
			if f := cmd.Flags().Lookup("server"); f != nil {
				_ = f.Value.Set(cfg.ServerURL)
			}
		}
	}
	return factory.NewClient(cmd)
}

// ---- trust renew ----

// newCmdTrustRenew 创建 trust renew 命令：用当前本端 SK 轮换出新 SK，并回填配置。
func newCmdTrustRenew(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider, cfgFile *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "renew",
		Short: "轮换当前 AK 的 SK（新 SK 立即生效，旧 SK 服务端仍可用）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := newTrustDirectClient(cmd, factory, cfgSvc)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			res, err := svc.RenewAccessKey(cmd.Context())
			if err != nil {
				ios.WriteErrLine("SK 轮换失败: %v", err)
				return fmt.Errorf("SK 轮换失败: %w", err)
			}

			// 回填配置：新 SK + 新 sk_id（本地持久化；旧 SK 在 config 中不再需要）。
			cfg, cerr := cfgSvc.LoadConfig()
			if cerr != nil {
				ios.WriteErrLine("加载配置失败: %v", cerr)
				return fmt.Errorf("加载配置失败: %w", cerr)
			}
			newSecret := hex.EncodeToString(res.NewSecret)
			cfg.AccessKeySecret = newSecret
			cfg.AccessKeyID = res.SKID
			if cfg.AccessKey == "" {
				cfg.AccessKey = res.AK
			}
			if *cfgFile == "" {
				// 防御：config 写入路径必须存在（生产由 root.go 生成默认路径）。
				return fmt.Errorf("配置文件路径为空，无法回填轮换后的 SK")
			}
			if err := client.SaveConfig(cfg, *cfgFile); err != nil {
				ios.WriteErrLine("保存配置失败: %v", err)
				return fmt.Errorf("保存配置失败: %w", err)
			}

			expiry := "永久"
			if !res.ExpiresAt.IsZero() {
				expiry = res.ExpiresAt.Format(time.RFC3339)
			}
			fmt.Fprintf(ios.Out, "SK 已轮换: ak=%s sk_id=%s 有效期至 %s (新 SK 已写入配置，立即生效)\n",
				res.AK, res.SKID, expiry)
			return nil
		},
	}
	return cmd
}

// ---- trust sk ----

// newCmdTrustSK 创建 trust sk 命令族（list / delete / expire）。
func newCmdTrustSK(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sk",
		Short: "SK 条目管理（list / delete / expire）",
	}
	cmd.PersistentFlags().String("ak", "", "目标 AccessKey（默认本端 access_key）")
	cmd.AddCommand(newCmdTrustSKList(factory, ios, cfgSvc))
	cmd.AddCommand(newCmdTrustSKDelete(factory, ios, cfgSvc))
	cmd.AddCommand(newCmdTrustSKExpire(factory, ios, cfgSvc))
	return cmd
}

// newCmdTrustSKList 创建 trust sk list 命令：列出目标 AK 的 SK 条目，只展示能解开的条目。
func newCmdTrustSKList(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出 AK 的 SK 条目（只展示本端能解开 secret 的条目，其余 masked）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ak, _ := cmd.Flags().GetString("ak")
			svc, err := newTrustDirectClient(cmd, factory, cfgSvc)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			if ak == "" {
				ak = svc.AccessKey()
			}
			if ak == "" {
				return fmt.Errorf("未配置 access_key 也未指定 --ak")
			}
			infos, err := svc.ListAccessKeys(cmd.Context(), ak)
			if err != nil {
				ios.WriteErrLine("查询 SK 列表失败: %v", err)
				return fmt.Errorf("查询 SK 列表失败: %w", err)
			}
			if len(infos) == 0 {
				fmt.Fprintln(ios.Out, "该 AK 暂无 SK 条目")
				return nil
			}
			fmt.Fprintf(ios.Out, "%-20s  %-20s  %-20s  %-8s  %-10s  %s\n",
				"SK_ID", "CREATED", "EXPIRES", "STATUS", "TYPE", "SECRET")
			for _, s := range infos {
				// SECRET 列只标记能否解开——明文 SK 属凭据（S49），绝不打印。
				secret := "<encrypted>"
				if !s.Masked {
					secret = "<decrypted>"
				}
				expires := "永久"
				if !s.Expires.IsZero() {
					expires = s.Expires.Format("2006-01-02 15:04")
				}
				fmt.Fprintf(ios.Out, "%-20s  %-20s  %-20s  %-8s  %-10s  %s\n",
					s.SKID, s.Created.Format("2006-01-02 15:04"), expires, s.Status, s.MetaType, secret)
			}
			return nil
		},
	}
}

// newCmdTrustSKDelete 创建 trust sk delete 命令。
func newCmdTrustSKDelete(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <sk_id>",
		Short: "删除单条 SK（删除后该 SK 签名立即失效）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ak, _ := cmd.Flags().GetString("ak")
			svc, err := newTrustDirectClient(cmd, factory, cfgSvc)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			if ak == "" {
				ak = svc.AccessKey()
			}
			if ak == "" {
				return fmt.Errorf("未配置 access_key 也未指定 --ak")
			}
			if err := svc.DeleteSK(cmd.Context(), ak, args[0]); err != nil {
				ios.WriteErrLine("删除 SK 失败: %v", err)
				return fmt.Errorf("删除 SK 失败: %w", err)
			}
			fmt.Fprintf(ios.Out, "SK 已删除: %s (ak=%s)\n", args[0], ak)
			return nil
		},
	}
}

// newCmdTrustSKExpire 创建 trust sk expire 命令。
func newCmdTrustSKExpire(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	var until string
	cmd := &cobra.Command{
		Use:   "expire <sk_id> [--until RFC3339]",
		Short: "设单条 SK 的生效截止时间（--until 缺省清空=恢复永久有效）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ak, _ := cmd.Flags().GetString("ak")
			svc, err := newTrustDirectClient(cmd, factory, cfgSvc)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			if ak == "" {
				ak = svc.AccessKey()
			}
			if ak == "" {
				return fmt.Errorf("未配置 access_key 也未指定 --ak")
			}
			var untilTime time.Time
			if until != "" {
				untilTime, err = time.Parse(time.RFC3339, until)
				if err != nil {
					ios.WriteErrLine("--until 需为 RFC3339 时间（如 2026-09-01T12:00:00Z）: %v", err)
					return fmt.Errorf("--until 需为 RFC3339 时间: %w", err)
				}
			}
			if err := svc.ExpireSK(cmd.Context(), ak, args[0], untilTime); err != nil {
				ios.WriteErrLine("设 SK 过期失败: %v", err)
				return fmt.Errorf("设 SK 过期失败: %w", err)
			}
			if untilTime.IsZero() {
				fmt.Fprintf(ios.Out, "SK 已设为永久有效: %s (ak=%s)\n", args[0], ak)
			} else {
				fmt.Fprintf(ios.Out, "SK 截止时间已更新: %s → %s (ak=%s)\n", args[0], untilTime.Format(time.RFC3339), ak)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&until, "until", "", "过期截止时间（RFC3339，如 2026-09-01T12:00:00Z；空=恢复永久有效）")
	return cmd
}

// ---- trust ak ----

// newCmdTrustAK 创建 trust ak 命令族（list / add / delete，admin-only）。
func newCmdTrustAK(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ak",
		Short: "AccessKey 管理（list / add / delete，需要 admin 权限）",
	}
	cmd.AddCommand(newCmdTrustAKList(factory, ios, cfgSvc))
	cmd.AddCommand(newCmdTrustAKAdd(factory, ios, cfgSvc))
	cmd.AddCommand(newCmdTrustAKDelete(factory, ios, cfgSvc))
	return cmd
}

// newCmdTrustAKList 创建 trust ak list 命令（admin）。
func newCmdTrustAKList(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出全部 AccessKey（admin）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := newTrustDirectClient(cmd, factory, cfgSvc)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			sums, err := svc.ListAKs(cmd.Context())
			if err != nil {
				ios.WriteErrLine("查询 AK 列表失败: %v", err)
				return fmt.Errorf("查询 AK 列表失败: %w", err)
			}
			if len(sums) == 0 {
				fmt.Fprintln(ios.Out, "暂无 AccessKey")
				return nil
			}
			fmt.Fprintf(ios.Out, "%-24s  %-16s  %-10s  %-10s\n", "AK", "OWNER", "SK_COUNT", "ALIVE_SK")
			for _, s := range sums {
				fmt.Fprintf(ios.Out, "%-24s  %-16s  %-10d  %-10d\n", s.AK, s.Owner, s.SKCount, s.AliveSK)
			}
			return nil
		},
	}
}

// newCmdTrustAKAdd 创建 trust ak add 命令（admin，生成 AK/SK 对并注册）。
//
// 未显式指定 ak 时本地生成一对：生成逻辑收归 pkg/accesskey.GeneratePair（M5 后
// 唯一事实源，替代已删除的 cmd generateAccessKeyPair）——ak=sk-<mesh>-<32hex>、
// sk=32B hex；mesh flag 已有（仅未指定 ak 参数时生效）。
func newCmdTrustAKAdd(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add [ak]",
		Short: "新增 AccessKey（admin；不指定 SK 时服务端生成并单次回传）",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := newTrustDirectClient(cmd, factory, cfgSvc)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			owner, _ := cmd.Flags().GetString("owner")
			secret, _ := cmd.Flags().GetString("secret")
			ak := ""
			if len(args) == 1 {
				ak = args[0]
			}
			if ak == "" {
				// 未指定 AK：本地生成一对（access-key 命令删除后内联，唯一实现）。
				var pairErr error
				mesh, _ := cmd.Flags().GetString("mesh")
				ak, secret, pairErr = accesskey.GeneratePair(nil, mesh)
				if pairErr != nil {
					ios.WriteErrLine("生成 AccessKey 失败: %v", pairErr)
					return fmt.Errorf("生成 AccessKey 失败: %w", pairErr)
				}
			}
			res, err := svc.AddAK(cmd.Context(), ak, owner, secret)
			if err != nil {
				ios.WriteErrLine("新增 AK 失败: %v", err)
				return fmt.Errorf("新增 AK 失败: %w", err)
			}
			fmt.Fprintf(ios.Out, "AK 已新增: ak=%s sk_id=%s\n", res.AK, res.SKID)
			if res.Secret != "" {
				// 服务端生成且单次回传（仅此一次展示机会）。
				fmt.Fprintf(ios.Out, "初始 Secret（仅本次展示，请保存）: %s\n", res.Secret)
			} else if secret != "" {
				// 客户端显式指定 → 服务端不回传，提示本端已持有。
				fmt.Fprintln(ios.Out, "已使用本端指定 Secret（请确保已保存）")
			}
			return nil
		},
	}
	cmd.Flags().String("owner", "", "AK 归属者（可选）")
	cmd.Flags().String("secret", "", "指定 Secret 值（64-hex；不指定则服务端生成并单次回传）")
	cmd.Flags().String("mesh", "", "生成 AK 时的 mesh 标识（仅未指定 ak 参数时生效）")
	return cmd
}

// errDeleteAKNotConfirmed 是 trust ak delete 在确认输入为空（stdin EOF / 管道空）时
// 返回的非零错误：无输入即中止，绝不把「未确认」当成功（M4——此前空输入与不匹配
// 一样打印「已取消」并返回 nil，退出码 0 误报成功）。
var errDeleteAKNotConfirmed = errors.New("删除已中止：未收到确认输入")

// newCmdTrustAKDelete 创建 trust ak delete 命令（admin + 交互确认 AK 名）。
func newCmdTrustAKDelete(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "delete <ak>",
		Short: "删除整个 AccessKey（admin；需交互确认 AK 名，--force 跳过活跃检查）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]
			// 交互二次确认：必须逐字输入目标 AK 名（对齐服务端 confirm 字段）。
			fmt.Fprintf(ios.ErrOut, "将永久删除 AccessKey %q（其下所有 SK 立即失效）。请输入 AK 名确认: ", target)
			reader := bufio.NewReader(ios.In)
			line, err := reader.ReadString('\n')
			if errors.Is(err, io.EOF) && strings.TrimSpace(line) == "" {
				// stdin 立即 EOF / 管道无输入：无确认输入 → 非零退出（M4）。
				ios.WriteErrLine("未确认（无输入），已中止")
				return errDeleteAKNotConfirmed
			}
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				// 读到空行（输入仅为换行）：同样视为无确认输入（M4）。
				ios.WriteErrLine("未确认（空输入），已中止")
				return errDeleteAKNotConfirmed
			}
			if trimmed != target {
				// 输入非空但不匹配：CLI 显式取消 = 正常返回（成功语义）。
				fmt.Fprintln(ios.Out, "已取消（输入与目标 AK 不一致）")
				return nil
			}

			svc, err := newTrustDirectClient(cmd, factory, cfgSvc)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			if err := svc.DeleteAK(cmd.Context(), target, force); err != nil {
				ios.WriteErrLine("删除 AK 失败: %v", err)
				return fmt.Errorf("删除 AK 失败: %w", err)
			}
			fmt.Fprintf(ios.Out, "AK 已删除: %s\n", target)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "忽略活跃 SK 检查强制删除（仍需交互确认 AK 名）")
	return cmd
}
