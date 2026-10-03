// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/volume/ext/pikpak"
	"github.com/spf13/cobra"
)

// newCmdPikpak 创建 `sproxy pikpak` 子命令：PikPak 网盘中转后端管理
// （官方 CLI 安装/登录状态/转存分享/下载）。
//
// 用法:
//
//	sproxy pikpak install [dir]          # 安装官方 CLI（跨系统）
//	sproxy pikpak login                  # OAuth device 登录（打印授权码）
//	sproxy pikpak status                 # 查看登录状态
//	sproxy pikpak restore <shareURL>     # 转存分享到个人网盘
//	sproxy pikpak download <shareURL>    # 分享 URL 完整下载（转存+CLI）
func newCmdPikpak(ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pikpak",
		Short: "PikPak 网盘中转后端（官方 CLI）",
		Long: `PikPak 网盘中转：把分享 URL 转存到个人网盘后用官方 CLI 完整下载。
绕过 PikPak 分享链接 40-50% 下载限制（官方对分享链接的反下载设计）。
子命令：install / login / status / restore / download。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newCmdPikpakInstall(ios),
		newCmdPikpakLogin(ios),
		newCmdPikpakStatus(ios),
		newCmdPikpakRestore(ios),
		newCmdPikpakDownload(ios),
		newCmdPikpakAccount(ios, nil, ""),
	)
	return cmd
}

// pikpakCLI 构造 CLI 包装（优先 BinaryPath 配置或 PATH，自动安装可选）。
func pikpakCLI(autoInstall bool, installDir string) (*pikpak.Cli, error) {
	return pikpak.NewCli(pikpak.CliConfig{
		BinaryPath:  os.Getenv("PIKPAK_BINARY"),
		InstallDir:  installDir,
		AutoInstall: autoInstall,
	})
}

// newCmdPikpakInstall 安装官方 CLI。
func newCmdPikpakInstall(ios cli.IOStreams) *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "install [dir]",
		Short: "安装 PikPak 官方 CLI（跨系统自动下载）",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				dir = args[0]
			}
			cli2, err := pikpak.NewCli(pikpak.CliConfig{InstallDir: dir, AutoInstall: true})
			if err != nil {
				return err
			}
			fmt.Fprintf(ios.Out, "PikPak CLI ready: %s\n", cli2.Path())
			fmt.Fprintf(ios.Out, "next: run 'sproxy pikpak login' to authorize\n")
			return nil
		},
	}
	return cmd
}

// newCmdPikpakLogin OAuth device 登录。
func newCmdPikpakLogin(ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login",
		Short: "OAuth device 授权登录（打印授权链接+码）",
		RunE: func(cmd *cobra.Command, args []string) error {
			cli2, err := pikpakCLI(false, "")
			if err != nil {
				return err
			}
			auth := pikpak.NewAuth(cli2)
			dc, err := auth.DeviceLoginStart(cmd.Context(), "sproxy")
			if err != nil {
				return err
			}
			fmt.Fprintf(ios.Out, "请打开以下链接并输入授权码完成授权：\n%s\n授权码：%s\n（有效 %d 秒）\n",
				dc.VerificationURI, dc.UserCode, dc.ExpiresIn)
			return nil
		},
	}
	return cmd
}

// newCmdPikpakStatus 查看登录状态。
func newCmdPikpakStatus(ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "查看 CLI 登录状态",
		RunE: func(cmd *cobra.Command, args []string) error {
			cli2, err := pikpakCLI(false, "")
			if err != nil {
				return err
			}
			auth := pikpak.NewAuth(cli2)
			st, err := auth.Status(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(ios.Out, "logged_in=%v user=%s name=%s\n", st.LoggedIn, st.UserID, st.Name)
			return nil
		},
	}
	return cmd
}

// newCmdPikpakRestore 转存分享到个人网盘。
func newCmdPikpakRestore(ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restore <shareURL>",
		Short: "转存 PikPak 分享到个人网盘",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cli2, err := pikpakCLI(false, "")
			if err != nil {
				return err
			}
			api := pikpak.NewAPI(pikpak.APIConfig{}, cli2)
			shareID, err := pikpak.ParseShareID(args[0])
			if err != nil {
				return err
			}
			files, err := api.ListShareRecursive(cmd.Context(), shareID)
			if err != nil {
				return err
			}
			target := pikpak.PickLargestVideo(files)
			if target == nil {
				return fmt.Errorf("no video in share")
			}
			fileID, err := api.RestoreShare(cmd.Context(), shareID, []string{target.ID}, "")
			if err != nil {
				return err
			}
			fmt.Fprintf(ios.Out, "restored %s (%s) -> file_id %s\n", target.Name, target.ID, fileID)
			return nil
		},
	}
	return cmd
}

// newCmdPikpakAccount PikPak 多账号管理（会话文件池：每账号一份完整 credentials，
// Use 时切换 CLI 会话，CLI 自动 refresh）。
//
// 用法：
//
//	sproxy pikpak account add <name> < creds.json   # 添加账号（凭据从 stdin 读，防 argv 泄漏）
//	sproxy pikpak account list                     # 列出账号（用量/配额）
//	sproxy pikpak account remove <name>            # 删除账号（连加密卷凭据）
//
// newCmdPikpakAccount 构造 account 子命令树。store 为凭据存储（nil = 从服务配置构建
// **加密卷** store，主密钥在默认 secrets 卷）；stateDir 为配额状态目录（空 = 默认用户
// 配置目录）。两者供测试注入（fake store + 临时状态目录），生产传 nil/""。
func newCmdPikpakAccount(ios cli.IOStreams, store pikpak.SecretStore, stateDir string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "account",
		Short: "PikPak 多账号管理（会话文件池）",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newCmdPikpakAccountAdd(ios, store, stateDir),
		newCmdPikpakAccountList(ios, store, stateDir),
		newCmdPikpakAccountRemove(ios, store, stateDir),
	)
	return cmd
}

// pikpakAccountStore 返回账号命令用的凭据存储：显式注入（测试 fake）优先，否则从服务
// 配置构建加密卷 store（C5：主密钥放默认 secrets 卷、凭据经 shardseal 加密落盘）。
func pikpakAccountStore(ctx context.Context, store pikpak.SecretStore) (pikpak.SecretStore, error) {
	if store != nil {
		return store, nil
	}
	cfg, err := server.LoadConfig(cfgFile)
	if err != nil {
		return nil, fmt.Errorf("pikpak account: 加载服务配置: %w", err)
	}
	return pikpakEncryptedSecretStore(ctx, cfg.StorageRoot, cfg.Pikpak.SecretsDir, nil)
}

// pikpakReadCreds 读取添加账号的凭据：--file 优先，否则 stdin（禁止 argv 直传，防进程
// 列表/日志泄漏 refresh_token）。独立承载读取分支，控制 add 命令认知复杂度。
func pikpakReadCreds(ios cli.IOStreams, credFile string) ([]byte, error) {
	if credFile != "" {
		b, err := os.ReadFile(credFile)
		if err != nil {
			return nil, fmt.Errorf("pikpak account: read creds file: %w", err)
		}
		return b, nil
	}
	b, err := io.ReadAll(ios.In)
	if err != nil {
		return nil, fmt.Errorf("pikpak account: read creds from stdin: %w", err)
	}
	return b, nil
}

// newCmdPikpakAccountAdd 添加账号：把完整 credentials 会话 JSON 写入**加密卷**。
// 凭据来源：先 `sproxy pikpak login`，再把 .credentials.json 内容通过
// **stdin 或 --file** 传入（禁止命令行参数直传：argv 会泄漏到进程列表/
// shell 历史/CI 日志——refresh_token 是永久账号接管凭据）。
//
// 用法：
//
//	sproxy pikpak account add <name> < file.json
//	sproxy pikpak account add --file <path> <name>
func newCmdPikpakAccountAdd(ios cli.IOStreams, store pikpak.SecretStore, stateDir string) *cobra.Command {
	var quota int64
	var credFile string
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "添加 PikPak 账号（credentials 会话写入加密卷；凭据从 stdin/--file 读）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			secStore, err := pikpakAccountStore(ctx, store)
			if err != nil {
				return err
			}
			pool, err := pikpak.NewAccountPool(pikpak.AccountPoolConfig{Secrets: secStore, StateDir: stateDir})
			if err != nil {
				return err
			}
			// 与 remove 一致：先 LoadAccounts（跨进程重名账号不再静默覆盖 secrets 文件）。
			if lerr := pool.LoadAccounts(ctx); lerr != nil {
				return lerr
			}
			// 凭据来源：--file 优先，否则 stdin（禁止 argv 直传，防进程列表/日志泄漏）。
			creds, err := pikpakReadCreds(ios, credFile)
			if err != nil {
				return err
			}
			name := args[0]
			if err := pool.Add(cmd.Context(), pikpak.Account{
				Name: name, SecretJSON: creds, DailyQuota: quota,
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "account %q added (secret %s)\n", name, "pikpak-"+name+".json")
			return nil
		},
	}
	sizeMB := pikpak.DefaultDailyQuota / (1 << 20)
	cmd.Flags().Int64Var(&quota, "quota", 0, fmt.Sprintf("每日下载配额字节（默认 %d MiB）", sizeMB))
	cmd.Flags().StringVar(&credFile, "file", "", "凭据 JSON 文件路径（空 = 从 stdin 读）")
	return cmd
}

// newCmdPikpakAccountList 列出账号（名字/用户/今日用量/配额/剩余）。
// LoadAccounts 错误必须上抛（不静默吞掉：list 依赖账号列表，失败即报错）。
func newCmdPikpakAccountList(ios cli.IOStreams, store pikpak.SecretStore, stateDir string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出 PikPak 账号（用量/配额）",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			secStore, err := pikpakAccountStore(ctx, store)
			if err != nil {
				return err
			}
			pool, err := pikpak.NewAccountPool(pikpak.AccountPoolConfig{Secrets: secStore, StateDir: stateDir})
			if err != nil {
				return err
			}
			if lerr := pool.LoadAccounts(ctx); lerr != nil {
				return lerr
			}
			accs := pool.Accounts()
			for _, a := range accs {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\tuser=%s\tused=%d/%d\tsecret=%s\n",
					a.Name, a.UserID, a.DailyUsed, a.DailyQuota, "pikpak-"+a.Name+".json")
			}
			return nil
		},
	}
	return cmd
}

// newCmdPikpakAccountRemove 删除账号及其加密卷凭据。
func newCmdPikpakAccountRemove(ios cli.IOStreams, store pikpak.SecretStore, stateDir string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "删除 PikPak 账号（连加密卷凭据）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			secStore, err := pikpakAccountStore(ctx, store)
			if err != nil {
				return err
			}
			pool, err := pikpak.NewAccountPool(pikpak.AccountPoolConfig{Secrets: secStore, StateDir: stateDir})
			if err != nil {
				return err
			}
			_ = pool.LoadAccounts(ctx)
			if err := pool.Remove(ctx, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "account %q removed\n", args[0])
			return nil
		},
	}
	return cmd
}

// newCmdPikpakDownload 分享 URL 完整下载。
// --output 语义：**输出文件路径**（默认空 = 落到 pikpak.download_dir 临时目录）。
func newCmdPikpakDownload(ios cli.IOStreams) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "download <shareURL>",
		Short: "PikPak 分享完整下载（转存+官方 CLI）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cli2, err := pikpakCLI(false, "")
			if err != nil {
				return err
			}
			api := pikpak.NewAPI(pikpak.APIConfig{}, cli2)
			downloadDir := ""
			if out != "" {
				downloadDir = filepath.Dir(out)
			}
			dl, err := pikpak.NewPikpakDownloader(pikpak.DownloaderConfig{
				Cli: cli2, API: api, DownloadDir: downloadDir, Timeout: 3 * time.Hour,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(ios.Out, "downloading %s ...\n", args[0])
			res, err := dl.Download(cmd.Context(), args[0], out, nil)
			if err != nil {
				return err
			}
			fmt.Fprintf(ios.Out, "done: size=%d\n", res.Size)
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "output", "o", "", "输出文件路径（空 = 临时目录）")
	return cmd
}
