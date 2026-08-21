// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/spf13/cobra"
)

// meshDialResult 是一次 mesh 连接的结果。
type meshDialResult struct {
	conn net.Conn
	// kind 是实际使用的路径：webrtc | relay。
	kind string
}

// meshDialFunc 建立一条到目标服务的连接（选路逻辑）。
// 默认实现：webrtc 打洞优先，失败回落 hub 中继。可注入测试桩。
type meshDialFunc func(ctx context.Context, svc *client.FileClient, signaler *hub.HubSignaler, target *client.MeshService, localNode string) (*meshDialResult, error)

// defaultMeshDial 是默认选路：webrtc 打洞优先，失败回落 hub 中继。
func defaultMeshDial(ctx context.Context, svc *client.FileClient, signaler *hub.HubSignaler, target *client.MeshService, _ string) (*meshDialResult, error) {
	// webrtc 打洞优先（数据面直连，不经过 hub）。
	// DialWithSignaler 内部用 defaultICETimeout（30s）作为信令等待上限，
	// 失败（对端无 p2p listen / 打洞不成功）后回落 hub 中继。
	if signaler != nil && target.Node != "" {
		conn, err := webrtc.DialWithSignaler(target.Node, signaler)
		if err == nil {
			return &meshDialResult{conn: conn, kind: "webrtc"}, nil
		}
		// 打洞失败回落中继
	}
	conn, err := svc.RelayStream(ctx, target.Node, target.Addr)
	if err != nil {
		return nil, err
	}
	return &meshDialResult{conn: conn, kind: "relay"}, nil
}

// NewCmdMesh 创建 mesh 父命令：基于 hub 服务注册表的服务发现与连接。
func NewCmdMesh(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mesh",
		Short: "mesh 服务发现与连接（webrtc 直连优先，hub 中继回落）",
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}
	cmd.AddCommand(newCmdMeshConnect(factory, ios))
	cmd.AddCommand(newCmdMeshStatus(factory, ios))
	return cmd
}

// newCmdMeshConnect 创建 mesh connect：按服务名连接（webrtc 优先，中继回落）。
func newCmdMeshConnect(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "connect <service> [-l :port]",
		Short: "连接到 mesh 服务（webrtc 直连优先，hub 中继回落）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := args[0]
			listenAddr, _ := cmd.Flags().GetString("listen")
			useWebRTC, _ := cmd.Flags().GetBool("webrtc")
			hubURL, _ := cmd.Flags().GetString("hub")
			token, _ := cmd.Flags().GetString("token")
			nodeID, _ := cmd.Flags().GetString("node-id")

			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}

			// 解析服务 → 目标节点 + 地址
			svcs, err := svc.MeshServices(cmd.Context())
			if err != nil {
				return err
			}
			var target *client.MeshService
			for i := range svcs {
				if svcs[i].Name == service {
					target = &svcs[i]
					break
				}
			}
			if target == nil {
				return fmt.Errorf("mesh 服务 %q 未找到（请确认目标节点已宣告该服务）", service)
			}
			ios.WriteOutLine("目标服务: %s（节点 %s, addr %s）", service, target.Node, target.Addr)

			// 构建信令器（webrtc 打洞用）。--hub 未指定时回退 serverURL。
			var signaler *hub.HubSignaler
			if useWebRTC {
				if hubURL == "" {
					hubURL = svc.ServerURL()
				}
				if nodeID == "" {
					nodeID = defaultLocalNodeID()
				}
				signaler = hub.NewHubSignaler(hubURL, token, nodeID)
			}
			dial := defaultMeshDial
			localNode := nodeID
			if localNode == "" {
				localNode = defaultLocalNodeID()
			}

			if listenAddr != "" {
				return meshForwardListen(cmd, svc, signaler, dial, target, localNode, listenAddr, ios)
			}
			return meshStdioOnce(cmd, svc, signaler, dial, target, localNode, ios)
		},
	}
	cmd.Flags().StringP("listen", "l", "", "本地监听地址（如 :2222）；留空为单次 stdin/stdout 模式")
	cmd.Flags().Bool("webrtc", true, "优先 webrtc 打洞直连，失败回落 hub 中继")
	cmd.Flags().String("hub", "", "hub 地址（webrtc 打洞信令用；默认取 server_url）")
	cmd.Flags().String("token", "", "信令 token")
	cmd.Flags().String("node-id", "", "本节点 ID（信令来源；默认主机名）")
	return cmd
}

