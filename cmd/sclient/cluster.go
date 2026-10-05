// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// cluster.go 是集群出口凭证的本地签发工具（评审 C2：签发/下发操作入口缺失——
// 此前仅测试可调 Credential.Sign/Marshal，config.example 的 encoded 是占位串，
// 运维无从产出签名凭证）。
//
// 在**持有节点**执行：提供本端签发 SK（cluster.credential_sign_key，64-hex）+
// 凭证范围参数，本地签发并输出自包含 encoded 串（Credential.Marshal），复制配置到
// 目标节点的 cluster.credentials[].encoded。SK 仅进程内使用（flag 传入，不落盘）。

import (
	"encoding/hex"
	"time"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/clustercred"
	"github.com/spf13/cobra"
)

// newClusterCredentialCmd 创建 cluster credential issue 子命令。
func newClusterCredentialCmd(ios cli.IOStreams) *cobra.Command {
	var (
		signKey    string
		node       string
		volumeName string
		owner      string
		recipient  string
		pathPrefix string
		ttl        time.Duration
	)
	issue := &cobra.Command{
		Use:   "issue",
		Short: "签发集群出口凭证（持有节点本地执行，输出自包含 encoded 串）",
		Long: `在持有节点签发一张集群出口凭证：提供本端签发 SK（cluster.credential_sign_key，
64-hex）+ 凭证范围（node/volume/owner/recipient 必填；path-prefix 限定路径；ttl 控时效）。
输出 encoded 串（Credential.Marshal），复制到目标节点配置 cluster.credentials[].encoded。

示例:
  sclient cluster credential issue \
    --sign-key "<64-hex 32B>" --node holder-a --volume main \
    --owner alice --recipient "sha256:<出口节点指纹>" --path-prefix docs --ttl 24h`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			sk, derr := hex.DecodeString(signKey)
			if derr != nil || len(sk) != 32 {
				ios.WriteErrLine("签发密钥需 32B（64 hex，cluster.credential_sign_key）")
				return
			}
			if node == "" || volumeName == "" || owner == "" || recipient == "" {
				ios.WriteErrLine("--node/--volume/--owner/--recipient 必填（fail-closed 不签发半凭证）")
				return
			}
			now := time.Now().Unix()
			cred := clustercred.Credential{
				Node:       node,
				Volume:     volumeName,
				Owner:      owner,
				Recipient:  recipient,
				Scope:      "read",
				PathPrefix: pathPrefix,
				IssuedAt:   now,
				ExpiresAt:  now + int64(ttl/time.Second),
			}
			cred.Sign(sk)
			encoded, merr := cred.Marshal()
			if merr != nil {
				ios.WriteErrLine("凭证编码失败: %v", merr)
				return
			}
			ios.WriteOutLine(encoded)
		},
	}
	issue.Flags().StringVar(&signKey, "sign-key", "", "签发方 SK（64 hex，cluster.credential_sign_key）")
	issue.Flags().StringVar(&node, "node", "", "持有节点 mesh node ID")
	issue.Flags().StringVar(&volumeName, "volume", "", "持有侧真实卷名")
	issue.Flags().StringVar(&owner, "owner", "", "数据归属 owner（凭证限定命名空间）")
	issue.Flags().StringVar(&recipient, "recipient", "", "出口节点 xfer 身份指纹（白名单）")
	issue.Flags().StringVar(&pathPrefix, "path-prefix", "", "路径范围前缀（可选，限定子树）")
	issue.Flags().DurationVar(&ttl, "ttl", 24*time.Hour, "凭证有效期（下发端控时效）")

	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "集群出口凭证签发工具（持有节点本地执行）",
	}
	cmd.AddCommand(issue)
	return cmd
}
