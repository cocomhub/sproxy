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

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/credrotate"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/iostream"
	"github.com/cocomhub/sproxy/pkg/netutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mesh"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/p2p"
	"github.com/cocomhub/sproxy/pkg/tunnel/relay"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/spf13/cobra"
)

// p2pUI 把 CLI 流映射为隧道层手工信令的窄接口（p2p.UI）。
//
// 隧道层（pkg/tunnel/p2p）不反向依赖 pkg/cli，适配只在本文件做一次；信令等待窗口
// （原本文件的 manualSignalingTimeout 常量）已随实现迁入隧道层，导出为 p2p.ManualSignalingTimeout。
func p2pUI(ios cli.IOStreams) p2p.UI {
	return p2p.UI{Out: ios.Out, Err: ios.ErrOut, In: ios.In}
}

// NewCmdP2P 创建 p2p 父命令：基于 WebRTC 打洞的点对点连接。
// 信令经 hub 的 /api/signal/* 桥，数据面打洞成功后直连（不经过 hub）。
// cfgSvc 为可选配置提供者（P2-配置3）：--hub/--node-id 未显式指定时从配置
// hub_url/node_id 回落；hub 注册准入用 SproxySig AK/SK（根 --access-key/--access-key-secret 或配置）。
func NewCmdP2P(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ...ConfigProvider) *cobra.Command {
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
	cmd.AddCommand(newCmdP2PListen(factory, ios, provider))
	return cmd
}

// p2pFlags 是 p2p 相关命令的公共 flag。
type p2pFlags struct {
	hub         string
	node        string
	stun        []string
	turn        []string
	turnUser    string
	turnPass    string
	turnREST    string
	turnRESTUsr string
	turnRESTSvc string
	creds       *credrotate.Credentials // 动态凭据（运行中自动轮换；nil = 静态）
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
	cmd.Flags().Duration("renew-interval", 24*time.Hour, "运行中凭据自动轮换间隔（0=关闭；默认 24h 自动 renew SK 并热替换，常驻无需重启）")
	cmd.Flags().StringVar(&f.turnREST, "turn-rest", "", "TURN REST API 短期凭证端点（如 https://turn.example.com/turn；http 仅限 loopback）；配 --turn 使用，REST 优先于 --turn-user/--turn-pass")
	cmd.Flags().StringVar(&f.turnRESTUsr, "turn-rest-user", "", "TURN REST API 认证用户名（与 --turn-rest 配合，透传给服务端）")
	cmd.Flags().StringVar(&f.turnRESTSvc, "turn-rest-service", "", "TURN REST API 可选 service 参数（与 --turn-rest 配合，透传给服务端）")
}

// applyConfig 应用运行时全局配置（STUN/TURN）。在连接创建前调用。
// TURN 相关仅对显式提供的 flag 生效：--turn 非空才调 SetTURNServers，
// --turn-user/--turn-pass 非空才调 SetTURNCredential（缺 flag = 保持现状）；
// --turn-rest 非空才调 SetTURNRESTURL（REST 优先于静态）。非法 REST 配置返回错误
// （fail-closed，命令终止，不静默忽略）。
func (f *p2pFlags) applyConfig() error {
	if f.stun != nil {
		webrtc.SetSTUNServers(f.stun)
	}
	if f.turn != nil {
		webrtc.SetTURNServers(f.turn)
	}
	if f.turnUser != "" || f.turnPass != "" {
		webrtc.SetTURNCredential(f.turnUser, f.turnPass)
	}
	if f.turnREST != "" {
		if err := webrtc.SetTURNRESTURL(f.turnREST, f.turnRESTUsr, f.turnRESTSvc); err != nil {
			return fmt.Errorf("--turn-rest 配置无效: %w", err)
		}
	}
	return nil
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
	caFile, _ := cmd.Flags().GetString("ca-file")
	ak, _ := cmd.Root().PersistentFlags().GetString("access-key")
	sk, _ := cmd.Root().PersistentFlags().GetString("access-key-secret")
	akID, _ := cmd.Root().PersistentFlags().GetString("access-key-id")
	ak, sk, akID, caFile = p2pCredsFromConfig(cfgSvc, ak, sk, akID, caFile)
	// 动态凭据：credrotate 轮换后每次重注册取最新 SK。
	if f.creds != nil {
		ak, sk, akID = f.creds.Get()
	}
	return mesh.AutoRegister(ctx, mesh.AutoRegisterParams{
		HubURL:          f.hub,
		AccessKey:       ak,
		AccessKeySecret: sk,
		AccessKeyID:     akID,
		NodeID:          f.localNode(),
		Prefix:          "p2p",
		ExactNode:       exactNode,
		Insecure:        insecure,
		CAFile:          caFile,
	})
}

