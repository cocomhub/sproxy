// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/iostream"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
)

// maxViaNodes 是 via-node 展开的中间节点候选上限（受全局 MaxCandidates 约束）。
const maxViaNodes = 3

// viaNodeProvider 实现 P4 多跳：经中间节点 X 中转（X 出站拨号到目标 T）。
// 每个 X 展开**双候选**（平级竞速，端到端 RTT 择优）：
//   - via-relay:<X>  —— 数据面经 hub 中继（RelayStream(X, T)）
//   - via-direct:<X> —— 数据面 webrtc 直连 X（DialWebRTC(HubSignaler(X))，X 出口拨 T）
//
// trustedNodes 是中间节点白名单（--trust-x，T5 信任收敛）：非空时仅白名单内的 X
// 参与竞速（白名单外节点即使声明 outbound-dial 也不选——X 是经手中转、可观测流量的
// 节点，白名单 = 显式信任声明）；空 = 全部可信（兼容现状）。
type viaNodeProvider struct {
	trustedNodes []string
}

func (viaNodeProvider) Name() string  { return "via-node" }
func (viaNodeProvider) Priority() int { return 80 } // direct(100) > via-node(80) > relay(50)

// SetTrustedNodes 设置中间节点白名单（--trust-x；空 = 全部可信）。
// 由 DialSmartWithOptions 在收集候选前注入（SmartOptions.TrustedNodes），
// 调用方须持 smartRegistryMu（与注册表修改同锁，防并行竞速竞态）。
//
// **白名单变化使缓存失效（R1 P1）**：白名单收窄会改变候选集合（信任控制不能旁路缓存
// fail-open）——实际变化时递增 smartRegistryGen（缓存快照一致性闸门，与 Register/Delete
// 同机制）；幂等 Set（同值）**不递增**（否则每次竞速都 miss，via-node 存在时缓存机制报废）。
func (p *viaNodeProvider) SetTrustedNodes(nodes []string) {
	// 调用方持 smartRegistryMu（smart.go 收集候选处注入时已持有）；比较新旧值防幂等递增。
	if slices.Equal(p.trustedNodes, nodes) {
		return // 幂等：白名单未变，不递增 gen（缓存保持命中）
	}
	p.trustedNodes = append([]string(nil), nodes...)
	smartRegistryGen.Add(1) // 白名单变化 → 缓存快照可能过期，强制重新竞速
}

// Expand 展开为每个候选中间节点 X 的双候选（ListHubNodes ∩ outbound-dial ∩ 白名单）。
func (p *viaNodeProvider) Expand(ctx context.Context, svc *client.FileClient, target *client.MeshService) []Candidate {
	if svc == nil || target == nil {
		return nil
	}
	nodes, err := svc.ListHubNodes(ctx)
	if err != nil {
		return nil // 发现失败：无 via-node 候选（direct/relay 仍参与竞速）
	}
	out := make([]Candidate, 0, maxViaNodes*2)
	for _, n := range nodes {
		if len(out) >= maxViaNodes*2 {
			break
		}
		if n.ID == target.Node || !slices.Contains(n.Capabilities, hub.CapabilityOutboundDial) {
			continue
		}
		// 白名单过滤（信任收敛）：非空时仅白名单内 X 保留，白名单外跳过（fail-closed）。
		if len(p.trustedNodes) > 0 && !slices.Contains(p.trustedNodes, n.ID) {
			continue
		}
		xID := n.ID
		// via-relay:<X>：数据面经 hub 中继（现有路径）。
		out = append(out, Candidate{
			ID:       "via-relay:" + xID,
			Priority: p.Priority(),
			Dial: func(ctx context.Context, svc *client.FileClient, _ webrtc.Signaler,
				target *client.MeshService, _ string, opts DialOptions) (*Result, error) {
				start := time.Now()
				conn, err := svc.RelayStream(ctx, xID, target.Addr)
				if err != nil {
					return nil, fmt.Errorf("via-relay(%s): %w", xID, err)
				}
				// 端到端加密（显式 E2E 配置）：X 是**中间节点**——L 侧把裸数据面连接
				// 包 DialE2EStream 并写 Path="via-relay" 标记（X 侧据此识别自己是中转，
				// 走透传分支而非解密）。X 出口拨 T 后把 Path 置空的 e2e 帧透传给 T，
				// T 侧 ServeE2EStream 解密——L⇄T 端到端加密，X 全程不见明文（T1 红线：
				// X 持 SK 也读不到明文，与 SK 解耦）。
				if opts.E2E != nil {
					e2eConn, derr := DialE2EStream(ctx, conn, target.Addr, "via-relay", *opts.E2E)
					if derr != nil {
						_ = conn.Close()
						return nil, fmt.Errorf("via-relay(%s) E2E 拨号失败: %w", xID, derr)
					}
					return &Result{Conn: e2eConn, Kind: KindViaNode, EndToEnd: true, Latency: time.Since(start)}, nil
				}
				return &Result{Conn: conn, Kind: KindViaNode, Latency: time.Since(start)}, nil
			},
		})
		// via-direct:<X>：数据面 webrtc 直连 X（信令器打洞到 X，X 出口拨 T）。
		out = append(out, Candidate{
			ID:       "via-direct:" + xID,
			Priority: p.Priority(),
			Dial: func(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler,
				target *client.MeshService, localNode string, opts DialOptions) (*Result, error) {
				return viaDirectXDial(ctx, signaler, xID, target, opts)
			},
		})
	}
	return out
}

