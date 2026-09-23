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
	"net/netip"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/meshconn"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/iostream"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	mesh "github.com/cocomhub/sproxy/pkg/tunnel/mesh"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws" // 注册 WebSocket 传输层（自动注册拨号用）
	"github.com/spf13/cobra"
)

// meshDialFunc 建立一条到目标服务的连接（选路逻辑）。
// 默认用 pkg/tunnel/mesh.Dial（webrtc 打洞优先，失败回落 hub 中继）；
// 指定 --gateway 时先经本地 mesh node 网关复用已建直连链路，无已建链路回落常规拨号。
// 可注入测试桩。
type meshDialFunc func(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler, target *client.MeshService, localNode string) (*mesh.Result, error)

// meshGatewayDial 构造带本地网关优先的选路 dial：先经本地 mesh node 网关复用已建
// 直连链路（零重新打洞），本地节点无到目标的已建链路（ErrNoPeerLink）时回落常规
// 拨号 mesh.Dial；其他网关错误（连接失败/协议错误/token 校验失败）也回落并提示
// （不回归既有路径）。gatewayToken 是网关认证 token（由调用方传入本机凭据的
// access_key_secret，即 SproxySig SK；与 mesh node 网关同一机制，已无 auth_token 明文 Bearer）。
func meshGatewayDial(gatewayAddr, gatewayToken string, ios cli.IOStreams) meshDialFunc {
	return func(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler, target *client.MeshService, localNode string) (*mesh.Result, error) {
		if conn, gerr := mesh.GatewayConnect(ctx, gatewayAddr, target.Node, target.Addr, gatewayToken); gerr == nil {
			// 复用已建立直连链路：网关在已建链路上写拨号帧，对端 relay.Serve 出口拨号。
			return &mesh.Result{Conn: conn, Kind: mesh.KindPeerLink}, nil
		} else if errors.Is(gerr, mesh.ErrNoPeerLink) {
			slog.Debug("本地网关无到目标节点的已建链路，回落常规拨号", "peer", target.Node, "addr", target.Addr)
		} else {
			ios.WriteErrLine("本地网关路由失败: %v（回落常规拨号）", gerr)
		}
		return mesh.Dial(ctx, svc, signaler, target, localNode)
	}
}

// NewCmdMesh 创建 mesh 父命令：基于 hub 服务注册表的服务发现与连接。
// cfgSvc 为可选配置提供者（mesh node 常驻节点用；hub/token/node-id 配置回落）。
func NewCmdMesh(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mesh",
		Short: "mesh 服务发现与连接（webrtc 直连优先，hub 中继回落）",
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}
	cmd.AddCommand(newCmdMeshConnect(factory, ios, cfgSvc))
	cmd.AddCommand(newCmdMeshStatus(factory, ios))
	cmd.AddCommand(newCmdMeshACL(factory, ios))
	cmd.AddCommand(newCmdMeshNode(ios, cfgSvc))
	cmd.AddCommand(newCmdMeshUp(factory, ios, cfgSvc))
	return cmd
}

