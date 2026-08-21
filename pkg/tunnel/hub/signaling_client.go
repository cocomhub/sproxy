// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HubSignaler 是通过 hub 的 /api/signal/* 桥实现 SDP 交换的网络信令器。
// 其方法签名与 xfer/ext/webrtc 的 Signaler 接口一致，可被直接传给
// webrtc.DialWithSignaler / webrtc.ListenWithSignaler（无需 import webrtc）。
//
// 跨机器场景：两端的 SDP 都经 hub 队列存转；即使 Mac→hub 链路抖动，
// 信令体量 K 级、可重试，也能最终完成。
type HubSignaler struct {
	// baseURL 是 hub 的地址，如 https://sg-vps-1:18083。
	baseURL string
	// authToken 是可选的 Bearer token。
	authToken string
	// nodeID 是本节点已注册的节点 ID（信令 from；服务端校验其已注册）。
	nodeID string
	// httpClient 用于调用 hub API。
	httpClient *http.Client
}

// NewHubSignaler 创建经 hub 信令桥的 Signaler。
// nodeID 是本节点在 hub 上注册的节点 ID（信令来源，服务端校验已注册）。
func NewHubSignaler(baseURL, authToken, nodeID string) *HubSignaler {
	return &HubSignaler{
		baseURL:    strings.TrimRight(baseURL, "/"),
		authToken:  authToken,
		nodeID:     nodeID,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// post 向 hub 发送一条信令消息。
func (s *HubSignaler) post(ctx context.Context, kind SignalKind, to, sdp, cand string) error {
	msg := SignalMsg{Kind: kind, From: s.nodeID, To: to, SDP: sdp, Cand: cand, At: time.Now().UnixMilli()}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/api/signal/"+string(kind), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.authToken)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("信令 post %s 失败: %w", kind, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("信令 post %s: HTTP %d: %s", kind, resp.StatusCode, b)
	}
	return nil
}

// poll 长轮询 peer 收件箱，返回消息数组。
func (s *HubSignaler) poll(ctx context.Context, peer string) ([]SignalMsg, error) {
	u := s.baseURL + "/api/signal/poll/" + url.PathEscape(peer)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if s.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.authToken)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("信令 poll 失败: %w", err)
	}
	defer resp.Body.Close()
	var msgs []SignalMsg
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		return nil, fmt.Errorf("信令 poll 解析失败: %w", err)
	}
	return msgs, nil
}

// SendOffer 发送 Offer SDP。
func (s *HubSignaler) SendOffer(peer string, sdp string) error {
	return s.post(context.Background(), SignalOffer, peer, sdp, "")
}

// WaitOffer 阻塞等待 Offer SDP。
// 仅接受来自指定 peer（From == peer）的消息，拒绝伪装来源的信令，
// 防止攻击者冒充对端注入伪造的 Offer。
func (s *HubSignaler) WaitOffer(ctx context.Context, peer string) (string, error) {
	for {
		msgs, err := s.poll(ctx, peer)
		if err != nil {
			return "", err
		}
		for _, m := range msgs {
			if m.From == peer && m.Kind == SignalOffer && m.SDP != "" {
				return m.SDP, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// SendAnswer 发送 Answer SDP。
func (s *HubSignaler) SendAnswer(peer string, sdp string) error {
	return s.post(context.Background(), SignalAnswer, peer, sdp, "")
}

// WaitAnswer 阻塞等待 Answer SDP。
// 仅接受来自指定 peer（From == peer）的消息，拒绝伪装来源的信令。
func (s *HubSignaler) WaitAnswer(ctx context.Context, peer string) (string, error) {
	for {
		msgs, err := s.poll(ctx, peer)
		if err != nil {
			return "", err
		}
		for _, m := range msgs {
			if m.From == peer && m.Kind == SignalAnswer && m.SDP != "" {
				return m.SDP, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
