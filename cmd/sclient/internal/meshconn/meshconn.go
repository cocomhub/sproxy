// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package meshconn 统一收敛 sclient 的 mesh 连接参数组与装配：
// socks / udp map / mesh connect / http-proxy 四命令共享同一套 flag 与连接上下文，
// 同一连接方式下的后续扩展（--exit-auto、新传输、竞速升级）一处修改所有使用方收益。
package meshconn

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/cliflag"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/httpproxy"
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
	// UpstreamProxy 是上游 HTTP 代理（如 http://user:pass@host:port）——本地 mesh
	// 拨号失败/超时后经它 CONNECT 转发（线路B：mac→国内 dev-live→SG 出口）。
	// 空 = 不启用（仅本地 mesh 路径）。
	UpstreamProxy string
	// Routes 是分流规则（--route <domain|cidr>=<exit-group>；声明序，首个命中）。
	Routes []RouteRule
	// routesRaw 是 --route 的原始条目（FromFlags 读取，ParseRoutes 解析进 Routes）。
	routesRaw []string
}

// RouteKind 是分流规则的类型（domain = 域名后缀匹配；cidr = IP 网段前缀匹配）。
type RouteKind string

const (
	// RouteDomain 域名后缀匹配：规则 Pattern 是去掉前导通配符/点的规范化域名
	// （*.example.com / .example.com / example.com 同义），目标 host 以
	// "."+Pattern 或等于 Pattern 结尾时命中（子域名与自身均命中，防子串误配）。
	RouteDomain RouteKind = "domain"
	// RouteCIDR IP 网段匹配：规则 Pattern 是 CIDR（netip.ParsePrefix 可解析），
	// 目标 host 为 IP 且属于该网段时命中。
	RouteCIDR RouteKind = "cidr"
)

// RouteRule 是一条分流规则（--route <domain|cidr>=<exit-group>）。
// Pattern 是规范化后的域名（小写、去首通配符/点）或 CIDR 文本；
// Group 是该规则命中的出口节点组（组内按序 failover，与 --exit-group 同语义）。
type RouteRule struct {
	Kind    RouteKind
	Pattern string
	Group   []string
}

// DefaultLocalTimeout 是本地直连探测默认超时（与 mesh.DefaultLocalDialTimeout 一致）。
const DefaultLocalTimeout = mesh.DefaultLocalDialTimeout

// flagLocalTimeout 是 --local-timeout 旗标名（注册/读取共享常量，防拼写漂移）。
const flagLocalTimeout = "local-timeout"

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
	f.Duration("renew-interval", 24*time.Hour, "运行中凭据自动轮换间隔（0=关闭；默认 24h 自动 renew SK 并热替换，常驻无需重启）")
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
	f.Duration(flagLocalTimeout, DefaultLocalTimeout, "本地直连探测超时（0 = 不试本地直连）")
	// StringArray（非 StringSlice）：规则值内含逗号（域名后缀与出口组分隔），
	// StringSlice 会在逗号处错误拆分为多个条目；StringArray 逐次追加保真。
	f.StringArray("route", nil, "分流规则 domain|cidr=exit-group（--route .example.com=node-a,node-b --route 10.0.0.0/8=node-c；域名后缀匹配，cidr 网段匹配；多规则按声明序首个命中；未命中回落默认出口）")
	f.String("upstream-proxy", "", "上游 HTTP 代理（http://user:pass@host:port，如 http://cg:pass@61.153.100.229:40086）——本地 mesh 拨号失败/超时后经它 CONNECT 转发到目标（线路B：本地直连不通时经国内服务器→新加坡出口）。数据面 TLS 端到端加密，控制面带认证")
}

// FromFlags 读 flags + 配置回落（stun/turn 从 context env 回落；hub/node-id 从 svc 回落；
// mdns-secret 回落 access_key_secret），并校验互斥与 fail-closed 约束。
// exit 族 flag 未注册（mesh connect 场景）时跳过对应读取，Conn 字段保持零值。
func (c *Conn) FromFlags(cmd *cobra.Command, cfgSvc ConfigProvider) error {
	// exit 族：仅当 flag 已注册（AddExitFlags）时读取；mesh connect 只注册 AddFlags → cliflag 跳过。
	if err := c.exitFlagsFromCmd(cmd); err != nil {
		return err
	}
	if err := c.meshFlagsFromCmd(cmd); err != nil {
		return err
	}
	// 配置回落（stun/turn 从 context env；hub/node-id/mdns-secret 需 svc，由调用方回落）
	return c.applySTUNTURNConfig(cmd, cfgSvc)
}

