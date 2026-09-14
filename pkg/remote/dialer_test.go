// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package remote_test

// dialer_test.go 钉住 RelayDialer 的**依赖收窄**与选路语义（Y 二期 P3-d 装配的前置）：
// 装配层（cmd/sproxy）要能注入自己的客户端实现与测试替身，故 NewRelayDialer 接受
// **最小能力接口**（MeshServices + RelayStream）而非具体 *client.FileClient。

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/remote"
)

// fakeRelayClient 是最小中继客户端替身：记录调用并按预设服务表/错误作答。
type fakeRelayClient struct {
	services []client.MeshService
	svcErr   error

	mu        []string // 依次记录 RelayStream 的 (target, addr)
	streamErr error
}

func (f *fakeRelayClient) MeshServices(context.Context) ([]client.MeshService, error) {
	if f.svcErr != nil {
		return nil, f.svcErr
	}
	return f.services, nil
}

func (f *fakeRelayClient) RelayStream(_ context.Context, target, addr string) (net.Conn, error) {
	f.mu = append(f.mu, target+"@"+addr)
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	c1, c2 := net.Pipe()
	_ = c2.Close()
	return c1, nil
}

// TestRelayDialer_UsesNarrowInterface 钉住「接受最小能力接口」：一个**非** *client.FileClient
// 的实现也能驱动拨号（装配层可注入自有实现，测试可用替身）。
func TestRelayDialer_UsesNarrowInterface(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	fake := &fakeRelayClient{services: []client.MeshService{
		{Node: "nodeA", Name: remote.ServiceName, Addr: "127.0.0.1:19000"},
		{Node: "nodeB", Name: remote.ServiceName, Addr: "127.0.0.1:19001"},
	}}

	var d remote.Dialer = remote.NewRelayDialer(fake, "")
	if _, err := d.Dial(context.Background(), "nodeB"); err != nil {
		t.Fatalf("应向 nodeB 建立中继连接: %v", err)
	}
	if len(fake.mu) != 1 || fake.mu[0] != "nodeB@127.0.0.1:19001" {
		t.Fatalf("应只拨目标节点宣告的地址: %v", fake.mu)
	}
}

// TestRelayDialer_ServiceNameAndFailClosed 钉住：服务名可覆盖（写面 volwrite）；目标节点未
// 宣告该服务即报错且**不回落**其它节点（授权按节点绑定，回落会破坏语义）。
func TestRelayDialer_ServiceNameAndFailClosed(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	fake := &fakeRelayClient{services: []client.MeshService{
		{Node: "nodeA", Name: remote.ServiceName, Addr: "127.0.0.1:19000"},
	}}

	// 写面服务名：nodeA 只宣告了 volread ⇒ 未宣告 volwrite，必须报错且**不拨**其它节点。
	d := remote.NewRelayDialer(fake, remote.ServiceNameWrite)
	if _, err := d.Dial(context.Background(), "nodeA"); err == nil {
		t.Fatal("目标节点未宣告 volwrite 应报错（fail-closed）")
	}
	if len(fake.mu) != 0 {
		t.Fatalf("未命中服务时不得发起任何中继拨号: %v", fake.mu)
	}

	// 目标节点不存在 ⇒ 同样报错。
	if _, err := d.Dial(context.Background(), "nodeZ"); err == nil {
		t.Fatal("未知节点应报错")
	}

	// 服务发现失败原样上抛（带上下文）。
	fakeErr := fmt.Errorf("hub 不可达")
	fail := &fakeRelayClient{svcErr: fakeErr}
	if _, err := remote.NewRelayDialer(fail, "").Dial(context.Background(), "nodeA"); err == nil {
		t.Fatal("服务发现失败应报错")
	}
}
