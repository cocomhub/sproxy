// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package meshconn 统一收敛 sclient 的 mesh 连接参数组与装配：
// socks / udp map / mesh connect / http-proxy 四命令共享同一套 flag 与连接上下文，
// 同一连接方式下的后续扩展（--exit-auto、新传输、竞速升级）一处修改所有使用方收益。
package meshconn

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/cliflag"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/iostream"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mesh"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/spf13/cobra"
)

// Conn 是一次命令的 mesh 连接上下文（由 flags + 配置回落装配）。
type Conn struct {
	ExitNode string
	// ExitGroup 是出口节点组（--exit-group；组内按序 failover）。
	ExitGroup []string
	// ExitGroupMode 是出口节点组负载均衡模式（--exit-group-mode，默认 failover；
	// round-robin/weighted 时每连接轮转选起点，仍保留组内 failover 兜底）。
	ExitGroupMode string
	// ExitGroupWeights 是加权模式的位置对应权重（--exit-group-weight node:weight,
	// 按组内 node 顺序映射；长度不足/缺失 → 等权回落，Warn 可观测）。
	ExitGroupWeights []int
	// ExitGroupWeightRaw 是 --exit-group-weight 原始条目（解析前暂存）。
	ExitGroupWeightRaw []string
	ExitAuto           bool
	ExitOnly           bool
	ExitExclude        []string
	LocalTimeout       time.Duration
	GatewayAddr        string
	Smart              bool
	SmartTTL           time.Duration
	TrustX             []string
	MDNS               bool
	MDNSSecret         string
	E2E                bool
	E2EIdentity        string
	E2EPeerFP          []string
	WebRTC             bool
	HubURL             string
	NodeID             string
	Insecure           bool
	STUN               []string
	TURN               []string
	TURNUser           string
	TURNPass           string
}

// DefaultLocalTimeout 是本地直连探测默认超时（与 mesh.DefaultLocalDialTimeout 一致）。
const DefaultLocalTimeout = mesh.DefaultLocalDialTimeout

// e2eOpts 构造端到端加密配置（--e2e 显式开关）：未启用返回 nil（不静默启用）。
// identity 来源：--e2e-identity 文件路径（默认 XDG 配置目录 sproxy/identity.json，
// 经 clientfactory.LoadIdentityOptional 加载；无身份文件 = 自动生成临时身份——纯 ECDH）。
// peerFPs 是对端指纹白名单（--e2e-peer-fp，显式 pinning 防 MITM；空 = 纯 ECDH 防窃听）。
// 安全语义（可观测）：启用后日志告警说明模式（纯 ECDH vs pinning），禁静默降级。
func (c *Conn) E2EOpts() (*mesh.EndToEndOptions, error) {
	if !c.E2E {
		return nil, nil // 显式开关未开 = 不启用（默认关）
	}
	var identity *tunnel.Identity
	var err error
	if c.E2EIdentity != "" {
		id, lerr := tunnel.LoadIdentity(c.E2EIdentity)
		if lerr != nil {
			return nil, fmt.Errorf("加载端到端加密身份文件 %s 失败: %w", c.E2EIdentity, lerr)
		}
		identity = id
	} else {
		identity, err = clientfactory.LoadIdentityOptional()
		if err != nil {
			return nil, fmt.Errorf("加载端到端加密默认身份失败: %w", err)
		}
	}
	opts := &mesh.EndToEndOptions{
		Enabled:          true,
		Identity:         identity,
		PeerFingerprints: append([]string(nil), c.E2EPeerFP...),
		HandshakeTimeout: 30 * time.Second, // Minor-1：显式握手超时，防裸 ctx 无限阻塞
	}
	if identity == nil && len(c.E2EPeerFP) == 0 {
		// 纯 ECDH（无身份无 pin）——防窃听不防 MITM，明确告警（可观测）。
		return opts, fmt.Errorf("端到端加密纯 ECDH 模式（未配置 --e2e-identity / --e2e-peer-fp）——防窃听但无 MITM 防护，建议配置指纹 pinning")
	}
	return opts, nil
}