// meshFlagsFromCmd 读取 mesh 连接参数组 flag（gateway/smart/smart-ttl/trust-x/mdns/
// mdns-secret/e2e/e2e-identity/e2e-peer-fp/webrtc/renew-interval/hub/node-id/
// insecure/stun/turn/turn-user/turn-pass）。
func (c *Conn) meshFlagsFromCmd(cmd *cobra.Command) error {
	for _, f := range []struct {
		name string
		read func(*cobra.Command, string, *string) error
	}{
		{"gateway", cliflag.String},
		{"hub", cliflag.String},
		{"node-id", cliflag.String},
		{"mdns-secret", cliflag.String},
		{"e2e-identity", cliflag.String},
		{"turn-user", cliflag.String},
		{"turn-pass", cliflag.String},
	} {
		if err := f.read(cmd, f.name, stringPtr(c, f.name)); err != nil {
			return err
		}
	}
	for _, f := range []struct {
		name string
		read func(*cobra.Command, string, *bool) error
	}{
		{"smart", cliflag.Bool},
		{"mdns", cliflag.Bool},
		{"e2e", cliflag.Bool},
		{"webrtc", cliflag.Bool},
		{"insecure", cliflag.Bool},
	} {
		if err := f.read(cmd, f.name, boolPtr(c, f.name)); err != nil {
			return err
		}
	}
	if err := cliflag.Duration(cmd, "smart-ttl", &c.SmartTTL); err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		read func(*cobra.Command, string, *[]string) error
	}{
		{"trust-x", cliflag.StringSlice},
		{"e2e-peer-fp", cliflag.StringSlice},
		{"stun", cliflag.StringSlice},
		{"turn", cliflag.StringSlice},
	} {
		if err := f.read(cmd, f.name, slicePtr(c, f.name)); err != nil {
			return err
		}
	}
	return nil
}

// stringPtr 返回 Conn 中对应 flag 字段的指针（meshFlagsFromCmd 用）。
func stringPtr(c *Conn, name string) *string {
	switch name {
	case "gateway":
		return &c.GatewayAddr
	case "hub":
		return &c.HubURL
	case "node-id":
		return &c.NodeID
	case "mdns-secret":
		return &c.MDNSSecret
	case "e2e-identity":
		return &c.E2EIdentity
	case "turn-user":
		return &c.TURNUser
	default:
		return &c.TURNPass
	}
}

// boolPtr 返回 Conn 中对应 flag 字段的指针（meshFlagsFromCmd 用）。
func boolPtr(c *Conn, name string) *bool {
	switch name {
	case "smart":
		return &c.Smart
	case "mdns":
		return &c.MDNS
	case "e2e":
		return &c.E2E
	case "webrtc":
		return &c.WebRTC
	default:
		return &c.Insecure
	}
}

// slicePtr 返回 Conn 中对应 flag 字段的指针（meshFlagsFromCmd 用）。
func slicePtr(c *Conn, name string) *[]string {
	switch name {
	case "trust-x":
		return &c.TrustX
	case "e2e-peer-fp":
		return &c.E2EPeerFP
	case "stun":
		return &c.STUN
	default:
		return &c.TURN
	}
}

