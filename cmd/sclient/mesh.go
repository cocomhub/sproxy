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
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws" // 注册 WebSocket 传输层（自动注册拨号用）
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
	// DialWithSignalerCtx 内部用 defaultICETimeout（30s）作为信令等待上限，
	// 失败（对端无 p2p listen / 打洞不成功）后回落 hub 中继。
	if signaler != nil && target.Node != "" {
		// ctx 预检：已取消则不触发 webrtc（避免无谓地启动 PeerConnection / STUN gathering）。
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// P1-12：探测受 meshWebRTCProbeTimeout（10s）约束——目标仅跑 relay start 时
		// 不再白等 30s 信令超时才回落中继；10s 覆盖 LAN/常见 NAT 打洞，更慢的直连
		// 回落 hub 中继（仍可用）。直连建立后用完整 ctx 开 mux 流（探测超时只约束
		// 拨号阶段，不约束已建立连接的数据面初始化）。
		probeCtx, probeCancel := context.WithTimeout(ctx, meshWebRTCProbeTimeout)
		conn, err := webrtc.DialWithSignalerCtx(probeCtx, target.Node, signaler)
		probeCancel()
		if err == nil {
			res, serr := meshWebRTCStream(ctx, conn, target.Addr)
			if serr != nil {
				// 直连已建立但 mux 流打开/拨号帧写入失败：关闭直连，回落中继。
				_ = conn.Close()
				slog.Debug("webrtc 直连 mux 流建立失败，回落 hub 中继", "error", serr, "target_node", target.Node)
			} else {
				return res, nil
			}
		}
		if ctx.Err() != nil {
			// ctx 取消（用户中断/命令超时）：不再尝试中继，直接返回。
			return nil, ctx.Err()
		}
		// 打洞失败回落中继（S57：不静默吞掉诊断，--verbose 下可见）。
		slog.Debug("webrtc 打洞失败，回落 hub 中继", "error", err, "target_node", target.Node)
	}
	conn, err := svc.RelayStream(ctx, target.Node, target.Addr)
	if err != nil {
		return nil, err
	}
	return &meshDialResult{conn: conn, kind: "relay"}, nil
}

// meshWebRTCStream 在已建立的 WebRTC 直连上打开 mux 流并写好拨号帧（P0-1 修复）。
//
// 协议对齐 p2p connect（p2p.go:197）：数据面必须经 mux 分帧。mesh connect 曾把
// [4B len][{"dial":addr}] 拨号帧以裸字节写在 DataChannel 上，而对端 p2p listen
// 用 mux.New(webrtc.ConnAsXfer) 按帧消费——帧协议载体错位，直连数据面 100% 失败
// （对端 readLoop 报 frame length mismatch 后拆会话，且因拨号"已成功"不回落
// 中继，纯坏路径）。这里与 p2p 一致：先 mux.New 包装，再在流上写拨号帧，
// 对端 relay.Serve 经流读到后出站拨号。
func meshWebRTCStream(ctx context.Context, conn *webrtc.Conn, addr string) (*meshDialResult, error) {
	m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleDialer)
	stream, err := m.Open(ctx)
	if err != nil {
		_ = m.Close()
		return nil, fmt.Errorf("打开 webrtc mux 流失败: %w", err)
	}
	if err := writeDialFrame(stream, addr); err != nil {
		_ = m.Close()
		return nil, fmt.Errorf("写 webrtc 拨号帧失败: %w", err)
	}
	return &meshDialResult{conn: &muxStreamConn{stream: stream, mux: m}, kind: "webrtc"}, nil
}

// muxStreamConn 把 mux.Stream 适配为 net.Conn（mesh webrtc 直连数据面）。
// Close 关闭整个 mux（连带关闭流与底层 WebRTC 连接）；CloseWrite 向对端传播
// 半关闭（流 EOF），供 pump 的 C1 半关闭收尾路径使用。
type muxStreamConn struct {
	stream mux.Stream
	mux    *mux.Mux
}

func (c *muxStreamConn) Read(p []byte) (int, error)  { return c.stream.Read(p) }
func (c *muxStreamConn) Write(p []byte) (int, error) { return c.stream.Write(p) }
func (c *muxStreamConn) Close() error                { return c.mux.Close() }
func (c *muxStreamConn) CloseWrite() error           { return c.stream.CloseWrite() }