// AddFlags 注册 mesh 连接共用 flag 集（连接参数组：hub/node-id/webrtc/insecure/stun/turn/
// gateway/smart/mdns）。出口路由 flag（--exit 族）用 AddExitFlags 单独注册——mesh connect
// 只注册连接参数组（无出口语义），socks/udp/http-proxy 两者都注册。
func AddFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("gateway", "", "经本地 mesh node 网关复用已建立直连链路路由（127.0.0.1:port）")
	f.Bool("smart", false, "自动选最佳路由：并行竞速直连/中继/经中间节点多跳（胜者缓存 TTL 30s；竞速全部失败/无可选路径时回退固定顺序 webrtc→relay）")
	f.Duration("smart-ttl", 0, "胜者缓存 TTL（配合 --smart；0 = 默认 30s）")
	f.Bool("quality-routing", false, "传输质量感知选路（配合 --smart；候选按历史重传率加权降序启动，劣化候选延迟 100ms——同 RTT 时质量高者先胜）")
	f.StringSlice("trust-x", nil, "via-node 中间节点白名单（配合 --smart；可重复/逗号分隔；非空时仅白名单内节点 X 作为多跳中间节点——信任收敛；空 = 全部可信）")
	f.Bool("mdns", false, "纯 mDNS 直连（不经 hub）")
	f.String("mdns-secret", "", "mDNS 模式共享密钥（为空回落 access_key_secret）")
	f.Bool("e2e", false, "端到端加密（显式开关，默认关）：RelayStream 数据面包 DialE2EStream（ECDH + AES-256-GCM），X/hub 只透传密文（持 SK 读不到明文）。需配合 --e2e-identity 与 --e2e-peer-fp（至少一个对端指纹；无指纹 = 纯 ECDH 防窃听，显式 pinning 防 MITM）")
	f.String("e2e-identity", "", "端到端加密本端身份文件路径（默认 XDG 配置目录 sproxy/identity.json；无 = 自动生成临时身份）")
	f.StringSlice("e2e-peer-fp", nil, "端到端加密对端指纹白名单（可重复/逗号分隔；非空时握手 fail-closed 校验对端指纹——显式 pinning 防 MITM；空 = 纯 ECDH 防窃听）")
	f.Bool("webrtc", true, "优先 webrtc 打洞直连，失败回落 hub 中继")
	f.String("hub", "", "hub 地址（http(s)/ws(s)；默认取配置 hub_url，再回落 server_url）")
	f.String("node-id", "", "本节点 ID（信令来源；默认主机名）")
	f.Bool("insecure", false, "跳过 TLS 证书验证（自签 wss hub）")
	f.StringSlice("stun", nil, "STUN 服务器地址（可重复/逗号分隔）")
	f.StringSlice("turn", nil, "TURN 服务器地址（可重复/逗号分隔）")
	f.String("turn-user", "", "TURN 用户名")
	f.String("turn-pass", "", "TURN 密码")
}

// AddExitFlags 注册出口路由 flag 族（--exit/--exit-auto/--exit-only/--exit-exclude/--local-timeout）。
// 仅 socks/udp/http-proxy 等「经出口访问」命令注册；mesh connect（服务名寻址）不注册。
func AddExitFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("exit", "", "出口节点 node-id（本地直连失败后回退经它出站；需该节点 --dial-allow 并放行目标）")
	f.StringSlice("exit-group", nil, "出口节点组（逗号分隔 node-id；模式见 --exit-group-mode，故障自动切换；与 --exit/--exit-auto 互斥）")
	f.String("exit-group-mode", "failover", "出口节点组负载均衡模式：failover（按序，默认）/ round-robin（轮询）/ weighted（加权，配合 --exit-group-weight）")
	f.StringSlice("exit-group-weight", nil, "加权模式的节点权重（逗号分隔 node:weight，如 a:3,b:1；仅 weighted 模式有效；缺失/长度不足 = 等权回落）")
	f.Bool("exit-auto", false, "自动选出口节点（hub 节点列表 outbound-dial 能力优先，候选 failover）")
	f.Bool("exit-only", false, "强制恒经出口（不试本地直连）")
	f.StringSlice("exit-exclude", nil, "出口候选排除名单（逗号分隔 node-id，可多次；仅 --exit-auto 有效；被排除节点仍可中转）")
	f.Duration("local-timeout", DefaultLocalTimeout, "本地直连探测超时（0 = 不试本地直连）")
}

