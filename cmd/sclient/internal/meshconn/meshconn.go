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
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/iostream"
	"github.com/cocomhub/sproxy/pkg/tunnel/mesh"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/spf13/cobra"
)

// Conn 是一次命令的 mesh 连接上下文（由 flags + 配置回落装配）。
type Conn struct {
	ExitNode     string
	ExitAuto     bool
	ExitOnly     bool
	ExitExclude  []string
	LocalTimeout time.Duration
	GatewayAddr  string
	Smart        bool
	SmartTTL     time.Duration
	MDNS         bool
	MDNSSecret   string
	WebRTC       bool
	HubURL       string
	NodeID       string
	Insecure     bool
	STUN         []string
	TURN         []string
	TURNUser     string
	TURNPass     string
}

// DefaultLocalTimeout 是本地直连探测默认超时（与 mesh.DefaultLocalDialTimeout 一致）。
const DefaultLocalTimeout = mesh.DefaultLocalDialTimeout

// AddFlags 注册 mesh 连接共用 flag 集（连接参数组：hub/node-id/webrtc/insecure/stun/turn/
// gateway/smart/mdns）。出口路由 flag（--exit 族）用 AddExitFlags 单独注册——mesh connect
// 只注册连接参数组（无出口语义），socks/udp/http-proxy 两者都注册。
func AddFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("gateway", "", "经本地 mesh node 网关复用已建立直连链路路由（127.0.0.1:port）")
	f.Bool("smart", false, "自动选最佳路由：并行竞速直连/中继/经中间节点多跳（胜者缓存 TTL 30s）")
	f.Duration("smart-ttl", 0, "胜者缓存 TTL（配合 --smart；0 = 默认 30s）")
	f.Bool("mdns", false, "纯 mDNS 直连（不经 hub）")
	f.String("mdns-secret", "", "mDNS 模式共享密钥（为空回落 access_key_secret）")
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
	// exit 族：仅当 flag 已注册（AddExitFlags）时读取；mesh connect 只注册 AddFlags → 跳过。
	if cmd.Flags().Lookup("exit") != nil {
		if c.ExitNode, err = cmd.Flags().GetString("exit"); err != nil {
			return err
		}
	}
	if cmd.Flags().Lookup("exit-auto") != nil {
		if c.ExitAuto, err = cmd.Flags().GetBool("exit-auto"); err != nil {
			return err
		}
	}
	if cmd.Flags().Lookup("exit-only") != nil {
		if c.ExitOnly, err = cmd.Flags().GetBool("exit-only"); err != nil {
			return err
		}
	}
	if cmd.Flags().Lookup("exit-exclude") != nil {
		if c.ExitExclude, err = cmd.Flags().GetStringSlice("exit-exclude"); err != nil {
			return err
		}
	}
	if cmd.Flags().Lookup("local-timeout") != nil {
		if c.LocalTimeout, err = cmd.Flags().GetDuration("local-timeout"); err != nil {
			return err
		}
	} else {
		c.LocalTimeout = DefaultLocalTimeout
	}
	// 互斥与 fail-closed（exit 族未注册时 ExitNode/ExitAuto 恒零值，校验不触发）
	if c.ExitNode != "" && c.ExitAuto {
		return fmt.Errorf("--exit 与 --exit-auto 互斥，不能同时使用")
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
	if c.GatewayAddr, err = cmd.Flags().GetString("gateway"); err != nil {
		return err
	}
	if c.Smart, err = cmd.Flags().GetBool("smart"); err != nil {
		return err
	}
	if c.SmartTTL, err = cmd.Flags().GetDuration("smart-ttl"); err != nil {
		return err
	}
	if c.MDNS, err = cmd.Flags().GetBool("mdns"); err != nil {
		return err
	}
	if c.MDNSSecret, err = cmd.Flags().GetString("mdns-secret"); err != nil {
		return err
	}
	if c.WebRTC, err = cmd.Flags().GetBool("webrtc"); err != nil {
		return err
	}
	if c.HubURL, err = cmd.Flags().GetString("hub"); err != nil {
		return err
	}
	if c.NodeID, err = cmd.Flags().GetString("node-id"); err != nil {
		return err
	}
	if c.Insecure, err = cmd.Flags().GetBool("insecure"); err != nil {
		return err
	}
	if c.STUN, err = cmd.Flags().GetStringSlice("stun"); err != nil {
		return err
	}
	if c.TURN, err = cmd.Flags().GetStringSlice("turn"); err != nil {
		return err
	}
	if c.TURNUser, err = cmd.Flags().GetString("turn-user"); err != nil {
		return err
	}
	if c.TURNPass, err = cmd.Flags().GetString("turn-pass"); err != nil {
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

// Target 构造拨号目标：--exit 固定节点 → MeshService{Node: exit, Addr: addr}。
// --exit-auto 时由 AutoDial 内部按候选 nodeID 构造；服务名模式（mesh connect）由
// 调用方经 refresher 解析，不使用本方法。
func (c *Conn) Target(addr string) *client.MeshService {
	return &client.MeshService{Name: "proxy", Node: c.ExitNode, Addr: addr}
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
				if c.SmartTTL > 0 {
					res, derr := mesh.DialSmartWithOptions(ctx, svc, signaler, target, localNode, mesh.DialOptions{}, mesh.SmartOptions{CacheTTL: c.SmartTTL})
					if derr != nil {
						return nil, derr
					}
					return res.Conn, nil
				}
				res, derr := mesh.DialSmartDefault(ctx, svc, signaler, target, localNode)
				if derr != nil {
					return nil, derr
				}
				return res.Conn, nil
			}
			res, derr := mesh.Dial(ctx, svc, signaler, target, localNode)
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
	if c.ExitNode != "" {
		return c.LocalOrExit(exitDialFor(c.ExitNode))
	}
	return c.LocalOrExit(nil) // 纯本地
}
