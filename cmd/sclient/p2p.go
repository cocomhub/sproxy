// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/iostream"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mesh"
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

// NewCmdP2P 创建 p2p 父命令：基于 WebRTC 打洞的点对点连接。
// 信令经 hub 的 /api/signal/* 桥，数据面打洞成功后直连（不经过 hub）。
// cfgSvc 为可选配置提供者（P2-配置3）：--hub/--node-id 未显式指定时从配置
// hub_url/node_id 回落；hub 注册准入用 SproxySig AK/SK（根 --access-key/--access-key-secret 或配置）。
func NewCmdP2P(ios cli.IOStreams, cfgSvc ...ConfigProvider) *cobra.Command {
	var provider ConfigProvider
	if len(cfgSvc) > 0 {
		provider = cfgSvc[0]
	}
	cmd := &cobra.Command{
		Use:   "p2p",
		Short: "WebRTC 点对点直连（经 hub 信令桥打洞）",
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}
	cmd.AddCommand(newCmdP2PConnect(ios, provider))
	cmd.AddCommand(newCmdP2PListen(ios, provider))
	return cmd
}

// p2pFlags 是 p2p 相关命令的公共 flag。
type p2pFlags struct {
	hub      string
	node     string
	stun     []string
	turn     []string
	turnUser string
	turnPass string
}

func (f *p2pFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.hub, "hub", "", "hub 地址（http(s) 或 ws(s) 均可，如 https://hub.example.com:18083）")
	cmd.Flags().StringVar(&f.node, "node-id", "", "本节点 ID（信令 from；默认主机名）")
	cmd.Flags().StringSliceVar(&f.stun, "stun", nil,
		"STUN 服务器地址（可重复/逗号分隔，如 stun:stun.qq.com:3478）；默认 Google+腾讯+小米混合，全不通时请指定本地可达服务器")
	cmd.Flags().StringSliceVar(&f.turn, "turn", nil,
		"TURN 中继服务器地址（可重复/逗号分隔，如 turn:relay.example.com:3478）；需配合 --turn-user/--turn-pass，提升对称 NAT 下打洞成功率")
	cmd.Flags().StringVar(&f.turnUser, "turn-user", "", "TURN 用户名（静态密码模式，配 --turn/--turn-pass 使用）")
	cmd.Flags().StringVar(&f.turnPass, "turn-pass", "", "TURN 密码（静态密码模式，配 --turn/--turn-user 使用）")
}

// applyConfig 应用运行时全局配置（STUN/TURN）。在连接创建前调用。
// TURN 相关仅对显式提供的 flag 生效：--turn 非空才调 SetTURNServers，
// --turn-user/--turn-pass 非空才调 SetTURNCredential（缺 flag = 保持现状）。
func (f *p2pFlags) applyConfig() {
	if f.stun != nil {
		webrtc.SetSTUNServers(f.stun)
	}
	if f.turn != nil {
		webrtc.SetTURNServers(f.turn)
	}
	if f.turnUser != "" || f.turnPass != "" {
		webrtc.SetTURNCredential(f.turnUser, f.turnPass)
	}
}

// applyConfigFallback 用配置文件补齐未显式指定的 hub/node-id
// （优先级：CLI flag > 配置文件；P2-配置3）。hub 注册准入的 SproxySig AK/SK 由
// registerSignaler 从配置/根 flag 获取。cfgSvc 为 nil 时是 no-op。
func (f *p2pFlags) applyConfigFallback(cfgSvc ConfigProvider) {
	if cfgSvc == nil {
		return
	}
	cfg, err := cfgSvc.LoadConfig()
	if err != nil {
		return
	}
	if f.hub == "" {
		f.hub = cfg.HubURL
	}
	if f.node == "" {
		f.node = cfg.NodeID
	}
}

// requireHub 前置校验 --hub 非空（S64 语义保留）：非 manual 模式信令依赖 hub，
// 空 hub 直接报错，不再把晦涩的 unsupported protocol scheme 留到注册/信令阶段。
func (f *p2pFlags) requireHub() error {
	if f.hub == "" {
		return fmt.Errorf("--hub 不能为空（p2p 信令依赖 hub；无 hub 场景请用 --manual）")
	}
	return nil
}

