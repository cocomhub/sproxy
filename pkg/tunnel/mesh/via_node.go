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
type viaNodeProvider struct{}

func (viaNodeProvider) Name() string  { return "via-node" }
func (viaNodeProvider) Priority() int { return 80 } // direct(100) > via-node(80) > relay(50)

// Expand 展开为每个候选中间节点 X 的双候选（ListHubNodes ∩ outbound-dial）。
func (p viaNodeProvider) Expand(ctx context.Context, svc *client.FileClient, target *client.MeshService) []Candidate {
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
		xID := n.ID
		// via-relay:<X>：数据面经 hub 中继（现有路径）。
		out = append(out, Candidate{
			ID:       "via-relay:" + xID,
			Priority: p.Priority(),
			Dial: func(ctx context.Context, svc *client.FileClient, _ webrtc.Signaler,
				target *client.MeshService, _ string, _ DialOptions) (*Result, error) {
				start := time.Now()
				conn, err := svc.RelayStream(ctx, xID, target.Addr)
				if err != nil {
					return nil, fmt.Errorf("via-relay(%s): %w", xID, err)
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
// **Latency 语义（T2.1/T2.2，整体链路就绪）**：本函数在 mux 流首部写带
// AwaitResult=true 的 dial 帧，X 侧（leaf.go dOK 分支）出口拨号成功后回写
// [4B len][{"dial_result":"ok"}] 结果帧（I27）——L 读到该帧才返回，Latency 含
// X→T 出口段。**新 X 回帧但出口拨号 > 2s** 时（首帧超时）：Abort 控制流、重开数据流
// 写普通 dial 帧，并**再次读结果帧**（新 X 对普通 dial 帧同样回帧——leaf.go dOK
// 分支无条件回帧）——读到 ok 才当数据面，防结果帧污染首字节。**旧 X / mDNS 直连
// （任何 dial 帧都不回帧）**：两次读帧都超时，此时才确认无前缀，数据流直接当数据面
// （兼容路径）。兼容路径 Latency 含至多 2×viaDirectEgressTimeout 等待（虚高，设计
// 取舍：无法区分「新 X 慢出口」与「旧 X 不回帧」，以数据面首字节不被污染优先）。
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
		// 首帧超时（新 X 慢出口 / 旧 X / mDNS 不回帧）：Abort 控制流（解除 reader
		// goroutine 阻塞），重开数据流写普通 dial 帧，并**再试一次读结果帧**——
		// 新 X（leaf.go 无条件回帧）会回 [4B len][DialResultFrame JSON] 前缀，必须消费
		// 掉再当数据面（否则首字节被污染）；旧 X / mDNS（任何帧都不回）再次超时，
		// 此时才确认无前缀（真正旧 X 兼容路径）。
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
		// 再试一次读结果帧（消费新 X 对普通 dial 帧的回帧前缀）。
		resCh2 := make(chan *hub.DialResultFrame, 1)
		errCh2 := make(chan error, 1)
		go readDialResultFrameAsync(ds, resCh2, errCh2)
		select {
		case fr2 := <-resCh2:
			if fr2.DialResult != hub.DialResultOK {
				_ = m.Close()
				return nil, fmt.Errorf("via-direct(%s): X 出口拨号失败: %s", xID, fr2.Message)
			}
			// 新 X 慢出口：结果帧已消费，数据流即数据面（出口已就绪）。
			return &Result{Conn: &MuxStreamConn{Stream: ds, Mux: m}, Kind: KindViaDirect, Latency: time.Since(start)}, nil
		case err2 := <-errCh2:
			_ = m.Close()
			return nil, fmt.Errorf("via-direct(%s): 读出口结果帧失败: %w", xID, err2)
		case <-time.After(viaDirectEgressTimeout):
			// 再次超时 = 真旧 X / mDNS（任何 dial 帧都不回帧）：数据流无前缀，
			// 直接当数据面（兼容路径）。Latency 含至多 2×超时等待（虚高，设计取舍）。
			_ = ds.Abort() // 解除 reader goroutine 阻塞（防其后续窃取数据面字节）
			ds2, d2err := m.Open(ctx)
			if d2err != nil {
				_ = m.Close()
				return nil, fmt.Errorf("via-direct(%s): 重开数据流失败: %w", xID, d2err)
			}
			if err := WriteDialFrame(ds2, target.Addr); err != nil {
				_ = m.Close()
				return nil, fmt.Errorf("via-direct(%s): 写数据流 dial 帧失败: %w", xID, err)
			}
			slog.Debug("via-direct 两次读帧均超时，按旧 X 兼容处理（无结果帧前缀）", "x", xID)
			return &Result{Conn: &MuxStreamConn{Stream: ds2, Mux: m}, Kind: KindViaDirect, Latency: time.Since(start)}, nil
		case <-ctx.Done():
			_ = m.Close()
			return nil, ctx.Err()
		}
	case <-ctx.Done():
		_ = m.Close()
		return nil, ctx.Err()
	}
}

// viaDirectEgressTimeout 是 via-direct 等待 X 出口结果帧的超时：新 X（支持
// AwaitResult）在出口拨号完成后立即回帧；超时视为旧 X 兼容路径。
const viaDirectEgressTimeout = 2 * time.Second

// writeAwaitDialFrame 写带 AwaitResult=true 的 dial 帧
// （[4B len][{"dial":addr,"await_result":true}]）。
// X 侧 leaf.go dOK 分支据此回写出口拨号结果帧（I27）。旧 X 忽略未知字段
// （AwaitResult 不在其解析范围）→ 不回帧，由调用方超时走兼容路径。
func writeAwaitDialFrame(w io.Writer, addr string) error {
	b, err := json.Marshal(hub.DialRequest{Dial: addr, AwaitResult: true})
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
