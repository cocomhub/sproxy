// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// client_accessors.go 是 SDK 的**只读访问器**：把构造期装配的配置暴露给调用方
// （初始化错误、server URL、凭据、mesh hub / node ID、身份与对端指纹等）。
// 单独成文件的理由：这些方法只是取值，集中放置便于核对「SDK 到底暴露了哪些配置」——
// 新增访问器时也更容易自问「是否真的需要暴露」。
//
// 拆分说明见 client.go 顶部。

package client

import (
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// InitError 返回初始化过程中的错误，如 WithTunnel/WithXfer 初始化失败。
// 如果返回 nil，表示所有初始化操作均成功完成。
func (c *FileClient) InitError() error {
	return c.initError
}

// ServerURL 返回客户端配置的服务端地址。
func (c *FileClient) ServerURL() string {
	return c.serverURL
}

// AccessKey 返回 SproxySig 认证的 AccessKey（公开标识）。
func (c *FileClient) AccessKey() string {
	return c.accessKey
}

// AccessKeySecret 返回 SproxySig 认证的 AccessKeySecret（本地密钥，仅计算签名）。
//
// 安全警示（S49）：返回值是认证凭据，严禁写入日志、错误输出或用于展示；
// 需要展示时使用配置层的掩码形式（如 config.go 中的 maskedToken）。
func (c *FileClient) AccessKeySecret() string {
	return c.accessKeySecret
}

// AccessKeyID 返回 SproxySig 的 SK 条目 ID（skeyID，skey-id=<skeyID>）。
// v2 协议 skey-id 强制必传（除 renew 引导外；配置了 access_key 后必须提供）。
func (c *FileClient) AccessKeyID() string {
	return c.accessKeyID
}

// AuthToken 返回多用户 API 密钥 Bearer（api_keys 场景）。
//
// 安全警示（S49）：返回值是认证凭据，严禁写入日志、错误输出或用于展示。
func (c *FileClient) AuthToken() string {
	return c.authToken
}

// MeshHubURL 返回配置的 mesh/relay/p2p hub 地址（可为空，调用方按命令语义回落）。
func (c *FileClient) MeshHubURL() string {
	return c.meshHubURL
}

// NodeID 返回配置的本节点默认 ID（可为空，回落主机名）。
func (c *FileClient) NodeID() string {
	return c.nodeID
}

// Identity 返回本端长时身份（P1 身份 pinning，可为 nil——未配置身份）。
// 仅诊断/测试用途；真实握手由 xfer 隧道在 TunnelDo 时消费。
func (c *FileClient) Identity() *tunnel.Identity {
	return c.identity
}

// PeerFingerprints 返回对端身份指纹 pinning 列表（可为空——未配置 pin）。
// 仅诊断/测试用途；真实校验由 xfer 隧道握手时执行（fail-closed）。
func (c *FileClient) PeerFingerprints() []string {
	return append([]string(nil), c.peerFingerprints...)
}

// doRequest 统一发送 HTTP 请求：当配置了隧道客户端时走加密隧道，否则直连。
//
// urlPath 是相对路径，如 "/upload" 或 "/download?filename=test.txt"。
// 隧道模式下 URL 保持相对路径，由服务端隧道 handler 本地路由；