// newCmdMeshConnect 创建 mesh connect：按服务名连接（webrtc 优先，中继回落）。
func newCmdMeshConnect(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "connect <service> [-l :port]",
		Short: "连接到 mesh 服务（webrtc 直连优先，hub 中继回落）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := args[0]
			listenAddr, _ := cmd.Flags().GetString("listen")
			virtualSubnet, _ := cmd.Flags().GetString("virtual-subnet")

			// mesh 连接参数组统一装配（flag + 配置回落；mesh connect 不注册 exit 族）。
			conn := &meshconn.Conn{}
			if err := conn.FromFlags(cmd, cfgSvc); err != nil {
				return err
			}
			useWebRTC := conn.WebRTC
			hubURL := conn.HubURL
			nodeID := conn.NodeID
			gatewayAddr := conn.GatewayAddr
			mdns := conn.MDNS
			mdnsSecret := conn.MDNSSecret
			insecure := conn.Insecure

			// 应用 STUN/TURN 全局配置（webrtc 打洞候选收集用）。
			if err := applyTURNRESTFlags(cmd); err != nil {
				return err
			}
			webrtcSetSTUNImpl(conn)
			if mdns {
				// 纯 mDNS 直连（不经 hub）：服务端需 `mesh node --mdns` 宣告该服务。
				// mDNS 认证密钥：--mdns-secret 优先；为空回落配置的 access_key_secret
				// （复用 mesh AK/SK 的 SK，避免双套凭据）；两者皆空 = LAN 信任。
				secret := mdnsSecret
				if secret == "" {
					if svc, cerr := factory.NewClient(cmd); cerr == nil && svc != nil {
						secret = svc.AccessKeySecret()
					}
				}
				return runMDNSConnect(cmd, service, listenAddr, nodeID, secret, virtualSubnet, ios)
			}

			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}
			// P2-配置3：通用 mesh 参数配置回落——--hub/--node-id 未显式指定时取配置
			// hub_url/node_id；hub 注册准入用 SproxySig AccessKey/SK（svc.AccessKey()
			// /AccessKeySecret() 已含 config + flag 覆盖），不需要额外 relay token。
			if hubURL == "" {
				hubURL = svc.MeshHubURL()
			}
			if nodeID == "" {
				nodeID = svc.NodeID()
			}

			// 虚拟 IP 寻址（<vip>:<port>）：目标地址 host ∈ 虚拟子网 → 拉 hub 节点列表
			// 构建 vipTable 解析 node-id（设计 AD-3/AD-6）。vipTable 只接受认证数据源
			// （hub 节点列表，SproxySig 签名）。未知虚拟 IP 报错，不猜测 node-id。
			// 非虚拟子网目标回落服务名解析（既有路径）。
			// 虚拟 IP 子网：--virtual-subnet 覆盖默认 CGNAT（自定义 hub.virtual_subnet
			// 时须与本 hub 配置一致，否则 VIP 寻址 fail-closed 拒绝，C 审查 Important）。
			// R-2：仅在虚拟 IP 寻址路径校验合法性——服务名寻址不因误传非法
			// --virtual-subnet 被拦。
			vipSubnet, vipPerr := netip.ParsePrefix(virtualSubnet)
			vipValid := vipPerr == nil && vipSubnet.Addr().Is4()
			var vipTable *mesh.VipTable
			var target *client.MeshService
			var refresher *client.MeshTargetRefresher
			isVIP := false
			if host, _, hErr := net.SplitHostPort(service); hErr == nil {
				if vip, ok := mesh.ParseVirtualAddr(host); ok && vipValid && mesh.IsVirtualAddr(vip, vipSubnet) {
					isVIP = true
					vipSubnet = vipSubnet.Masked()
					nodes, lErr := svc.ListHubNodes(cmd.Context())
					if lErr != nil {
						return fmt.Errorf("拉取 hub 节点列表解析虚拟 IP 失败: %w", lErr)
					}
					vipTable = mesh.NewVipTable(vipSubnet)
					for _, n := range nodes {
						if n.VirtualIP != "" {
							a, pErr := netip.ParseAddr(n.VirtualIP)
							if pErr != nil {
								continue
							}
							if !vipTable.Add(a, n.ID) {
								// R-2：hub 权威列表内同一 VIP 被多个节点声明（异常），
								// 不静默丢弃——fail-closed 报错避免误导。
								return fmt.Errorf("hub 节点列表虚拟 IP %s 冲突（多个节点声明），无法解析虚拟 IP 目标", a)
							}
						}
					}
					targetNode, ok := vipTable.NodeByAddr(vip)
					if !ok {
						// R-5：目标节点重连/hub 重启后虚拟 IP 可能变化，提示重试。
						return fmt.Errorf("虚拟 IP %s 未在 mesh 节点列表中找到对应节点（请确认目标节点已在线且 hub 已分配虚拟 IP；若目标节点刚重连导致虚拟 IP 变化，请重试本命令）", vip)
					}
					target = &client.MeshService{Node: targetNode, Addr: service}
					// 固定目标 refresher：vip → node 映射已由 vipTable 解析，无需服务名刷新。
					refresher = client.NewStaticMeshTargetRefresher(target)
				}
			}
			if !isVIP {
				// 按需解析服务 → 目标节点 + 地址（带 TTL 缓存与单飞刷新，感知节点上下线）。
				refresher = client.NewMeshTargetRefresher(svc, service)
				target, err = refresher.Resolve(cmd.Context())
				if err != nil {
					return err
				}
			}
			ios.WriteOutLine("目标服务: %s（节点 %s, addr %s）", service, target.Node, target.Addr)

			// 构建信令器（webrtc 打洞用）。连接前自动注册自身（声明 per-node-secret
			// 能力），从 REG_OK:<secret> 拿 per-node secret 供 B3 服务端信令身份校验。
			var signaler *hub.HubSignaler
			if useWebRTC {
				if nodeID == "" {
					nodeID = iostream.LocalHostname("mesh-node")
				}
				caFile, _ := cmd.Flags().GetString("ca-file")
				if caFile == "" {
					if cfg, cerr := cfgSvc.LoadConfig(); cerr == nil {
						caFile = cfg.XferCAFile
					}
				}
				r, regErr := mesh.AutoRegister(cmd.Context(), mesh.AutoRegisterParams{
					HubURL:          hubURL,
					ServerURL:       svc.ServerURL(),
					AccessKey:       svc.AccessKey(),
					AccessKeySecret: svc.AccessKeySecret(),
					AccessKeyID:     svc.AccessKeyID(),
					NodeID:          nodeID,
					Prefix:          "mesh",
					ExactNode:       false,
					Insecure:        insecure,
					CAFile:          caFile,
				})
				if regErr != nil {
					// 注册失败不静默：warn + 回落中继（relay 路径只认 SproxySig 凭据
					// --access-key*，与本机临时注册无关，独立可用）。
					ios.WriteErrLine("webrtc 信令注册失败: %v（回落 hub 中继）", regErr)
				} else {
					signaler = r.Signaler
					// 信令结束/命令退出确定性关闭注册连接，防 WS 泄漏
					// （hub 侧断开即 RemoveIfOwned 移除临时节点）。
					defer func() { _ = r.Closer() }()
				}
			}
			localNode := nodeID
			if localNode == "" {
				localNode = iostream.LocalHostname("mesh-node")
			}

			// --gateway：先经本地 mesh node 网关复用已建直连链路（零重新打洞），
			// 本地节点无到目标的已建链路时回落常规拨号（不回归既有路径）。
			// 网关认证 token 复用本机凭据的 access_key_secret（SproxySig SK），
			// 与 mesh node 网关一致——已无 auth_token 明文 Bearer 概念。
			// 装配顺序（整体审核确认）：先装配网关选路（内层），再包虚拟 IP 解析
			// （最外层）——保证 isVIP && --gateway 同时存在时，"vip → node-id 运行时
			// 重新解析"仍先执行，随后回落网关复用已建链路（或 mesh.Dial）。若反序
			// （gateway 包最外），meshGatewayDial 会覆盖 meshVIPDial，目标节点 VIP
			// 变化（R-5）时解析不到最新 node-id。
			dial := meshDialFunc(mesh.Dial)
			// 端到端加密（--e2e 显式开关）：mesh.Dial 的 RelayStream 分支包
			// DialE2EStream（ECDH + AES-256-GCM），X/hub 只透传密文。一期 L 直连 T
			// （hub 中继路径）；via-node 多跳（X 中转）二期。纯 ECDH 告警是提示
			// 非致命（防窃听仍生效）；身份加载失败才报错。
			// e2eVar 提升到外层作用域：--e2e + --smart 时 via-relay 候选需要把 E2E
			// 配置传给 DialSmartWithOptions（via_node 候选内部包 E2E）——否则
			// via-relay 多跳路径不加密（CLI 外层包层只包 KindRelay，via-node 结果
			// 是 KindViaNode 被跳过——漏包即静默明文，违反安全红线）。
			var e2eVar *mesh.EndToEndOptions
			if conn.E2E {
				e2e, eerr := conn.E2EOpts()
				if eerr != nil && !strings.Contains(eerr.Error(), "纯 ECDH") {
					return eerr
				}
				e2eVar = e2e
				// 端到端加密启用可观测（用户红线：安全开关生效状态必须可观测，禁静默降级）：
				// 打印启用模式（pinning 防 MITM / 纯 ECDH 防窃听），用户可确认生效。
				if e2e != nil {
					mode := "指纹 pinning（防中间人）"
					if len(e2e.PeerFingerprints) == 0 {
						mode = "纯 ECDH（防窃听，无 MITM 防护——建议配置 --e2e-peer-fp）"
					}
					ios.WriteErrLine("端到端加密已启用（%s）", mode)
				}
				base := dial
				dial = meshDialFunc(func(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler, target *client.MeshService, localNode string) (*mesh.Result, error) {
					res, derr := base(ctx, svc, signaler, target, localNode)
					if derr != nil {
						return nil, derr
					}
					// 把返回的裸数据面连接包 E2E（仅 hub 中继路径；webrtc 直连一期不接，
					// mux-over-mux 留二期）。via-relay 候选（KindViaNode）内部已包 E2E，
					// 此处仅包主路径（KindRelay）结果——互斥，不双重包。
					if res.Kind == mesh.KindRelay && e2e != nil {
						e2eConn, derr := mesh.DialE2EStream(ctx, res.Conn, target.Addr, "", *e2e)
						if derr != nil {
							_ = res.Conn.Close()
							return nil, fmt.Errorf("E2E 拨号失败: %w", derr)
						}
						res.Conn = e2eConn
						res.EndToEnd = true
					}
					return res, nil
				})
			}
			// --smart：并行竞速直连/中继/经中间节点多跳，按端到端建连耗时择优
			// （默认关 = 现有固定顺序 webrtc→relay，零回归）。
			// DialSmartDefault 是 5 参便捷包装；--smart-ttl 覆盖默认缓存 TTL（30s）。
			// 优雅降级（T3）：竞速全部候选失败/无可选路径时回退到固定顺序 mesh.Dial
			// （FallbackDial），连接仍可用而非报错。
			smart, _ := cmd.Flags().GetBool("smart")
			if smart {
				smartTTL, _ := cmd.Flags().GetDuration("smart-ttl")
				// 传输质量感知选路（roadmap 5.3 P1）：--quality-routing 显式开关，
				// 候选按历史重传率加权（劣化候选延迟 100ms 启动，健康候选先胜出）。
				qualityRouting, _ := cmd.Flags().GetBool("quality-routing")
				// 竞速失败降级到固定顺序（T3 优雅降级）：DialSmartWithOptions 的
				// FallbackDial 字段承载 mesh.Dial，全部候选失败/无可选路径时回退。
				fallback := meshDialFunc(mesh.Dial)
				dial = meshDialFunc(func(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler, target *client.MeshService, localNode string) (*mesh.Result, error) {
					so := mesh.SmartOptions{FallbackDial: fallback}
					if smartTTL > 0 {
						so.CacheTTL = smartTTL
					}
					if qualityRouting {
						so.QualityRouting = true
					}
					return mesh.DialSmartWithOptions(ctx, svc, signaler, target, localNode, mesh.DialOptions{E2E: e2eVar}, so)
				})
			}
			if gatewayAddr != "" {
				dial = meshGatewayDial(gatewayAddr, svc.AccessKeySecret(), ios)
			}
			if isVIP {
				dial = meshVIPDial(vipTable, vipSubnet, dial, ios)
			}

			if listenAddr != "" {
				return meshForwardListen(cmd, svc, signaler, dial, refresher, target, localNode, listenAddr, ios)
			}
			return meshStdioOnce(cmd, svc, signaler, dial, refresher, localNode, ios)
		},
	}
	cmd.Flags().StringP("listen", "l", "", "本地监听地址（如 127.0.0.1:2222；裸 :2222 归一为 127.0.0.1:2222）；留空为单次 stdin/stdout 模式")
	cmd.Flags().String("virtual-subnet", hub.DefaultVirtualSubnet, "虚拟 IP 子网（CIDR，仅 IPv4；需与 hub.virtual_subnet 配置一致；默认 CGNAT 100.64.0.0/10）")
	// mesh 连接参数组（hub/node-id/webrtc/insecure/stun/turn/gateway/smart/mdns）；
	// mesh connect 是服务名寻址，不注册 exit 族（--exit 语义不同）。
	meshconn.AddFlags(cmd)
	addTURNRESTFlags(cmd)
	return cmd
}