// p2pCredsFromConfig 以配置文件补齐未显式指定的 AK/SK/AKID/CAFile
// （优先级：CLI flag > 配置文件；P2-配置3）。cfgSvc 为 nil 时原样返回。
func p2pCredsFromConfig(cfgSvc ConfigProvider, ak, sk, akID, caFile string) (string, string, string, string) {
	if cfgSvc == nil {
		return ak, sk, akID, caFile
	}
	if cfg, cerr := cfgSvc.LoadConfig(); cerr == nil {
		if ak == "" {
			ak = cfg.AccessKey
		}
		if sk == "" {
			sk = cfg.AccessKeySecret
		}
		if akID == "" {
			akID = cfg.AccessKeyID
		}
		if caFile == "" {
			caFile = cfg.XferCAFile
		}
	}
	return ak, sk, akID, caFile
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
			return runP2PConnect(cmd, &f, cfgSvc, ios, p2pConnectParams{manual: manual, offerFile: offerFile, answerFile: answerFile, peer: peer, tcpAddr: tcpAddr, listenAddr: listenAddr})
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
func newCmdP2PListen(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	var f p2pFlags
	cmd := &cobra.Command{
		Use:   "listen",
		Short: "作为对端监听 WebRTC 直连（信令经 hub 或手工 SDP）",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			httpClient := &http.Client{Timeout: 30 * time.Second, Transport: netutil.DefaultTransport()}
			manual, _ := cmd.Flags().GetBool("manual")
			offerFile, _ := cmd.Flags().GetString("offer")
			answerFile, _ := cmd.Flags().GetString("answer")
			services, _ := cmd.Flags().GetStringArray("service")
			dialAllowCIDRs, _ := cmd.Flags().GetStringArray("dial-allow-cidr")
			f.applyConfigFallback(cfgSvc) // P2-配置3：未显式指定的 hub/token/node-id 取配置文件
			if err := f.applyConfig(); err != nil {
				return err
			}

			// I46：relay 会话诊断日志经 ios.ErrOut 输出（用户可见 + 可测试），带 node
			// 上下文便于多会话区分；丢弃日志会让出口拨号拒绝/失败原因完全不可见。
			serveLogger := slog.New(slog.NewTextHandler(ios.ErrOut, nil)).With("node", f.localNode())
			// I45：出口拨号策略——--service 宣告地址精确放行 + --dial-allow-cidr 网段放行
			// + 虚拟 IP NAT（selfVIP 初始无效；经 hub 信令注册后由 REG_OK 下发再更新）。
			// 无任何配置时 opts 为空 → Serve 回落默认 DialAllowed（仅公网），向后兼容。
			// 虚拟 IP 子网：--virtual-subnet 覆盖默认 CGNAT（S-1 审查修复，匹配自定义 hub 子网）。
			vipSubnet := parseVIPSubnetFlag(cmd, ios, cfgSvc)
			serveOpts := buildP2PServeOpts(services, dialAllowCIDRs, netip.Addr{}, vipSubnet, ios)

			// 选信令器：--manual 用文件或 stdin/stdout 交换（单次连接，不循环）；
			// 否则经 hub 信令桥（B17 自动注册，exact node 供 --peer 寻址）。
			sig, reg, serveOpts, err := p2pListenSignaler(ctx, cmd, &f, factory, cfgSvc, p2pListenParams{
				manual: manual, offerFile: offerFile, answerFile: answerFile,
				services: services, dialAllowCIDRs: dialAllowCIDRs, vipSubnet: vipSubnet,
				initialOpts: serveOpts, serveLogger: serveLogger, ios: ios,
			})
			if err != nil {
				return err
			}

			// --manual 需人工拷文件/粘贴 JSON，信令等待放宽到 10 分钟（默认 30s 必然不够）
			if manual {
				webrtc.SetSignalingTimeout(p2p.ManualSignalingTimeout)
				// S69：命令结束恢复默认超时，防全局泄漏污染库内嵌场景与后续测试。
				defer webrtc.ResetSignalingTimeout()
			}

			// 手动模式单次连接：无论打洞成功/失败/panic，退出前都兜底清理本侧写出的 SDP 文件
			if ms, ok := sig.(*p2p.ManualSignaler); ok {
				defer ms.Cleanup()
			}

			loop := &p2pListenLoop{
				ctx: ctx, cmd: cmd, f: &f, factory: factory, cfgSvc: cfgSvc,
				manual: manual, sig: sig, reg: reg,
				services: services, dialAllowCIDRs: dialAllowCIDRs, vipSubnet: vipSubnet,
				serveOpts: serveOpts, httpClient: httpClient, logger: serveLogger, ios: ios,
			}
			return loop.run()
		},
	}
	cmd.Flags().Bool("manual", false, "手工 SDP 信令（不依赖 hub）：提供 --offer/--answer 走文件交换，否则走 stdin/stdout 粘贴 JSON")
	cmd.Flags().String("offer", "", "--manual 文件模式的 offer SDP 文件路径（需同时给 --answer）")
	cmd.Flags().String("answer", "", "--manual 文件模式的 answer SDP 文件路径（需同时给 --offer）")
	cmd.Flags().StringArray("service", nil,
		"出口拨号白名单：宣告的服务地址（格式 name:addr，可重复；仅取 addr 精确放行，不注册到 hub）")
	cmd.Flags().StringArray("dial-allow-cidr", nil,
		"出口拨号白名单网段（如 192.168.0.0/16；配合放行内网服务，默认仅公网）")
	cmd.Flags().String(flagVirtualSubnet, hub.DefaultVirtualSubnet, "虚拟 IP 子网（CIDR，仅 IPv4；需与 hub.virtual_subnet 配置一致；默认 CGNAT 100.64.0.0/10）")
	f.add(cmd)
	return cmd
}

