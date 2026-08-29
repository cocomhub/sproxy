// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// SignalBroker 封装 hub 端的信令队列，挂在 Handlers 上。
// rt 用于校验信令消息的 from/to 必须是已注册节点（已准入节点信任域内的
// 身份绑定：阻止向不存在/未注册节点投递或轮询）。
type SignalBroker struct {
	queue  *hub.SignalQueue
	rt     *hub.MeshRouteTable
	logger *slog.Logger
	// persist 是 hub 状态持久化器（nil 表示未启用持久化）。提供 setter 注入
	// （启动时配置了 hub.persist_file 才设置），写入当前收件箱快照。
	// 仅用于触发信令变更持久化，不持有生命周期（进程退出前的最终 flush 由
	// cmd 层在 handlers.Close 前调用）。
	persist *hub.Persister
	// pollTimeout 是 poll 长轮询的单次最长等待（I32）。
	// 默认 hub.PollTimeout；测试可注入更小值避免空 poll 阻塞拖慢用例（I63）。
	pollTimeout time.Duration
}

// NewSignalBroker 创建信令 broker（信令队列）。
// rt 非 nil 时注册节点下线回调：节点从路由表真正移除（连接断开 RemoveIfOwned /
// 手动踢除 Remove）即清空其信令收件箱（I6）。仅当节点被真正移除（而非同名节点
// 重连替换）时触发——重连时 RemoveIfOwned 所有权不匹配返回 false，不误删在线
// 节点收件箱。
func NewSignalBroker(rt *hub.MeshRouteTable) *SignalBroker {
	b := &SignalBroker{
		queue:       hub.NewSignalQueue(),
		rt:          rt,
		logger:      slog.Default(),
		pollTimeout: hub.PollTimeout,
	}
	if rt != nil {
		rt.SetRemoveHook(b.PurgeNode)
	}
	return b
}

// PurgeNode 清空指定节点（下线）的信令收件箱与 waiter，释放 maxSignalTotal
// 全局配额（I6）。幂等：节点从未有消息时是 no-op。
func (b *SignalBroker) PurgeNode(id hub.NodeID) {
	b.queue.Purge(string(id))
}

// SetPersister 设置 hub 状态持久化器（配置了 hub.persist_file 时由 cmd 层注入）。
// 传 nil 清除（关闭持久化）。
func (b *SignalBroker) SetPersister(p *hub.Persister) {
	b.persist = p
}

// FlushSignal 持久化完整 hub 快照（节点注册 + 信令收件箱）。由 handleSignalPost/
// handleSignalPoll 在信令入队/消费后调用（服务端事件驱动，把最新收件箱同步写入文件）。
// 必须与节点持久化共用同一 Snapshot 结构——只写 messages 会把已落盘的节点注册
// 从文件里抹掉（重启后节点全丢），故这里合并路由表快照一起写。
// 无持久化配置（persist == nil）时是 no-op，返回 false。
func (b *SignalBroker) FlushSignal(persist *hub.Persister) bool {
	if persist == nil {
		persist = b.persist
	}
	if persist == nil {
		return false
	}
	return persist.FlushFn(func() *hub.Snapshot {
		snap := &hub.Snapshot{
			Nodes:    hub.SnapshotRouteTable(b.rt).Nodes,
			Messages: hub.SnapshotSignalQueue(b.queue),
		}
		if len(snap.Nodes) == 0 && len(snap.Messages) == 0 {
			return nil
		}
		return snap
	})
}

// maxSignalBodyBytes 是单条信令消息体的最大字节数（SDP 通常 < 8 KiB）。
const maxSignalBodyBytes = 8 << 10

// signalNodeHeader 是客户端声明自身节点 ID 的请求头。
// 身份绑定：post 的 From 与 poll 的 peer 必须等于调用方声明的 node-id，
// 防止已准入节点互相冒充/窃听对方收件箱。
const signalNodeHeader = "X-Node-ID"

// signalNodeSecretHeader 是客户端声明自身 per-node secret 的请求头（I1）。
// 服务端用 B1 下发的 NodeInfo.Secret 恒定时间比对，防止已准入节点以他人
// node-id 窃听收件箱 / 投毒 SDP（零成本静默冒充被关闭）。
const signalNodeSecretHeader = "X-Node-Secret"

// signalPollBackoff 是 kind 过滤下队列积压其他 kind 消息时的轮询退避，
// 避免 Wait 立即返回（有积压即唤醒）导致的空转忙等（I5/I9）。
const signalPollBackoff = 100 * time.Millisecond