// meshACLLines 把「本 owner 的跨节点授权」格式化为逐行文本（**纯函数**，便于单测）。
//
// 口径：指纹**不截断**——用户需要拿它与配置文件里的值逐字对照；列对齐便于一眼看出「谁能写」。
// 无授权是正常态（未配任何 mesh_readers），故给提示行而非空输出；nil 输入给提示行不 panic。
func meshACLLines(acl *client.MeshACL) []string {
	if acl == nil {
		return []string{"跨节点授权: 不可用"}
	}
	if len(acl.Entries) == 0 {
		return []string{
			fmt.Sprintf("跨节点授权（owner=%s）: 无", dashIfEmpty(acl.Owner)),
			"提示：授权需在服务端卷 ACL 的 mesh_readers 中配置（scope = read|write|rw）",
		}
	}
	out := []string{fmt.Sprintf("跨节点授权（owner=%s, %d 条）:", dashIfEmpty(acl.Owner), len(acl.Entries))}
	for _, e := range acl.Entries {
		out = append(out, fmt.Sprintf("  %-12s %-12s %-5s %s", e.Volume, e.Node, e.Scope, e.Fingerprint))
	}
	return out
}

// newCmdMeshACL 创建 mesh acl：列出**本 owner** 的跨节点授权（卷 × 节点 × scope）。
//
// 可见性由服务端按已认证身份判定（仅 owner 自身，见 pkg/server/mesh_acl.go），故本命令没有
// owner 参数——不是遗漏，而是**故意**不给客户端指定他人的能力。
func newCmdMeshACL(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "acl",
		Short: "列出本 owner 的跨节点授权（卷 × 节点 × scope）",
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}
			acl, err := svc.MeshACL(cmd.Context())
			if err != nil {
				return err
			}
			for _, line := range meshACLLines(acl) {
				ios.WriteOutLine("%s", line)
			}
			return nil
		},
	}
	return cmd
}

