// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"log/slog"
	"net"
	"strings"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// ParseServiceDecls 解析 --service name:addr 形式的服务宣告：
//   - 返回宣告到 hub 的 []hub.Service（mesh connect 服务发现）与
//     出口拨号精确放行地址 []string（供 relay.NewServiceDialPolicy，含 loopback/私网）。
//   - 非法条目（缺 name/addr、addr 非 host:port 且 host 为空，S60）跳过并 Warn，
//     避免注册"可见不可连"的服务（mesh connect 命中后必然拨号失败）。
//
// relay start 与 mesh node 共用；mesh 服务端 hub/router validateServices 是防御纵深。
func ParseServiceDecls(services []string, logger *slog.Logger) ([]hub.Service, []string) {
	var svcs []hub.Service
	var addrs []string
	for _, svc := range services {
		name, addr, ok := strings.Cut(svc, ":")
		if !ok || name == "" || addr == "" {
			logger.Warn("忽略无效服务宣告（应为 name:addr）", "raw", svc)
			continue
		}
		if host, _, err := net.SplitHostPort(addr); err != nil || host == "" {
			logger.Warn("忽略无效服务宣告（addr 应为 host:port）", "raw", svc, "addr", addr, "error", err)
			continue
		}
		svcs = append(svcs, hub.Service{Name: name, Addr: addr})
		addrs = append(addrs, addr)
	}
	return svcs, addrs
}