// exitFlagsFromCmd 读取 exit 族 flag（--exit/--exit-group/--exit-group-mode/
// --exit-group-weight/--exit-auto/--exit-only/--exit-exclude/--local-timeout/--route/
// --upstream-proxy）并校验互斥与 fail-closed 约束。exit 族 flag 未注册（mesh connect
// 场景）时跳过对应读取，Conn 字段保持零值。
func (c *Conn) exitFlagsFromCmd(cmd *cobra.Command) error {
	var err error
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
	if err = c.validateExitWeights(); err != nil {
		return err
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
	if err = cliflag.Duration(cmd, flagLocalTimeout, &c.LocalTimeout); err != nil {
		return err
	} else if cmd.Flags().Lookup(flagLocalTimeout) == nil {
		c.LocalTimeout = DefaultLocalTimeout
	}
	if err = cliflag.StringArray(cmd, "route", &c.routesRaw); err != nil {
		return err
	}
	if err = cliflag.String(cmd, "upstream-proxy", &c.UpstreamProxy); err != nil {
		return err
	}
	return c.validateExitMutualExclusion()
}

// validateExitWeights 解析 --exit-group-weight（仅 weighted 模式配合）并校验
// fail-closed：无 --exit-group / mode 非 weighted / 解析失败均报错。
func (c *Conn) validateExitWeights() error {
	if len(c.ExitGroupWeightRaw) == 0 {
		return nil
	}
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
	return nil
}

// validateExitMutualExclusion 校验 exit 族互斥与 fail-closed 约束（exit 族未注册时
// ExitNode/ExitAuto 恒零值，校验不触发），并解析 --route 规则。
func (c *Conn) validateExitMutualExclusion() error {
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
	if c.ExitOnly && c.ExitNode == "" && !c.ExitAuto && len(c.ExitGroup) == 0 {
		// --exit-group 也是有效出口（组内 failover）——放行恒经出口语义。
		return fmt.Errorf("--exit-only 需要 --exit / --exit-group / --exit-auto 指定出口")
	}
	// 路由互斥与 fail-closed（--route 解析）：
	// ① 与 --exit-only 语义冲突（恒经出口时分流无意义）→ 拒绝；
	// ② 与 --exit/--exit-group/--exit-auto 可共存（route 是更高优先级分流，未命中回落默认）。
	if len(c.routesRaw) > 0 && c.ExitOnly {
		return fmt.Errorf("--route 与 --exit-only 语义冲突，不能同时使用（--exit-only 恒经出口，分流无意义）")
	}
	routes, rerr := ParseRoutes(c.routesRaw)
	if rerr != nil {
		return rerr
	}
	c.Routes = routes
	return nil
}

// applySTUNTURNConfig 应用 STUN/TURN 配置回落（从 context env；未显式指定且 Conn
// 无值时用配置值）。hub/node-id/mdns-secret 需 svc，由调用方回落。
func (c *Conn) applySTUNTURNConfig(cmd *cobra.Command, cfgSvc ConfigProvider) error {
	if cfgSvc == nil {
		return nil
	}
	cfg, cerr := cfgSvc.LoadConfig()
	if cerr != nil {
		return nil
	}
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
	return nil
}

// DefaultMDNSLookupTimeout 是 mDNS 单次服务发现的等待窗口（对齐 cmd/sclient/mesh_mdns.go
// 既有 5s；下沉到本包供 socks/http-proxy 等命令复用）。
const DefaultMDNSLookupTimeout = 5 * time.Second

// ParseRoutes 解析 --route 规则列表（每条 `domain|cidr=exit-group`），fail-closed：
// 非法格式（无 `=` 或 group 为空）、非法 CIDR → 返回含具体规则文本的错误。
// 域名规则归一化：去首 `*` 与前导 `.`、转小写（`*.example.com` / `.example.com` /
// `example.com` 同义）；空 pattern 报错。cidr 用 netip.ParsePrefix 解析（失败即非法）。
func ParseRoutes(raw []string) ([]RouteRule, error) {
	var routes []RouteRule
	for _, r := range raw {
		rule, rerr := parseRouteRule(r)
		if rerr != nil {
			return nil, rerr
		}
		routes = append(routes, rule)
	}
	return routes, nil
}

// parseRouteRule 解析单条 --route 规则（`domain|cidr=exit-group`），fail-closed：
// 非法格式（无 `=` 或 group 为空）、非法 CIDR、裸 IP → 返回含具体规则文本的错误。
// 域名规则归一化：去首 `*`（*.example.com）与全部前导 `.`，转小写（大小写不敏感
// 匹配）；空 pattern 报错。
func parseRouteRule(r string) (RouteRule, error) {
	spec, groupSpec, ok := strings.Cut(r, "=")
	if !ok || strings.TrimSpace(groupSpec) == "" {
		return RouteRule{}, fmt.Errorf("--route 规则 %q 格式非法（应为 <domain|cidr>=<exit-group>）", r)
	}
	group := strings.Split(groupSpec, ",")
	for i := range group {
		group[i] = strings.TrimSpace(group[i])
		if group[i] == "" {
			return RouteRule{}, fmt.Errorf("--route 规则 %q 的出口组包含空节点", r)
		}
	}
	pat := strings.TrimSpace(spec)
	if pat == "" {
		return RouteRule{}, fmt.Errorf("--route 规则 %q 缺失匹配目标（应为 <domain|cidr>=<exit-group>）", r)
	}
	if _, perr := netip.ParsePrefix(pat); perr == nil {
		return RouteRule{Kind: RouteCIDR, Pattern: pat, Group: group}, nil
	} else if strings.Contains(pat, "/") {
		// 形如 CIDR 却解析失败（含 "/"）→ 非法 CIDR fail-closed 报错。
		return RouteRule{}, fmt.Errorf("--route 规则 %q 的 CIDR 非法: %v", r, perr)
	}
	// 裸 IP（netip.ParseAddr 成功）→ 非法（缺少网段前缀）；域名不可能含 "/"，
	// 此处仅拦裸 IP（作域名规则永不命中，拒绝而非静默）。
	if _, aerr := netip.ParseAddr(pat); aerr == nil {
		return RouteRule{}, fmt.Errorf("--route 规则 %q 为裸 IP（缺少网段前缀，应为 CIDR 如 10.0.0.0/8）", r)
	}
	// 域名：去首 `*`（*.example.com）与全部前导 `.`，转小写（大小写不敏感匹配）。
	domain := strings.ToLower(strings.TrimLeft(strings.TrimPrefix(pat, "*"), "."))
	if domain == "" {
		return RouteRule{}, fmt.Errorf("--route 域名规则 %q 归一化后为空（应为有效域名或 CIDR）", r)
	}
	return RouteRule{Kind: RouteDomain, Pattern: domain, Group: group}, nil
}

// SelectRoute 按声明序匹配分流规则：解析 addr 的 host（net.SplitHostPort）→
// 域名 host 走域名后缀匹配（子域名与自身均命中，防子串误配）、IP host 走 cidr
// 网段匹配 → 首个命中返回其出口组；无命中返回 nil（调用方回落默认出口/本地直连）。
// 边界（注释明示）：域名规则不依赖 DNS；cidr 规则对纯域名 host 不命中（回落默认）。
func (c *Conn) SelectRoute(addr string) []string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // 无端口（测试/畸形输入）：整串按 host 匹配
	}
	host = strings.Trim(host, "[]")
	for _, r := range c.Routes {
		switch r.Kind {
		case RouteDomain:
			if routeMatchesDomain(host, r.Pattern) {
				return r.Group
			}
		case RouteCIDR:
			if routeMatchesCIDR(host, r.Pattern) {
				return r.Group
			}
		}
	}
	return nil
}

