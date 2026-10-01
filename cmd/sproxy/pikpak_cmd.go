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
