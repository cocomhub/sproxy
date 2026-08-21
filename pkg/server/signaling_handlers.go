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

// signalNodeHeader 是客户端声明自身节点 ID 的请求头。
// 身份绑定：post 的 From 与 poll 的 peer 必须等于调用方声明的 node-id，
// 防止共享 relay_token 下节点互相冒充/窃听对方收件箱。
const signalNodeHeader = "X-Node-ID"

// callerNode 读取并校验调用方声明的节点身份。
func (b *SignalBroker) callerNode(r *http.Request) (hub.NodeID, error) {
	if b.rt == nil {
		return "", errSignalHubDisabled
	}
	node := r.Header.Get(signalNodeHeader)
	if node == "" {
		return "", errSignalMissingNode
	}
	if !b.rt.Has(hub.NodeID(node)) {
		return "", errSignalNodeNotRegistered
	}
	return hub.NodeID(node), nil
}

// signalErrorCode 提取 signalError 的 HTTP 状态码（非 signalError 兜底 400）。
func signalErrorCode(err error) int {
	if se, ok := err.(*signalError); ok {
		return se.code
	}
	return http.StatusBadRequest
}

var (
	errSignalHubDisabled       = &signalError{msg: "hub 未启用", code: http.StatusNotFound}
	errSignalMissingNode       = &signalError{msg: "缺少 " + signalNodeHeader + " 请求头", code: http.StatusBadRequest}
	errSignalNodeNotRegistered = &signalError{msg: "节点未注册", code: http.StatusBadRequest}
	errSignalPeerMismatch      = &signalError{msg: "poll peer 与调用方节点身份不一致", code: http.StatusForbidden}
)

type signalError struct {
	msg  string
	code int
}

func (e *signalError) Error() string { return e.msg }

// handleSignalPost 处理 POST /api/signal/{kind}：把一条信令消息投递到目标 peer 收件箱。
// kind ∈ offer|answer|candidate。
// 身份绑定：msg.From 必须等于调用方声明的 node-id（X-Node-ID 头），且已注册。
func (b *SignalBroker) handleSignalPost(w http.ResponseWriter, r *http.Request, kind hub.SignalKind) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSignalBodyBytes)
	from, cerr := b.callerNode(r)
	if cerr != nil {
		http.Error(w, cerr.Error(), signalErrorCode(cerr))
		return
	}
	var msg hub.SignalMsg
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "解析信令消息失败", http.StatusBadRequest)
		return
	}
	// 身份绑定：From 完全由服务端从 X-Node-ID 派生，忽略 body 里的 From。
	// 防止攻击者在请求体里伪造他人身份（body 注入面）。
	msg.From = string(from)
	msg.Kind = kind
	msg.At = time.Now().UnixMilli()
	if msg.To == "" {
		http.Error(w, "缺少 to", http.StatusBadRequest)
		return
	}
	if msg.To == msg.From {
		http.Error(w, "不能给自己发信令", http.StatusBadRequest)
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
// 身份绑定：peer 必须等于调用方声明的 node-id（只能轮询自己的收件箱）。
func (b *SignalBroker) handleSignalPoll(w http.ResponseWriter, r *http.Request) {
	peer := r.PathValue("peer")
	if peer == "" {
		http.Error(w, "缺少 peer", http.StatusBadRequest)
		return
	}
	caller, cerr := b.callerNode(r)
	if cerr != nil {
		http.Error(w, cerr.Error(), signalErrorCode(cerr))
		return
	}
	if hub.NodeID(peer) != caller {
		http.Error(w, errSignalPeerMismatch.msg, errSignalPeerMismatch.code)
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