// meshServerStatusLines 把服务端跨节点状态格式化为逐行文本（**纯函数**，便于单测）。
//
// 输出口径与 Web UI 状态卡一致：面（地址 + pin 数）、节点角色（含「未运行」显式标注）、hub/信令；
// **未启用的面/角色不输出**（不产生无意义空行）；nil 输入给出提示行（不 panic）。
func meshServerStatusLines(st *client.MeshStatus) []string {
	if st == nil {
		return []string{"跨节点状态: 不可用"}
	}
	out := []string{"sproxy 跨节点状态:"}
	if st.RemoteRead != nil {
		out = append(out, fmt.Sprintf("  只读面:   %s  pin=%d", dashIfEmpty(st.RemoteRead.Addr), st.RemoteRead.Pinned))
	}
	if st.RemoteWrite != nil {
		out = append(out, fmt.Sprintf("  写面:     %s  pin=%d", dashIfEmpty(st.RemoteWrite.Addr), st.RemoteWrite.Pinned))
	}
	if st.Node != nil {
		state := "未运行"
		if st.Node.Running {
			state = "运行中"
		}
		extra := ""
		if st.Node.WebRTC {
			extra += " webrtc=true"
		}
		if len(st.Node.Services) > 0 {
			extra += " 服务=" + strings.Join(st.Node.Services, "/")
		}
		out = append(out, fmt.Sprintf("  节点角色: %s (%s)%s", dashIfEmpty(st.Node.NodeID), state, extra))
	}
	hub := st.HubURL
	if hub == "" {
		hub = "本机"
	}
	sig := "未启用"
	if st.SignalingEnabled {
		sig = "已启用"
	}
	out = append(out, fmt.Sprintf("  hub: %s  信令: %s", hub, sig))
	return out
}