// p2pManualSignaler 构造手工 SDP 信令器（--manual 文件或 stdin/stdout 交换）。
// 文件模式需同时提供 --offer 与 --answer；S67：两路径相同会在交换中读到同一文件
// （type 不匹配），或对端重写导致误读——前置拒绝。
func p2pManualSignaler(offerFile, answerFile string, ios cli.IOStreams) (webrtc.Signaler, error) {
	needFile := offerFile != "" || answerFile != ""
	if needFile && (offerFile == "" || answerFile == "") {
		return nil, fmt.Errorf("--manual 文件模式需要同时提供 --offer 与 --answer")
	}
	if needFile {
		if offerFile == answerFile {
			return nil, fmt.Errorf("--offer 与 --answer 不能指向同一路径（文件交换需两个独立文件）")
		}
		return p2p.NewManualSignaler(offerFile, answerFile, p2pUI(ios)), nil
	}
	return p2p.NewManualStdioSignaler(p2pUI(ios)), nil
}

// p2pStartCredRotation 启动运行中凭据自动轮换（f.creds 非 nil 且 --renew-interval>0 时）：
// factory 建临时 svc + credrotate.Start → OnRotate 热替换 f.creds → registerSignaler
// 重注册用最新 SK（同 mesh node/relay start）。返回 stop 函数（未启动时为 nil），
// 由调用方 defer 保证命令退出时停止轮换。
func p2pStartCredRotation(ctx context.Context, cmd *cobra.Command, f *p2pFlags, factory clientfactory.Factory, logger *slog.Logger) func() {
	if f.creds == nil {
		return nil
	}
	renewInterval, _ := cmd.Flags().GetDuration("renew-interval")
	if renewInterval <= 0 {
		return nil
	}
	if renewSvc, rerr := factory.NewClient(cmd); rerr == nil && renewSvc != nil {
		if stopRenew, ok := credrotate.Start(ctx, renewSvc, credrotate.Options{
			Interval: renewInterval,
			Logger:   logger,
			OnRotate: f.creds.Update,
		}); ok {
			return stopRenew
		}
	}
	return nil
}