// KindViaDirect 表示经中间节点 X 的 webrtc 直连数据面（L→X 不经 hub 字节）。
const KindViaDirect = "via-direct"

// viaDirectXDial 是 via-direct:X 候选的拨号函数：信令器打洞到 X（webrtc 直连），
// mux 流写 DialRequest(T) → X 的 relay.Serve 出口拨 T → 数据面 pump。
//
// 数据面路径：L ⇄(webrtc 打洞)⇄ X ⇄ T（不经 hub 字节；hub 只承载信令控制面）。
// 信令前提：signaler 须为 *hub.HubSignaler（可对任意已注册节点 X 打洞）——
// mDNS DirectSignaler 或 nil 无法寻址 X，返回错误（fail-closed，via-relay:X 仍参与竞速）。
//
// **Latency 语义（T2.1/T2.2，整体链路就绪）**：DialWebRTC 返回的连接在打洞完成即返回，
// **未含 X→T 出口拨号耗时**。本函数在 mux 流首部写带 AwaitResult=true 的 dial 帧，X 侧
// （leaf.go dOK 分支，方案 B 后：sOpts.DialResultFrames && d.AwaitResult 才回帧）出口
// 拨号成功后回写 [4B len][{"dial_result":"ok"}] 结果帧（I27）——L 读到该帧才返回，
// Latency 含出口段。真旧 X / mDNS 直连（不回帧）→ 超时后 Abort 该流并重开数据流
// （普通 dial 帧），Latency 含至多 1×viaDirectEgressTimeout 等待（虚高，设计取舍——
// 生产 X 条件回帧后此路径仅剩真旧 X/mDNS）。
func viaDirectXDial(ctx context.Context, signaler webrtc.Signaler, xID string,
	target *client.MeshService, opts DialOptions) (*Result, error) {
	start := time.Now()
	if !SignalerUsable(signaler) {
		return nil, fmt.Errorf("via-direct(%s): 无可用信令器（需 hub 信令桥打洞到 X）", xID)
	}
	// 1. 打洞到 X（受 WebRTCProbeTimeout 约束，与 DialWebRTC 一致）。
	probeCtx, probeCancel := context.WithTimeout(ctx, WebRTCProbeTimeout)
	conn, err := webrtc.DialWithSignalerOptsCtx(probeCtx, xID, signaler, opts.ICE)
	probeCancel()
	if err != nil {
		return nil, fmt.Errorf("via-direct(%s): 打洞失败: %w", xID, err)
	}
	m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleDialer)
	// 2. 控制流：写 AwaitResult dial 帧 → 读 X 出口结果帧（整体链路就绪判定）。
	ctrl, err := m.Open(ctx)
	if err != nil {
		_ = m.Close()
		return nil, fmt.Errorf("via-direct(%s): 打开控制流失败: %w", xID, err)
	}
	if err := writeAwaitDialFrame(ctrl, target.Addr); err != nil {
		_ = m.Close()
		return nil, fmt.Errorf("via-direct(%s): 写 AwaitResult dial 帧失败: %w", xID, err)
	}
	// 读结果帧（有界：ctx 或超时）。读到 ok → 控制流即数据面（X 出口已就绪）。
	resCh := make(chan *hub.DialResultFrame, 1)
	errCh := make(chan error, 1)
	go readDialResultFrameAsync(ctrl, resCh, errCh)
	select {
	case fr := <-resCh:
		if fr.DialResult != hub.DialResultOK {
			_ = m.Close()
			return nil, fmt.Errorf("via-direct(%s): X 出口拨号失败: %s", xID, fr.Message)
		}
		// X 出口已就绪：控制流即数据面（无需重开）。Latency 含出口段。
		return &Result{Conn: &MuxStreamConn{Stream: ctrl, Mux: m}, Kind: KindViaDirect, Latency: time.Since(start)}, nil
	case err := <-errCh:
		_ = m.Close()
		return nil, fmt.Errorf("via-direct(%s): 读出口结果帧失败: %w", xID, err)
	case <-time.After(viaDirectEgressTimeout):
		// 真旧 X / mDNS 直连（不回帧）：Abort 控制流（解除 reader goroutine 阻塞，防
		// 其后续窃取数据面字节），重开数据流写普通 dial 帧（无 AwaitResult）——
		// 兼容路径。生产 X（方案 B 条件回帧）此刻已回帧，此路径仅剩真旧 X/mDNS。
		// Latency 含至多 1×viaDirectEgressTimeout 等待（虚高，设计取舍）。
		_ = ctrl.Abort()
		ds, derr := m.Open(ctx)
		if derr != nil {
			_ = m.Close()
			return nil, fmt.Errorf("via-direct(%s): 重开数据流失败: %w", xID, derr)
		}
		if err := WriteDialFrame(ds, target.Addr); err != nil {
			_ = m.Close()
			return nil, fmt.Errorf("via-direct(%s): 写数据流 dial 帧失败: %w", xID, err)
		}
		slog.Debug("via-direct 未收到出口结果帧，按打洞完成处理（真旧 X/mDNS 兼容）", "x", xID, "timeout", viaDirectEgressTimeout)
		return &Result{Conn: &MuxStreamConn{Stream: ds, Mux: m}, Kind: KindViaDirect, Latency: time.Since(start)}, nil
	case <-ctx.Done():
		_ = m.Close()
		return nil, ctx.Err()
	}
}

