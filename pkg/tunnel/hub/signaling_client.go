// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/sproxysig"
)

// HubSignaler 是通过 hub 的 /api/signal/* 桥实现 SDP 交换的网络信令器。
// 其方法签名与 xfer/ext/webrtc 的 Signaler 接口一致，可被直接传给
// webrtc.DialWithSignaler / webrtc.ListenWithSignaler（无需 import webrtc）。
//
// 跨机器场景：两端的 SDP 都经 hub 队列存转；即使 Mac→hub 链路抖动，
// 信令体量 K 级、可重试，也能最终完成。
type HubSignaler struct {
	// baseURL 是 hub 的地址，如 https://hub.example.com:18083。
	baseURL string
	// accessKey 是 SproxySig 签名认证的 AccessKey（公开标识）。
	accessKey string
	// accessKeySecret 是 SproxySig AccessKeySecret（本地密钥，仅计算签名，永不上线）。
	// 空则不签名（无认证开发环境）。
	accessKeySecret string
	// nodeID 是本节点已注册的节点 ID（信令 from；服务端校验其已注册）。
	nodeID string
	// secret 是本节点的 per-node secret（I1）：非空时 post/poll 携带
	// X-Node-Secret 头，供 B3 服务端校验身份（向后兼容：空则不带头）。
	secret string
	// ctx 是可注入的 base context（I7）：未注入时 Send*/post 回退
	// context.Background()，注入后受调用方取消控制。
	ctx context.Context
	// httpClient 用于调用 hub API。单次 poll 超时 60s（I11）> 服务端
	// PollTimeout(25s) + 网络余量，避免客户端先于服务端超时。
	httpClient *http.Client

	// seenMu 保护 seen/seenList（I10 去重，简单 FIFO 近似 LRU，上限 1024）。
	seenMu   sync.Mutex
	seen     map[string]struct{}
	seenList []string
}

// maxSeenSignalIDs 是客户端 seen-set 的最大条目数（I10，防无界增长）。
const maxSeenSignalIDs = 1024

// NewHubSignaler 创建经 hub 信令桥的 Signaler。
// accessKey 是本 mesh 的 SproxySig AccessKey（公开标识；配合 SetAccessKeySecret 签名）。
// nodeID 是本节点在 hub 上注册的节点 ID（信令来源，服务端校验已注册）。
// secret 为可选变参：传入 per-node secret 后 post/poll 携带 X-Node-Secret 头。
func NewHubSignaler(baseURL, accessKey, nodeID string, secret ...string) *HubSignaler {
	s := &HubSignaler{
		baseURL:    strings.TrimRight(baseURL, "/"),
		accessKey:  accessKey,
		nodeID:     nodeID,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
	if len(secret) > 0 {
		s.secret = secret[0]
	}
	return s
}

// SetAccessKeySecret 设置 SproxySig AccessKeySecret（本地密钥，仅计算签名）。
// 空则信令请求不签名（无认证开发环境）。
func (s *HubSignaler) SetAccessKeySecret(sk string) {
	s.accessKeySecret = sk
}

// sign 为信令请求打上 SproxySig 签名头（bodyHash 由调用方预计算）。
func (s *HubSignaler) sign(req *http.Request, bodyHash string) {
	if s.accessKeySecret == "" {
		return
	}
	now := time.Now()
	h := sproxysig.Header{
		Version:    sproxysig.Version,
		AK:         s.accessKey,
		TS:         now.UnixMilli(),
		Exp:        now.Add(sproxysig.DefaultExpiry).UnixMilli(),
		Nonce:      sproxysig.NewNonce(),
		BodySHA256: bodyHash,
	}
	req.Header.Set("Authorization", sproxysig.SignAndFormat(s.accessKeySecret, h, req.Method, req.URL.EscapedPath(), req.URL.RawQuery))
}

// SetContext 注入 base context（I7）。SendOffer/SendAnswer/post 在注入后
// 使用该 ctx 而非 context.Background()，受调用方（mesh/p2p 命令 ctx）取消控制。
// 不调用则保持 context.Background()（向后兼容）。
func (s *HubSignaler) SetContext(ctx context.Context) {
	s.ctx = ctx
}

// SetHTTPClient 注入自定义 http.Client（TLS 配置 / 超时）。nil 忽略（保留默认）。
// 对齐 SetContext 模式（I7）：不调用则保持默认 &http.Client{Timeout:60s}（向后兼容）。
// 供 sclient --insecure 场景注入跳过证书校验的 client（自签 wss hub 信令链路）。
func (s *HubSignaler) SetHTTPClient(hc *http.Client) {
	if hc != nil {
		s.httpClient = hc
	}
}

// baseCtx 返回注入的 base context；未注入时回退 context.Background()。
func (s *HubSignaler) baseCtx() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// signalHTTPError 携带信令请求的 HTTP 状态码（供错误分类/重试判断，I8）。
type signalHTTPError struct {
	code int
	body string
}

func (e *signalHTTPError) Error() string {
	body := strings.TrimSpace(e.body)
	if body != "" {
		return fmt.Sprintf("信令: HTTP %d: %s", e.code, body)
	}
	return fmt.Sprintf("信令: HTTP %d", e.code)
}

// isRetriableSignalError 判断信令错误是否可瞬时重试（I8）：
//   - HTTP 4xx（未注册 400 / 未授权 403 / 未启用 404）→ 直接返回，不重试
//     （配置/权限类错误，重试无意义，且与 I1 联动：secret 轮换后的 403 快速失败回落）；
//   - HTTP 5xx 与网络/解析错误 → 可重试（退避后重试）。
func isRetriableSignalError(err error) bool {
	if err == nil {
		return false
	}
	var herr *signalHTTPError
	if errors.As(err, &herr) {
		return herr.code >= 500
	}
	return true
}

// post 向 hub 发送一条信令消息。
func (s *HubSignaler) post(ctx context.Context, kind SignalKind, to, sdp, cand string) error {
	msg := SignalMsg{Kind: kind, From: s.nodeID, To: to, SDP: sdp, Cand: cand}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/api/signal/"+string(kind), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Node-ID", s.nodeID)
	if s.secret != "" {
		req.Header.Set("X-Node-Secret", s.secret)
	}
	s.sign(req, sproxysig.BodyHash(body))
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("信令 post %s 失败: %w", kind, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return &signalHTTPError{code: resp.StatusCode, body: string(b)}
	}
	return nil
}

