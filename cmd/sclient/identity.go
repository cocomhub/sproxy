// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/hex"
	"fmt"
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
		Short: "节点身份密钥与指纹管理（Ed25519 长时身份，供对端指纹 pinning）",
	}
	cmd.AddCommand(NewCmdIdentityGenerate(ios))
	cmd.AddCommand(NewCmdIdentityShow(ios))
	cmd.AddCommand(NewCmdIdentityFingerprint(ios))
	return cmd
}

// NewCmdIdentityGenerate 创建 identity generate 命令：
// 生成并持久化 Ed25519 身份密钥对，打印指纹。默认存 XDG 配置目录 sproxy/identity.json。
func NewCmdIdentityGenerate(ios cli.IOStreams) *cobra.Command {
	var file string
	var force bool
	cmd := &cobra.Command{
		Use:          "generate",
		Short:        "生成并持久化节点身份密钥（Ed25519）",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := resolveIdentityPath(file)
			if err != nil {
				return err
			}
			if !force {
				if _, statErr := os.Stat(path); statErr == nil {
					return fmt.Errorf("身份文件已存在: %s（使用 --force 覆盖）", path)
				}
			}
			id, gErr := tunnel.GenerateIdentity()
			if gErr != nil {
				return fmt.Errorf("生成身份失败: %w", gErr)
			}
			if sErr := tunnel.SaveIdentity(id, path); sErr != nil {
				return fmt.Errorf("保存身份失败: %w", sErr)
			}
			ios.WriteOutLine("身份已生成: %s", path)
			ios.WriteOutLine("Fingerprint: %s", id.Fingerprint())
			return nil
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
		Use:          "show",
		Short:        "展示本节点身份指纹与公钥",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := loadIdentityForCLI(file)
			if err != nil {
				return err
			}
			ios.WriteOutLine("Fingerprint: %s", id.Fingerprint())
			ios.WriteOutLine("PublicKey:   %s", hex.EncodeToString(id.PublicKey()))
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "身份文件路径（默认 XDG 配置目录 sproxy/identity.json）")
	return cmd
}

// NewCmdIdentityFingerprint 创建 identity fingerprint 命令：仅打印指纹（供脚本/复制）。
func NewCmdIdentityFingerprint(ios cli.IOStreams) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:          "fingerprint",
		Short:        "仅打印本节点身份指纹（供脚本/复制）",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := loadIdentityForCLI(file)
			if err != nil {
				return err
			}
			ios.WriteOutLine("%s", id.Fingerprint())
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "身份文件路径（默认 XDG 配置目录 sproxy/identity.json）")
	return cmd
}

// resolveIdentityPath 解析身份文件路径：--file 覆盖时用显式路径，否则用默认 XDG 路径。
func resolveIdentityPath(file string) (string, error) {
	if file != "" {
		return file, nil
	}
	return clientfactory.DefaultIdentityPath()
}

// loadIdentityForCLI 加载本端身份供 show/fingerprint 展示。
// 文件不存在或损坏时返回带恢复路径的 error（RunE 使退出码非 0）。
func loadIdentityForCLI(file string) (*tunnel.Identity, error) {
	path, err := resolveIdentityPath(file)
	if err != nil {
		return nil, err
	}
	id, lErr := tunnel.LoadIdentity(path)
	if lErr != nil {
		if os.IsNotExist(lErr) {
			return nil, fmt.Errorf("身份文件不存在: %s（请先运行 sclient identity generate）", path)
		}
		return nil, fmt.Errorf("加载身份失败: %w", lErr)
	}
	return id, nil
}