// FromFlags 读 flags + 配置回落（stun/turn 从 context env 回落；hub/node-id 从 svc 回落；
// mdns-secret 回落 access_key_secret），并校验互斥与 fail-closed 约束。
// exit 族 flag 未注册（mesh connect 场景）时跳过对应读取，Conn 字段保持零值。
func (c *Conn) FromFlags(cmd *cobra.Command, cfgSvc ConfigProvider) error {
	var err error
	// exit 族：仅当 flag 已注册（AddExitFlags）时读取；mesh connect 只注册 AddFlags → cliflag 跳过。
	if err = cliflag.String(cmd, "exit", &c.ExitNode); err != nil {
		return err
	}
	if err = cliflag.StringSlice(cmd, "exit-group", &c.ExitGroup); err != nil {
		return err
	}
	// exit 组负载均衡（11.1-④）：mode 校验 fail-closed；weight 仅配合 weighted。
	if err = cliflag.String(cmd, "exit-group-mode", &c.ExitGroupMode); err != nil {
		return err
	}
	if c.ExitGroupMode != "" {
		if _, nerr := mesh.NormalizeExitGroupMode(c.ExitGroupMode); nerr != nil {
			return nerr
		}
	}
	if err = cliflag.StringSlice(cmd, "exit-group-weight", &c.ExitGroupWeightRaw); err != nil {
		return err
	}
	if len(c.ExitGroupWeightRaw) > 0 {
		if len(c.ExitGroup) == 0 {
			return fmt.Errorf("--exit-group-weight 需要 --exit-group 指定出口节点组")
		}
		if c.ExitGroupMode != "" && c.ExitGroupMode != "weighted" {
			return fmt.Errorf("--exit-group-weight 仅配合 --exit-group-mode weighted 使用（当前模式 %q）", c.ExitGroupMode)
		}
		weights, werr := parseExitWeights(c.ExitGroupWeightRaw, c.ExitGroup)
		if werr != nil {
			return werr
		}
		c.ExitGroupWeights = weights
	}
	if err = cliflag.Bool(cmd, "exit-auto", &c.ExitAuto); err != nil {
		return err
	}
	if err = cliflag.Bool(cmd, "exit-only", &c.ExitOnly); err != nil {
		return err
	}
	if err = cliflag.StringSlice(cmd, "exit-exclude", &c.ExitExclude); err != nil {
		return err
	}
	if err = cliflag.Duration(cmd, "local-timeout", &c.LocalTimeout); err != nil {
		return err
	} else if cmd.Flags().Lookup("local-timeout") == nil {
		c.LocalTimeout = DefaultLocalTimeout
	}
	// 互斥与 fail-closed（exit 族未注册时 ExitNode/ExitAuto 恒零值，校验不触发）
	if c.ExitNode != "" && c.ExitAuto {
		return fmt.Errorf("--exit 与 --exit-auto 互斥，不能同时使用")
	}
	if len(c.ExitGroup) > 0 && c.ExitNode != "" {
		return fmt.Errorf("--exit-group 与 --exit 互斥（组内 failover 已覆盖单节点）")
	}
	if len(c.ExitGroup) > 0 && c.ExitAuto {
		return fmt.Errorf("--exit-group 与 --exit-auto 互斥（显式组 vs 自动候选）")
	}
	if c.ExitOnly && c.ExitAuto {
		return fmt.Errorf("--exit-only 与 --exit-auto 互斥，不能同时使用")
	}
	if len(c.ExitExclude) > 0 && c.ExitNode != "" && !c.ExitAuto {
		return fmt.Errorf("--exit-exclude 仅配合 --exit-auto 使用（固定 --exit 时无意义）")
	}
	if c.ExitOnly && c.ExitNode == "" && !c.ExitAuto {
		return fmt.Errorf("--exit-only 需要 --exit 或 --exit-auto 指定出口")
	}
	if err = cliflag.String(cmd, "gateway", &c.GatewayAddr); err != nil {
		return err
	}
	if err = cliflag.Bool(cmd, "smart", &c.Smart); err != nil {
		return err
	}
	if err = cliflag.Duration(cmd, "smart-ttl", &c.SmartTTL); err != nil {
		return err
	}
	if err = cliflag.StringSlice(cmd, "trust-x", &c.TrustX); err != nil {
		return err
	}
	if err = cliflag.Bool(cmd, "mdns", &c.MDNS); err != nil {
		return err
	}
	if err = cliflag.String(cmd, "mdns-secret", &c.MDNSSecret); err != nil {
		return err
	}
	if err = cliflag.Bool(cmd, "e2e", &c.E2E); err != nil {
		return err
	}
	if err = cliflag.String(cmd, "e2e-identity", &c.E2EIdentity); err != nil {
		return err
	}
	if err = cliflag.StringSlice(cmd, "e2e-peer-fp", &c.E2EPeerFP); err != nil {
		return err
	}
	if err = cliflag.Bool(cmd, "webrtc", &c.WebRTC); err != nil {
		return err
	}
	if err = cliflag.String(cmd, "hub", &c.HubURL); err != nil {
		return err
	}
	if err = cliflag.String(cmd, "node-id", &c.NodeID); err != nil {
		return err
	}
	if err = cliflag.Bool(cmd, "insecure", &c.Insecure); err != nil {
		return err
	}
	if err = cliflag.StringSlice(cmd, "stun", &c.STUN); err != nil {
		return err
	}
	if err = cliflag.StringSlice(cmd, "turn", &c.TURN); err != nil {
		return err
	}
	if err = cliflag.String(cmd, "turn-user", &c.TURNUser); err != nil {
		return err
	}
	if err = cliflag.String(cmd, "turn-pass", &c.TURNPass); err != nil {
		return err
	}
	// 配置回落（stun/turn 从 context env；hub/node-id/mdns-secret 需 svc，由调用方回落）
	if cfgSvc != nil {
		if cfg, cerr := cfgSvc.LoadConfig(); cerr == nil {
			if !cmd.Flags().Changed("stun") && len(c.STUN) == 0 && len(cfg.STUNServers) > 0 {
				c.STUN = cfg.STUNServers
			}
			if !cmd.Flags().Changed("turn") && len(c.TURN) == 0 && len(cfg.TURNServers) > 0 {
				c.TURN = cfg.TURNServers
				if c.TURNUser == "" {
					c.TURNUser = cfg.TURNUser
					c.TURNPass = cfg.TURNPass
				}
			}
		}
	}
	return nil
}