// newCmdMeshStatus 创建 mesh status：列出 hub 上所有 mesh 服务。
func newCmdMeshStatus(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "列出 hub 上的 mesh 服务",
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}
			svcs, err := svc.MeshServices(cmd.Context())
			if err != nil {
				return err
			}
			if len(svcs) == 0 {
				ios.WriteOutLine("暂无 mesh 服务")
				return nil
			}
			ios.WriteOutLine("mesh 服务 (%d):", len(svcs))
			for _, s := range svcs {
				addr := s.Addr
				if addr == "" {
					addr = "-"
				}
				ios.WriteOutLine("  %-24s node=%s  addr=%s", s.Name, s.Node, addr)
			}
			return nil
		},
	}
}

// meshForwardListen 监听本地端口，每个入站连接独立建立一条 mesh 连接（选路 dial）。
func meshForwardListen(cmd *cobra.Command, svc *client.FileClient, signaler *hub.HubSignaler, dial meshDialFunc, target *client.MeshService, localNode, listenAddr string, ios cli.IOStreams) error {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("监听本地端口失败: %w", err)
	}
	defer ln.Close()
	ios.WriteOutLine("端口转发: %s ⇄ mesh(%s) ⇄ %s", listenAddr, target.Node, target.Addr)

	ctx := cmd.Context()
	// ctx 取消时关闭 listener，使 Accept 立即返回（优雅停止端口转发）。
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		local, aerr := ln.Accept()
		if aerr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return aerr
		}
		go func(c net.Conn) {
			defer c.Close()
			res, cerr := dial(ctx, svc, signaler, target, localNode)
			if cerr != nil {
				ios.WriteErrLine("建立 mesh 流失败: %v", cerr)
				return
			}
			conn := res.conn
			defer conn.Close()
			// webrtc 连接已是纯字节流；relay 连接需写 dial 帧（出口节点拨目标）
			if res.kind == "relay" {
				if werr := meshRelayDial(conn, target.Addr); werr != nil {
					ios.WriteErrLine("写 relay dial 帧失败: %v", werr)
					return
				}
			}
			ios.WriteOutLine("连接已建立（%s）: %s ⇄ %s", res.kind, target.Node, target.Addr)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); _, _ = io.Copy(conn, c) }()
			go func() { defer wg.Done(); _, _ = io.Copy(c, conn) }()
			wg.Wait()
		}(local)
	}
}

// meshStdioOnce 单次模式：stdin/stdout 与一条 mesh 连接直通（选路 dial）。
func meshStdioOnce(cmd *cobra.Command, svc *client.FileClient, signaler *hub.HubSignaler, dial meshDialFunc, target *client.MeshService, localNode string, ios cli.IOStreams) error {
	res, err := dial(cmd.Context(), svc, signaler, target, localNode)
	if err != nil {
		return err
	}
	conn := res.conn
	defer conn.Close()
	if res.kind == "relay" {
		if err := meshRelayDial(conn, target.Addr); err != nil {
			return err
		}
	}
	ios.WriteOutLine("已连接（%s）: stdin/stdout ⇄ %s (Ctrl+D / EOF 断开)", res.kind, target.Name)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(conn, ios.In) }()
	go func() { defer wg.Done(); _, _ = io.Copy(ios.Out, conn) }()
	wg.Wait()
	return nil
}

// meshRelayDial 在 relay 连接上写 [4B len][{"dial":addr}] 帧，指示出口节点拨目标。
// 与 relay_stream.go / relay.leaf.go / p2p.writeDialFrame 的帧格式一致。
func meshRelayDial(conn net.Conn, addr string) error {
	head, err := json.Marshal(hub.DialRequest{Dial: addr})
	if err != nil {
		return err
	}
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(head)))
	if _, err := conn.Write(lenBuf); err != nil {
		return err
	}
	_, err = conn.Write(head)
	return err
}

// defaultLocalNodeID 返回本机节点 ID（mesh webrtc 信令来源）。
func defaultLocalNodeID() string {
	return localHostname()
}

// localHostname 返回本机主机名作为默认节点 ID。
func localHostname() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "mesh-node"
	}
	return host
}