type muxStreamAddr struct{}

func (muxStreamAddr) Network() string { return "mux" }
func (muxStreamAddr) String() string  { return "mux" }

func (c *muxStreamConn) LocalAddr() net.Addr                { return muxStreamAddr{} }
func (c *muxStreamConn) RemoteAddr() net.Addr               { return muxStreamAddr{} }
func (c *muxStreamConn) SetDeadline(_ time.Time) error      { return nil }
func (c *muxStreamConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *muxStreamConn) SetWriteDeadline(_ time.Time) error { return nil }

const (
	// meshTargetTTL 是 mesh 服务解析缓存的新鲜窗口。过期后下一次 resolve 触发
	// 重新拉取 /api/hub/services，使 relay 节点下线/重上线的变化被 mesh 感知。
	meshTargetTTL = 3 * time.Second
	// meshResolveTimeout 是单次服务解析的网络超时，防止 hub 无响应拖住连接建立。
	meshResolveTimeout = 5 * time.Second
	// meshWebRTCProbeTimeout 是 webrtc 直连探测的超时上限（P1-12）。
	// DialWithSignalerCtx 内部信令等待默认 30s；目标仅跑 relay start（不消费信令
	// 收件箱）时每条连接都白等满 30s 才回落中继（端口转发模式每入站连接一次）。
	// 10s 覆盖 LAN/常见 NAT 打洞，更慢的直连回落 hub 中继（仍可用）；彻底跳过
	// 探测用 --webrtc=false。
	meshWebRTCProbeTimeout = 10 * time.Second
)

// errMeshServiceUnavailable 报告服务当前不可用（节点离线或未宣告）。
func errMeshServiceUnavailable(service string) error {
	return fmt.Errorf("mesh 服务 %q 当前不可用（节点离线或未宣告）", service)
}

// meshTargetRefresher 按需解析 mesh 目标，带 TTL 缓存与单飞（single-flight）刷新。
//
// 并发安全设计：
//   - 所有缓存字段由 mu 保护；
//   - 刷新期间**不持有 mu 做网络调用**——承担刷新的 goroutine 置位后解锁再请求，
//     等待者在 done 通道上等待，完成后重新抢锁读取最终状态。
//
// 这样 TTL 内并发调用只打一次 hub（单飞），且不引入「锁内做 I/O」死锁风险。
type meshTargetRefresher struct {
	svc     *client.FileClient
	service string
	ttl     time.Duration
	now     func() time.Time // 可注入时钟（测试用）

	mu          sync.Mutex
	target      *client.MeshService
	lastRefresh time.Time
	refreshing  bool          // 一次只允许一个 goroutine 刷新
	done        chan struct{} // 本次刷新完成时 close
	refreshErr  error         // 最近一次刷新的错误（供等待者复用）
	// lastFailedNode 是最近一次拨号失败的节点（P1-13）：resolve 优先跳过它选择
	// 其他健康候选，防止死节点永久遮蔽健康副本。
	lastFailedNode string
}

// newMeshTargetRefresher 创建 refresher。
func newMeshTargetRefresher(svc *client.FileClient, service string) *meshTargetRefresher {
	return &meshTargetRefresher{svc: svc, service: service, ttl: meshTargetTTL, now: time.Now}
}