// p2pManualSignaling 是手工 SDP 信令参数（--manual 开关 + --offer/--answer 文件），
// p2p listen/connect 共用（避免 S107 参数爆炸）。
type p2pManualSignaling struct {
	manual     bool
	offerFile  string
	answerFile string
}

// p2pConnectParams 是 p2p connect 的拨号参数（对端/目标/监听 + 手工 SDP 模式）。
type p2pConnectParams struct {
	p2pManualSignaling
	peer       string
	tcpAddr    string
	listenAddr string
}

// p2pListenParams 是 p2p listen 信令器装配与出口拨号策略参数（flag 派生）。
type p2pListenParams struct {
	p2pManualSignaling
	services       []string
	dialAllowCIDRs []string
	vipSubnet      netip.Prefix
	initialOpts    []relay.ServeOptions
	serveLogger    *slog.Logger
	ios            cli.IOStreams
}

// p2pListenSignaler 选择 listen 的信令器并建立 serve 出口策略：
// --manual 用文件或 stdin/stdout 交换（单次连接，不循环，保留传入的初始 serveOpts）；
// 否则经 hub 信令桥（B17 自动注册，exact node 供 --peer 寻址），并启动运行中凭据
// 自动轮换，selfVIP 由 REG_OK 下发后更新出口拨号策略（虚拟 IP NAT）。返回 signaler、
// 注册（manual 时 nil，closer 由调用方 defer）、serveOpts 与错误。
func p2pListenSignaler(ctx context.Context, cmd *cobra.Command, f *p2pFlags, factory clientfactory.Factory, cfgSvc ConfigProvider, lp p2pListenParams) (webrtc.Signaler, *mesh.TempRegistration, []relay.ServeOptions, error) {
	if lp.manual {
		sig, merr := p2pManualSignaler(lp.offerFile, lp.answerFile, lp.ios)
		return sig, nil, lp.initialOpts, merr
	}
	// B17：经 hub 信令前自动注册自身（声明 per-node-secret 能力）。p2p listen 是
	// 被寻址方，必须用精确 node_id（f.localNode()）注册，否则 connect 的 --peer <id>
	// 无法寻址。注册连接保活整个 accept 循环，closer 在命令退出时关闭；信令 400/403
	// （节点被 hub 移除，secret 已轮换）时在重连退避循环内重注册自愈。
	if err := f.requireHub(); err != nil {
		return nil, nil, nil, err
	}
	if stop := p2pStartCredRotation(ctx, cmd, f, factory, lp.serveLogger); stop != nil {
		defer stop()
	}
	reg, rerr := f.registerSignaler(ctx, cmd, cfgSvc, true)
	if rerr != nil {
		return nil, nil, nil, rerr
	}
	// 虚拟 IP NAT：selfVIP 由 REG_OK 下发（稳定常驻注册），更新出口拨号策略。
	opts := buildP2PServeOpts(lp.services, lp.dialAllowCIDRs, reg.VirtualIP, lp.vipSubnet, lp.ios)
	return reg.Signaler, reg, opts, nil
}

