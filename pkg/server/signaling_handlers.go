// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// SignalBroker 封装 hub 端的信令队列，挂在 Handlers 上。
// rt 用于校验信令消息的 from/to 必须是已注册节点（共享 relay_token
// 信任域内的身份绑定：阻止向不存在/未注册节点投递或轮询）。
type SignalBroker struct {
	queue *hub.SignalQueue
	rt    *hub.RouteTable
}

// NewSignalBroker 创建信令 broker（信令队列）。
func NewSignalBroker(rt *hub.RouteTable) *SignalBroker {
	return &SignalBroker{queue: hub.NewSignalQueue(), rt: rt}
}

// maxSignalBodyBytes 是单条信令消息体的最大字节数（SDP 通常 < 8 KiB）。
const maxSignalBodyBytes = 8 << 10

// handleSignalPost 处理 POST /api/signal/{kind}：把一条信令消息投递到目标 peer 收件箱。
// kind ∈ offer|answer|candidate。
// 校验：from/to 均为当前已注册节点（防向幽灵节点投递）。
func (b *SignalBroker) handleSignalPost(w http.ResponseWriter, r *http.Request, kind hub.SignalKind) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSignalBodyBytes)
	var msg hub.SignalMsg
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "解析信令消息失败", http.StatusBadRequest)
		return
	}
	msg.Kind = kind
	msg.At = time.Now().UnixMilli()
	if msg.To == "" || msg.From == "" {
		http.Error(w, "缺少 to/from", http.StatusBadRequest)
		return
	}
	if b.rt == nil {
		http.Error(w, "hub 未启用", http.StatusNotFound)
		return
	}
	if !b.rt.Has(hub.NodeID(msg.From)) {
		http.Error(w, "from 节点未注册", http.StatusBadRequest)
		return
	}
	if !b.rt.Has(hub.NodeID(msg.To)) {
		http.Error(w, "to 节点未注册", http.StatusBadRequest)
		return
	}
	b.queue.Push(msg)
	w.WriteHeader(http.StatusAccepted)
}

// handleSignalPoll 处理 GET /api/signal/poll/{peer}：长轮询目标 peer 的收件箱。
// 返回 [{...}] 数组（可能为空）。每条消息被取走即从队列删除。
// 校验：peer 必须是已注册节点（防向幽灵节点无意义地长轮询占用连接）。
func (b *SignalBroker) handleSignalPoll(w http.ResponseWriter, r *http.Request) {
	peer := r.PathValue("peer")
	if peer == "" {
		http.Error(w, "缺少 peer", http.StatusBadRequest)
		return
	}
	if b.rt == nil || !b.rt.Has(hub.NodeID(peer)) {
		http.Error(w, "peer 节点未注册", http.StatusNotFound)
		return
	}

	// 长轮询：最多等 pollTimeout，期间一旦有新消息立即返回
	ctx := r.Context()
	deadline := time.Now().Add(hub.PollTimeout)
	for {
		if m := b.queue.Pop(peer); m != nil {
			writeSignalMessages(w, []hub.SignalMsg{*m})
			return
		}
		if time.Now().After(deadline) {
			writeSignalMessages(w, []hub.SignalMsg{})
			return
		}
		// 等待新消息信号，带上剩余时间
		remaining := time.Until(deadline)
		waitCtx, cancel := contextWithTimeout(ctx, remaining)
		err := b.queue.Wait(waitCtx, peer)
		cancel()
		if err != nil {
			// ctx 取消（客户端断开）或超时：返回空数组
			writeSignalMessages(w, []hub.SignalMsg{})
			return
		}
		// Wait 返回后循环 Pop
	}
}

// contextWithTimeout 为长轮询 wait 构造带超时的 context。
func contextWithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}

// writeSignalMessages 以 JSON 数组写回信令消息。
func writeSignalMessages(w http.ResponseWriter, msgs []hub.SignalMsg) {
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(msgs); err != nil {
		return
	}
}