// resolve 返回服务当前目标。缓存新鲜（<TTL）直接返回副本；否则触发一次刷新，
// 并发调用共享同一刷新（单飞）。服务不在列表返回 errMeshServiceUnavailable。
func (r *meshTargetRefresher) resolve(ctx context.Context) (*client.MeshService, error) {
	r.mu.Lock()
	if r.target != nil && r.now().Sub(r.lastRefresh) < r.ttl {
		t := *r.target
		r.mu.Unlock()
		return &t, nil
	}
	if r.refreshing {
		done := r.done
		r.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.refreshErr != nil {
			return nil, r.refreshErr
		}
		if r.target != nil {
			t := *r.target
			return &t, nil
		}
		return nil, errMeshServiceUnavailable(r.service)
	}
	// 本 goroutine 承担刷新：置位后立即解锁，绝不在锁内做网络调用。
	r.refreshing = true
	r.refreshErr = nil
	r.done = make(chan struct{})
	r.mu.Unlock()

	fetchCtx, cancel := context.WithTimeout(ctx, meshResolveTimeout)
	svcs, err := r.svc.MeshServices(fetchCtx)
	cancel()

	r.mu.Lock()
	r.refreshing = false
	close(r.done) // 等待者唤醒后先抢锁再读最终状态，无竞态
	if err != nil {
		r.refreshErr = fmt.Errorf("查询 mesh 服务失败: %w", err)
		r.mu.Unlock()
		return nil, r.refreshErr
	}
	// P1-13：收集该服务的全部候选；优先选择最近未失败的节点，避免死节点
	// 永久遮蔽健康副本（旧实现恒取 node-ID 字典序首个）。
	var first, candidate *client.MeshService
	for i := range svcs {
		if svcs[i].Name != r.service {
			continue
		}
		t := svcs[i]
		if first == nil {
			first = &t
		}
		if candidate == nil && t.Node != r.lastFailedNode {
			candidate = &t
		}
	}
	if candidate == nil {
		candidate = first // 全部候选都是最近失败节点：回退到首个（失败信息仍真实）
	}
	if candidate != nil {
		t := *candidate
		r.target = &t
		r.lastRefresh = r.now()
		r.mu.Unlock()
		return &t, nil
	}
	r.target = nil
	r.lastRefresh = time.Time{}
	r.mu.Unlock()
	return nil, errMeshServiceUnavailable(r.service)
}