// routeMatchesDomain 域名后缀匹配：等于或子域（后缀 .<pattern>）；大小写不敏感。
func routeMatchesDomain(host, pattern string) bool {
	return strings.EqualFold(host, pattern) || strings.HasSuffix(strings.ToLower(host), "."+pattern)
}

// routeMatchesCIDR 检查 host（解析为 IP）是否落在 pattern（CIDR）网段内。
func routeMatchesCIDR(host, pattern string) bool {
	ip, aerr := netip.ParseAddr(host)
	if aerr != nil {
		return false
	}
	p, perr := netip.ParsePrefix(pattern)
	return perr == nil && p.Contains(ip)
}

// ConfigProvider 是配置回落接口（cmd/sclient 的 ConfigProvider 满足）。
type ConfigProvider interface {
	LoadConfig() (*client.Config, error)
}

// DialFunc 是拨号函数签名（与 pkg/httpproxy / pkg/socks5 的 DialFunc 兼容）。
type DialFunc func(ctx context.Context, addr string) (net.Conn, error)

// parseExitWeights 解析 --exit-group-weight 条目（node:weight,node:weight）为
// 按组内 node 顺序的位置对应权重。语法校验 fail-closed：无冒号/非正整数/未知 node → 报错。
// 条目数少于组长度 → 缺失位填 0（PickExitGroup 对 ≤0 权重扇区按等权处理，不静默错位）。
func parseExitWeights(entries, group []string) ([]int, error) {
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

// smartFallbackDial 构造 smart 竞速全部候选失败时的降级拨号（T3 优雅降级）。
// ⚠️ 安全：E2E 配置时 fallback 保留 E2E（DialWithOptions+E2E）——禁静默降级明文
// （裸 mesh.Dial 会让错误盐/指纹不匹配场景悄悄明文转发，违反"所有数据必须加密"）。
func smartFallbackDial(c *Conn) func(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler, target *client.MeshService, localNode string) (*mesh.Result, error) {
	return func(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler, target *client.MeshService, localNode string) (*mesh.Result, error) {
		e2e, eerr := c.exitE2EOpts()
		if eerr != nil {
			return nil, eerr
		}
		return mesh.DialWithOptions(ctx, svc, signaler, target, localNode, mesh.DialOptions{AllowRelayFallback: true, E2E: e2e})
	}
}

// exitE2EOpts 获取 E2E 选项（--e2e 显式开关）。纯 ECDH 告警是提示非致命（防窃听仍
// 生效）；仅身份加载失败才报错（禁止静默降级明文）。
func (c *Conn) exitE2EOpts() (*mesh.EndToEndOptions, error) {
	e2e, eerr := c.E2EOpts()
	if eerr != nil {
		if !strings.Contains(eerr.Error(), "纯 ECDH") {
			return nil, eerr
		}
	}
	return e2e, nil
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
			return c.exitDialOnce(ctx, svc, signaler, localNode, nodeID, addr, mdnsSrv)
		}
	}
}