// poll 长轮询 peer 收件箱，返回消息数组。
// kind 非空时附加 ?kind= 过滤，服务端只返回该 kind 的消息（I9）。
func (s *HubSignaler) poll(ctx context.Context, peer string, kind SignalKind) ([]SignalMsg, error) {
	u := s.baseURL + "/api/signal/poll/" + url.PathEscape(peer)
	if kind != "" {
		u += "?kind=" + url.QueryEscape(string(kind))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Node-ID", s.nodeID)
	if s.secret != "" {
		req.Header.Set("X-Node-Secret", s.secret)
	}
	s.sign(req, sproxysig.EmptyBodyHash())
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("信令 poll 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, &signalHTTPError{code: resp.StatusCode, body: string(b)}
	}
	var msgs []SignalMsg
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		return nil, fmt.Errorf("信令 poll 解析失败: %w", err)
	}
	return msgs, nil
}

// SendOffer 向对端 to 发送 Offer SDP（cand 为 trickle 预留，当前恒传空串）。
func (s *HubSignaler) SendOffer(to string, sdp string) error {
	return s.post(s.baseCtx(), SignalOffer, to, sdp, "")
}

// WaitOffer 阻塞等待发给本节点（s.nodeID）的 Offer SDP。
// 返回发送方节点 ID 与 SDP。任何已注册节点发来的 offer 都接受
// （listener 无法预知拨号方，身份由服务端已校验 From 为注册节点）。
func (s *HubSignaler) WaitOffer(ctx context.Context) (string, string, error) {
	return s.waitSignal(ctx, SignalOffer)
}

// SendAnswer 向对端 to 发送 Answer SDP（cand 为 trickle 预留，当前恒传空串）。
func (s *HubSignaler) SendAnswer(to string, sdp string) error {
	return s.post(s.baseCtx(), SignalAnswer, to, sdp, "")
}

// WaitAnswer 阻塞等待发给本节点（s.nodeID）的 Answer SDP。
// 返回发送方节点 ID 与 SDP；调用方可校验 from 是否为目标对端。
func (s *HubSignaler) WaitAnswer(ctx context.Context) (string, string, error) {
	return s.waitSignal(ctx, SignalAnswer)
}

// waitSignal 长轮询等待指定 kind 的信令消息。
// - 瞬时错误（5xx/网络）有限退避重试（I8）；HTTP 4xx 直接返回；
// - 已消费的消息 ID 进入 seen-set，跳过重投/超时兜底造成的重复（I10）。
func (s *HubSignaler) waitSignal(ctx context.Context, kind SignalKind) (string, string, error) {
	for {
		msgs, err := s.pollWithRetry(ctx, kind)
		if err != nil {
			return "", "", err
		}
		for _, m := range msgs {
			if m.To == s.nodeID && m.Kind == kind && m.SDP != "" {
				if s.seenMsg(m.ID) {
					continue // 重复消息（服务端重投/超时兜底），跳过
				}
				s.markSeen(m.ID)
				return m.From, m.SDP, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// pollWithRetry 对瞬时错误做有限退避重试（最多 3 次，200ms 起始）。
// 不可重试错误（4xx）直接返回；重试耗尽返回最后一次错误。
func (s *HubSignaler) pollWithRetry(ctx context.Context, kind SignalKind) ([]SignalMsg, error) {
	const maxAttempts = 3
	baseDelay := 200 * time.Millisecond
	var msgs []SignalMsg
	var err error
	for attempt := range maxAttempts {
		msgs, err = s.poll(ctx, s.nodeID, kind)
		if err == nil {
			return msgs, nil
		}
		if !isRetriableSignalError(err) {
			return nil, err
		}
		if attempt == maxAttempts-1 {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(baseDelay):
		}
	}
	return msgs, err
}

// markSeen 记录已消费的消息 ID（有界 FIFO，近似 LRU）。
func (s *HubSignaler) markSeen(id string) {
	if id == "" {
		return
	}
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	if s.seen == nil {
		s.seen = make(map[string]struct{})
	}
	if _, ok := s.seen[id]; ok {
		return
	}
	s.seen[id] = struct{}{}
	s.seenList = append(s.seenList, id)
	if len(s.seenList) > maxSeenSignalIDs {
		old := s.seenList[0]
		s.seenList = s.seenList[1:]
		delete(s.seen, old)
	}
}

// seenMsg 判断消息 ID 是否已在 seen-set 中。
func (s *HubSignaler) seenMsg(id string) bool {
	if id == "" {
		return false
	}
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	_, ok := s.seen[id]
	return ok
}