// registerSignaler 经 hub 自动注册并返回携带 per-node secret 的信令器（B17）。
// 调用方须先 requireHub() 校验 hub 非空。exactNode=true 时注册成 f.localNode()
// 原样（p2p listen 的被寻址方需稳定 ID 供 --peer 寻址）；false 用临时 node_id
// （p2p connect 的 Answer 回给 offerFrom，对端无需预知本端 ID）。
func (f *p2pFlags) registerSignaler(ctx context.Context, cmd *cobra.Command, cfgSvc ConfigProvider, exactNode bool) (*mesh.TempRegistration, error) {
	insecure, _ := cmd.Flags().GetBool("insecure")
	ak, _ := cmd.Root().PersistentFlags().GetString("access-key")
	sk, _ := cmd.Root().PersistentFlags().GetString("access-key-secret")
	if cfgSvc != nil {
		if cfg, cerr := cfgSvc.LoadConfig(); cerr == nil {
			if ak == "" {
				ak = cfg.AccessKey
			}
			if sk == "" {
				sk = cfg.AccessKeySecret
			}
		}
	}
	return mesh.AutoRegister(ctx, mesh.AutoRegisterParams{
		HubURL:          f.hub,
		AccessKey:       ak,
		AccessKeySecret: sk,
		NodeID:          f.localNode(),
		Prefix:          "p2p",
		ExactNode:       exactNode,
		Insecure:        insecure,
	})
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
func newCmdP2PConnect(ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
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
			f.applyConfigFallback(cfgSvc) // P2-配置3：未显式指定的 hub/token/node-id 取配置文件
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
				// B17：经 hub 信令前自动注册自身（声明 per-node-secret 能力），从
				// REG_OK:<secret> 拿 per-node secret 供 B3 服务端信令身份校验。
				// 用临时 node_id（p2p-<base>-<nano>），对端无需预知本端 ID。
				if err := f.requireHub(); err != nil {
					return err
				}
				reg, rerr := f.registerSignaler(ctx, cmd, cfgSvc, false)
				if rerr != nil {
					return fmt.Errorf("webrtc 信令注册失败: %w", rerr)
				}
				defer func() { _ = reg.Closer() }()
				sig = reg.Signaler
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
	cmd.Flags().StringP("listen", "l", "", "本地监听地址（如 127.0.0.1:2222；裸 :2222 归一为 127.0.0.1:2222）；留空为单次 stdin/stdout 模式")
	cmd.Flags().Bool("manual", false, "手工 SDP 信令（不依赖 hub）：提供 --offer/--answer 走文件交换，否则走 stdin/stdout 粘贴 JSON")
	cmd.Flags().String("offer", "", "--manual 文件模式的 offer SDP 文件路径（需同时给 --answer）")
	cmd.Flags().String("answer", "", "--manual 文件模式的 answer SDP 文件路径（需同时给 --offer）")
	_ = cmd.MarkFlagRequired("peer")
	_ = cmd.MarkFlagRequired("tcp")
	f.add(cmd)
	return cmd
}

// newCmdP2PListen 创建 p2p listen：作为对端等待入站 WebRTC 直连。
func newCmdP2PListen(ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
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
			f.applyConfigFallback(cfgSvc) // P2-配置3：未显式指定的 hub/token/node-id 取配置文件
			f.applyConfig()

			// I46：relay 会话诊断日志经 ios.ErrOut 输出（用户可见 + 可测试），带 node
			// 上下文便于多会话区分；丢弃日志会让出口拨号拒绝/失败原因完全不可见。
			serveLogger := slog.New(slog.NewTextHandler(ios.ErrOut, nil)).With("node", f.localNode())
			// I45：出口拨号策略——--service 宣告地址精确放行 + --dial-allow-cidr 网段放行
			// + 虚拟 IP NAT（selfVIP 初始无效；经 hub 信令注册后由 REG_OK 下发再更新）。
			// 无任何配置时 opts 为空 → Serve 回落默认 DialAllowed（仅公网），向后兼容。
			// DialResultFrames 保持 false（webrtc 直连数据流约束，见 relay/leaf.go 注释）。
			serveOpts := buildP2PServeOpts(services, dialAllowCIDRs, netip.Addr{}, ios)

			// 选信令器：--manual 用文件或 stdin/stdout 交换（单次连接，不循环）；否则经 hub 信令桥
			var sig webrtc.Signaler
			var reg *mesh.TempRegistration // 自动注册（exact node），accept 循环内重注册时替换
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
				// B17：经 hub 信令前自动注册自身（声明 per-node-secret 能力）。p2p listen
				// 是被寻址方，必须用精确 node_id（f.localNode()）注册，否则 connect 的
				// --peer <id> 无法寻址。注册连接保活整个 accept 循环，closer 在命令退出时
				// 关闭；信令 400/403（节点被 hub 移除，secret 已轮换）时在重连退避循环内
				// 重注册自愈。
				if err := f.requireHub(); err != nil {
					return err
				}
				var rerr error
				reg, rerr = f.registerSignaler(ctx, cmd, cfgSvc, true)
				if rerr != nil {
					return rerr
				}
				defer func() { _ = reg.Closer() }()
				sig = reg.Signaler
				// 虚拟 IP NAT：selfVIP 由 REG_OK 下发（稳定常驻注册），更新出口拨号策略。
				serveOpts = buildP2PServeOpts(services, dialAllowCIDRs, reg.VirtualIP, ios)
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
					if errors.Is(err, webrtc.ErrNoIncomingConnection) {
						// P1-11：空闲超时（signalingTimeout 内无对端发起连接）——不是失败。
						// 旧实现把 30s 空闲当失败，无条件重注册 + per-node secret 轮换，
						// hub 每次替换都关旧 WS，注册连接持续抖动。空闲时保持注册、重置
						// 退避、继续监听；仅真实失败（如信令 400/403，节点被 hub 移除）才
						// 走下方重注册自愈。
						delay = reconnectBaseDelay
						continue
					}
					ios.WriteErrLine("p2p 监听失败，%v 后重试: %v", delay, err)
					// B17：节点可能已被 hub 移除（注册 WS 断 / 心跳超时），per-node secret 已
					// 轮换——信令 400/403 时在重连退避循环内重注册自愈；重注册失败不阻断
					// 退避（保持既有网络抖动重试行为），下一轮循环继续尝试。
					if reg2, rerr2 := f.registerSignaler(ctx, cmd, cfgSvc, true); rerr2 == nil {
						_ = reg.Closer()
						reg, sig = reg2, reg2.Signaler
						// selfVIP 随重注册可能轮换，同步更新出口拨号策略。
						serveOpts = buildP2PServeOpts(services, dialAllowCIDRs, reg.VirtualIP, ios)
					} else {
						ios.WriteErrLine("p2p 重注册失败: %v", rerr2)
					}
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
// 精确放行 + --dial-allow-cidr 网段放行 + 虚拟 IP NAT（selfVIP 由 AutoRegister 的
// REG_OK 下发；默认 CGNAT 子网，宣告端口自动开放）。复用 relay.NewVirtualIPDialPolicy
// （内部已含宣告地址精确匹配与公网/CIDR 回落）。
// 无任何放行配置且 selfVIP 无效时返回 nil → Serve 回落默认 DialAllowed（仅公网）。
//
// 注意：--service 仅作拨号白名单，不宣告到 hub（p2p listen 不注册节点），
// 语义与 relay start 的注册宣告解耦（I45 子决策）。
func buildP2PServeOpts(services, dialAllowCIDRs []string, selfVIP netip.Addr, ios cli.IOStreams) []relay.ServeOptions {
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
	if len(serviceAddrs) == 0 && len(dialAllowCIDRs) == 0 && !selfVIP.IsValid() {
		return nil
	}
	vipSubnet := netip.MustParsePrefix(hub.DefaultVirtualSubnet)
	return []relay.ServeOptions{
		{DialPolicy: relay.NewVirtualIPDialPolicy(vipSubnet, selfVIP, nil, dialAllowCIDRs, serviceAddrs)},
	}
}

// p2pForward 在已建立的 p2p mux 上做本地端口转发。
func p2pForward(ctx context.Context, m *mux.Mux, peer, tcpAddr, listenAddr string, ios cli.IOStreams) error {
	// 裸 :port 归一为 127.0.0.1:port（loopback 安全默认，防 LAN 暴露 + Windows
	// 防火墙弹窗），与 mesh connect / relay dial 对齐（S56）；显式通配地址:port /
	// 具体 IP 保持原样。
	listenAddr = iostream.NormalizeListenAddr(listenAddr)
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
			if werr := mesh.WriteDialFrame(stream, tcpAddr); werr != nil {
				return
			}
			iostream.Pump(local, stream, iostream.PumpGrace)
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
	if err := mesh.WriteDialFrame(stream, tcpAddr); err != nil {
		return err
	}
	ios.WriteOutLine("已连接: stdin/stdout ⇄ p2p ⇄ %s (Ctrl+D / EOF 断开)", tcpAddr)
	// H1-C2：方向区分通道——对端断开（outDone）→ 会话结束立即返回，不再挂起；
	// 本地 stdin 读完（inDone，如 EOF/管道结束）→ 等待对端把剩余响应写完
	// （保留 `echo x | p2p connect` 的响应语义）。原 `<-done; <-done` 在对端断开
	// 但 stdin 未 EOF 时永久挂起（对齐 meshStdioOnce 的 I38 修复范本）。
	inDone := make(chan struct{})
	outDone := make(chan struct{})
	go func() {
		defer close(inDone)
		_, _ = io.Copy(stream, ios.In)
		// P0-5：stdin EOF 后传播半关闭（流 EOF），否则对端永远等不到"输入写完"，
		// <outDone 永久挂起（与 meshStdioOnce / relayDialOnce 同款修复）。
		iostream.CloseWrite(stream)
	}()
	go func() { defer close(outDone); _, _ = io.Copy(ios.Out, stream) }()
	select {
	case <-outDone: // 对端断开：会话结束
	case <-inDone: // 本地 stdin 读完：半关闭已传播，等对端把剩余数据写完
		<-outDone
	}
	return nil
}