// invalidate 使缓存过期：dial 失败（relay 404 / webrtc 失败）后调用，
// 下一个连接立即重新解析而非等待 TTL。
// failedNode 记录最近拨号失败的节点，resolve 会跳过它优先尝试其他健康候选
// （P1-13：防止死节点永久遮蔽健康副本）。
func (r *meshTargetRefresher) invalidate(failedNode string) {
	r.mu.Lock()
	r.target = nil
	r.lastRefresh = time.Time{}
	r.refreshErr = nil
	r.lastFailedNode = failedNode
	r.mu.Unlock()
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
			relayToken, _ := cmd.Flags().GetString("relay-token")
			nodeID, _ := cmd.Flags().GetString("node-id")
			insecure, _ := cmd.Flags().GetBool("insecure")
			stunServers, _ := cmd.Flags().GetStringSlice("stun")
			if stunServers != nil {
				webrtc.SetSTUNServers(stunServers)
			}

			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}

			// 按需解析服务 → 目标节点 + 地址（带 TTL 缓存与单飞刷新，感知节点上下线）
			refresher := newMeshTargetRefresher(svc, service)
			target, err := refresher.resolve(cmd.Context())
			if err != nil {
				return err
			}
			ios.WriteOutLine("目标服务: %s（节点 %s, addr %s）", service, target.Node, target.Addr)

			// 构建信令器（webrtc 打洞用）。连接前自动注册自身（声明 per-node-secret
			// 能力），从 REG_OK:<secret> 拿 per-node secret 供 B3 服务端信令身份校验。
			var signaler *hub.HubSignaler
			if useWebRTC {
				if nodeID == "" {
					nodeID = defaultLocalNodeID()
				}
				r, regErr := autoRegister(cmd.Context(), autoRegisterParams{
					hubURL:      hubURL,
					serverURL:   svc.ServerURL(),
					relayToken:  meshRelayToken(relayToken, token, svc),
					signalToken: meshSignalToken(token, svc),
					nodeID:      nodeID,
					prefix:      "mesh",
					exactNode:   false,
					insecure:    insecure,
				})
				if regErr != nil {
					// 注册失败不静默：warn + 回落中继（relay 路径只认 auth_token，
					// 与本机临时注册无关，独立可用）。
					ios.WriteErrLine("webrtc 信令注册失败: %v（回落 hub 中继）", regErr)
				} else {
					signaler = r.signaler
					// 信令结束/命令退出确定性关闭注册连接，防 WS 泄漏
					// （hub 侧断开即 RemoveIfOwned 移除临时节点）。
					defer func() { _ = r.closer() }()
				}
			}
			dial := defaultMeshDial
			localNode := nodeID
			if localNode == "" {
				localNode = defaultLocalNodeID()
			}

			if listenAddr != "" {
				return meshForwardListen(cmd, svc, signaler, dial, refresher, target, localNode, listenAddr, ios)
			}
			return meshStdioOnce(cmd, svc, signaler, dial, refresher, localNode, ios)
		},
	}
	cmd.Flags().StringP("listen", "l", "", "本地监听地址（如 127.0.0.1:2222；裸 :2222 归一为 127.0.0.1:2222）；留空为单次 stdin/stdout 模式")
	cmd.Flags().Bool("webrtc", true, "优先 webrtc 打洞直连，失败回落 hub 中继")
	cmd.Flags().String("hub", "", "hub 地址（http(s) 或 ws(s) 均可；默认取 server_url）")
	cmd.Flags().String("token", "", "信令 Bearer token（默认复用 --auth-token / 配置 auth_token）")
	cmd.Flags().String("relay-token", "", "hub 的 relay_token（自动注册用；与 relay start --token 一致；默认复用 --token / auth_token）")
	cmd.Flags().String("node-id", "", "本节点 ID（信令来源；默认主机名）")
	cmd.Flags().StringSlice("stun", nil,
		"STUN 服务器地址（可重复/逗号分隔，如 stun:stun.qq.com:3478）；默认 Google+腾讯+小米混合，全不通时请指定本地可达服务器")
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
// ref 负责按需解析最新 target（带 TTL 缓存，感知节点上下线）；initial 仅用于启动横幅。
func meshForwardListen(cmd *cobra.Command, svc *client.FileClient, signaler *hub.HubSignaler, dial meshDialFunc, ref *meshTargetRefresher, initial *client.MeshService, localNode, listenAddr string, ios cli.IOStreams) error {
	// S56：裸 :port 归一为 127.0.0.1:port（loopback 安全默认，防 LAN 暴露 +
	// Windows 防火墙弹窗）；需 LAN 访问时显式 0.0.0.0:port / 具体 IP。
	listenAddr = normalizeListenAddr(listenAddr)
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("监听本地端口失败: %w", err)
	}
	defer ln.Close()
	if initial != nil {
		ios.WriteOutLine("端口转发: %s ⇄ mesh(%s) ⇄ %s", listenAddr, initial.Node, initial.Addr)
	}

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
			// 每个连接用最新 target：服务已下线（不在列表）→ 立即清晰报错并关闭，
			// 不再等 webrtc 30s ICE 超时（静默卡死）。
			target, rerr := ref.resolve(ctx)
			if rerr != nil {
				ios.WriteErrLine("建立 mesh 流失败: %v", rerr)
				return
			}
			res, cerr := dial(ctx, svc, signaler, target, localNode)
			if cerr != nil {
				// dial 失败（relay 404 / webrtc 失败）→ 强制缓存过期 + 记录失败节点，
				// 下个连接立即重取并优先跳过该节点（P1-13 候选 failover）。
				ref.invalidate(target.Node)
				ios.WriteErrLine("建立 mesh 流失败: %v（目标 node=%s addr=%s 不可达或离线）", cerr, target.Node, target.Addr)
				return
			}
			conn := res.conn
			defer conn.Close()
			// 拨号帧已由 dial 内部写好（P0-1）：
			//   - relay：hub 的 RelayStreamHandler 已写好 dial 帧（叶子拨目标），
			//     客户端直接透传字节，不得再写帧；
			//   - webrtc：meshWebRTCStream 已在 mux 流上写好 dial 帧，此处同样透传。
			// 客户端不再按路径手动写帧——曾把帧以裸字节写 DataChannel 导致直连 100% 失败。
			ios.WriteOutLine("连接已建立（%s）: %s ⇄ %s", res.kind, target.Node, target.Addr)
			// 双向泵送（CloseWrite 半关闭 + grace 宽限期，C1 范本）：任一方向完成
			//（如 relay 端拒绝拨号后关流 → conn EOF）即向对端传播半关闭（TCP FIN /
			// 流 EOF），让在途响应仍可被读回（不截断）；对端不回应 FIN 时 grace 超时
			// 强制双侧关闭解除阻塞。返回后由外层 defer c.Close()/conn.Close() 收尾。
			pumpConns(c, conn, pumpGracePeriod)
		}(local)
	}
}

