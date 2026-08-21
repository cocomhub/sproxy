// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package p2p 提供点对点连接的路由选路与建立。
// 期3：mesh 选路——从调用方视角为"访问某服务"选择最佳数据路径。
package p2p

import (
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// Plan 描述到达一个 mesh 服务的完整数据路径方案。
type Plan struct {
	// Service 目标服务名。
	Service string
	// Host 托管该服务的节点 ID。
	Host hub.NodeID
	// Addr 目标数据面地址：
	//   - relay/webrtc 直连场景 = 服务在 Host 上的地址（127.0.0.1:22）
	//   - exit 场景 = Host 出口可达的外部地址（如 sg-vps-2:22）
	Addr string
	// Kind 首选路径类型：direct | webrtc | relay | exit | none。
	Kind string
	// Fallbacks 是 Kind 失败后的备选路径（依次尝试）。
	Fallbacks []string
}

// Kind 常量。
const (
	KindDirect = "direct"
	KindWebRTC = "webrtc"
	KindRelay  = "relay"
	KindExit   = "exit"
	KindNone   = "none"
)

// PlanFromService 根据 hub 服务注册表为调用方生成到目标服务的路径方案。
//
// 调用方视角的路径选择：
//   - 若目标节点宣告了公开可达地址（Meta.Addr 非空且非回环/私有）→ 尝试 direct；
//   - 否则优先 webrtc 打洞（数据面直连，不经过 hub）；
//   - 打洞失败回落 hub 中继（relay stream，TCP 级）；
//   - exit：目标服务由出口节点托管（公司电脑 --dial-allow），
//     经 hub 中继到出口，由出口代拨目标地址。
func PlanFromService(svc hub.Service, host hub.NodeID) Plan {
	p := Plan{
		Service:   svc.Name,
		Host:      host,
		Addr:      svc.Addr,
		Kind:      KindRelay, // 默认经 hub 中继（最通用、兜底）
		Fallbacks: []string{KindRelay},
	}
	if svc.Addr != "" {
		// 目标是公网/主机名：出口节点可代拨（exit），webrtc 直连亦可
		p.Kind = KindWebRTC
		p.Fallbacks = []string{KindWebRTC, KindRelay, KindExit}
	}
	return p
}

// PlanForExit 生成经出口节点的路径方案：调用方 → hub → exit → 外部地址。
func PlanForExit(svc hub.Service, host hub.NodeID) Plan {
	return Plan{
		Service:   svc.Name,
		Host:      host,
		Addr:      svc.Addr,
		Kind:      KindExit,
		Fallbacks: []string{KindExit, KindRelay},
	}
}

// PlanForRelay 生成经 hub 中继的通用路径（任何可注册节点都可用）。
func PlanForRelay(svc hub.Service, host hub.NodeID) Plan {
	return Plan{
		Service:   svc.Name,
		Host:      host,
		Addr:      svc.Addr,
		Kind:      KindRelay,
		Fallbacks: []string{KindRelay},
	}
}

// PlanForDirect 生成直连路径（目标节点有可达地址）。
func PlanForDirect(svc hub.Service, host hub.NodeID) Plan {
	return Plan{
		Service:   svc.Name,
		Host:      host,
		Addr:      svc.Addr,
		Kind:      KindDirect,
		Fallbacks: []string{KindDirect, KindWebRTC, KindRelay, KindExit},
	}
}
