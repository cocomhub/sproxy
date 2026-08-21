// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// NewCmdMesh 创建 mesh 父命令：基于 hub 服务注册表的服务发现与连接。
func NewCmdMesh(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mesh",
		Short: "mesh 服务发现与连接（经 hub 中继）",
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}
	cmd.AddCommand(newCmdMeshConnect(factory, ios))
	cmd.AddCommand(newCmdMeshStatus(factory, ios))
	return cmd
}

// newCmdMeshConnect 创建 mesh connect：按服务名经 hub 建立流中继。
func newCmdMeshConnect(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "connect <service> [-l :port]",
		Short: "连接到 mesh 服务（经 hub 流中继）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := args[0]
			listenAddr, _ := cmd.Flags().GetString("listen")
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

			if listenAddr != "" {
				return meshForwardListen(cmd, svc, target, listenAddr, ios)
			}
			return meshStdioOnce(cmd, svc, target, ios)
		},
	}
	cmd.Flags().StringP("listen", "l", "", "本地监听地址（如 :2222）；留空为单次 stdin/stdout 模式")
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

// meshForwardListen 监听本地端口，每个入站连接独立建立一条 mesh 流中继。
func meshForwardListen(cmd *cobra.Command, svc *client.FileClient, target *client.MeshService, listenAddr string, ios cli.IOStreams) error {
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
			conn, cerr := svc.RelayStream(ctx, target.Node, target.Addr)
			if cerr != nil {
				ios.WriteErrLine("建立 mesh 流失败: %v", cerr)
				return
			}
			defer conn.Close()
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); _, _ = io.Copy(conn, c) }()
			go func() { defer wg.Done(); _, _ = io.Copy(c, conn) }()
			wg.Wait()
		}(local)
	}
}

// meshStdioOnce 单次模式：stdin/stdout 与一条 mesh 流直通。
func meshStdioOnce(cmd *cobra.Command, svc *client.FileClient, target *client.MeshService, ios cli.IOStreams) error {
	conn, err := svc.RelayStream(cmd.Context(), target.Node, target.Addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	ios.WriteOutLine("已连接: stdin/stdout ⇄ %s (Ctrl+D / EOF 断开)", target.Name)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(conn, ios.In) }()
	go func() { defer wg.Done(); _, _ = io.Copy(ios.Out, conn) }()
	wg.Wait()
	return nil
}