// viaDirectEgressTimeout 是 via-direct 等待 X 出口结果帧的超时：新 X（支持
// AwaitResult）在出口拨号完成后立即回帧；超时视为旧 X 兼容路径。
const viaDirectEgressTimeout = 2 * time.Second

// writeAwaitDialFrame 写带 AwaitResult=true 的 dial 帧
// （[4B len][{"dial":addr,"await_result":true,"path":"via-direct"}]）。
// X 侧 leaf.go dOK 分支据此回写出口拨号结果帧（I27）。旧 X 忽略未知字段
// （AwaitResult/Path 不在其解析范围）→ 不回帧，由调用方超时走兼容路径。
func writeAwaitDialFrame(w io.Writer, addr string) error {
	b, err := json.Marshal(hub.DialRequest{Dial: addr, AwaitResult: true, Path: "via-direct"})
	if err != nil {
		return err
	}
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(b)))
	for _, chunk := range [][]byte{lenBuf, b} {
		if err := iostream.WriteFull(w, chunk); err != nil {
			return err
		}
	}
	return nil
}

// readDialResultFrameAsync 异步读一条出口结果帧（[4B len][DialResultFrame JSON]，
// I27）。必须在 goroutine 中调用（mux.Stream 无 deadline），由调用方 select 超时/取消；
// 超时路径调用方须 Abort 该流解除本 goroutine 的 Read 阻塞（防窃取数据面字节）。
func readDialResultFrameAsync(s io.Reader, resCh chan<- *hub.DialResultFrame, errCh chan<- error) {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(s, lenBuf); err != nil {
		errCh <- err
		return
	}
	metaLen := binary.BigEndian.Uint32(lenBuf)
	if metaLen == 0 || metaLen > maxDialFrameBytes {
		errCh <- fmt.Errorf("非法结果帧长度 %d", metaLen)
		return
	}
	meta := make([]byte, metaLen)
	if _, err := io.ReadFull(s, meta); err != nil {
		errCh <- err
		return
	}
	var fr hub.DialResultFrame
	if err := json.Unmarshal(meta, &fr); err != nil {
		errCh <- err
		return
	}
	resCh <- &fr
}