// meshStdioOnce 单次模式：stdin/stdout 与一条 mesh 连接直通（选路 dial）。
// ref 负责解析最新 target（单次拨号使用当前缓存；失败返回错误可由调用方重试）。
func meshStdioOnce(cmd *cobra.Command, svc *client.FileClient, signaler *hub.HubSignaler, dial meshDialFunc, ref *meshTargetRefresher, localNode string, ios cli.IOStreams) error {
	target, err := ref.resolve(cmd.Context())
	if err != nil {
		return err
	}
	res, err := dial(cmd.Context(), svc, signaler, target, localNode)
	if err != nil {
		return err
	}
	conn := res.conn
	defer conn.Close()
	// 拨号帧已由 dial 内部写好（P0-1，同 meshForwardListen）：relay 由 hub 写，
	// webrtc 由 meshWebRTCStream 在 mux 流上写，客户端均直接透传。
	ios.WriteOutLine("已连接（%s）: stdin/stdout ⇄ %s (Ctrl+D / EOF 断开)", res.kind, target.Name)
	// 方向区分通道（I38）：对端断开（outDone）→ 会话结束立即返回，不再挂起；
	// 本地 stdin 读完（inDone，如 EOF/管道结束）→ 等待对端把剩余响应写完
	// （保留 `echo x | mesh connect` 的响应语义）。原 wg.Wait() 在对端断开但
	// stdin 未 EOF 时永久挂起。
	inDone := make(chan struct{})
	outDone := make(chan struct{})
	go func() {
		defer close(inDone)
		_, _ = io.Copy(conn, ios.In)
		// P0-5：stdin EOF 后必须向对端传播半关闭（FIN / 流 EOF），否则对端
		// 永远等不到"输入写完"，<outDone 永久挂起（I38/C2 声称修复的挂死，
		// 在同一批代码的另一方向仍然存在——此处补上）。
		closeWriteConn(conn)
	}()
	go func() { defer close(outDone); _, _ = io.Copy(ios.Out, conn) }()
	select {
	case <-outDone: // 对端断开：会话结束
	case <-inDone: // 本地 stdin 读完：半关闭已传播，等对端把剩余数据写完
		<-outDone
	}
	return nil
}

// writeDialFrameTo 在任意 io.Writer 上写 [4B len][{"dial":addr}] 帧（与 relay 协议一致）。
// 供 p2p.writeDialFrame（mux.Stream）与 meshWebRTCStream 共用（S51），一处修复
// 多处生效；net.Conn.Write / mux.Stream.Write 均满足 io.Writer。
func writeDialFrameTo(w io.Writer, addr string) error {
	head, err := json.Marshal(hub.DialRequest{Dial: addr})
	if err != nil {
		return err
	}
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(head)))
	// S68：io.Writer 契约允许部分写（mux 流在发送窗口小于 buf 时返回短写），
	// 逐段写入不检查会致帧损坏——用 writeFull 循环写满。
	if err := writeFull(w, lenBuf); err != nil {
		return err
	}
	return writeFull(w, head)
}

// writeFull 循环写满整个 buf，处理 io.Writer 的部分写（mux 流在发送窗口小于
// buf 长度时返回 n<len 的短写）。仅用于小帧（长度前缀 + 元数据）；数据面泵送
// 用 io.Copy。与 relay 包 leaf.go 的 writeFull 同款（S68）。
func writeFull(w io.Writer, buf []byte) error {
	for len(buf) > 0 {
		n, err := w.Write(buf)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		buf = buf[n:]
	}
	return nil
}

// meshSignalToken 返回信令 Bearer token：显式 --token 优先，否则复用
// FileClient 的 auth token（--auth-token / 配置 auth_token）。
// hub 的 /api/signal/* 走 authMiddleware（校验 auth_token），与 MeshServices /
// RelayStream 的认证一致；relay start --token 是另一套 relay 注册 token，不混用。
func meshSignalToken(flagToken string, svc *client.FileClient) string {
	if flagToken != "" {
		return flagToken
	}
	return svc.AuthToken()
}