// exitDialOnce 单次出口拨号（nodeID 目标）：gateway 优先（复用已建直连链路）→ mDNS
// 直连 → mesh.Dial（--smart 时 DialSmart 竞速）。signaler 为 nil（--webrtc=false /
// 注册失败）时 mesh.Dial 回落 relay-only。
func (c *Conn) exitDialOnce(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler, localNode, nodeID, addr string, mdnsSrv *mesh.MDNSServer) (net.Conn, error) {
	target := &client.MeshService{Name: "proxy", Node: nodeID, Addr: addr}
	if conn, ok := c.exitDialGateway(ctx, svc, nodeID, addr); ok {
		return conn, nil
	}
	if c.MDNS && mdnsSrv != nil {
		return c.exitDialMDNS(ctx, localNode, nodeID, target, mdnsSrv)
	}
	if svc == nil {
		return nil, fmt.Errorf("无可用 mesh 路由（需 --mdns 或可用的 hub 配置）")
	}
	return c.exitDialMesh(ctx, svc, signaler, localNode, nodeID, target)
}

// exitDialGateway 优先尝试本地 mesh node 网关复用已建直连链路（零重新打洞）。
// 返回 (conn, true) 表示网关成功；否则 (nil, false) 回落后续路径（与既有回落语义一致）。
func (c *Conn) exitDialGateway(ctx context.Context, svc *client.FileClient, nodeID, addr string) (net.Conn, bool) {
	if c.GatewayAddr == "" || svc == nil {
		return nil, false
	}
	conn, gerr := mesh.GatewayConnect(ctx, c.GatewayAddr, nodeID, addr, svc.AccessKeySecret())
	if gerr != nil {
		return nil, false
	}
	return conn, true
}

// exitDialMDNS 经 mDNS 直连信令拨号出口节点（--mdns 纯局域网模式）。
func (c *Conn) exitDialMDNS(ctx context.Context, localNode, nodeID string, target *client.MeshService, mdnsSrv *mesh.MDNSServer) (net.Conn, error) {
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
	return withRoute(res.Conn, nodeID+"|webrtc|e2e?"), nil
}