// dashIfEmpty 为空时返回 "-"（对齐本文件其它输出风格）。
func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// newCmdMeshStatus 创建 mesh status：列出 hub 上的 mesh 服务；
// 指定 --gateway 时改查本地 mesh node 网关拓扑（node-id + 服务宣告 + 已建直连链路）。
func newCmdMeshStatus(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "列出 hub 上的 mesh 服务（或 --gateway 查本地节点直连拓扑）",
		RunE: func(cmd *cobra.Command, args []string) error {
			// --server：查**服务端**的跨节点面/角色状态（GET /api/mesh/status；W4）。
			// 与 --gateway（本地 mesh node 网关拓扑）语义不同：前者是 sproxy 进程自身的状态。
			if serverStatus, _ := cmd.Flags().GetBool("server"); serverStatus {
				svc, err := factory.NewClient(cmd)
				if err != nil {
					return err
				}
				st, err := svc.MeshStatus(cmd.Context())
				if err != nil {
					return err
				}
				for _, line := range meshServerStatusLines(st) {
					ios.WriteOutLine("%s", line)
				}
				return nil
			}
			gatewayAddr, _ := cmd.Flags().GetString("gateway")
			if gatewayAddr != "" {
				// 网关认证 token = 本端 SproxySig SK（与 mesh node 的 NodeConfig.AccessKeySecret
				// 同源，网关侧做恒时比较；为空即不认证）。来自全局 --access-key-secret / 配置文件，
				// 与 HTTP Bearer（api_keys）无关。
				svc, err := factory.NewClient(cmd)
				if err != nil {
					return err
				}
				st, err := mesh.QueryGatewayStatus(cmd.Context(), gatewayAddr, svc.AccessKeySecret())
				if err != nil {
					return err
				}
				ios.WriteOutLine("mesh 节点: %s", st.NodeID)
				if len(st.Services) == 0 {
					ios.WriteOutLine("服务宣告: 无")
				} else {
					ios.WriteOutLine("服务宣告 (%d):", len(st.Services))
					for _, s := range st.Services {
						ios.WriteOutLine("  %-24s addr=%s", s.Name, s.Addr)
					}
				}
				if len(st.Peers) == 0 {
					ios.WriteOutLine("已建直连链路: 无")
				} else {
					ios.WriteOutLine("已建直连链路 (%d):", len(st.Peers))
					for _, p := range st.Peers {
						ios.WriteOutLine("  %-24s link=%s  since=%s", p.Peer, p.Link, p.Since.Format(time.RFC3339))
					}
				}
				return nil
			}
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
			// 拉节点列表构建 node → 虚拟 IP 映射（mesh status 显示 virtual_ip，设计 AD-5）。
			nodeVIP := map[string]string{}
			if nodes, nerr := svc.ListHubNodes(cmd.Context()); nerr == nil {
				for _, n := range nodes {
					if n.VirtualIP != "" {
						nodeVIP[n.ID] = n.VirtualIP
					}
				}
			}
			ios.WriteOutLine("mesh 服务 (%d):", len(svcs))
			for _, s := range svcs {
				addr := s.Addr
				if addr == "" {
					addr = "-"
				}
				vip := nodeVIP[s.Node]
				if vip == "" {
					vip = "-"
				}
				ios.WriteOutLine("  %-24s node=%s  vip=%s  addr=%s", s.Name, s.Node, vip, addr)
			}
			return nil
		},
	}
	cmd.Flags().Bool("server", false, "查询**服务端**（sproxy）自身的跨节点面/角色状态（GET /api/mesh/status）")
	cmd.Flags().String("gateway", "", "查询本地 mesh node 网关拓扑（127.0.0.1:port；node-id + 服务宣告 + 已建直连链路/链路类型）")
	return cmd
}