// p2pListenLoop 承载 p2p listen 的常驻 accept 循环。sig/reg/serveOpts 为可变状态：
// 重注册自愈时替换（selfVIP 随重注册可能轮换）。
type p2pListenLoop struct {
	ctx            context.Context // NOSONAR: S8242 — 长期驻留结构体持有 ctx（装配/测试底座生命周期），非请求作用域
	cmd            *cobra.Command
	f              *p2pFlags
	factory        clientfactory.Factory
	cfgSvc         ConfigProvider
	manual         bool
	sig            webrtc.Signaler
	reg            *mesh.TempRegistration
	services       []string
	dialAllowCIDRs []string
	vipSubnet      netip.Prefix
	serveOpts      []relay.ServeOptions
	httpClient     *http.Client
	logger         *slog.Logger
	ios            cli.IOStreams
	delay          time.Duration // 监听失败重试退避（指数，封顶 reconnectMaxDelay）
}

// run 常驻 accept 循环：每条 p2p 连接交给 relay.Serve 分发（dial 帧 / HTTP 中继）。
// 信令失败（如临时网络抖动）时带退避重试，作为常驻服务不应轻易退出。
func (l *p2pListenLoop) run() error {
	l.delay = reconnectBaseDelay
	for {
		conn, err := webrtc.ListenWithSignaler(l.f.localNode(), l.sig)
		if err != nil {
			handled, herr := l.handleListenFailure(err)
			if !handled {
				return herr
			}
			continue
		}
		l.delay = reconnectBaseDelay
		if l.serveConn(conn) {
			return nil
		}
	}
}

// handleListenFailure 处理 accept 循环的监听错误。handled=true 时调用方应 continue
// （空闲超时重置退避 / 退避后重试）；handled=false 时返回终止错误（ctx 取消为 nil、
// manual 单次失败为包装错误），调用方直接 return。
func (l *p2pListenLoop) handleListenFailure(err error) (bool, error) {
	if l.ctx.Err() != nil {
		return false, nil
	}
	// manual 模式单次连接，失败直接返回（文件已消费，重试无意义）
	if l.manual {
		return false, fmt.Errorf("p2p 打洞失败: %w", err)
	}
	if errors.Is(err, webrtc.ErrNoIncomingConnection) {
		// P1-11：空闲超时（signalingTimeout 内无对端发起连接）——不是失败。
		// 旧实现把 30s 空闲当失败，无条件重注册 + per-node secret 轮换，hub 每次
		// 替换都关旧 WS，注册连接持续抖动。空闲时保持注册、重置退避、继续监听；
		// 仅真实失败（如信令 400/403，节点被 hub 移除）才走下方重注册自愈。
		l.delay = reconnectBaseDelay
		return true, nil
	}
	l.ios.WriteErrLine("p2p 监听失败，%v 后重试: %v", l.delay, err)
	// B17：节点可能已被 hub 移除（注册 WS 断 / 心跳超时），per-node secret 已轮换——
	// 信令 400/403 时在重连退避循环内重注册自愈；重注册失败不阻断退避（保持既有
	// 网络抖动重试行为），下一轮循环继续尝试。
	if reg2, rerr2 := l.f.registerSignaler(l.ctx, l.cmd, l.cfgSvc, true); rerr2 == nil {
		_ = l.reg.Closer()
		l.reg, l.sig = reg2, reg2.Signaler
		// selfVIP 随重注册可能轮换，同步更新出口拨号策略。
		l.serveOpts = buildP2PServeOpts(l.services, l.dialAllowCIDRs, l.reg.VirtualIP, l.vipSubnet, l.ios)
	} else {
		l.ios.WriteErrLine("p2p 重注册失败: %v", rerr2)
	}
	select {
	case <-time.After(l.delay):
		l.delay *= 2
		if l.delay > reconnectMaxDelay {
			l.delay = reconnectMaxDelay
		}
	case <-l.ctx.Done():
		return false, nil
	}
	return true, nil
}