// meshRelayToken 返回自动注册用的 relay_token：显式 --relay-token 优先，否则
// 回落 meshSignalToken（--token → auth_token）。与 relay start --token 语义对齐，
// 使 hub 设不同 relay_token/auth_token 时 mesh connect 也能正确完成注册（I37 子决策 A）。
func meshRelayToken(flagRelayToken, flagToken string, svc *client.FileClient) string {
	if flagRelayToken != "" {
		return flagRelayToken
	}
	return meshSignalToken(flagToken, svc)
}

// defaultLocalNodeID 返回本机节点 ID（mesh webrtc 信令来源）。
// 注：与 p2pFlags.localNode() 功能重复（S53），但 fallback 名有意不同
// （mesh-node vs p2p-node）保持命令语义隔离，故不合并。
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

// normalizeHubEndpoints 将 hub 地址归一为信令 HTTP 基址与注册 WS 端点：
//   - httpBase（信令 post/poll 用，http(s)://host[:port]，剥 path）；
//   - wsURL（自动注册用，ws(s)://host[:port]/ws）。
//
// hubURL 接受 http(s):// 或 ws(s)://（含 /ws 等 path）；空串回退 serverURL。
// 这样 --hub 传 relay start 的 ws://.../ws 形式也能正确推导信令基址（I40），
// 并产出自动注册所需的 WS 端点（I37）。畸形 URL / 未知 scheme 显式报错。
func normalizeHubEndpoints(hubURL, serverURL string) (httpBase, wsURL string, err error) {
	if hubURL == "" {
		hubURL = serverURL
	}
	if hubURL == "" {
		return "", "", fmt.Errorf("hub 地址为空（--hub 未指定且 server_url 为空）")
	}
	u, perr := url.Parse(hubURL)
	if perr != nil {
		return "", "", fmt.Errorf("解析 hub 地址失败: %w", perr)
	}
	switch u.Scheme {
	case "http", "https", "ws", "wss":
	default:
		return "", "", fmt.Errorf("不支持的 hub scheme %q（支持 http/https/ws/wss）", u.Scheme)
	}
	httpScheme, wsScheme := u.Scheme, u.Scheme
	switch u.Scheme {
	case "ws":
		httpScheme = "http"
	case "wss":
		httpScheme = "https"
	case "http":
		wsScheme = "ws"
	case "https":
		wsScheme = "wss"
	}
	return httpScheme + "://" + u.Host, wsScheme + "://" + u.Host + "/ws", nil
}

// normalizeListenAddr 将裸 :port 归一为 127.0.0.1:port（loopback 安全默认，
// 防 LAN 暴露 + Windows 防火墙弹窗）；显式 IP/主机名/0.0.0.0 保持原样（S56）。
func normalizeListenAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	return addr
}

// meshTempRegistration 是一次 mesh connect 的临时注册（生命周期与本次命令绑定）。
type meshTempRegistration struct {
	signaler *hub.HubSignaler // 携带临时 node_id + per-node secret
	closer   func() error     // 关闭注册连接 → hub 移除临时节点
	tempNode string           // 临时节点 ID（调试/日志用）
}

// autoRegisterParams 是一次自动注册（mesh/p2p 信令前置）的参数。
// 与 meshAutoRegister 的区别：支持 temp/exact 两种 node 生成模式与 insecure TLS，
// 供 mesh connect（temp）、p2p connect（temp）、p2p listen（exact）三处复用（B17）。
type autoRegisterParams struct {
	hubURL      string // hub 地址（空时回落 serverURL）
	serverURL   string // hubURL 为空的回退基址（mesh 用 svc.ServerURL()；p2p 传 ""）
	relayToken  string // 注册用（hub relay_token）
	signalToken string // 信令 Bearer（hub auth_token）
	nodeID      string // 节点 ID 基础
	prefix      string // 临时 node 前缀："mesh" | "p2p"
	exactNode   bool   // true=注册成 nodeID 原样（p2p listen 需稳定 ID 供 --peer 寻址）；false=临时 nodeID
	insecure    bool   // 缺口 2：注册 WS 拨号 + HubSignaler HTTP 跳过证书校验（自签 wss hub）
}

