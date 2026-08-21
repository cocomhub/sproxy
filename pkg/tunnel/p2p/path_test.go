// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package p2p

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

func TestPlanFromService_WithAddr(t *testing.T) {
	svc := hub.Service{Name: "sg-vps-2:22", Addr: "sg-vps-2.example.com:22"}
	host := hub.NodeID("exit-node")
	p := PlanFromService(svc, host)
	if p.Kind != KindWebRTC {
		t.Fatalf("expected Kind webrtc (addr present), got %q", p.Kind)
	}
	if p.Host != host {
		t.Fatalf("unexpected host: %q", p.Host)
	}
	// fallbacks 应含 relay + exit
	seen := map[string]bool{}
	for _, f := range p.Fallbacks {
		seen[f] = true
	}
	if !seen[KindRelay] || !seen[KindExit] {
		t.Fatalf("fallbacks missing relay/exit: %+v", p.Fallbacks)
	}
}

func TestPlanFromService_NoAddr(t *testing.T) {
	svc := hub.Service{Name: "local-http"} // addr 为空 = 本地服务（127.0.0.1:8080 隐含）
	host := hub.NodeID("peer")
	p := PlanFromService(svc, host)
	// 无显式 addr 时默认经 hub 中继
	if p.Kind != KindRelay {
		t.Fatalf("expected Kind relay (no addr), got %q", p.Kind)
	}
	if len(p.Fallbacks) == 0 || p.Fallbacks[0] != KindRelay {
		t.Fatalf("unexpected fallbacks: %+v", p.Fallbacks)
	}
}

func TestPlanForDirect(t *testing.T) {
	svc := hub.Service{Name: "ssh", Addr: "203.0.113.9:22"}
	p := PlanForDirect(svc, hub.NodeID("node-a"))
	if p.Kind != KindDirect {
		t.Fatalf("expected Kind direct, got %q", p.Kind)
	}
	if p.Addr != "203.0.113.9:22" {
		t.Fatalf("unexpected addr: %q", p.Addr)
	}
}

func TestPlanForExit(t *testing.T) {
	svc := hub.Service{Name: "sg-ssh", Addr: "sg-vps-2.example.com:22"}
	p := PlanForExit(svc, hub.NodeID("company-exit"))
	if p.Kind != KindExit {
		t.Fatalf("expected Kind exit, got %q", p.Kind)
	}
}

func TestPlanForRelay(t *testing.T) {
	svc := hub.Service{Name: "web", Addr: "127.0.0.1:8080"}
	p := PlanForRelay(svc, hub.NodeID("web-node"))
	if p.Kind != KindRelay {
		t.Fatalf("expected Kind relay, got %q", p.Kind)
	}
}
