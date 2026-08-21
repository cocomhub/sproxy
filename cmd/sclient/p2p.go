// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/relay"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/spf13/cobra"
)

// discardLogger 返回输出到 io.Discard 的 logger（p2p listen 后台会话用）。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// NewCmdP2P 创建 p2p 父命令：基于 WebRTC 打洞的点对点连接。
// 信令经 hub 的 /api/signal/* 桥，数据面打洞成功后直连（不经过 hub）。
func NewCmdP2P() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "p2p",
		Short: "WebRTC 点对点直连（经 hub 信令桥打洞）",
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}
	cmd.AddCommand(newCmdP2PConnect())
	cmd.AddCommand(newCmdP2PListen())
	return cmd
}

// p2pFlags 是 p2p 相关命令的公共 flag。
type p2pFlags struct {
	hub  string
	tok  string
	node string
}

func (f *p2pFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.hub, "hub", "", "hub 地址（如 https://sg-vps-1:18083）")
	cmd.Flags().StringVar(&f.tok, "token", "", "信令/中继 token")
	cmd.Flags().StringVar(&f.node, "node-id", "", "本节点 ID（信令 from；默认主机名）")
}

func (f *p2pFlags) signaler() *hub.HubSignaler {
	return hub.NewHubSignaler(f.hub, f.tok, f.localNode())
}

func (f *p2pFlags) localNode() string {
	if f.node != "" {
		return f.node
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "p2p-node"
	}
	return host
}

// newCmdP2PConnect 创建 p2p connect：拨号到对端建立 WebRTC 直连。
func newCmdP2PConnect() *cobra.Command {
	var f p2pFlags
	cmd := &cobra.Command{
		Use:   "connect --peer <id> --tcp <addr> [-l :port]",
		Short: "与对端建立 WebRTC 直连（打洞成功则数据面不经 hub）",
		RunE: func(cmd *cobra.Command, args []string) error {
			peer, _ := cmd.Flags().GetString("peer")
			tcpAddr, _ := cmd.Flags().GetString("tcp")
			listenAddr, _ := cmd.Flags().GetString("listen")
			if peer == "" || tcpAddr == "" {
				return fmt.Errorf("--peer 与 --tcp 均不能为空")
			}
			ctx := cmd.Context()

			conn, err := webrtc.DialWithSignaler(peer, f.signaler())
			if err != nil {
				return fmt.Errorf("p2p 打洞失败: %w", err)
			}
			defer conn.Close()
			fmt.Printf("p2p 直连已建立: %s ⇄ %s（数据面不经过 hub）\n", f.localNode(), peer)

			m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleDialer)
			defer m.Close()

			if listenAddr != "" {
				return p2pForward(ctx, m, peer, tcpAddr, listenAddr)
			}
			// 单次模式：stdin/stdout 直通
			return p2pStdio(ctx, m, tcpAddr)
		},
	}
	cmd.Flags().String("peer", "", "对端节点 ID")
	cmd.Flags().String("tcp", "", "对端要出站连接的 TCP 地址（如 sg-vps-2:22）")
	cmd.Flags().StringP("listen", "l", "", "本地监听地址（如 :2222）；留空为单次 stdin/stdout 模式")
	_ = cmd.MarkFlagRequired("peer")
	_ = cmd.MarkFlagRequired("tcp")
	f.add(cmd)
	return cmd
}

// newCmdP2PListen 创建 p2p listen：作为对端等待入站 WebRTC 直连。
func newCmdP2PListen() *cobra.Command {
	var f p2pFlags
	cmd := &cobra.Command{
		Use:   "listen",
		Short: "作为对端监听 WebRTC 直连（信令经 hub，出口模式）",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			httpClient := &http.Client{Timeout: 30 * 1_000_000_000}

			// 循环 accept：每条 p2p 连接交给 relay.Serve 分发（dial 帧 / HTTP 中继）
			for {
				conn, err := webrtc.ListenWithSignaler(f.localNode(), f.signaler())
				if err != nil {
					fmt.Printf("p2p 监听失败: %v\n", err)
					return err
				}
				m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleListener)
				go func() {
					defer m.Close()
					if err := relay.Serve(ctx, m, "http://127.0.0.1:8080", true, httpClient, discardLogger()); err != nil {
						fmt.Printf("p2p 会话结束: %v\n", err)
					}
				}()
			}
		},
	}
	f.add(cmd)
	return cmd
}

// p2pForward 在已建立的 p2p mux 上做本地端口转发。
func p2pForward(ctx context.Context, m *mux.Mux, peer, tcpAddr, listenAddr string) error {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("监听本地端口失败: %w", err)
	}
	defer ln.Close()
	fmt.Printf("端口转发: %s ⇄ p2p(%s) ⇄ %s\n", listenAddr, peer, tcpAddr)
	for {
		c, aerr := ln.Accept()
		if aerr != nil {
			return aerr
		}
		go func(local net.Conn) {
			defer local.Close()
			stream, oerr := m.Open(ctx)
			if oerr != nil {
				return
			}
			defer stream.Close()
			if werr := writeDialFrame(stream, tcpAddr); werr != nil {
				return
			}
			pump(local, stream)
		}(c)
	}
}

// p2pStdio 单次模式：stdin/stdout 与远端直通。
func p2pStdio(ctx context.Context, m *mux.Mux, tcpAddr string) error {
	stream, err := m.Open(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()
	if err := writeDialFrame(stream, tcpAddr); err != nil {
		return err
	}
	fmt.Printf("已连接: stdin/stdout ⇄ p2p ⇄ %s (Ctrl+D / EOF 断开)\n", tcpAddr)
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(stream, os.Stdin); done <- struct{}{} }()
	go func() { _, _ = io.Copy(os.Stdout, stream); done <- struct{}{} }()
	<-done
	<-done
	return nil
}

// writeDialFrame 在 mux 流上写入 [4B len][{"dial":addr}] 帧（与 relay 协议一致）。
func writeDialFrame(s mux.Stream, addr string) error {
	head, err := json.Marshal(hub.DialRequest{Dial: addr})
	if err != nil {
		return err
	}
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(head)))
	if _, err := s.Write(lenBuf); err != nil {
		return err
	}
	_, err = s.Write(head)
	return err
}

// pump 双向泵送：本地 socket <-> mux 流。
func pump(local net.Conn, s mux.Stream) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(s, local)
		_ = s.CloseWrite()
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(local, s)
		done <- struct{}{}
	}()
	<-done
	<-done
}