// exitDialMesh 经 hub 信令拨号出口节点：--smart 时 DialSmart 竞速（优雅降级到固定
// 顺序拨号 FallbackDial，连接仍可用而非报错，T3 语义），否则 DialWithOptions
// （AllowRelayFallback）。--trust-x 中间节点白名单（T5 信任收敛）。
func (c *Conn) exitDialMesh(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler, localNode, nodeID string, target *client.MeshService) (net.Conn, error) {
	if c.Smart {
		// 优雅降级：竞速全部候选失败/无可选路径时回退固定顺序拨号（FallbackDial）。
		// ⚠️ 安全：E2E 配置时 fallback 必须**保留 E2E**（DialWithOptions+E2E），
		// 禁静默降级明文（裸 mesh.Dial 会让错误盐/指纹不匹配场景悄悄明文转发）。
		so := mesh.SmartOptions{FallbackDial: smartFallbackDial(c)}
		if c.SmartTTL > 0 {
			so.CacheTTL = c.SmartTTL
		}
		if len(c.TrustX) > 0 {
			so.TrustedNodes = c.TrustX // --trust-x 中间节点白名单（T5 信任收敛）
		}
		e2e, eerr := c.exitE2EOpts()
		if eerr != nil {
			return nil, eerr
		}
		res, derr := mesh.DialSmartWithOptions(ctx, svc, signaler, target, localNode, mesh.DialOptions{E2E: e2e}, so)
		if derr != nil {
			return nil, derr
		}
		return withRoute(res.Conn, routeDesc(nodeID, res)), nil
	}
	e2e, eerr := c.exitE2EOpts()
	if eerr != nil {
		return nil, eerr
	}
	res, derr := mesh.DialWithOptions(ctx, svc, signaler, target, localNode, mesh.DialOptions{AllowRelayFallback: true, E2E: e2e})
	if derr != nil {
		return nil, derr
	}
	return withRoute(res.Conn, routeDesc(nodeID, res)), nil
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
	// 分流规则命中（--route）：每连接按目标 host 重新匹配（socks CONNECT / http-proxy
	// 绝对 URI / cloud download URL 的目标每连接可能不同），命中组用 NewExitGroupDial
	// （本地直连优先 + 组内 failover，与 --exit-group 同装配）。未命中 → 原默认逻辑。
	nodeLister := c.nodeLister(svc)
	base := c.baseDial(ctx, exitDialFor, nodeLister)
	// --upstream-proxy：本地 mesh 拨号失败/超时后，经上游 HTTP 代理 CONNECT 转发
	// （线路B：本地直连不通/慢时自动经国内服务器→新加坡出口）。数据面由目标 TLS
	// 端到端加密；上游段控制面 CONNECT 带认证。本地成功 → 不经上游（零开销）。
	if c.UpstreamProxy != "" {
		return c.upstreamDial(base, logger)
	}
	return base
}

// baseDial 构造基础拨号（无 --route 时直接装配；有 --route 时按目标 host 分流）。
func (c *Conn) baseDial(ctx context.Context, exitDialFor func(nodeID string) func(ctx context.Context, addr string) (net.Conn, error), nodeLister func(ctx context.Context) ([]client.HubNodeInfo, error)) DialFunc {
	if len(c.Routes) > 0 {
		return func(ctx context.Context, addr string) (net.Conn, error) {
			if group := c.SelectRoute(addr); len(group) > 0 {
				return mesh.NewExitGroupDial(c.LocalTimeout, group, exitDialFor)(ctx, addr)
			}
			return c.defaultDial(ctx, addr, exitDialFor, nodeLister)
		}
	}
	return c.defaultDialWithClosure(ctx, exitDialFor, nodeLister)
}

// upstreamDial 包装上游代理拨号：本地失败且非取消时经上游 CONNECT 转发。
// 配置非法时 fail-closed（返回始终失败的 Dial，不静默回落本地明文）。
func (c *Conn) upstreamDial(base DialFunc, logger *slog.Logger) DialFunc {
	up, uerr := parseUpstreamProxy(c.UpstreamProxy)
	if uerr != nil {
		// 配置非法 fail-closed：返回始终失败的 Dial（不静默回落本地明文）。
		return func(context.Context, string) (net.Conn, error) {
			return nil, fmt.Errorf("--upstream-proxy 配置非法: %w", uerr)
		}
	}
	return func(ctx context.Context, addr string) (net.Conn, error) {
		conn, derr := base(ctx, addr)
		if derr == nil {
			// 本地路径：mesh 出口已带路由（RouteInfoer）；纯本地直连补 "direct"。
			if _, ok := conn.(httpproxy.RouteInfoer); !ok {
				conn = withRoute(conn, "direct")
			}
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, derr // 调用方取消：不 fallback
		}
		logger.Warn("本地 mesh 拨号失败，经上游代理", "addr", addr, "upstream", up.Host, "error", derr)
		uconn, uerr := upstreamConnect(ctx, up, addr)
		if uerr != nil {
			return nil, uerr
		}
		return withRoute(uconn, "upstream|"+up.Host), nil
	}
}

