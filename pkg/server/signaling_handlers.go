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
type SignalBroker struct {
	queue *hub.SignalQueue
}

// NewSignalBroker 创建信令 broker（信令队列）。
func NewSignalBroker() *SignalBroker {
	return &SignalBroker{queue: hub.NewSignalQueue()}
}

// handleSignalPost 处理 POST /api/signal/{kind}：把一条信令消息投递到目标 peer 收件箱。
// kind ∈ offer|answer|candidate。
func (b *SignalBroker) handleSignalPost(w http.ResponseWriter, r *http.Request, kind hub.SignalKind) {
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
	b.queue.Push(msg)
	w.WriteHeader(http.StatusAccepted)
}

// handleSignalPoll 处理 GET /api/signal/poll/{peer}：长轮询目标 peer 的收件箱。
// 返回 [{...}] 数组（可能为空）。每条消息被取走即从队列删除。
func (b *SignalBroker) handleSignalPoll(w http.ResponseWriter, r *http.Request) {
	peer := r.PathValue("peer")
	if peer == "" {
		http.Error(w, "缺少 peer", http.StatusBadRequest)
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