// callerNode 读取并校验调用方声明的节点身份。
// I1：除「已注册」外，还要求 X-Node-Secret 与 B1 下发的 per-node secret
// 恒定时间匹配；节点未声明 secret（Secret==""）显式短路 403（fail-closed），
// 防止空 header 对空 secret 通过比对。
// M-9：节点必须与请求 ctx 的 mesh 一致（防跨 mesh 信令身份冒用）。
func (b *SignalBroker) callerNode(r *http.Request) (hub.NodeID, error) {
	if b.rt == nil {
		return "", errSignalHubDisabled
	}
	node := r.Header.Get(signalNodeHeader)
	if node == "" {
		return "", errSignalMissingNode
	}
	info, ok := b.rt.LookupInfo(hub.NodeID(node))
	if !ok {
		return "", errSignalNodeNotRegistered
	}
	if info.Mesh != meshFromRequest(r) {
		return "", errSignalMeshMismatch
	}
	if info.Secret == "" {
		// 节点未声明 per-node-secret 能力：无 secret 可校验，fail-closed 拒绝。
		// 必须显式短路，否则空 header 对空 secret 会通过恒定时间比对。
		return "", errSignalSecretMismatch
	}
	secret := r.Header.Get(signalNodeSecretHeader)
	if subtle.ConstantTimeCompare([]byte(secret), []byte(info.Secret)) != 1 {
		return "", errSignalSecretMismatch
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
	errSignalHubDisabled       = &signalError{msg: errMsgHubNotEnabled, code: http.StatusNotFound}
	errSignalMissingNode       = &signalError{msg: "缺少 " + signalNodeHeader + " 请求头", code: http.StatusBadRequest}
	errSignalNodeNotRegistered = &signalError{msg: "节点未注册", code: http.StatusBadRequest}
	errSignalPeerMismatch      = &signalError{msg: "poll peer 与调用方节点身份不一致", code: http.StatusForbidden}
	errSignalSecretMismatch    = &signalError{msg: "节点信令 secret 不匹配", code: http.StatusForbidden}
	errSignalMeshMismatch      = &signalError{msg: "信令跨 mesh 拒绝", code: http.StatusForbidden}
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
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			// S41：body 超过 maxSignalBodyBytes → 413（与普通解析错误区分）
			http.Error(w, "信令消息体过大", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "解析信令消息失败", http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
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
	// M-9 信令跨 mesh 校验（纵深）：from 与 to 必须同 mesh。callerNode 已保证 from
	// 与请求 ctx mesh 一致，此处再确认 to 也在同一 mesh（防信息面交叉投递）。
	fromInfo, _ := b.rt.LookupInfo(from)
	toInfo, _ := b.rt.LookupInfo(hub.NodeID(msg.To))
	if fromInfo.Mesh != toInfo.Mesh {
		http.Error(w, errSignalMeshMismatch.msg, errSignalMeshMismatch.code)
		return
	}
	// I12：Push 返回 error（全局满 ErrSignalQueueFull / per-sender 配额
	// ErrSignalPerSenderCap），本次 POST 的消息被拒绝 → 429，供发送方感知并
	// 回落（客户端 post 非 2xx 即报错）。收件箱驱逐旧消息仍返回 nil（策略）。
	if err := b.queue.Push(msg); err != nil {
		b.logger.Warn("信令队列拒绝投递", "from", msg.From, "to", msg.To, "kind", msg.Kind, "error", err)
		http.Error(w, "信令队列已满，稍后重试", http.StatusTooManyRequests)
		return
	}
	// 持久化：同步写入当前收件箱（持久化启用时）。与节点注册/移除的 onChange
	// 回调共享同一 Persister——序列化在同一 mutex 下，不会互相覆盖。
	p := b.persist
	if p != nil {
		b.FlushSignal(p)
	}
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

	// I9/I5：kind 过滤（?kind=offer|answer|candidate）。非空时只取该 kind 的消息，
	// 其余 kind 的消息保留在队列，交给对应 kind 的消费者（破坏性消费不再吞掉
	// 其他 kind 的信令）。空串匹配任意 kind。
	kind := hub.SignalKind(r.URL.Query().Get("kind"))

	// I32：覆盖写 deadline 为 pollTimeout + 2s 余量，解耦 server_timeouts.write
	// 对长轮询的掐断（per-handler 覆盖已实测有效，不依赖全局 WriteTimeout）。
	// httptest.ResponseRecorder 不支持 SetWriteDeadline → ErrNotSupported，忽略。
	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.SetWriteDeadline(time.Now().Add(b.pollTimeout + 2*time.Second))
	}

	// 长轮询：最多等 b.pollTimeout，期间一旦有新消息立即返回。
	// 单条返回为长轮询快响应设计：客户端 Wait* 以 500ms 周期重 poll，最终收敛。
	ctx := r.Context()
	deadline := time.Now().Add(b.pollTimeout)
	for {
		// I5：先 Peek（非破坏），Encode 成功后才 Confirm 消费，避免 Encode 失败丢消息。
		if m := b.queue.Peek(peer, kind); m != nil {
			if !b.writeSignalMessages(w, []hub.SignalMsg{*m}) {
				return
			}
			b.queue.Confirm(peer, m.ID)
			// 信令被取走即从持久化镜像中消失：同步刷新快照，避免重启后
			// 重新投递已被消费的旧消息（收件箱快照与实际队列保持一致）。
			if p := b.persist; p != nil {
				b.FlushSignal(p)
			}
			return
		}
		if time.Now().After(deadline) {
			b.writeSignalMessages(w, []hub.SignalMsg{})
			return
		}
		// 等待新消息信号，带上剩余时间
		remaining := time.Until(deadline)
		waitCtx, cancel := context.WithTimeout(ctx, remaining)
		err := b.queue.Wait(waitCtx, peer)
		cancel()
		if err != nil {
			// ctx 取消（客户端断开）或超时：返回空数组
			b.writeSignalMessages(w, []hub.SignalMsg{})
			return
		}
		// kind 过滤下 Wait 可能因队列积压其他 kind 的消息立即返回（有积压即唤醒），
		// Peek 目标 kind 仍无匹配：短促退避后再试，避免空转忙等。
		if b.queue.Peek(peer, kind) == nil && b.queue.Peek(peer, "") != nil {
			select {
			case <-ctx.Done():
				b.writeSignalMessages(w, []hub.SignalMsg{})
				return
			case <-time.After(signalPollBackoff):
			}
		}
	}
}

// writeSignalMessages 以 JSON 数组写回信令消息。
// 返回是否写入成功（false = 客户端断开/写失败，调用方不应消费该消息，I5）。
func (b *SignalBroker) writeSignalMessages(w http.ResponseWriter, msgs []hub.SignalMsg) bool {
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(msgs); err != nil {
		b.logger.Warn("写回信令消息失败", "count", len(msgs), "error", err)
		return false
	}
	return true
}
