// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/hex"
	"os"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/spf13/cobra"
)

// NewCmdIdentity 创建 identity 父命令（节点长时身份密钥与指纹管理，P1 身份 pinning）。
func NewCmdIdentity(ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "identity",
		Short: "节点身份密钥与指纹管理（X25519 长时身份，供对端指纹 pinning）",
	}
	cmd.AddCommand(NewCmdIdentityGenerate(ios))
	cmd.AddCommand(NewCmdIdentityShow(ios))
	cmd.AddCommand(NewCmdIdentityFingerprint(ios))
	return cmd
}

// NewCmdIdentityGenerate 创建 identity generate 命令：
// 生成并持久化 X25519 身份密钥对，打印指纹。默认存 XDG 配置目录 sproxy/identity.json。
func NewCmdIdentityGenerate(ios cli.IOStreams) *cobra.Command {
	var file string
	var force bool
	cmd := &cobra.Command{
		Use:   "generate",
		Short: "生成并持久化节点身份密钥（X25519）",
		Run: func(cmd *cobra.Command, _ []string) {
			path, err := resolveIdentityPath(ios, file)
			if err != nil {
				return
			}
			if !force {
				if _, statErr := os.Stat(path); statErr == nil {
					ios.WriteErrLine("身份文件已存在: %s（使用 --force 覆盖）", path)
					return
				}
			}
			id, gErr := tunnel.GenerateIdentity()
			if gErr != nil {
				ios.WriteErrLine("生成身份失败: %v", gErr)
				return
			}
			if sErr := tunnel.SaveIdentity(id, path); sErr != nil {
				ios.WriteErrLine("保存身份失败: %v", sErr)
				return
			}
			ios.WriteOutLine("身份已生成: %s", path)
			ios.WriteOutLine("Fingerprint: %s", id.Fingerprint())
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "身份文件路径（默认 XDG 配置目录 sproxy/identity.json）")
	cmd.Flags().BoolVar(&force, "force", false, "覆盖已有身份文件")
	return cmd
}

// NewCmdIdentityShow 创建 identity show 命令：展示本节点身份指纹与公钥。
func NewCmdIdentityShow(ios cli.IOStreams) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "展示本节点身份指纹与公钥",
		Run: func(cmd *cobra.Command, _ []string) {
			id, err := loadIdentityForCLI(ios, file)
			if err != nil {
				return
			}
			ios.WriteOutLine("Fingerprint: %s", id.Fingerprint())
			ios.WriteOutLine("PublicKey:   %s", hex.EncodeToString(id.PublicKey()))
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "身份文件路径（默认 XDG 配置目录 sproxy/identity.json）")
	return cmd
}

// NewCmdIdentityFingerprint 创建 identity fingerprint 命令：仅打印指纹（供脚本/复制）。
func NewCmdIdentityFingerprint(ios cli.IOStreams) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "fingerprint",
		Short: "仅打印本节点身份指纹（供脚本/复制）",
		Run: func(cmd *cobra.Command, _ []string) {
			id, err := loadIdentityForCLI(ios, file)
			if err != nil {
				return
			}
			ios.WriteOutLine("%s", id.Fingerprint())
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "身份文件路径（默认 XDG 配置目录 sproxy/identity.json）")
	return cmd
}

// resolveIdentityPath 解析身份文件路径：--file 覆盖时用显式路径，否则用默认 XDG 路径。
func resolveIdentityPath(ios cli.IOStreams, file string) (string, error) {
	if file != "" {
		return file, nil
	}
	path, err := clientfactory.DefaultIdentityPath()
	if err != nil {
		ios.WriteErrLine("获取默认身份路径失败: %v", err)
		return "", err
	}
	return path, nil
}

// loadIdentityForCLI 加载本端身份供 show/fingerprint 展示。
// 文件不存在或损坏时输出错误信息并返回 error。
func loadIdentityForCLI(ios cli.IOStreams, file string) (*tunnel.Identity, error) {
	path, err := resolveIdentityPath(ios, file)
	if err != nil {
		return nil, err
	}
	id, lErr := tunnel.LoadIdentity(path)
	if lErr != nil {
		if os.IsNotExist(lErr) {
			ios.WriteErrLine("身份文件不存在: %s（请先运行 sclient identity generate）", path)
		} else {
			ios.WriteErrLine("加载身份失败: %v", lErr)
		}
		return nil, lErr
	}
	return id, nil
}