// meshForwardListen 监听本地端口，每个入站连接独立建立一条 mesh 连接（选路 dial）。
// ref 负责按需解析最新 target（带 TTL 缓存，感知节点上下线）；initial 仅用于启动横幅。
func meshForwardListen(cmd *cobra.Command, svc *client.FileClient, signaler webrtc.Signaler, dial meshDialFunc, ref *client.MeshTargetRefresher, initial *client.MeshService, localNode, listenAddr string, ios cli.IOStreams) error {
	// S56：裸 :port 归一为 127.0.0.1:port（loopback 安全默认，防 LAN 暴露 +
	// Windows 防火墙弹窗）；需 LAN 访问时显式通配地址:port 或具体 IP。
	listenAddr = iostream.NormalizeListenAddr(listenAddr)
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
			target, rerr := ref.Resolve(ctx)
			if rerr != nil {
				ios.WriteErrLine("建立 mesh 流失败: %v", rerr)
				return
			}
			res, cerr := dial(ctx, svc, signaler, target, localNode)
			if cerr != nil {
				// dial 失败（relay 404 / webrtc 失败）→ 强制缓存过期 + 记录失败节点，
				// 下个连接立即重取并优先跳过该节点（P1-13 候选 failover）。
				ref.Invalidate(target.Node)
				ios.WriteErrLine("建立 mesh 流失败: %v（目标 node=%s addr=%s 不可达或离线）", cerr, target.Node, target.Addr)
				return
			}
			conn := res.Conn
			defer conn.Close()
			// 拨号帧已由 dial 内部写好（P0-1）：relay 由 hub 写，webrtc 由
			// mesh.WebRTCStream 在 mux 流上写，客户端均直接透传。
			// T7 竞速结果可见性：连接提示行展示实际路径（Kind）+ 建连耗时（Latency，SmartDial
			// 填充；单路径 Dial 为 0 不显示）。
			if res.Latency > 0 {
				ios.WriteOutLine("连接已建立（%s, %s）: %s ⇄ %s", res.Kind, res.Latency, target.Node, target.Addr)
			} else {
				ios.WriteOutLine("连接已建立（%s）: %s ⇄ %s", res.Kind, target.Node, target.Addr)
			}
			// 双向泵送（CloseWrite 半关闭 + grace 宽限期，C1 范本，见 iostream.Pump）：
			// 任一方向完成即向对端传播半关闭，让在途响应仍可被读回；对端不回应 FIN
			// 时 grace 超时强制双侧关闭解除阻塞。返回后由外层 defer 收尾。
			iostream.Pump(c, conn, iostream.PumpGrace)
		}(local)
	}
}

