// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/relay"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/spf13/cobra"
)

const (
	// manualSignalingTimeout 是 --manual 场景（文件或 stdin/stdout 交换）信令等待的整体超时。
	// 默认 10 分钟：人工拷文件/复制粘贴 JSON 需要较长窗口。
	manualSignalingTimeout = 10 * time.Minute
)

// discardLogger 返回输出到 io.Discard 的 logger（p2p listen 后台会话用）。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// NewCmdP2P 创建 p2p 父命令：基于 WebRTC 打洞的点对点连接。
// 信令经 hub 的 /api/signal/* 桥，数据面打洞成功后直连（不经过 hub）。
func NewCmdP2P(ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "p2p",
		Short: "WebRTC 点对点直连（经 hub 信令桥打洞）",
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}
	cmd.AddCommand(newCmdP2PConnect(ios))
	cmd.AddCommand(newCmdP2PListen(ios))
	return cmd
}

// p2pFlags 是 p2p 相关命令的公共 flag。
type p2pFlags struct {
	hub  string
	tok  string
	node string
	stun []string
}

func (f *p2pFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.hub, "hub", "", "hub 地址（http(s) 或 ws(s) 均可，如 https://hub.example.com:18083）")
	cmd.Flags().StringVar(&f.tok, "token", "", "信令/中继 token")
	cmd.Flags().StringVar(&f.node, "node-id", "", "本节点 ID（信令 from；默认主机名）")
	cmd.Flags().StringSliceVar(&f.stun, "stun", nil,
		"STUN 服务器地址（可重复/逗号分隔，如 stun:stun.qq.com:3478）；默认 Google+腾讯+小米混合，全不通时请指定本地可达服务器")
}

// applyConfig 应用运行时全局配置（STUN 列表）。在连接创建前调用。
func (f *p2pFlags) applyConfig() {
	if f.stun != nil {
		webrtc.SetSTUNServers(f.stun)
	}
}

func (f *p2pFlags) signaler() *hub.HubSignaler {
	// I40：--hub 传 ws(s):// 时归一到 http(s)://（HubSignaler post/poll 用 http.Client，
	// 对 ws:// 直接报 unsupported protocol scheme）。
	if f.hub != "" {
		if httpBase, _, err := normalizeHubEndpoints(f.hub, ""); err == nil {
			return hub.NewHubSignaler(httpBase, f.tok, f.localNode())
		}
	}
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
func newCmdP2PConnect(ios cli.IOStreams) *cobra.Command {
	var f p2pFlags
	cmd := &cobra.Command{
		Use:   "connect --peer <id> --tcp <addr> [-l :port]",
		Short: "与对端建立 WebRTC 直连（打洞成功则数据面不经 hub）",
		RunE: func(cmd *cobra.Command, args []string) error {
			peer, _ := cmd.Flags().GetString("peer")
			tcpAddr, _ := cmd.Flags().GetString("tcp")
			listenAddr, _ := cmd.Flags().GetString("listen")
			manual, _ := cmd.Flags().GetBool("manual")
			offerFile, _ := cmd.Flags().GetString("offer")
			answerFile, _ := cmd.Flags().GetString("answer")
			if peer == "" || tcpAddr == "" {
				return fmt.Errorf("--peer 与 --tcp 均不能为空")
			}
			ctx := cmd.Context()
			f.applyConfig()

			// 选信令器：--manual 用文件或 stdin/stdout 交换（不依赖 hub）；否则经 hub 信令桥
			var sig webrtc.Signaler
			if manual {
				needFile := offerFile != "" || answerFile != ""
				if needFile && (offerFile == "" || answerFile == "") {
					return fmt.Errorf("--manual 文件模式需要同时提供 --offer 与 --answer")
				}
				if needFile {
					sig = newManualSignaler(offerFile, answerFile, ios)
				} else {
					sig = newManualStdioSignaler(ios)
				}
			} else {
				sig = f.signaler()
			}
			// --manual 需人工拷文件/粘贴 JSON，信令等待放宽到 10 分钟（默认 30s 必然不够）
			if manual {
				webrtc.SetSignalingTimeout(manualSignalingTimeout)
			}
			// 手动模式单次连接：无论打洞成功/失败/panic，退出前都兜底清理本侧写出的 SDP 文件
			if ms, ok := sig.(*manualSignaler); ok {
				defer ms.Cleanup()
			}
			conn, err := webrtc.DialWithSignaler(peer, sig)
			if err != nil {
				return fmt.Errorf("p2p 打洞失败: %w", err)
			}
			defer conn.Close()
			ios.WriteOutLine("p2p 直连已建立: %s ⇄ %s（数据面不经过 hub）", f.localNode(), peer)

			m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleDialer)
			defer m.Close()

			if listenAddr != "" {
				return p2pForward(ctx, m, peer, tcpAddr, listenAddr, ios)
			}
			// 单次模式：stdin/stdout 直通
			return p2pStdio(ctx, m, tcpAddr, ios)
		},
	}
	cmd.Flags().String("peer", "", "对端节点 ID")
	cmd.Flags().String("tcp", "", "对端要出站连接的 TCP 地址（如 target-host:22）")
	cmd.Flags().StringP("listen", "l", "", "本地监听地址（如 :2222）；留空为单次 stdin/stdout 模式")
	cmd.Flags().Bool("manual", false, "手工 SDP 信令（不依赖 hub）：提供 --offer/--answer 走文件交换，否则走 stdin/stdout 粘贴 JSON")
	cmd.Flags().String("offer", "", "--manual 文件模式的 offer SDP 文件路径（需同时给 --answer）")
	cmd.Flags().String("answer", "", "--manual 文件模式的 answer SDP 文件路径（需同时给 --offer）")
	_ = cmd.MarkFlagRequired("peer")
	_ = cmd.MarkFlagRequired("tcp")
	f.add(cmd)
	return cmd
}

