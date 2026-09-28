// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// mesh_up.go 是虚拟子网接入（roadmap 11.1-⑤ tun/tap VPN P1 片）：
//
//   - `sclient mesh up [-l :port] [--exit <node>]`：默认形态 = 用户态 SOCKS5
//     代理（复用 socks 命令的 mesh 路由；零回归，无 tun/tap 特权要求）；
//   - `sclient mesh up --tun --vip <addr>`：tun/tap 内核 VPN 形态（P1 装配）：
//     PlatformProbe → TUNDevice → Router 主循环。**P1 不做真设备功能**——
//     --tun 装配当前 fail-closed（探测不通过/未支持平台时明确报错，不静默
//     回落 SOCKS5；禁静默降级），真设备打开留 P2（见设计文档片划分）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/credrotate"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/meshconn"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/iostream"
	mesh "github.com/cocomhub/sproxy/pkg/tunnel/mesh"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/cocomhub/sproxy/pkg/vpn"
	"github.com/spf13/cobra"
)

// meshVPNSubnet 是 mesh up --tun 的虚拟子网默认值（与 mesh connect --virtual-subnet
// 同源；VIP 表构建用同一 CGNAT 段，对齐 hub.virtual_subnet 配置）。
const meshVPNSubnet = "100.64.0.0/10"