// serveConn 处理一条已接受的 p2p 连接：交给 relay.Serve 分发。spawn 前快照 serveOpts
// ——避免重注册分支主循环重写（selfVIP 轮换）导致 serve goroutine 并发读（B-审查竞态），
// 每条连接固定使用其建立时刻的策略。manual 模式单次连接返回 stop=true（阻塞等待
// 连接结束，不进入 accept 循环）。
func (l *p2pListenLoop) serveConn(conn *webrtc.Conn) bool {
	m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleListener)
	opts := l.serveOpts
	go func() {
		defer m.Close()
		// 契约：relay.Serve「ctx 取消 → nil，真错误 → 非 nil」（见 pkg/tunnel/relay/leaf.go；
		// mux 被 Close 属后者，会打印）。故只在真错误时提示会话异常结束——正常关闭
		// （ctx 取消）不再打印误导性的错误行。
		if err := relay.Serve(l.ctx, m, "http://127.0.0.1:8080", true, l.httpClient, l.logger, opts...); err != nil {
			l.ios.WriteErrLine("p2p 会话结束: %v", err)
		}
	}()
	// manual 模式单次连接：不再进入 accept 循环，但必须阻塞等待连接结束（返回会让
	// main 退出，直接杀掉 relay.Serve/心跳 goroutine 与 WebRTC 连接）。阻塞到 mux
	// 关闭（任一侧断开/心跳超时）或 ctx 取消为止，无额外超时。
	if l.manual {
		select {
		case <-m.Done():
		case <-l.ctx.Done():
		}
		return true
	}
	return false
}

// buildP2PServeOpts 构造 p2p listen 的 relay.ServeOptions：--service 宣告地址
// 精确放行 + --dial-allow-cidr 网段放行 + 虚拟 IP NAT（selfVIP 由 AutoRegister 的
// REG_OK 下发；默认 CGNAT 子网，宣告端口自动开放）。复用 relay.NewVirtualIPDialPolicy
// （内部已含宣告地址精确匹配与公网/CIDR 回落）。
// 无任何放行配置且 selfVIP 无效时返回 nil → Serve 回落默认 DialAllowed（仅公网）。
//
// 注意：--service 仅作拨号白名单，不宣告到 hub（p2p listen 不注册节点），
// 语义与 relay start 的注册宣告解耦（I45 子决策）。
func buildP2PServeOpts(services, dialAllowCIDRs []string, selfVIP netip.Addr, vipSubnet netip.Prefix, ios cli.IOStreams) []relay.ServeOptions {
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
	return []relay.ServeOptions{
		{DialPolicy: relay.NewVirtualIPDialPolicy(vipSubnet.Masked(), selfVIP, nil, dialAllowCIDRs, serviceAddrs)},
	}
}