// autoRegister 是 mesh/p2p 共用的信令自动注册实现：声明 per-node-secret 能力，
// 从 REG_OK:<secret> 解析出 per-node secret，构建携带 secret 的 HubSignaler，
// 供 webrtc 信令身份校验（B3 服务端对未声明/不匹配 secret 的信令 fail-closed 返回 403）。
//
// 注册连接用 mux.New 保活（自动跑 readLoop/pingLoop 处理心跳，镜像 runRelayOnce）；
// closer 关闭 mux → 底层 WS → hub RemoveIfOwned 移除节点。
//
// node 生成规则：
//   - exactNode=false：临时 node_id = <prefix>-<base>-<unixnano>（唯一，防踢长驻
//     relay 注册，对齐 mesh 语义）；base 取 nodeID，为空回落 defaultLocalNodeID()。
//   - exactNode=true：直接注册成 nodeID 原样（p2p listen 的被寻址方需稳定 ID，
//     否则 p2p connect --peer <id> 无法寻址）。
//
// insecure=true 时：注册 WS 用 hubWSDial 注入跳过证书校验的 HTTPClient，且
// HubSignaler 注入 insecureHTTPClient()（自签 wss hub 场景）。
func autoRegister(ctx context.Context, p autoRegisterParams) (*meshTempRegistration, error) {
	httpBase, wsURL, err := normalizeHubEndpoints(p.hubURL, p.serverURL)
	if err != nil {
		return nil, err
	}
	base := p.nodeID
	if base == "" {
		base = defaultLocalNodeID()
	}
	nodeID := base
	if !p.exactNode {
		nodeID = fmt.Sprintf("%s-%s-%d", p.prefix, base, time.Now().UnixNano())
	}

	conn, err := hubWSDial(ctx, wsURL, p.insecure)
	if err != nil {
		return nil, fmt.Errorf("连接 Hub 注册端点失败: %w", err)
	}
	// 注册帧：声明 per-node-secret 能力，hub 回 REG_OK:<secret>（B1）。
	if err := conn.Send(ctx, hub.NewRegisterFrame(nodeID, p.relayToken, hub.Meta{}, hub.CapabilityPerNodeSecret)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("发送注册帧失败: %w", err)
	}
	ackCtx, ackCancel := context.WithTimeout(ctx, registerAckTimeout)
	ack, ackErr := conn.Receive(ackCtx)
	ackCancel()
	if ackErr != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("等待注册 ACK 失败: %w", ackErr)
	}
	secret, ackErr := parseRegisterAck(string(ack))
	if ackErr != nil {
		_ = conn.Close()
		return nil, ackErr
	}
	if secret == "" {
		_ = conn.Close()
		return nil, fmt.Errorf("hub 未下发 per-node secret（未声明能力或能力不被支持）")
	}
	// mux 保活：自动跑 readLoop/writeLoop/pingLoop 处理心跳，注册连接存活到命令退出。
	m := mux.New(conn, mux.RoleListener)
	signaler := hub.NewHubSignaler(httpBase, p.signalToken, nodeID, secret)
	signaler.SetContext(ctx)
	if p.insecure {
		signaler.SetHTTPClient(insecureHTTPClient())
	}
	return &meshTempRegistration{
		signaler: signaler,
		closer:   func() error { return m.Close() },
		tempNode: nodeID,
	}, nil
}

// meshAutoRegister 连接前自动注册（B12 语义不变）：mesh connect 的薄包装，
// 固定 prefix="mesh"、temp node 模式、insecure=false。insecure 场景由 mesh connect
// 直接调 autoRegister 注入。签名保持不变，mesh_test 的既有用例零破坏。
func meshAutoRegister(ctx context.Context, svc *client.FileClient, hubURL, relayToken, signalToken, nodeID string) (*meshTempRegistration, error) {
	return autoRegister(ctx, autoRegisterParams{
		hubURL:      hubURL,
		serverURL:   svc.ServerURL(),
		relayToken:  relayToken,
		signalToken: signalToken,
		nodeID:      nodeID,
		prefix:      "mesh",
		exactNode:   false,
		insecure:    false,
	})
}