// defaultDial 是 AutoDial 的默认出口选择（无 --route 或路由未命中时的回落路径）。
// exitDialFor 与 nodeLister 由 AutoDial 捕获（svc 经闭包传入），避免重复构造。
func (c *Conn) defaultDial(ctx context.Context, addr string, exitDialFor func(nodeID string) func(ctx context.Context, addr string) (net.Conn, error), nodeLister func(ctx context.Context) ([]client.HubNodeInfo, error)) (net.Conn, error) {
	if c.ExitAuto {
		return mesh.NewAutoExitDial(c.LocalTimeout, nodeLister, exitDialFor, c.ExitExclude)(ctx, addr)
	}
	if len(c.ExitGroup) > 0 {
		// --exit-group：组内按序 failover（NewExitGroupDial 内嵌本地直连优先）。
		return mesh.NewExitGroupDial(c.LocalTimeout, c.ExitGroup, exitDialFor)(ctx, addr)
	}
	if c.ExitNode != "" {
		return c.LocalOrExit(exitDialFor(c.ExitNode))(ctx, addr)
	}
	return c.LocalOrExit(nil)(ctx, addr) // 纯本地
}

// defaultDialWithClosure 无 --route 时的直接装配（等价原 AutoDial 行为）。
func (c *Conn) defaultDialWithClosure(ctx context.Context, exitDialFor func(nodeID string) func(ctx context.Context, addr string) (net.Conn, error), nodeLister func(ctx context.Context) ([]client.HubNodeInfo, error)) DialFunc {
	if c.ExitAuto {
		return mesh.NewAutoExitDial(c.LocalTimeout, nodeLister, exitDialFor, c.ExitExclude)
	}
	if len(c.ExitGroup) > 0 {
		// --exit-group：组内负载均衡（--exit-group-mode 选模式；默认 failover 零回归），
		// 仍内嵌本地直连优先（NewExitGroupDialWithMode 经 NewLocalOrExitDial 包装）。
		return mesh.NewExitGroupDialWithMode(c.LocalTimeout, c.ExitGroup, exitDialFor, c.exitGroupMode(), c.ExitGroupWeights)
	}
	if c.ExitNode != "" {
		return c.LocalOrExit(exitDialFor(c.ExitNode))
	}
	return c.LocalOrExit(nil) // 纯本地
}

// exitGroupMode 返回 --exit-group-mode 归一值（空 → 默认 failover 零回归）。
func (c *Conn) exitGroupMode() mesh.ExitGroupMode {
	if c.ExitGroupMode == "" {
		return mesh.ExitGroupFailover // 未指定/flag 未注册 → 默认 failover（零回归）
	}
	return mesh.ExitGroupMode(c.ExitGroupMode)
}

// nodeLister 构造 --exit-auto 的候选源闭包（svc.ListHubNodes；svc 为 nil 时报错）。
func (c *Conn) nodeLister(svc *client.FileClient) func(ctx context.Context) ([]client.HubNodeInfo, error) {
	return func(ctx context.Context) ([]client.HubNodeInfo, error) {
		if svc == nil {
			return nil, fmt.Errorf("--exit-auto 需要可用的 hub 客户端（--mdns 纯局域网模式不支持自动选出口）")
		}
		return svc.ListHubNodes(ctx)
	}
}

// upstreamProxy 是解析后的上游 HTTP 代理配置。
type upstreamProxy struct {
	Host string // 上游地址（host:port）
	User string // Basic 认证用户名
	Pass string // Basic 认证密码
}