// parseVIPSubnetFlag 解析 --virtual-subnet（非法/非 IPv4 回落默认 CGNAT 并告警）。
// T6b：flag 未显式指定时从 context env 回落（cfgSvc 合成视图；config.yaml 不存在
// 时回落平铺旧字段为空 → 用默认 CGNAT，行为不变）。
func parseVIPSubnetFlag(cmd *cobra.Command, ios cli.IOStreams, cfgSvc ConfigProvider) netip.Prefix {
	s, _ := cmd.Flags().GetString(flagVirtualSubnet)
	if !cmd.Flags().Changed(flagVirtualSubnet) && s == hub.DefaultVirtualSubnet {
		if cfg, cerr := loadTrustLoginConfig(cfgSvc); cerr == nil && cfg.VirtualSubnet != "" {
			s = cfg.VirtualSubnet
		}
	}
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is4() {
		ios.WriteErrLine("--virtual-subnet %q 非法，使用默认子网 %s", s, hub.DefaultVirtualSubnet)
		return netip.MustParsePrefix(hub.DefaultVirtualSubnet)
	}
	return p.Masked()
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
			if mesh.WriteDialFrame(stream, tcpAddr) != nil {
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

// runP2PConnect 执行 p2p connect 命令主体：建立 WebRTC 直连后按模式转发/直通。
func runP2PConnect(cmd *cobra.Command, f *p2pFlags, cfgSvc ConfigProvider, ios cli.IOStreams, cp p2pConnectParams) error {
	if cp.peer == "" || cp.tcpAddr == "" {
		return fmt.Errorf("--peer 与 --tcp 均不能为空")
	}
	ctx := cmd.Context()
	f.applyConfigFallback(cfgSvc) // P2-配置3：未显式指定的 hub/token/node-id 取配置文件
	if err := f.applyConfig(); err != nil {
		return err
	}
	// 动态凭据容器（运行中自动轮换支持）。
	setP2PConnectCreds(cmd, f, cfgSvc)

	sig, reg, err := p2pConnectSignaler(ctx, cmd, f, cfgSvc, cp.p2pManualSignaling, ios)
	if err != nil {
		return err
	}
	if reg != nil {
		defer func() { _ = reg.Closer() }()
	}
	// --manual 需人工拷文件/粘贴 JSON，信令等待放宽到 10 分钟（默认 30s 必然不够）
	if cp.manual {
		webrtc.SetSignalingTimeout(p2p.ManualSignalingTimeout)
		// S69：命令结束恢复默认超时，防全局泄漏污染库内嵌场景与后续测试。
		defer webrtc.ResetSignalingTimeout()
	}
	// 手动模式单次连接：无论打洞成功/失败/panic，退出前都兜底清理本侧写出的 SDP 文件
	if ms, ok := sig.(*p2p.ManualSignaler); ok {
		defer ms.Cleanup()
	}
	conn, err := webrtc.DialWithSignaler(cp.peer, sig)
	if err != nil {
		return fmt.Errorf("p2p 打洞失败: %w", err)
	}
	defer conn.Close()
	ios.WriteOutLine("p2p 直连已建立: %s ⇄ %s（数据面不经过 hub）", f.localNode(), cp.peer)

	m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleDialer)
	defer m.Close()

	if cp.listenAddr != "" {
		return p2pForward(ctx, m, cp.peer, cp.tcpAddr, cp.listenAddr, ios)
	}
	// 单次模式：stdin/stdout 直通
	return p2pStdio(ctx, m, cp.tcpAddr, ios)
}

// setP2PConnectCreds 从根 flag 与配置文件构造动态凭据容器（f.creds）。
func setP2PConnectCreds(cmd *cobra.Command, f *p2pFlags, cfgSvc ConfigProvider) {
	ak0, _ := cmd.Root().PersistentFlags().GetString("access-key")
	sk0, _ := cmd.Root().PersistentFlags().GetString("access-key-secret")
	id0, _ := cmd.Root().PersistentFlags().GetString("access-key-id")
	ak0, sk0, id0, _ = p2pCredsFromConfig(cfgSvc, ak0, sk0, id0, "")
	f.creds = credrotate.NewCredentials(ak0, sk0, id0)
}

// p2pConnectSignaler 选择 p2p connect 的信令器：--manual 用文件或 stdin/stdout
// 交换（不依赖 hub）；否则经 hub 信令桥自动注册（B17，临时 node_id）。
func p2pConnectSignaler(ctx context.Context, cmd *cobra.Command, f *p2pFlags, cfgSvc ConfigProvider, ms p2pManualSignaling, ios cli.IOStreams) (webrtc.Signaler, *mesh.TempRegistration, error) {
	if ms.manual {
		sig, merr := p2pManualSignaler(ms.offerFile, ms.answerFile, ios)
		if merr != nil {
			return nil, nil, merr
		}
		return sig, nil, nil
	}
	// B17：经 hub 信令前自动注册自身（声明 per-node-secret 能力），从
	// REG_OK:<secret> 拿 per-node secret 供 B3 服务端信令身份校验。
	// 用临时 node_id（p2p-<base>-<nano>），对端无需预知本端 ID。
	if err := f.requireHub(); err != nil {
		return nil, nil, err
	}
	reg, rerr := f.registerSignaler(ctx, cmd, cfgSvc, false)
	if rerr != nil {
		return nil, nil, fmt.Errorf("webrtc 信令注册失败: %w", rerr)
	}
	return reg.Signaler, reg, nil
}
