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
	"strings"
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

	// p2pPumpGracePeriod 是 pump 第二方向完成收尾的宽限期（对齐 leaf.go pump 的
	// C1 修复）：首方向完成（已传播半关闭）后，第二方向需在此时间内完成；超时
	// 视为对端非合作，强制关闭两端防 goroutine / FD 泄漏。长连接（双向持续
	// 活跃）不触发宽限期——计时器只在某方向完成且另一方向仍空闲时启动。
	p2pPumpGracePeriod = 60 * time.Second
)

// discardLogger 返回输出到 io.Discard 的 logger（供测试桩使用，如 mesh_test 的
// hub 测试服务器）。p2p listen 的 relay 会话日志已改用经 ios.ErrOut 输出的
// serveLogger（I46），不再吞诊断。
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

// signaler 构造 hub 信令器。
// S64：空 hub 显式报错（原来直接 NewHubSignaler("")，post/poll 时才报
// unsupported protocol scheme 等晦涩错误）；normalize 失败（畸形 URL/未知
// scheme）透出，不再被 if ...; err == nil 静默吞掉。
func (f *p2pFlags) signaler() (*hub.HubSignaler, error) {
	if f.hub == "" {
		return nil, fmt.Errorf("--hub 不能为空（p2p 信令依赖 hub；无 hub 场景请用 --manual）")
	}
	// I40：--hub 传 ws(s):// 时归一到 http(s)://（HubSignaler post/poll 用 http.Client，
	// 对 ws:// 直接报 unsupported protocol scheme）。
	httpBase, _, err := normalizeHubEndpoints(f.hub, "")
	if err != nil {
		return nil, fmt.Errorf("解析 hub 地址失败: %w", err)
	}
	return hub.NewHubSignaler(httpBase, f.tok, f.localNode()), nil
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
					if offerFile == answerFile {
						// S67：--offer 与 --answer 同路径会在 SendOffer 后 WaitAnswer 读到
						// 同一文件（type 不匹配），或对端重写导致误读——前置拒绝。
						return fmt.Errorf("--offer 与 --answer 不能指向同一路径（文件交换需两个独立文件）")
					}
					sig = newManualSignaler(offerFile, answerFile, ios)
				} else {
					sig = newManualStdioSignaler(ios)
				}
			} else {
				s, serr := f.signaler()
				if serr != nil {
					return serr
				}
				sig = s
			}
			// --manual 需人工拷文件/粘贴 JSON，信令等待放宽到 10 分钟（默认 30s 必然不够）
			if manual {
				webrtc.SetSignalingTimeout(manualSignalingTimeout)
				// S69：命令结束恢复默认超时，防全局泄漏污染库内嵌场景与后续测试。
				defer webrtc.ResetSignalingTimeout()
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
			services, _ := cmd.Flags().GetStringArray("service")
			dialAllowCIDRs, _ := cmd.Flags().GetStringArray("dial-allow-cidr")
			f.applyConfig()

			// I46：relay 会话诊断日志经 ios.ErrOut 输出（用户可见 + 可测试），带 node
			// 上下文便于多会话区分；丢弃日志会让出口拨号拒绝/失败原因完全不可见。
			serveLogger := slog.New(slog.NewTextHandler(ios.ErrOut, nil)).With("node", f.localNode())
			// I45：出口拨号策略——--service 宣告地址精确放行 + --dial-allow-cidr 网段放行；
			// 无任何配置时 opts 为空 → Serve 回落默认 DialAllowed（仅公网），向后兼容。
			// DialResultFrames 保持 false（webrtc 直连数据流约束，见 relay/leaf.go 注释）。
			serveOpts := buildP2PServeOpts(services, dialAllowCIDRs, ios)

			// 选信令器：--manual 用文件或 stdin/stdout 交换（单次连接，不循环）；否则经 hub 信令桥
			var sig webrtc.Signaler
			if manual {
				needFile := offerFile != "" || answerFile != ""
				if needFile && (offerFile == "" || answerFile == "") {
					return fmt.Errorf("--manual 文件模式需要同时提供 --offer 与 --answer")
				}
				if needFile {
					if offerFile == answerFile {
						// S67：--offer 与 --answer 同路径会在 SendAnswer 后 WaitOffer 读到
						// 同一文件（type 不匹配），或对端重写导致误读——前置拒绝。
						return fmt.Errorf("--offer 与 --answer 不能指向同一路径（文件交换需两个独立文件）")
					}
					sig = newManualSignaler(offerFile, answerFile, ios)
				} else {
					sig = newManualStdioSignaler(ios)
				}
			} else {
				s, serr := f.signaler()
				if serr != nil {
					return serr
				}
				sig = s
			}

			// --manual 需人工拷文件/粘贴 JSON，信令等待放宽到 10 分钟（默认 30s 必然不够）
			if manual {
				webrtc.SetSignalingTimeout(manualSignalingTimeout)
				// S69：命令结束恢复默认超时，防全局泄漏污染库内嵌场景与后续测试。
				defer webrtc.ResetSignalingTimeout()
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
					if err := relay.Serve(ctx, m, "http://127.0.0.1:8080", true, httpClient, serveLogger, serveOpts...); err != nil {
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
	cmd.Flags().StringArray("service", nil,
		"出口拨号白名单：宣告的服务地址（格式 name:addr，可重复；仅取 addr 精确放行，不注册到 hub）")
	cmd.Flags().StringArray("dial-allow-cidr", nil,
		"出口拨号白名单网段（如 192.168.0.0/16；配合放行内网服务，默认仅公网）")
	f.add(cmd)
	return cmd
}

// buildP2PServeOpts 构造 p2p listen 的 relay.ServeOptions：--service 宣告地址
// 精确放行 + --dial-allow-cidr 网段放行（复用 relay.NewServiceDialPolicy）。
// 无任何放行配置（或全部无效）时返回 nil → Serve 回落默认 DialAllowed（仅公网）。
//
// 注意：--service 仅作拨号白名单，不宣告到 hub（p2p listen 不注册节点），
// 语义与 relay start 的注册宣告解耦（I45 子决策）。
func buildP2PServeOpts(services, dialAllowCIDRs []string, ios cli.IOStreams) []relay.ServeOptions {
	var serviceAddrs []string
	for _, svc := range services {
		_, addr, ok := strings.Cut(svc, ":")
		if !ok || addr == "" {
			ios.WriteErrLine("忽略无效服务（应为 name:addr）: %s", svc)
			continue
		}
		if host, _, err := net.SplitHostPort(addr); err != nil || host == "" {
			ios.WriteErrLine("忽略无效服务 addr（应为 host:port）: %s", svc)
			continue
		}
		serviceAddrs = append(serviceAddrs, addr)
	}
	if len(serviceAddrs) == 0 && len(dialAllowCIDRs) == 0 {
		return nil
	}
	return []relay.ServeOptions{
		{DialPolicy: relay.NewServiceDialPolicy(dialAllowCIDRs, serviceAddrs)},
	}
}

// p2pForward 在已建立的 p2p mux 上做本地端口转发。
func p2pForward(ctx context.Context, m *mux.Mux, peer, tcpAddr, listenAddr string, ios cli.IOStreams) error {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("监听本地端口失败: %w", err)
	}
	defer ln.Close()
	ios.WriteOutLine("端口转发: %s ⇄ p2p(%s) ⇄ %s", listenAddr, peer, tcpAddr)

	// I44：会话死亡（m.Done()）或 ctx 取消时关闭 listener，解除 ln.Accept() 永久
	// 阻塞。cobra ctx 默认是 context.Background()（进程内永不取消），因此必须监听
	// m.Done() 才能感知 p2p 会话死亡（对端断开/心跳超时/WebRTC 连接关闭）——
	// 否则命令僵尸常驻、defer ln.Close() 不执行。stopAccept 保证函数提前返回
	// （监听错误）时 goroutine 也退出，不留活体。
	stopAccept := make(chan struct{})
	defer close(stopAccept)
	go func() {
		select {
		case <-ctx.Done():
		case <-m.Done():
		case <-stopAccept:
		}
		_ = ln.Close()
	}()

	for {
		c, aerr := ln.Accept()
		if aerr != nil {
			// 三态区分：主动关闭（ctx 取消 / 会话死亡）返回 nil，外部监听错误透出。
			select {
			case <-ctx.Done():
				return nil // 优雅取消
			case <-m.Done():
				return nil // 会话已死亡
			default:
				return aerr
			}
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
	// H1-C2：方向区分通道——对端断开（outDone）→ 会话结束立即返回，不再挂起；
	// 本地 stdin 读完（inDone，如 EOF/管道结束）→ 等待对端把剩余响应写完
	// （保留 `echo x | p2p connect` 的响应语义）。原 `<-done; <-done` 在对端断开
	// 但 stdin 未 EOF 时永久挂起（对齐 meshStdioOnce 的 I38 修复范本）。
	inDone := make(chan struct{})
	outDone := make(chan struct{})
	go func() { defer close(inDone); _, _ = io.Copy(stream, ios.In) }()
	go func() { defer close(outDone); _, _ = io.Copy(ios.Out, stream) }()
	select {
	case <-outDone: // 对端断开：会话结束
	case <-inDone: // 本地 stdin 读完：等对端把剩余数据写完
		<-outDone
	}
	return nil
}

// writeDialFrame 在 mux 流上写入 [4B len][{"dial":addr}] 帧（与 relay 协议一致）。
func writeDialFrame(s mux.Stream, addr string) error {
	return writeDialFrameTo(s, addr)
}

// pump 双向泵送：本地 socket <-> mux 流。
//
// 关闭语义（S63，对齐 leaf.go pump 的 C1 修复）：
//   - 每个方向 io.Copy 完成后向对端 CloseWrite 传播半关闭（TCP FIN / 流 EOF），
//     而不是立即全关——让在途的响应仍可被另一方向读回（不截断）。
//   - 首方向完成后武装 grace 宽限期计时器：宽限期内另一方向完成则正常收尾；
//     超时视为对端非合作（对 FIN 不回应），强制关闭两端解除 Read 阻塞，
//     防 goroutine / FD 泄漏。长连接（双向持续活跃）不触发计时器，不误断。
//   - 正常路径不在此显式全关两端，由调用方 defer local.Close() / stream.Close() 收尾。
func pump(local net.Conn, s mux.Stream) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(s, local)
		_ = s.CloseWrite()
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(local, s)
		if tc, ok := local.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		} else {
			_ = local.Close()
		}
		done <- struct{}{}
	}()

	remaining := 2
	var timeoutCh <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for remaining > 0 {
		select {
		case <-done:
			remaining--
			if remaining == 1 {
				// 一个方向完成：启动宽限期等待另一半完成半关闭收尾。
				timer = time.NewTimer(p2pPumpGracePeriod)
				timeoutCh = timer.C
			}
		case <-timeoutCh:
			// 非合作对端：强制关闭两端，解除 local.Read / s.Read 阻塞。
			_ = local.Close()
			_ = s.Close()
			for remaining > 0 { // 关闭后 Read/Write 立即返回，等待 goroutine 退出
				<-done
				remaining--
			}
			return
		}
	}
}