// parseUpstreamProxy 解析 --upstream-proxy URL（http://user:pass@host:port）。
// 仅支持 http:// 前缀（CONNECT 隧道）；带 https:// 拒绝（上游 TLS 隧道的 TLS-in-TLS
// 不必要——目标本身端到端 TLS）。非法 URL / 缺 host → 报错（fail-closed）。
func parseUpstreamProxy(raw string) (*upstreamProxy, error) {
	if !strings.HasPrefix(raw, "http://") {
		return nil, fmt.Errorf("--upstream-proxy 仅支持 http:// 前缀（https:// 上游不必要）: %q", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("解析上游代理: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("上游代理缺 host: %q", raw)
	}
	up := &upstreamProxy{Host: u.Host}
	if u.User != nil {
		up.User = u.User.Username()
		up.Pass, _ = u.User.Password()
	}
	return up, nil
}

// upstreamConnect 经上游 HTTP 代理建立到目标 addr 的 CONNECT 隧道。
// 控制面：TCP 连上游 → 发 CONNECT addr HTTP/1.1 + Proxy-Authorization Basic。
// 返回隧道连接（双向字节流，数据面由目标 TLS 端到端加密）。
func upstreamConnect(ctx context.Context, up *upstreamProxy, addr string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", up.Host)
	if err != nil {
		return nil, fmt.Errorf("连上游代理 %s 失败: %w", up.Host, err)
	}
	req := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n"
	if up.User != "" || up.Pass != "" {
		token := base64.StdEncoding.EncodeToString([]byte(up.User + ":" + up.Pass))
		req += "Proxy-Authorization: Basic " + token + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("写 CONNECT 失败: %w", err)
	}
	// 读响应状态行 + 头（到空行止）。手动解析避免 http.ReadResponse 把隧道
	// 数据当 body 消费（CONNECT 200 后紧跟数据面字节——ReadResponse 会提前读）。
	br := bufio.NewReader(conn)
	statusLine, rerr := br.ReadString('\n')
	if rerr != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("读上游状态行失败: %w", rerr)
	}
	// 状态行格式 "HTTP/1.1 200 Connection Established"
	parts := strings.SplitN(strings.TrimSpace(statusLine), " ", 3)
	if len(parts) < 2 {
		_ = conn.Close()
		return nil, fmt.Errorf("上游状态行非法: %q", statusLine)
	}
	if parts[1] != "200" {
		// 读剩余头（到空行）后报错，附带上游信息。
		_, _ = br.ReadString('\n')
		_ = conn.Close()
		return nil, fmt.Errorf("上游 CONNECT 失败: %s", strings.TrimSpace(statusLine))
	}
	// 跳过剩余头（到空行）。
	for {
		line, lerr := br.ReadString('\n')
		if lerr != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("读上游响应头失败: %w", lerr)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	// 返回 bufio.Reader 包装的连接：br 可能已预读隧道数据（跳过响应头时），
	// 直接返回底层 conn 会丢失 br 缓冲内的字节。Reader=br（保留缓冲），
	// Writer/Closer=conn（隧道写与关闭走底层）。
	return &bufferedConn{Reader: br, conn: conn}, nil
}

// bufferedConn 组合 bufio.Reader（保留预读缓冲）与底层连接（写/关闭）。
type bufferedConn struct {
	*bufio.Reader
	conn net.Conn
}

func (b *bufferedConn) Write(p []byte) (int, error) { return b.conn.Write(p) }
func (b *bufferedConn) Close() error                { return b.conn.Close() }
func (b *bufferedConn) LocalAddr() net.Addr         { return b.conn.LocalAddr() }
func (b *bufferedConn) RemoteAddr() net.Addr        { return b.conn.RemoteAddr() }
func (b *bufferedConn) SetDeadline(t time.Time) error {
	return b.conn.SetDeadline(t)
}
func (b *bufferedConn) SetReadDeadline(t time.Time) error {
	return b.conn.SetReadDeadline(t)
}
func (b *bufferedConn) SetWriteDeadline(t time.Time) error {
	return b.conn.SetWriteDeadline(t)
}

// RoutedConn 是携带路由信息的 net.Conn 包装：AutoDial/ExitDialFor 返回它，
// httpproxy 检测到实现 RouteInfoer 接口时写 X-Mesh-Path 头 / Debug 日志。
// Route 描述格式："<exitNode>|<kind>|<e2e>"（如 "sg-t|relay|e2e"）。
type RoutedConn struct {
	net.Conn
	route string
}

// Route 返回路由描述（httpproxy.RouteInfoer 实现）。
func (c *RoutedConn) Route() string { return c.route }

// withRoute 包装连接携带路由信息（route 空 = 不包装，零开销）。
func withRoute(conn net.Conn, route string) net.Conn {
	if conn == nil || route == "" {
		return conn
	}
	return &RoutedConn{Conn: conn, route: route}
}

// routeDesc 构造路由描述："<exitNode>|<kind>[|<e2e>]"
// kind = res.Kind（webrtc/relay/via-node）；e2e 标记（res.EndToEnd 或 opts.E2E）。
func routeDesc(nodeID string, res *mesh.Result) string {
	if res == nil || res.Conn == nil {
		return ""
	}
	desc := nodeID + "|" + res.Kind
	if res.EndToEnd {
		desc += "|e2e"
	}
	return desc
}
