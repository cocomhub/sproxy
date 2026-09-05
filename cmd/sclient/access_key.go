// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// NewCmdAccessKey 创建 access-key 命令（生成 SproxySig 请求签名认证的一对 AK/SK）。
//
// 自任务 6（trust 命令族）起标记 deprecated：AK/SK 生成与凭据管理统一走 `trust`
// （`trust ak add` 生成注册 / `trust renew` 轮换）。本命令保留 generateAccessKeyPair
// 的纯生成能力（`access-key create` 仍可用），Aliases 兼容旧调用。
func NewCmdAccessKey(ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:        "access-key",
		Short:      "生成 AccessKey/AccessKeySecret（SproxySig 请求签名认证）",
		Deprecated: "请使用 `trust ak add`（生成并注册）或 `trust renew`（轮换）。`access-key create` 仅保留纯生成能力。",
		Aliases:    []string{"trust"}, // 兼容：`sclient access-key` 与 `sclient trust` 均可达（警告提示 deprecated）。
	}
	cmd.AddCommand(NewCmdAccessKeyCreate(ios))
	return cmd
}

// NewCmdAccessKeyCreate 创建 access-key create 命令：生成一对 AK/SK 打印，
// 供服务端 access_keys 配置与客户端 access_key/access_key_secret 使用。
func NewCmdAccessKeyCreate(ios cli.IOStreams) *cobra.Command {
	var mesh string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "生成一对 AccessKey/AccessKeySecret 并打印",
		Run: func(cmd *cobra.Command, args []string) {
			ak, sk, err := generateAccessKeyPair(mesh)
			if err != nil {
				ios.WriteErrLine("生成 AccessKey 失败: %v", err)
				return
			}
			ios.WriteOutLine("AccessKey:       %s", ak)
			ios.WriteOutLine("AccessKeySecret: %s", sk)
		},
	}
	cmd.Flags().StringVar(&mesh, "mesh", "", "所属 mesh 标识（可选，用于多 mesh 隔离命名，如 sk-<mesh>-<hex>）")
	return cmd
}

// generateAccessKeyPair 生成一对 AccessKey/AccessKeySecret。
//   - AccessKey（公开标识）= sk-<mesh>-<16B hex>；mesh 为空时 sk-<16B hex>
//   - AccessKeySecret（本地密钥）= 32B 随机 hex（64 hex chars）
//
// 与客户端 pkg/client 的 access_key/access_key_secret 配置及服务端
// pkg/server 的 access_keys 配置对应；trust ak add 在未显式指定 AK 时也复用本函数。
func generateAccessKeyPair(mesh string) (string, string, error) {
	akBytes := make([]byte, 16)
	if _, err := rand.Read(akBytes); err != nil {
		return "", "", err
	}
	skBytes := make([]byte, 32)
	if _, err := rand.Read(skBytes); err != nil {
		return "", "", err
	}
	prefix := "sk"
	if mesh != "" {
		prefix += "-" + mesh
	}
	return prefix + "-" + hex.EncodeToString(akBytes), hex.EncodeToString(skBytes), nil
}
