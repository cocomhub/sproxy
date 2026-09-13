// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"

	"github.com/cocomhub/sproxy/pkg/client"
)

// RelayDialer 是基于 `pkg/client` 的默认 Dialer：查 hub 服务发现表找到目标节点宣告的
// 卷访问服务，再经 hub **中继**建立字节流连接。
//
// 为什么默认只走中继：WebRTC 直连实现位于 `pkg/tunnel/mesh`——那是**独立 module**
// （实测根 go.mod 无其 replace，只有 cmd/sclient 通过 replace 引入），根 module 的包不得
// 导入。需要直连的场景由 `cmd/sclient` 侧注入自己的 Dialer（`WithDialer`）。
type RelayDialer struct {
	svc     *client.FileClient
	service string
}

// NewRelayDialer 构造中继 Dialer。service 为空时使用 ServiceName。
func NewRelayDialer(svc *client.FileClient, service string) *RelayDialer {
	if service == "" {
		service = ServiceName
	}
	return &RelayDialer{svc: svc, service: service}
}

// Dial 查服务发现表并建立到目标节点的中继连接。
//
// 多个节点宣告同名服务时只挑**目标节点**的那条（本函数不负责选路策略）；目标节点未宣告
// 该服务即报错（fail-closed，不回落其它节点——回落会破坏「授权按节点绑定」的语义）。
func (d *RelayDialer) Dial(ctx context.Context, node string) (net.Conn, error) {
	if d.svc == nil {
		return nil, fmt.Errorf("remote: RelayDialer 未配置 FileClient")
	}
	svcs, err := d.svc.MeshServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("remote: 查询 mesh 服务失败: %w", err)
	}
	addr := ""
	for _, s := range svcs {
		if s.Node == node && s.Name == d.service {
			addr = s.Addr
			break
		}
	}
	if addr == "" {
		return nil, fmt.Errorf("remote: 节点 %q 未宣告服务 %q", node, d.service)
	}
	conn, err := d.svc.RelayStream(ctx, node, addr)
	if err != nil {
		return nil, fmt.Errorf("remote: 中继连接节点 %q（%s）失败: %w", node, addr, err)
	}
	return conn, nil
}

// DialerFunc 把函数适配为 Dialer（测试注入、CLI 自定义拨号都用它）。
type DialerFunc func(ctx context.Context, node string) (net.Conn, error)

// Dial 实现 Dialer。
func (f DialerFunc) Dial(ctx context.Context, node string) (net.Conn, error) {
	if f == nil {
		return nil, fmt.Errorf("remote: DialerFunc 为 nil")
	}
	return f(ctx, node)
}

// urlQueryEscape 是查询参数转义（保持与 net/url 一致）。
func urlQueryEscape(s string) string { return url.QueryEscape(s) }

// jsonDecode 解析 JSON（薄包装，便于统一错误上下文）。
func jsonDecode(r io.Reader, v any) error { return json.NewDecoder(r).Decode(v) }

// normalizeRelPath 归一卷内相对路径：折叠重复斜杠、去首尾斜杠、拒绝 `.`/`..` 段与反斜杠。
//
// 与 `pkg/sync` 的 FS 契约一致（正斜杠、根相对、无根前缀）；对端会再做一次
// `ValidateFilePath`（段名校验 + 防穿越），两侧都不放松。
func normalizeRelPath(p string) (string, error) {
	p = strings.ReplaceAll(p, `\`, "/")
	segs := strings.Split(p, "/")
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		switch s {
		case "", ".":
			continue
		case "..":
			return "", fmt.Errorf("remote: 路径含 .. 段（拒绝）: %q", p)
		}
		out = append(out, s)
	}
	return strings.Join(out, "/"), nil
}