// meshStdioOnce 单次模式：stdin/stdout 与一条 mesh 连接直通（选路 dial）。
// ref 负责解析最新 target（单次拨号使用当前缓存；失败返回错误可由调用方重试）。
func meshStdioOnce(cmd *cobra.Command, svc *client.FileClient, signaler webrtc.Signaler, dial meshDialFunc, ref *client.MeshTargetRefresher, localNode string, ios cli.IOStreams) error {
	target, err := ref.Resolve(cmd.Context())
	if err != nil {
		return err
	}
	res, err := dial(cmd.Context(), svc, signaler, target, localNode)
	if err != nil {
		return err
	}
	conn := res.Conn
	defer conn.Close()
	// 拨号帧已由 dial 内部写好（P0-1，同 meshForwardListen）：relay 由 hub 写，
	// webrtc 由 mesh.WebRTCStream 在 mux 流上写，客户端均直接透传。
	// T7 竞速结果可见性：展示实际路径（Kind）+ 建连耗时（Latency，SmartDial 填充；
	// 单路径 Dial 为 0 不显示）。
	if res.Latency > 0 {
		ios.WriteOutLine("已连接（%s, %s）: stdin/stdout ⇄ %s (Ctrl+D / EOF 断开)", res.Kind, res.Latency, target.Name)
	} else {
		ios.WriteOutLine("已连接（%s）: stdin/stdout ⇄ %s (Ctrl+D / EOF 断开)", res.Kind, target.Name)
	}
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
		// 在同一批代码的另一方向仍然存在——此处补上）。委托 iostream.CloseWrite。
		iostream.CloseWrite(conn)
	}()
	go func() { defer close(outDone); _, _ = io.Copy(ios.Out, conn) }()
	select {
	case <-outDone: // 对端断开：会话结束
	case <-inDone: // 本地 stdin 读完：半关闭已传播，等对端把剩余数据写完
		<-outDone
	}
	return nil
}
