// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/tunnel/mesh"
)

// withNATAlert 包装拨号函数接入 NAT 穿透失败告警（roadmap 11.1-①）：
//   - 拨号失败 → e.OnNATFailure(ctx, peer, err)；成功 → e.OnNATRecovered(ctx, peer)（recover 内部
//     已判非 firing 即 no-op，故每次成功都可安全调用）；
//   - 返回原 conn/err（告警是旁路副作用，绝不吞错、绝不改拨号语义——出口失败仍 fail-closed 向上传播）；
//   - e 为 nil（告警引擎未启用）时直接返回原 dial（零开销，拨号行为零变化）。
func withNATAlert(dial func(context.Context, string) (net.Conn, error), e *server.AlertEngine, peer string) func(context.Context, string) (net.Conn, error) {
	if e == nil || dial == nil {
		return dial
	}
	return func(ctx context.Context, addr string) (net.Conn, error) {
		conn, err := dial(ctx, addr)
		if err != nil {
			e.OnNATFailure(ctx, peer, err.Error())
		} else {
			e.OnNATRecovered(ctx, peer)
		}
		return conn, err
	}
}

// buildCloudExitDial 构造云端下载的经 mesh 出口拨号函数：
// 本地（服务端）直连优先 → 失败/超时回退经 hub 中继（RelayStream）到指定出口节点出站拨号。
//
// 装配落在 main 包（cmd/sproxy）：pkg/server 不 import pkg/client（client 的 e2e_test
// import server 构成包级环），FileClient 构造复用既有 newMeshHubClient。
//
// 前置（fail-closed）：cloud_download_exit_node 启用时必须齐备
//   - 远端 hub：mesh.hub_url + mesh.access_key + mesh.access_key_secret；
//   - 本机 hub（mesh.hub_url 为空）：凭据仍需显式 mesh.access_key/secret。
//
// 安全边界：出口节点侧拨号策略（NewServiceDialPolicy）把关目标地址；与 http-proxy
// 的 --exit 语义一致——本函数只管拨号闭包，目标可达性由出口节点裁决。
func buildCloudExitDial(cfg *server.Config) (func(ctx context.Context, addr string) (net.Conn, error), error) {
	if cfg.CloudDownloadExitNode == "" {
		return nil, fmt.Errorf("cloud_download_exit_node 为空（需指定出口节点 ID）")
	}
	fc, err := newMeshHubClient(cfg, cfg.Mesh.AccessKey, cfg.Mesh.AccessKeySecret, cfg.Mesh.SkeyID)
	if err != nil {
		return nil, err
	}
	if cfg.Mesh.AccessKey == "" || cfg.Mesh.AccessKeySecret == "" {
		return nil, fmt.Errorf("需配置 mesh.access_key/access_key_secret（cloud_download_exit_node 经 hub 拨号依赖 SproxySig 凭据）")
	}
	exitNode := cfg.CloudDownloadExitNode
	relayDial := func(ctx context.Context, addr string) (net.Conn, error) {
		return fc.RelayStream(ctx, exitNode, addr)
	}
	// 本地直连优先（服务端本地能直连 URL 时直连，被墙时经出口）——R2-1 并行竞速升级后，
	// 本地黑洞时出口立即胜出（不等探测超时）。
	return mesh.NewLocalOrExitDial(mesh.DefaultLocalDialTimeout, relayDial), nil
}
