// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package vpn 提供 tun/tap 内核 VPN 的接口与路由核心（roadmap 11.1-⑤，P1 片）。
//
// 本期（P1）只做接口 + 平台探测 + Router 路由逻辑，**不做真设备功能**：
//   - TUNDevice 接口（Open/SetMTU/Addr）由平台实现经 build tag 隔离
//     （device_linux.go / device_windows.go），未支持平台不参与构建；
//   - Router 主循环：读 IP 包 → 解析目标 → vipTable.NodeByAddr → dial → 写包；
//     单包 dial 失败丢包不崩循环（与 meshForwardListen 的 per-conn 失败处理同构）；
//   - PlatformProbe 三态（supported/err）：无特权/设备不可用时明确报错，
//     不静默回落 SOCKS5（禁静默降级）。
//
// Windows wintun 真驱动加载（P2）、真设备 e2e（P3）见设计文档片划分。
package vpn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"sync/atomic"

	"github.com/cocomhub/sproxy/pkg/tunnel/mesh"
)

// DefaultMTU 是 VPN 隧道建议 MTU（防 IP 分片；设计数据流第 1 步）。
// 与隧道传输分块/密文开销对齐，避免虚拟网卡大包在隧道内被切碎。
const DefaultMTU = 1400

// TUNDevice 是内核虚拟网卡（tun/tap）的平台抽象。
//
// 实现按平台 build tag 隔离（device_linux.go / device_windows.go）；未支持平台
// 无实现（不参与构建，编译期即可发现误用）。Open 返回的句柄是 io.ReadWriteCloser：
// 读 = 应用发往虚拟子网的 IP 包，写 = 把隧道回包写入虚拟网卡。
type TUNDevice interface {
	// Open 打开（或创建）名为 name 的虚拟网卡并返回其文件句柄。
	// 无特权（EPERM）/驱动缺失时返回明确错误（提示 sudo/管理员、wintun 驱动）。
	Open(name string) (io.ReadWriteCloser, error)
	// SetMTU 设置虚拟网卡 MTU（建议 DefaultMTU）。
	SetMTU(mtu int) error
	// Addr 返回本虚拟网卡地址（虚拟 IP）。
	Addr() netip.Addr
}

// DialFunc 建立一条到目标虚拟 IP（<vip> 语义）的 mesh 数据面连接。
// 与 mesh 装配层的 dial 签名对齐（拨号帧写入由 dial 实现内部完成，Router 直接透传 IP 包）。
type DialFunc func(ctx context.Context, addr string) (io.ReadWriteCloser, error)

// Router 是 tun 读包 → 按 VIP 选路 → dial → 写包的主循环。
//
// P1 数据流（设计文档「数据流」第 3 步；对端回复链读 → 写 tun 为后续片）：
//   - 目标不在 vipTable → 丢弃该包（不猜测 node-id，防地址注入，R-5 fail-closed）；
//   - 单包 dial 失败 → 丢弃该包 + 计数日志（不崩循环，与 meshForwardListen 同构）；
//   - ctx 取消 / 设备读 EOF → 正常退出。
type Router struct {
	dev    io.ReadWriteCloser
	dial   DialFunc
	vip    *mesh.VipTable
	subnet netip.Prefix
	log    *slog.Logger
	// dropped 是累计丢包数（诊断/测试用）。
	dropped atomic.Uint64
}

// NewRouter 创建 Router。dev 为 tun 设备句柄（PlatformProbe + Open 成功后由装配层
// 注入），dial 为 mesh dial（可由测试注入桩）。log 为空时使用包级日志。
func NewRouter(dev io.ReadWriteCloser, dial DialFunc, vip *mesh.VipTable, subnet netip.Prefix) *Router {
	log := slog.Default()
	if dev == nil {
		log.Warn("vpn: NewRouter 收到 nil 设备，Serve 将立即返回")
		return &Router{dev: emptyConn{}, dial: dial, vip: vip, subnet: subnet, log: log}
	}
	return &Router{dev: dev, dial: dial, vip: vip, subnet: subnet, log: log}
}

// Serve 运行主循环直到 ctx 取消或设备读返回错误。返回 nil（正常退出）
// 或设备读错误。包级错误不返回（丢弃继续），故调用方无需处理 dial 错误。
func (r *Router) Serve(ctx context.Context) error {
	if r.dial == nil {
		return fmt.Errorf("vpn: router dial 未装配")
	}
	if r.vip == nil {
		return fmt.Errorf("vpn: router vipTable 未装配")
	}
	if r.dev == nil {
		return fmt.Errorf("vpn: router 设备未装配")
	}
	go func() {
		<-ctx.Done()
		if c, ok := r.dev.(io.Closer); ok {
			_ = c.Close()
		}
	}()
	buf := make([]byte, 65536)
	for {
		n, err := r.dev.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if n == 0 {
			continue
		}
		pkt := buf[:n]
		dst, ok := ipv4Dst(pkt)
		if !ok {
			// 非 IPv4 / 畸形包：丢弃（不路由非 IP 包）。
			continue
		}
		if !mesh.IsVirtualAddr(dst, r.subnet) {
			// 虚拟子网外目标：非本 VPN 职责（本地直连或系统路由处理），丢弃。
			continue
		}
		nodeID, ok := r.vip.NodeByAddr(dst)
		if !ok {
			// R-5 fail-closed：未知虚拟 IP 丢弃 + 可观测计数（不猜测 node-id）。
			r.dropped.Add(1)
			r.log.Warn("vpn: 目标虚拟 IP 不在 mesh 节点列表，丢弃包", "dst", dst)
			continue
		}
		conn, derr := r.dial(ctx, dst.String())
		if derr != nil {
			// 单包 dial 失败：丢包不崩循环（per-conn 失败处理同构）。
			r.dropped.Add(1)
			r.log.Warn("vpn: 拨号失败，丢弃 IP 包", "node", nodeID, "dst", dst, "error", derr)
			continue
		}
		// 隧道内透传 IP 包；写失败同样丢包继续（数据面单向失败不崩路由）。
		if _, werr := conn.Write(pkt); werr != nil {
			r.dropped.Add(1)
			r.log.Warn("vpn: 写隧道失败，丢弃 IP 包", "node", nodeID, "dst", dst, "error", werr)
		}
		_ = conn.Close()
	}
}

// Dropped 返回累计丢弃包数（诊断/测试用）。
func (r *Router) Dropped() uint64 { return r.dropped.Load() }

// emptyConn 是空设备兜底（NewRouter(nil) 时 Serve 立即读 EOF 结束，不 panic）。
type emptyConn struct{}

func (emptyConn) Read(p []byte) (int, error)  { return 0, io.EOF }
func (emptyConn) Write(p []byte) (int, error) { return len(p), nil }
func (emptyConn) Close() error                { return nil }

// ipv4Dst 从 IPv4 包解析目标地址。仅支持 IHL=5（无选项）的标准头；
// 畸形/非 IPv4 返回 ok=false（调用方丢弃，不路由）。
func ipv4Dst(pkt []byte) (netip.Addr, bool) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return netip.Addr{}, false
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl {
		return netip.Addr{}, false
	}
	return netip.AddrFrom4([4]byte{pkt[16], pkt[17], pkt[18], pkt[19]}), true
}