// DefaultMDNSLookupTimeout 是 mDNS 单次服务发现的等待窗口（对齐 cmd/sclient/mesh_mdns.go
// 既有 5s；下沉到本包供 socks/http-proxy 等命令复用）。
const DefaultMDNSLookupTimeout = 5 * time.Second

// ConfigProvider 是配置回落接口（cmd/sclient 的 ConfigProvider 满足）。
type ConfigProvider interface {
	LoadConfig() (*client.Config, error)
}

// DialFunc 是拨号函数签名（与 pkg/httpproxy / pkg/socks5 的 DialFunc 兼容）。
type DialFunc func(ctx context.Context, addr string) (net.Conn, error)

// parseExitWeights 解析 --exit-group-weight 条目（node:weight,node:weight）为
// 按组内 node 顺序的位置对应权重。语法校验 fail-closed：无冒号/非正整数/未知 node → 报错。
// 条目数少于组长度 → 缺失位填 0（PickExitGroup 对 ≤0 权重扇区按等权处理，不静默错位）。
func parseExitWeights(entries []string, group []string) ([]int, error) {
	weights := make([]int, len(group))
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		name, wstr, ok := strings.Cut(e, ":")
		if !ok || name == "" {
			return nil, fmt.Errorf("--exit-group-weight 条目 %q 格式无效（应为 node:weight，如 a:3）", e)
		}
		w, err := strconv.Atoi(wstr)
		if err != nil || w <= 0 {
			return nil, fmt.Errorf("--exit-group-weight 条目 %q 权重无效（应为正整数）", e)
		}
		idx := slices.Index(group, name)
		if idx < 0 {
			return nil, fmt.Errorf("--exit-group-weight 指定未知节点 %q（不在 --exit-group 中）", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("--exit-group-weight 节点 %q 重复指定", name)
		}
		seen[name] = true
		weights[idx] = w
	}
	return weights, nil
}

// LocalOrExit 构造最终拨号函数：本地直连优先（NewLocalOrExitDial）或恒出口（--exit-only）。
// exitDial 是经出口的拨号闭包（由调用方按 svc/signaler 构造；exit-auto 时内部按 nodeID 选择）。
func (c *Conn) LocalOrExit(exitDial DialFunc) DialFunc {
	if c.ExitOnly || c.LocalTimeout <= 0 {
		if exitDial == nil {
			return func(ctx context.Context, addr string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", addr)
			}
		}
		return exitDial
	}
	return mesh.NewLocalOrExitDial(c.LocalTimeout, exitDial)
}

