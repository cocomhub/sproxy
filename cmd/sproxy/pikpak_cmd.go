// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cocomhub/sproxy/pkg/cli"
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
		newCmdPikpakAccount(ios),
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
// 用法:
//
//	sproxy pikpak account add <name> <credsJSON>   # 添加账号（会话写入 secrets 卷）
//	sproxy pikpak account list                     # 列出账号（用量/配额）
//	sproxy pikpak account remove <name>            # 删除账号（连 secrets）
func newCmdPikpakAccount(ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "account",
		Short: "PikPak 多账号管理（会话文件池）",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newCmdPikpakAccountAdd(ios),
		newCmdPikpakAccountList(ios),
		newCmdPikpakAccountRemove(ios),
	)
	return cmd
}

// newCmdPikpakAccountAdd 添加账号：把完整 credentials 会话 JSON 写入 secrets 卷。
// 凭据来源：先 `sproxy pikpak login`，再提供 .credentials.json 内容，
// 或直接传 OAuth 登录后导出的完整会话 JSON（含 refresh_token）。
func newCmdPikpakAccountAdd(ios cli.IOStreams) *cobra.Command {
	var quota int64
	cmd := &cobra.Command{
		Use:   "add <name> <credsJSON>",
		Short: "添加 PikPak 账号（credentials 会话写入 secrets 卷）",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := pikpak.NewDirSecretStore(pikpakSecretsDir())
			if err != nil {
				return err
			}
			pool, err := pikpak.NewAccountPool(pikpak.AccountPoolConfig{Secrets: store})
			if err != nil {
				return err
			}
			name := args[0]
			if err := pool.Add(cmd.Context(), pikpak.Account{
				Name: name, SecretJSON: []byte(args[1]), DailyQuota: quota,
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "account %q added (secret %s)\n", name, "pikpak-"+name+".json")
			return nil
		},
	}
	sizeMB := pikpak.DefaultDailyQuota / (1 << 20)
	cmd.Flags().Int64Var(&quota, "quota", 0, fmt.Sprintf("每日下载配额字节（默认 %d MiB）", sizeMB))
	return cmd
}

// newCmdPikpakAccountList 列出账号（名字/用户/今日用量/配额/剩余）。
func newCmdPikpakAccountList(ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出 PikPak 账号（用量/配额）",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := pikpak.NewDirSecretStore(pikpakSecretsDir())
			if err != nil {
				return err
			}
			pool, err := pikpak.NewAccountPool(pikpak.AccountPoolConfig{Secrets: store})
			if err != nil {
				return err
			}
			accs := pool.Accounts()
			if len(accs) == 0 {
				_ = pool.LoadAccounts(cmd.Context())
				accs = pool.Accounts()
			}
			for _, a := range accs {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\tuser=%s\tused=%d/%d\tsecret=%s\n",
					a.Name, a.UserID, a.DailyUsed, a.DailyQuota, a.SecretURL)
			}
			return nil
		},
	}
	return cmd
}

// newCmdPikpakAccountRemove 删除账号及其 secrets 卷凭据。
func newCmdPikpakAccountRemove(ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "删除 PikPak 账号（连 secrets 卷凭据）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := pikpak.NewDirSecretStore(pikpakSecretsDir())
			if err != nil {
				return err
			}
			pool, err := pikpak.NewAccountPool(pikpak.AccountPoolConfig{Secrets: store})
			if err != nil {
				return err
			}
			_ = pool.LoadAccounts(cmd.Context())
			if err := pool.Remove(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "account %q removed\n", args[0])
			return nil
		},
	}
	return cmd
}

// pikpakSecretsDirOverride 可被测试注入临时目录，避免读写真实 ~/.pi/pikpak-secrets。
// 非空时优先（仅测试使用）。
var pikpakSecretsDirOverride string

// pikpakSecretsDir 返回账号 secrets 卷的本地目录（~/.pi/pikpak-secrets）。
func pikpakSecretsDir() string {
	if pikpakSecretsDirOverride != "" {
		return pikpakSecretsDirOverride
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "pikpak-secrets")
	}
	return filepath.Join(home, ".pi", "pikpak-secrets")
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