// newCmdP2PListen 创建 p2p listen：作为对端等待入站 WebRTC 直连。
func newCmdP2PListen(ios cli.IOStreams) *cobra.Command {
	var f p2pFlags
	cmd := &cobra.Command{
		Use:   "listen",
		Short: "作为对端监听 WebRTC 直连（信令经 hub 或手工 SDP）",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			httpClient := &http.Client{Timeout: 30 * time.Second}
			manual, _ := cmd.Flags().GetBool("manual")
			offerFile, _ := cmd.Flags().GetString("offer")
			answerFile, _ := cmd.Flags().GetString("answer")
			f.applyConfig()

			// 选信令器：--manual 用文件或 stdin/stdout 交换（单次连接，不循环）；否则经 hub 信令桥
			var sig webrtc.Signaler
			if manual {
				needFile := offerFile != "" || answerFile != ""
				if needFile && (offerFile == "" || answerFile == "") {
					return fmt.Errorf("--manual 文件模式需要同时提供 --offer 与 --answer")
				}
				if needFile {
					sig = newManualSignaler(offerFile, answerFile, ios)
				} else {
					sig = newManualStdioSignaler(ios)
				}
			} else {
				sig = f.signaler()
			}

			// --manual 需人工拷文件/粘贴 JSON，信令等待放宽到 10 分钟（默认 30s 必然不够）
			if manual {
				webrtc.SetSignalingTimeout(manualSignalingTimeout)
			}

			// 手动模式单次连接：无论打洞成功/失败/panic，退出前都兜底清理本侧写出的 SDP 文件
			if ms, ok := sig.(*manualSignaler); ok {
				defer ms.Cleanup()
			}

			// 循环 accept：每条 p2p 连接交给 relay.Serve 分发（dial 帧 / HTTP 中继）。
			// 信令失败（如临时网络抖动）时带退避重试，作为常驻服务不应轻易退出。
			delay := reconnectBaseDelay
			for {
				conn, err := webrtc.ListenWithSignaler(f.localNode(), sig)
				if err != nil {
					if ctx.Err() != nil {
						return nil
					}
					// manual 模式单次连接，失败直接返回（文件已消费，重试无意义）
					if manual {
						return fmt.Errorf("p2p 打洞失败: %w", err)
					}
					ios.WriteErrLine("p2p 监听失败，%v 后重试: %v", delay, err)
					select {
					case <-time.After(delay):
						delay *= 2
						if delay > reconnectMaxDelay {
							delay = reconnectMaxDelay
						}
					case <-ctx.Done():
						return nil
					}
					continue
				}
				delay = reconnectBaseDelay
				m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleListener)
				go func() {
					defer m.Close()
					if err := relay.Serve(ctx, m, "http://127.0.0.1:8080", true, httpClient, discardLogger()); err != nil {
						ios.WriteErrLine("p2p 会话结束: %v", err)
					}
				}()
				// manual 模式单次连接：不再进入 accept 循环，但必须阻塞等待连接结束
				// （返回会让 main 退出，直接杀掉 relay.Serve/心跳 goroutine 与 WebRTC 连接）。
				// 阻塞到 mux 关闭（任一侧断开/心跳超时）或 ctx 取消为止，无额外超时。
				if manual {
					select {
					case <-m.Done():
					case <-ctx.Done():
					}
					return nil
				}
			}
		},
	}
	cmd.Flags().Bool("manual", false, "手工 SDP 信令（不依赖 hub）：提供 --offer/--answer 走文件交换，否则走 stdin/stdout 粘贴 JSON")
	cmd.Flags().String("offer", "", "--manual 文件模式的 offer SDP 文件路径（需同时给 --answer）")
	cmd.Flags().String("answer", "", "--manual 文件模式的 answer SDP 文件路径（需同时给 --offer）")
	f.add(cmd)
	return cmd
}

// p2pForward 在已建立的 p2p mux 上做本地端口转发。
func p2pForward(ctx context.Context, m *mux.Mux, peer, tcpAddr, listenAddr string, ios cli.IOStreams) error {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("监听本地端口失败: %w", err)
	}
	defer ln.Close()
	ios.WriteOutLine("端口转发: %s ⇄ p2p(%s) ⇄ %s", listenAddr, peer, tcpAddr)
	for {
		c, aerr := ln.Accept()
		if aerr != nil {
			if ctx.Err() != nil {
				return nil // 优雅取消
			}
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
func p2pStdio(ctx context.Context, m *mux.Mux, tcpAddr string, ios cli.IOStreams) error {
	stream, err := m.Open(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()
	if err := writeDialFrame(stream, tcpAddr); err != nil {
		return err
	}
	ios.WriteOutLine("已连接: stdin/stdout ⇄ p2p ⇄ %s (Ctrl+D / EOF 断开)", tcpAddr)
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(stream, ios.In); done <- struct{}{} }()
	go func() { _, _ = io.Copy(ios.Out, stream); done <- struct{}{} }()
	<-done
	<-done
	return nil
}

// writeDialFrame 在 mux 流上写入 [4B len][{"dial":addr}] 帧（与 relay 协议一致）。
func writeDialFrame(s mux.Stream, addr string) error {
	return writeDialFrameTo(s, addr)
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
	// 一个方向完成即解除另一方向阻塞，防非合作对端永久挂起
	<-done
	_ = local.Close()
	<-done
}