// NormalizeListen 归一监听地址（loopback 安全默认）。
func NormalizeListen(addr string) string { return iostream.NormalizeListenAddr(addr) }

// Signalers 装配信令器：--mdns 时返回 (nil, nil, nil)（mDNS 服务器由调用方独占构造，
// 经 mdnsSrv.LookupPeer 后建直连信令，本方法不重复 NewMDNS/Start——避免双实例组播/
// Windows 二次 bind 5353 EADDRINUSE）；否则 hub AutoRegister 信令器（webrtc 打洞用）。
// 返回 signaler + close 闭包（nil 安全）；AutoRegister 注册失败回落中继（不致命，
// 返回非 nil regErr 供调用方打印诊断，signaler 为 nil 由 mesh.Dial 回落 relay-only）。
func (c *Conn) Signalers(ctx context.Context, svc *client.FileClient, caFile string) (webrtc.Signaler, func() error, error) {
	if c.MDNS {
		// mDNS 模式：信令由调用方经 mdnsSrv.LookupPeer 后建直连（ExitDialFor 的
		// mdnsSrv 参数），本方法不构造服务器（防双实例，P1-1）。
		return nil, nil, nil
	}
	if svc == nil || !c.WebRTC {
		return nil, nil, nil // 无信令（relay-only 或纯本地）
	}
	r, regErr := mesh.AutoRegister(ctx, mesh.AutoRegisterParams{
		HubURL:          c.HubURL,
		ServerURL:       svc.ServerURL(),
		AccessKey:       svc.AccessKey(),
		AccessKeySecret: svc.AccessKeySecret(),
		AccessKeyID:     svc.AccessKeyID(),
		NodeID:          c.NodeID,
		Prefix:          "mesh",
		ExactNode:       false,
		Insecure:        c.Insecure,
		CAFile:          caFile,
	})
	if regErr != nil {
		// 注册失败回落中继（mesh.Dial 内部处理 relay-only）：返回非 nil regErr 供
		// 调用方打印诊断（对齐 pre-diff socks 的「webrtc 信令注册失败: %v（回落 hub 中继）」）。
		return nil, nil, fmt.Errorf("webrtc 信令注册失败: %w", regErr)
	}
	return r.Signaler, r.Closer, nil
}

// ExitDialFor 构造经指定节点的出口拨号闭包（固定 --exit 或 --exit-auto 候选）。
// 收敛 socks.go 既有出口装配：gateway 优先（复用已建直连链路）→ mDNS 直连 →
// mesh.Dial（--smart 时 DialSmart 竞速）。signaler 为 nil（--webrtc=false / 注册失败）
// 时 mesh.Dial 回落 relay-only。
// 返回 nodeID → (ctx, addr) → (net.Conn, error)；AutoDial 内部为每个候选调用。
func (c *Conn) ExitDialFor(svc *client.FileClient, signaler webrtc.Signaler, localNode string, mdnsSrv *mesh.MDNSServer, logger *slog.Logger) func(nodeID string) func(ctx context.Context, addr string) (net.Conn, error) {
	_ = logger // 预留：出口拨号失败诊断日志（当前由错误向上传播，调用方处理）
	return func(nodeID string) func(ctx context.Context, addr string) (net.Conn, error) {
		return func(ctx context.Context, addr string) (net.Conn, error) {
			target := &client.MeshService{Name: "proxy", Node: nodeID, Addr: addr}
			if c.GatewayAddr != "" && svc != nil {
				if conn, gerr := mesh.GatewayConnect(ctx, c.GatewayAddr, nodeID, addr, svc.AccessKeySecret()); gerr == nil {
					return conn, nil
				}
			}
			if c.MDNS && mdnsSrv != nil {
				peer, perr := mdnsSrv.LookupPeer(ctx, nodeID, DefaultMDNSLookupTimeout)
				if perr != nil {
					return nil, fmt.Errorf("mDNS 未发现出口节点 %s: %w", nodeID, perr)
				}
				if verr := mesh.ValidateSignalAddr(peer.SignalAddr); verr != nil {
					return nil, verr
				}
				sig, serr := mesh.DialDirectSignaler(ctx, peer.SignalAddr, localNode)
				if serr != nil {
					return nil, serr
				}
				sig.SetSecret(c.MDNSSecret)
				res, derr := mesh.DialDirect(ctx, sig, target)
				_ = sig.Close()
				if derr != nil {
					return nil, derr
				}
				return res.Conn, nil
			}
			if svc == nil {
				return nil, fmt.Errorf("无可用 mesh 路由（需 --mdns 或可用的 hub 配置）")
			}
			if c.Smart {
				// 优雅降级：竞速全部候选失败/无可选路径时回退固定顺序 mesh.Dial（FallbackDial），
				// 连接仍可用而非报错（T3 语义融入 http-proxy 出口收敛架构）。
				so := mesh.SmartOptions{FallbackDial: mesh.Dial}
				if c.SmartTTL > 0 {
					so.CacheTTL = c.SmartTTL
				}
				if len(c.TrustX) > 0 {
					so.TrustedNodes = c.TrustX // --trust-x 中间节点白名单（T5 信任收敛）
				}
				e2e, eerr := c.E2EOpts()
				if eerr != nil {
					// 纯 ECDH 告警是提示非致命（防窃听仍生效）；仅身份加载失败才报错。
					if !strings.Contains(eerr.Error(), "纯 ECDH") {
						return nil, eerr
					}
				}
				res, derr := mesh.DialSmartWithOptions(ctx, svc, signaler, target, localNode, mesh.DialOptions{E2E: e2e}, so)
				if derr != nil {
					return nil, derr
				}
				return res.Conn, nil
			}
			e2e, eerr := c.E2EOpts()
			if eerr != nil {
				if !strings.Contains(eerr.Error(), "纯 ECDH") {
					return nil, eerr
				}
			}
			res, derr := mesh.DialWithOptions(ctx, svc, signaler, target, localNode, mesh.DialOptions{AllowRelayFallback: true, E2E: e2e})
			if derr != nil {
				return nil, derr
			}
			return res.Conn, nil
		}
	}
}