// newCmdMeshUp 构造 `mesh up` 子命令（虚拟子网接入）。
// 默认委托 socks 命令（用户态 SOCKS5，零回归）；--tun 时走内核 VPN 装配。
func newCmdMeshUp(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	socksCmd := newCmdSocks(factory, ios, cfgSvc)
	cmd := &cobra.Command{
		Use:   "up [-l :port] [--exit <node>] | up --tun --vip <addr>",
		Short: "启动虚拟子网接入（本地 SOCKS5 代理，或 --tun 内核 VPN）",
		Long: `启动虚拟子网接入。默认启动本地 SOCKS5 代理（curl --socks5-hostname /
系统代理指向本代理，虚拟子网流量经 mesh 路由到 --exit 出口节点出站——纯用户态，
无需内核特权）；--tun 时启动 tun/tap 内核 VPN 形态（整网段透明路由，需要
PlatformProbe 通过 + TUNDevice；P1 为接口 + 平台探测装配，真设备功能留 P2）。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			tunMode, _ := cmd.Flags().GetBool("tun")
			if !tunMode {
				// 默认形态：委托 socks 命令执行（flag 透传：把本命令 flag 复制到 socks）。
				socksCmd.Flags().AddFlagSet(cmd.Flags())
				socksCmd.SetArgs(args)
				return socksCmd.Execute()
			}
			vipStr, _ := cmd.Flags().GetString("vip")
			return runMeshUpTUN(cmd, factory, ios, cfgSvc, vipStr, vpn.PlatformProbe)
		},
	}
	// socks 同款 flag 族（listen + 出口族 + 认证）。
	cmd.Flags().AddFlagSet(socksCmd.Flags())
	// --tun 内核 VPN 形态 flag（默认关，零回归）。
	cmd.Flags().Bool("tun", false, "tun/tap 内核 VPN 形态（默认关：纯用户态 SOCKS5；P1 为接口 + 平台探测装配，真设备功能 P2）")
	cmd.Flags().String("vip", "", "本机虚拟 IP（--tun 时必填；须已由 hub 分配，如 100.64.0.5）")
	return cmd
}

// runMeshUpTUN 是 mesh up --tun 的装配（P1 边界）：
//  1. PlatformProbe：不支持/无特权 → fail-closed 明确报错（不静默回落 SOCKS5）；
//  2. --vip 解析 + vipTable 构建（hub 权威列表拉取；未分配/冲突 fail-closed）；
//  3. TUNDevice 装配 + Router 主循环（dev 由平台实现打开；P1 平台实现 Open
//     fail-closed，故 Router 以内存/空设备兜底装配，Serve 正常退出）。
//
// probe 为平台探测函数（生产传 vpn.PlatformProbe；测试注入桩——避免全局
// 可变状态，保证并行测试无竞态）。
//
// 真设备打开/MTU/地址装配留 P2（平台实现 Open 当前返回「P2 未实现」错误——
// 此处不强行吞掉：探测通过时给出明确说明，仍以 Router 装配收尾，保证 CLI
// 形态与后续 P2 无缝衔接）。
func runMeshUpTUN(cmd *cobra.Command, factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider, vipStr string, probe func() (bool, error)) error {
	// 1) 平台探测三态（设计「错误处理」：失败明确报错，不静默回落）。
	ok, perr := probe()
	if perr != nil {
		if errors.Is(perr, vpn.ErrPlatformNotSupported) {
			return fmt.Errorf("tun/tap 内核 VPN 暂不支持当前平台：%w", perr)
		}
		return fmt.Errorf("tun/tap 设备不可用：%w", perr)
	}
	if !ok {
		return fmt.Errorf("tun/tap 内核 VPN 不可用（平台探测未通过）")
	}

	// 2) --vip 解析（fail-closed：缺失/非法 → 报错）。
	vip, subnet, verr := meshUpValidateVIP(vipStr)
	if verr != nil {
		return verr
	}

	// 3) mesh 连接参数组装配（hub/node-id/webrtc/insecure/…）与配置回落。
	conn := &meshconn.Conn{}
	if err := conn.FromFlags(cmd, cfgSvc); err != nil {
		return err
	}
	svc, svcErr := factory.NewClient(cmd)
	if svcErr != nil {
		svc = nil
	}
	localNode := meshUpConfigFallback(conn, svc)

	// 4) vipTable：hub 权威节点列表构建（与 mesh connect 同源、同 fail-closed
	//    R-5 语义：VIP 未分配/不在列表 → 明确报错提示重试/确认 hub 已分配）。
	vt, verr2 := meshUpBuildVipTable(cmd.Context(), svc, vip, subnet)
	if verr2 != nil {
		return verr2
	}

	// 5) 信令器（webrtc 打洞；注册失败回落中继，对齐 socks 语义）。
	signaler, closeSig := meshUpSignaler(cmd.Context(), cmd, conn, svc, cfgSvc, ios)
	if closeSig != nil {
		defer func() { _ = closeSig() }()
	}

	// 6) dial 装配（复用 mesh connect 既有链路：mesh.Dial → --smart → --gateway
	//    → --e2e 包层顺序不变；虚拟 IP 目标经 vipTable 解析 node-id 后回落 base）。
	base := meshDialFunc(mesh.Dial)
	base = meshUpSmartDial(cmd, base)
	gatewayAddr := conn.GatewayAddr
	if gatewayAddr != "" && svc != nil {
		base = meshGatewayDial(gatewayAddr, svc.AccessKeySecret(), ios)
	}
	// 虚拟 IP 目标解析（meshVIPDial 最外层，与 mesh connect 装配顺序一致）。
	base = meshVIPDial(vt, subnet, base, ios)
	vpnDial := func(ctx context.Context, addr string) (io.ReadWriteCloser, error) {
		res, derr := base(ctx, svc, signaler, &client.MeshService{Name: "vpn", Addr: addr}, localNode)
		if derr != nil {
			return nil, derr
		}
		return res.Conn, nil
	}

	// 7) TUNDevice + Router 装配。P1：平台实现 Open fail-closed（不做真设备），以内存
	//    空设备装配 Router（Serve 正常退出），启动横幅说明 P1 边界；P2 换真设备句柄
	//    即可（装配签名不变）。
	rc, closeRC := meshUpOpenDevice(vip, ios)
	defer closeRC()

	router := vpn.NewRouter(rc, vpnDial, vt, subnet)
	ios.WriteOutLine("tun/tap VPN 就绪: vip=%s subnet=%s（P1 接口 + 平台探测装配，Ctrl+C 退出）", vip, subnet)
	// 运行中凭据自动轮换（统一 credrotate 工具）。
	renewInterval, _ := cmd.Flags().GetDuration("renew-interval")
	if stopRenew, ok := credrotate.Start(cmd.Context(), svc, credrotate.Options{
		Interval: renewInterval,
		Logger:   slog.Default(),
	}); ok {
		defer stopRenew()
	}
	return router.Serve(cmd.Context())
}

// meshUpValidateVIP 解析并校验 --vip：缺失/非法/非 IPv4/不在虚拟子网内 → fail-closed
// 报错。返回解析后的 vip 与默认虚拟子网（meshVPNSubnet，VIP 表构建用同一 CGNAT 段，
// 对齐 hub.virtual_subnet 配置）。
func meshUpValidateVIP(vipStr string) (netip.Addr, netip.Prefix, error) {
	if vipStr == "" {
		return netip.Addr{}, netip.Prefix{}, fmt.Errorf("--tun 需要 --vip <addr> 指定本机虚拟 IP（如 100.64.0.5；可由 hub 分配或 sclient mesh status 查看）")
	}
	vip, verr := netip.ParseAddr(vipStr)
	if verr != nil || !vip.Is4() {
		return netip.Addr{}, netip.Prefix{}, fmt.Errorf("--vip %q 非法（应为 IPv4 地址）", vipStr)
	}
	subnet, serr := netip.ParsePrefix(meshVPNSubnet)
	if serr != nil {
		return netip.Addr{}, netip.Prefix{}, fmt.Errorf("虚拟子网 %q 非法: %v", meshVPNSubnet, serr)
	}
	if !mesh.IsVirtualAddr(vip, subnet) {
		return netip.Addr{}, netip.Prefix{}, fmt.Errorf("--vip %s 不在虚拟子网 %s 内", vip, subnet)
	}
	return vip, subnet, nil
}

// meshUpConfigFallback 用 svc 补齐 hub/node-id 配置回落（对齐既有 T6b 模式）。
// svc 为 nil（--mdns 可无客户端）时跳过回落；node-id 为空回落主机名。
// 返回 localNode（vpnDial 目标解析用）。
func meshUpConfigFallback(conn *meshconn.Conn, svc *client.FileClient) string {
	if conn.HubURL == "" && svc != nil {
		conn.HubURL = svc.MeshHubURL()
	}
	if conn.NodeID == "" && svc != nil {
		conn.NodeID = svc.NodeID()
	}
	if conn.NodeID == "" {
		conn.NodeID = iostream.LocalHostname("mesh-node")
	}
	return conn.NodeID
}

// meshUpBuildVipTable 构建 VIP 表：hub 权威节点列表（与 mesh connect 同源、同
// fail-closed R-5 语义）。svc 为 nil（--mdns 无 hub 模式）时跳过列表拉取。本机 VIP
// 自身也应入表（目标为本地时由本地服务处理；入表防把发往自身 VIP 的包误路由到其他
// 节点——hub 未分配时 fail-closed 报错）。
func meshUpBuildVipTable(ctx context.Context, svc *client.FileClient, vip netip.Addr, subnet netip.Prefix) (*mesh.VipTable, error) {
	vt := mesh.NewVipTable(subnet.Masked())
	if svc != nil {
		nodes, lerr := svc.ListHubNodes(ctx)
		if lerr != nil {
			return nil, fmt.Errorf("拉取 hub 节点列表构建虚拟 IP 表失败: %w", lerr)
		}
		for _, n := range nodes {
			if n.VirtualIP == "" {
				continue
			}
			a, aerr := netip.ParseAddr(n.VirtualIP)
			if aerr != nil {
				continue
			}
			if !vt.Add(a, n.ID) {
				return nil, fmt.Errorf("hub 节点列表虚拟 IP %s 冲突（多个节点声明），无法构建虚拟 IP 表", a)
			}
		}
	}
	// 本机 VIP 自身也应入表（R-5 语义：目标为本地时由本地服务处理；入表防
	// 把发往自身 VIP 的包误路由到其他节点——hub 未分配时 fail-closed 报错）。
	if _, ok := vt.NodeByAddr(vip); !ok {
		if svc == nil {
			return nil, fmt.Errorf("虚拟 IP %s 未在 mesh 节点列表中找到（--mdns 无 hub 模式 P1 不支持；请确认 hub 已分配虚拟 IP）", vip)
		}
		// hub 列表已拉取且不含本机 VIP → fail-closed（R-5：提示重试/确认 hub 已分配）。
		return nil, fmt.Errorf("虚拟 IP %s 未在 mesh 节点列表中找到对应节点（请确认本节点已在线且 hub 已分配虚拟 IP）", vip)
	}
	return vt, nil
}

// meshUpSignaler 构建 webrtc 打洞信令器（经 hub 自动注册；注册失败回落 hub 中继，
// 对齐 socks 语义）。svc 为 nil（--mdns 无 hub 模式）时返回空信令器（不注册）。
func meshUpSignaler(ctx context.Context, cmd *cobra.Command, conn *meshconn.Conn, svc *client.FileClient, cfgSvc ConfigProvider, ios cli.IOStreams) (webrtc.Signaler, func() error) {
	if svc == nil {
		return nil, nil
	}
	caFile, _ := cmd.Flags().GetString("ca-file")
	if caFile == "" {
		if cfg, cerr := cfgSvc.LoadConfig(); cerr == nil {
			caFile = cfg.XferCAFile
		}
	}
	signaler, closeSig, sigErr := conn.Signalers(ctx, svc, caFile)
	if sigErr != nil {
		ios.WriteErrLine("webrtc 信令注册失败: %v（回落 hub 中继）", sigErr)
	}
	return signaler, closeSig
}

// meshUpSmartDial 当 --smart 开启时把 base 包装为并行竞速选路（webrtc/中继按端到端
// 建连耗时择优；--smart-ttl 覆盖默认缓存 TTL）。未开启时原样返回（默认固定顺序
// webrtc→relay，零回归）。
func meshUpSmartDial(cmd *cobra.Command, base meshDialFunc) meshDialFunc {
	smart, _ := cmd.Flags().GetBool("smart")
	if !smart {
		return base
	}
	smartTTL, _ := cmd.Flags().GetDuration("smart-ttl")
	fallback := meshDialFunc(mesh.Dial)
	return meshDialFunc(func(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler, target *client.MeshService, localNode string) (*mesh.Result, error) {
		so := mesh.SmartOptions{FallbackDial: fallback}
		if smartTTL > 0 {
			so.CacheTTL = smartTTL
		}
		return mesh.DialSmartWithOptions(ctx, svc, signaler, target, localNode, mesh.DialOptions{}, so)
	})
}

// meshUpOpenDevice 打开 TUNDevice。P1：平台实现 Open fail-closed（不做真设备），以
// 内存空设备兜底装配 Router（Serve 正常退出）；打开错误明示 P1 边界（不吞错、不
// 静默回落 SOCKS5）。返回设备 rc 与清理函数。
func meshUpOpenDevice(vip netip.Addr, ios cli.IOStreams) (io.ReadWriteCloser, func()) {
	dev := vpn.NewTUNDevice(vip)
	rc, openErr := dev.Open("sproxy0")
	if openErr != nil {
		// P1 边界明示（不吞错、不静默回落）：探测已通过 → 真设备打开留 P2。
		ios.WriteErrLine("tun/tap 真设备打开留 P2（当前为接口 + 平台探测装配）: %v", openErr)
	}
	if rc == nil {
		rc = vpn.NewMemoryTUN() // P1 内存设备兜底（Router 装配形态可验证）
	}
	if err := dev.SetMTU(vpn.DefaultMTU); err != nil && !errors.Is(err, vpn.ErrPlatformNotSupported) {
		// MTU 与隧道不符 → 启动日志 warn + 建议值（设计「错误处理」，不强制）。
		ios.WriteErrLine("tun/tap SetMTU 失败: %v（建议 MTU %d）", err, vpn.DefaultMTU)
	}
	return rc, func() { _ = rc.Close() }
}
