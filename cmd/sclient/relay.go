// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	mesh "github.com/cocomhub/sproxy/pkg/tunnel/mesh"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/relay"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin" // 注册内置 TCP 传输层（--transport tcp）
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws"  // 注册 WebSocket 传输层（--transport ws）
	"github.com/spf13/cobra"
)

const (
	reconnectBaseDelay = 1 * time.Second
	reconnectMaxDelay  = 30 * time.Second
	// registerAckTimeout 是等待 hub 注册 ACK 的超时。
	registerAckTimeout = 10 * time.Second
)

// NewCmdRelay 创建 relay 父命令的工厂函数。
func runRelayStart(cmd *cobra.Command, transport, hubURL, local, nodeID, accessKey, accessKeySecret, accessKeyID string, insecure bool, dialAllow bool, services, dialAllowCIDRs []string) error {
	switch transport {
	case "ws", "tcp":
	default:
		return fmt.Errorf("未知传输层 %q（仅支持 ws/tcp）", transport)
	}
	if nodeID == "" {
		nodeID = fmt.Sprintf("relay-%d", time.Now().UnixMilli())
	}
	// 本地默认 hub（--hub 与配置 hub_url 均未提供时）。注意与 sproxy 默认监听端口
	// :18083 不同——请按实际 hub 地址显式 --hub 或配置 hub_url。
	// ws 传输用 ws(s):// URL；tcp 传输用裸 host:port（hub.transports.tcp.listen）。
	if hubURL == "" {
		if transport == "tcp" {
			hubURL = "127.0.0.1:18084"
		} else {
			hubURL = "ws://127.0.0.1:18084/ws"
		}
	}

	logger := slog.With("node", nodeID, "hub", hubURL, "local", local, "dial_allow", dialAllow, "transport", transport)
	logger.Info("中继节点启动")
	// hub 注册准入已改 SproxySig AccessKey + HMAC proof：Secret 只本端计算签名/证明，
	// 永不上线，故明文 ws:// 不再泄露凭据；仍提示自签证书场景用 wss://。
	if insecure && transport != "tcp" {
		logger.Warn("--insecure 已启用，跳过 TLS 证书验证；仅限开发/测试", "hub", hubURL)
	}

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	// 虚拟 IP 子网：--virtual-subnet 覆盖默认 CGNAT（S-1 审查修复，匹配自定义 hub 子网）。
	virtualSubnet, _ := cmd.Flags().GetString("virtual-subnet")
	return runRelayWithRetry(ctx, transport, nodeID, hubURL, local, accessKey, accessKeySecret, accessKeyID, insecure, dialAllow, services, dialAllowCIDRs, virtualSubnet, logger)
}