// AutoDial 构造最终拨号函数：
//   - --exit-auto → NewAutoExitDial（nodeLister = svc.ListHubNodes，候选 outbound-dial 优先
//   - ExitExclude 排除名单）；
//   - --exit <node> → LocalOrExit（本地直连优先，回退经该节点出口）；
//   - 无出口 → 纯本地直连（等价 Config.Dial=nil）。
//
// fail-closed：出口拨号错误一律向上传播（不吞错回退本地），由调用方映射为用户可见错误；
// --exit-only 时本地直连被 LocalOrExit 禁用（恒经出口）。
func (c *Conn) AutoDial(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler, localNode string, mdnsSrv *mesh.MDNSServer, logger *slog.Logger) DialFunc {
	exitDialFor := c.ExitDialFor(svc, signaler, localNode, mdnsSrv, logger)
	if c.ExitAuto {
		return mesh.NewAutoExitDial(c.LocalTimeout, func(ctx context.Context) ([]client.HubNodeInfo, error) {
			if svc == nil {
				return nil, fmt.Errorf("--exit-auto 需要可用的 hub 客户端（--mdns 纯局域网模式不支持自动选出口）")
			}
			return svc.ListHubNodes(ctx)
		}, exitDialFor, c.ExitExclude)
	}
	if len(c.ExitGroup) > 0 {
		// --exit-group：组内负载均衡（--exit-group-mode 选模式；默认 failover 零回归），
		// 仍内嵌本地直连优先（NewExitGroupDialWithMode 经 NewLocalOrExitDial 包装）。
		mode := mesh.ExitGroupMode(c.ExitGroupMode)
		if mode == "" {
			mode = mesh.ExitGroupFailover // 未指定/flag 未注册 → 默认 failover（零回归）
		}
		if mode == mesh.ExitGroupWeighted && len(c.ExitGroupWeights) == 0 && logger != nil {
			// 可观测：weighted 未配权重 → 等权回落（不静默）。
			logger.Warn("--exit-group-mode weighted 未配置 --exit-group-weight，等权回落（round-robin 分发）")
		}
		return mesh.NewExitGroupDialWithMode(c.LocalTimeout, c.ExitGroup, exitDialFor, mode, c.ExitGroupWeights)
	}
	if c.ExitNode != "" {
		return c.LocalOrExit(exitDialFor(c.ExitNode))
	}
	return c.LocalOrExit(nil) // 纯本地
}