func runRelayWithRetry(ctx context.Context, transport, nodeID, hubURL, local, accessKey, accessKeySecret, accessKeyID string, insecure bool, dialAllow bool, services, dialAllowCIDRs []string, virtualSubnet string, logger *slog.Logger) error {
	delay := reconnectBaseDelay
	for {
		err := runRelayOnce(ctx, transport, nodeID, hubURL, local, accessKey, accessKeySecret, accessKeyID, insecure, dialAllow, services, dialAllowCIDRs, virtualSubnet, logger)
		if err == nil || ctx.Err() != nil {
			return err
		}
		if isTerminalRelayError(err) {
			return err
		}
		logger.Warn("中继断开，即将重连", "delay", delay, "error", err)
		select {
		case <-time.After(delay):
			delay *= 2
			if delay > reconnectMaxDelay {
				delay = reconnectMaxDelay
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// errRelayRegistrationRejected 表示 hub 通过注册 ACK 明确拒绝本次注册（鉴权/格式错误）。
// isTerminalRelayError 判断是否应因配置/权限错误终止而非重试。
// 仅当 hub 通过注册 ACK **明确拒绝** 注册时才终止；其余（连接断开、超时、EOF、
// ACK 未到达）均视为可重连的网络问题。哨兵错误定义在 pkg/tunnel/hub
// （hub.ErrRegisterRejected），errors.Is 可穿透任意 %w 包装。
func isTerminalRelayError(err error) bool {
	return errors.Is(err, hub.ErrRegisterRejected)
}

func runRelayOnce(ctx context.Context, transport, nodeID, hubURL, local, accessKey, accessKeySecret, accessKeyID string, insecure bool, dialAllow bool, services, dialAllowCIDRs []string, virtualSubnet string, logger *slog.Logger) error {
	// 注册准入：hub 已废除共享 token，改用 SproxySig AccessKey + HMAC proof。
	// fail-closed：AccessKeySecret 为空时直接报错（防止无凭据注册被 hub fail-closed
	// 拒绝后客户端困惑——明明连上了却被拒）。
	if accessKeySecret == "" {
		return fmt.Errorf("注册失败: access_key_secret 为空，无法计算注册 proof")
	}
	ts := time.Now().UnixMilli()
	nonce := hub.NewRegisterNonce()
	proof, err := hub.ComputeRegisterProof(accessKeySecret, nodeID, ts, nonce)
	if err != nil {
		return fmt.Errorf("注册失败: 计算注册证明失败: %w", err)
	}
	// 传输层选择：--transport tcp 走裸 TCP（hub.transports.tcp.listen，hubURL 为
	// host:port）；默认 ws 走 WebSocket（hubURL 为 ws(s):// 或 host:port）。
	// 两者注册/信令/数据面协议完全一致，仅 xfer.Conn 载体不同。
	var conn xfer.Conn
	switch transport {
	case "tcp":
		// 用户沿用 WS 习惯传 ws:// URL 时给清晰错误（tcp.Dial 会把 "ws://..." 当
		// host 解析，报 "missing port" 之类难懂的错）。
		if strings.HasPrefix(hubURL, "ws://") || strings.HasPrefix(hubURL, "wss://") {
			return fmt.Errorf("--transport tcp 的 --hub 应为 host:port（如 127.0.0.1:18084），不能是 ws:// 地址，got %q", hubURL)
		}
		tp := xfer.Get("tcp")
		if tp == nil {
			return fmt.Errorf("tcp 传输层未注册")
		}
		conn, err = tp.Dial(ctx, hubURL)
	case "ws", "":
		// B17：insecure 时经 hubWSDial 注入跳过证书校验的 HTTPClient（自签 wss hub）；
		// 非 insecure 路径保持 xfer.Get("ws").Dial 原样（零行为变化）。
		conn, err = mesh.HubWSDial(ctx, hubURL, insecure)
	default:
		return fmt.Errorf("未知传输层 %q（仅支持 ws/tcp）", transport)
	}
	if err != nil {
		return fmt.Errorf("连接到 Hub 失败: %w", err)
	}
	logger.Info("已连接到 Hub")

	// 注册协议：连接建立后，在 xfer 层直接发送一条注册帧（JSON 或裸 nodeID）。
	// 与 HubServer.readRegisterFrame 对齐：hub 在创建 mux 前通过 conn.Receive 读取，
	// 因此这里也必须用 conn.Send，而非 mux 控制流。
	meta := hub.Meta{}
	var serviceAddrs []string
	if dialAllow {
		meta.Tags = append(meta.Tags, "exit")
	}
	for _, svc := range services {
		name, addr, ok := strings.Cut(svc, ":")
		if !ok || name == "" || addr == "" {
			logger.Warn("忽略无效服务宣告（应为 name:addr）", "raw", svc)
			continue
		}
		// S60：addr 必须是合法 host:port（net.SplitHostPort），且 host 非空
		// （拒绝 "x::22" 这类空 host）。否则注册了"可见不可连"的服务，
		// mesh connect 命中后必然拨号失败。服务端 hub/router validateServices
		// 应同步补 host:port 校验（B1 防御纵深，本批仅客户端）。
		if host, _, sperr := net.SplitHostPort(addr); sperr != nil || host == "" {
			logger.Warn("忽略无效服务宣告（addr 应为 host:port）", "raw", svc, "addr", addr, "error", sperr)
			continue
		}
		meta.Services = append(meta.Services, hub.Service{Name: name, Addr: addr})
		// 收集宣告的服务地址：出口拨号时精确放行这些地址（含 loopback/私网），
		// 否则 mesh connect 回落中继路径拨 127.0.0.1:xxx 会被默认策略拒绝。
		serviceAddrs = append(serviceAddrs, addr)
	}
	// 声明 per-node-secret 能力：hub 回 REG_OK:<base64url secret>（B1 已支持，
	// B3 服务端将据此校验信令身份）；声明 virtual-ip 能力：hub 在 REG_OK 携带本节点
	// 虚拟 IP（Discover=false 的 relay 出口节点也能立即得知自身 VIP）。不感知能力的
	// 旧 hub 忽略未知能力位，回旧格式。现有调用不传 caps 时行为不变。
	if serr := conn.Send(ctx, hub.NewRegisterFrame(nodeID, accessKey, proof, ts, nonce, meta, hub.CapabilityPerNodeSecret, hub.CapabilityVirtualIP)); serr != nil {
		_ = conn.Close() // P1-15：mux 创建前失败必须关闭 WS，否则重连循环泄漏连接+sendLoop goroutine
		return fmt.Errorf("发送注册帧失败: %w", serr)
	}

	// 等待 hub 注册 ACK（token 错误/格式错误尽早报错，而非等建流失败才发现）
	ackCtx, ackCancel := context.WithTimeout(ctx, registerAckTimeout)
	ack, ackErr := conn.Receive(ackCtx)
	ackCancel()
	if ackErr != nil {
		_ = conn.Close() // P1-15：同守卫
		return fmt.Errorf("等待注册 ACK 失败: %w", ackErr)
	}
	ackFull, ackErr := hub.ParseRegisterAckFull(string(ack))
	if ackErr != nil {
		_ = conn.Close() // P1-15：同守卫
		return ackErr
	}
	nodeSecret := ackFull.Secret
	if nodeSecret != "" {
		// per-node secret 与本次注册连接生命周期绑定（重连即轮换），
		// 只在注册流程内使用，不落盘、不打印值（I1，方案 B）。
		logger.Info("已注册到 Hub（per-node secret 已获取）")
	} else {
		logger.Info("已注册到 Hub")
	}
	selfVIP := ackFull.VirtualIP

	m := mux.New(conn, mux.RoleListener)
	defer m.Close()

	// 本地 HTTP 服务地址（HTTP 中继转发目标）
	localAddr := local
	if localAddr == "" {
		localAddr = "http://127.0.0.1:8080"
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}

	logger.Info("等待中继请求...")
	// 始终传入包含宣告服务地址的拨号策略（--dial-allow=false 时 Serve 在咨询
	// 策略前就拒绝 dial 帧，策略不生效）。无服务宣告且无 CIDR 时等价默认
	// DialAllowed（仅公网）。
	// DialResultFrames=true：经 hub 中继时向 hub 回写拨号结果帧，使 hub 在写 200
	// 前能确认数据面就绪（I27）。注意 p2p listen（webrtc 直连）必须保持 false，
	// 否则结果帧会污染数据流。
	// 出口拨号策略：虚拟 IP NAT（selfVIP 由 REG_OK 下发；默认 CGNAT 子网，可
	// --virtual-subnet 覆盖以匹配自定义 hub.virtual_subnet，服务宣告端口自动开放）
	// 优先，内部已含宣告地址精确匹配（逃生口）与公网/CIDR 回落（S-1 审查修复）。
	vipSubnet, vperr := netip.ParsePrefix(virtualSubnet)
	if vperr != nil || !vipSubnet.Addr().Is4() {
		return fmt.Errorf("--virtual-subnet %q 非法（应为 IPv4 CIDR）", virtualSubnet)
	}
	vipSubnet = vipSubnet.Masked()
	opts := []relay.ServeOptions{
		{DialPolicy: relay.NewVirtualIPDialPolicy(vipSubnet, selfVIP, nil, dialAllowCIDRs, serviceAddrs), DialResultFrames: true},
	}
	err = relay.Serve(ctx, m, localAddr, dialAllow, httpClient, logger, opts...)
	if err != nil {
		logger.Warn("中继服务停止", "error", err)
	}
	return err
}

// ---- 工厂函数 ----

// NewCmdRelay 创建 relay 父命令的工厂函数。
func NewCmdRelay(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "relay",
		Short: "中继节点管理",
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}
	cmd.AddCommand(NewCmdRelayStart(ios, cfgSvc))
	cmd.AddCommand(NewCmdRelayStatus(ios, cfgSvc))
	cmd.AddCommand(NewCmdRelayStop(ios))
	cmd.AddCommand(NewCmdRelayRemoveNode(ios, cfgSvc))
	cmd.AddCommand(NewCmdRelayStats(ios, cfgSvc))
	cmd.AddCommand(NewCmdRelayDial(factory, ios))
	return cmd
}

// NewCmdRelayStart 创建 relay start 命令的工厂函数。
func NewCmdRelayStart(ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "启动中继节点，连接到 Hub",
		Long: `作为中继节点连接到 Hub，注册自身，然后等待远程请求并通过隧道转发到本地 HTTP 服务。

使用示例:
  sclient relay start --hub ws://hub.example.com/ws --local http://127.0.0.1:8080 --node-id my-node
  sclient relay start --transport tcp --hub 127.0.0.1:18084 --node-id my-node --dial-allow`,
		RunE: func(cmd *cobra.Command, args []string) error {
			transport, _ := cmd.Flags().GetString("transport")
			hubURL, _ := cmd.Flags().GetString("hub")
			local, _ := cmd.Flags().GetString("local")
			nodeID, _ := cmd.Flags().GetString("node-id")
			accessKey, _ := cmd.Flags().GetString("access-key")
			accessKeySecret, _ := cmd.Flags().GetString("access-key-secret")
			accessKeyID, _ := cmd.Flags().GetString("access-key-id")
			insecure, _ := cmd.Flags().GetBool("insecure")
			dialAllow, _ := cmd.Flags().GetBool("dial-allow")
			services, _ := cmd.Flags().GetStringArray("service")
			dialAllowCIDRs, _ := cmd.Flags().GetStringArray("dial-allow-cidr")
			// P2-配置3：通用参数配置回落——--hub/--node-id/--access-key/--access-key-secret/--access-key-id
			// 未显式指定时取配置 hub_url/node_id/access_key/access_key_secret/access_key_id（CLI > 配置 > 默认）。
			if cfgSvc != nil {
				if cfg, cerr := cfgSvc.LoadConfig(); cerr == nil {
					if hubURL == "" {
						hubURL = cfg.HubURL
					}
					if accessKey == "" {
						accessKey = cfg.AccessKey
					}
					if accessKeySecret == "" {
						accessKeySecret = cfg.AccessKeySecret
					}
					if accessKeyID == "" {
						accessKeyID = cfg.AccessKeyID
					}
					if nodeID == "" {
						nodeID = cfg.NodeID
					}
				}
			}
			return runRelayStart(cmd, transport, hubURL, local, nodeID, accessKey, accessKeySecret, accessKeyID, insecure, dialAllow, services, dialAllowCIDRs)
		},
	}
	cmd.Flags().String("transport", "ws", "连接到 Hub 的传输层: ws（默认，WebSocket）/ tcp（裸 TCP，hub.transports.tcp.listen）")
	cmd.Flags().String("hub", "", "Hub 地址（默认取配置 hub_url；均未配置时 ws 用 ws://127.0.0.1:18084/ws、tcp 用 127.0.0.1:18084）")
	cmd.Flags().String("local", "http://127.0.0.1:8080", "本地 HTTP 服务地址")
	cmd.Flags().String("node-id", "", "节点唯一标识 (默认使用时间戳)")
	cmd.Flags().Bool("dial-allow", false, "作为出口节点：允许收到 dial 帧时向目标地址发起出站 TCP 连接（供中继端充当出口网关）")
	cmd.Flags().StringArray("service", nil, "宣告一个 mesh 服务（格式 name:addr，可重复；供 sclient mesh connect 发现）")
	cmd.Flags().StringArray("dial-allow-cidr", nil, "出口拨号白名单网段（如 192.168.0.0/16；配合 --dial-allow 放行内网服务，默认仅公网）")
	cmd.Flags().String("virtual-subnet", hub.DefaultVirtualSubnet, "虚拟 IP 子网（CIDR，仅 IPv4；需与 hub.virtual_subnet 配置一致；默认 CGNAT 100.64.0.0/10）")
	return cmd
}

// NewCmdRelayStatus 创建 relay status 命令的工厂函数。
func NewCmdRelayStatus(ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "查看 Hub 节点状态",
		RunE: func(cmd *cobra.Command, args []string) error {
			// 获取服务器地址（从根命令的 persistent flag 或 --hub flag 或配置文件）
			serverURL, _ := cmd.Flags().GetString("server")
			if serverURL == "" {
				if hubURL, _ := cmd.Flags().GetString("hub"); hubURL != "" {
					if u, parseErr := url.Parse(hubURL); parseErr == nil {
						u.Scheme = "http"
						u.Path = ""
						serverURL = u.String()
					}
				}
			}
			if serverURL == "" && cfgSvc != nil {
				if cfg, err := cfgSvc.LoadConfig(); err == nil {
					serverURL = cfg.ServerURL
				}
			}
			if serverURL == "" {
				return fmt.Errorf("未指定服务器地址，请使用 --server 或 --hub 或配置 server_url")
			}

			// 获取 SproxySig 认证密钥（v2 skey-id 必传：同时取 access-key-id）
			accessKey, _ := cmd.Flags().GetString("access-key")
			accessKeySecret, _ := cmd.Flags().GetString("access-key-secret")
			accessKeyID, _ := cmd.Flags().GetString("access-key-id")
			if accessKeySecret == "" && cfgSvc != nil {
				if cfg, err := cfgSvc.LoadConfig(); err == nil {
					accessKey = cfg.AccessKey
					accessKeySecret = cfg.AccessKeySecret
					accessKeyID = cfg.AccessKeyID
				}
			}

			// 查询节点列表
			nodesURL := strings.TrimRight(serverURL, "/") + "/api/hub/nodes"
			req, err := http.NewRequest("GET", nodesURL, nil)
			if err != nil {
				return fmt.Errorf("创建请求失败: %w", err)
			}
			sproxysig.SignRequestWithSkeyID(req, accessKey, accessKeyID, accessKeySecret)
			// B17：--insecure 时复用 insecureHTTPClient（跳过证书校验，自签 https hub 场景）。
			httpClient := &http.Client{Timeout: 10 * time.Second}
			if insecure, _ := cmd.Flags().GetBool("insecure"); insecure {
				httpClient = client.InsecureHTTPClient()
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				return fmt.Errorf("查询 Hub 状态失败: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
				return fmt.Errorf("查询 Hub 状态失败 (HTTP %d): %s", resp.StatusCode, string(body))
			}

			var nodes []struct {
				ID        string `json:"id"`
				Addr      string `json:"addr,omitempty"`
				Connected string `json:"connected,omitempty"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
				return fmt.Errorf("解析响应失败: %w", err)
			}

			if len(nodes) == 0 {
				ios.WriteOutLine("暂无已连接节点")
				return nil
			}

			ios.WriteOutLine("已连接节点 (%d):", len(nodes))
			for _, n := range nodes {
				connected := n.Connected
				if connected != "" {
					if t, parseErr := time.Parse(time.RFC3339, connected); parseErr == nil {
						connected = t.Format("2006-01-02 15:04:05")
					}
				}
				ios.WriteOutLine("  - ID:       %s", n.ID)
				ios.WriteOutLine("    地址:     %s", n.Addr)
				ios.WriteOutLine("    连接时间: %s", connected)
			}
			return nil
		},
	}
	cmd.Flags().String("hub", "", "Hub 的 HTTP 地址 (如 http://127.0.0.1:18083)")
	return cmd
}

// NewCmdRelayStop 创建 relay stop 命令的工厂函数。
func NewCmdRelayStop(ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "停止中继节点",
		Long: `向正在运行的中继节点发送停止信号。

中继节点作为独立进程运行时，请使用 kill 或 SIGINT 停止。
如果通过 sclient relay start 前台运行，按 Ctrl+C 即可停止。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ios.WriteOutLine("请向中继进程发送 SIGINT 信号以优雅停止。")
			ios.WriteOutLine("如果中继在前台运行，请按 Ctrl+C。")
			return nil
		},
	}
}
