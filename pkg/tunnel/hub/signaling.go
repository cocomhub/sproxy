// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// SignalKind 标识信令消息类型。
//
// candidate 类型为 trickle ICE 的预留注入点：当前实现是 **non-trickle**（WebRTC
// ICE 候选全内联在 offer/answer 的 SDP 中，pion/webrtc 的 `OnICECandidate` 未被
// 桥接成逐帧消息），故 SignalCandidate 在生产无 sender/consumer。保留常量与
// endpoint 的原因：① 兼容存量对端——连 `/api/signal/candidate` 不报 404；
// ② 未来做 trickle ICE 增量（首连提速）时无需另一个协议面，直接复用本信令
// 通道（SignalQueue 的 PopKind/Peek/Confirm 已按 kind 通用支持）。
// 启用 trickle 的评估：服务端 ~0.5h（endpoint+push 复用），客户端 webrtc 集成
// 1–2 天（OnICECandidate/AddICECandidate 双向桥 + 与 SDP 内联并存 + 时序测试）。
type SignalKind string

const (
	SignalOffer     SignalKind = "offer"
	SignalAnswer    SignalKind = "answer"
	SignalCandidate SignalKind = "candidate" // trickle ICE 预留（见上注释）
)

// SignalMsg 是经 hub 存转的一条信令消息。
type SignalMsg struct {
	ID   string     `json:"id,omitempty"` // 消息去重 ID（服务端 Push 时赋，见 I10）
	Kind SignalKind `json:"kind"`
	From string     `json:"from"` // 发送方 peer
	To   string     `json:"to"`   // 目标 peer
	SDP  string     `json:"sdp,omitempty"`
	// Cand 是 trickle ICE 候选（预留注入点）：当前 non-trickle 实现恒空串、生产无
	// sender/consumer（ICE 候选全内联 SDP）。保留以兼容旧对端与未来 trickle 增量，
	// 见 SignalKind 注释。字段名沿用信号协议（json:"cand"），勿删。
	Cand string `json:"cand,omitempty"`
	At   int64  `json:"at"` // 毫秒时间戳（服务端设置；TTL 判定依据）
}

// SignalQueue 是 per-peer 的信令收件箱（有界队列）。
// hub 收到 offer/answer/candidate 后 push 到目标 peer 的队列；
// 目标 peer 通过 poll 长轮询取走。
//
// 资源边界：per-peer 队列有上限（maxSignalInbox），并设全局消息总量上限
// （maxSignalTotal）与 per-sender 未消费上限（maxSignalPerSender），
// 防止大量空转 peer 的收件箱无界累积内存、以及单 sender 灌流逐出目标消息（I9）。
type SignalQueue struct {
	mu      sync.Mutex
	inboxes map[string][]SignalMsg // peer -> 待消费消息
	// waiters 是 peer -> 广播唤醒通道：每个 peer 最多一个「开放」通道，
	// Push/Purge 用 close 唤醒**所有**阻塞在该通道上的 Wait（close-broadcast，
	// P1-8），随后删除 map 条目，下次 Wait 新建通道。相比旧的"单 waiter 通道 +
	// 唤醒即删"，并发 Wait 不再有第二个被搁浅到 ctx 截止（最长 25s）。
	waiters map[string]chan struct{}
	total   int // 当前积压消息总数
}

// NewSignalQueue 创建信令队列。
func NewSignalQueue() *SignalQueue {
	return &SignalQueue{
		inboxes: make(map[string][]SignalMsg),
		waiters: make(map[string]chan struct{}),
	}
}

var (
	// ErrSignalQueueFull 表示全局信令消息总量已达上限，新消息被拒绝（I9）。
	ErrSignalQueueFull = errors.New("信令队列已满")
	// ErrSignalPerSenderCap 表示同一发送方到目标收件箱的未消费消息已达上限（I9）。
	ErrSignalPerSenderCap = errors.New("信令发送方配额已满")
)

const (
	// maxSignalInbox 是单个 peer 收件箱的最大消息数。
	maxSignalInbox = 128
	// maxSignalTotal 是全局积压消息总数上限。
	maxSignalTotal = 1024
	// maxSignalWaiters 是 waiters 表的最大条目数（防空转 peer 无限注册 waiter）。
	maxSignalWaiters = 256
	// maxSignalPerSender 是单收件箱对同一 sender 的未消费消息上限：
	// 超过后拒绝该 sender 的新消息（防单 sender 灌流把目标消息逐出队列）。
	maxSignalPerSender = 32
	// signalMsgTTL 是单条信令消息的最长存活时间：超过后视为过期，
	// 由 Push/Pop/PopKind/Wait 惰性清除。取 2x webrtc 信令预算（60s），
	// 保证正常信令流程不被 TTL 误杀，同时防陈旧消息长期占队（I9）。
	signalMsgTTL = 2 * time.Minute
)

// newSignalID 生成消息去重 ID（crypto/rand 8 字节 hex；极端失败用纳秒时间戳兜底）。
func newSignalID() string {
	var b [8]byte
	if _, err := cryptorand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// signalMsgExpired 判断消息是否已超过 TTL 过期。
// At == 0（未设置时间戳）视为不过期——真实路径下服务端在 Push 前总会设置 At。
func signalMsgExpired(m SignalMsg, now time.Time) bool {
	if m.At == 0 {
		return false
	}
	return now.UnixMilli()-m.At > int64(signalMsgTTL/time.Millisecond)
}

// compactExpiredLocked 惰性清除 target 收件箱中已过期的消息，并扣减 total。
// 调用方必须持有 q.mu。返回清理后的收件箱切片。
func (q *SignalQueue) compactExpiredLocked(target string, now time.Time) []SignalMsg {
	inbox := q.inboxes[target]
	if len(inbox) == 0 {
		return inbox
	}
	kept := inbox[:0]
	for _, m := range inbox {
		if signalMsgExpired(m, now) {
			q.total--
		} else {
			kept = append(kept, m)
		}
	}
	if len(kept) == 0 {
		delete(q.inboxes, target)
		return nil
	}
	q.inboxes[target] = kept
	return kept
}

// Push 投递一条消息到目标 peer 的收件箱。
// 全局总量达上限返回 ErrSignalQueueFull；同一 sender 在目标收件箱的未消费
// 消息达上限返回 ErrSignalPerSenderCap（两者都不入队，供调用方感知并回 429）。
// Push 会为消息赋去重 ID（若为空）并确保 At 时间戳存在（TTL 判定依据）。
func (q *SignalQueue) Push(m SignalMsg) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := time.Now()
	// 先清理过期消息，避免全局总量被陈旧消息占满（I9）。
	q.compactExpiredLocked(m.To, now)
	if q.total >= maxSignalTotal {
		return ErrSignalQueueFull
	}
	if m.ID == "" {
		m.ID = newSignalID()
	}
	if m.At == 0 {
		m.At = now.UnixMilli()
	}

	inbox := q.inboxes[m.To]
	// per-sender cap：同一 From 的未消费消息已达上限则拒绝，防灌流逐出目标消息。
	cnt := 0
	for _, x := range inbox {
		if x.From == m.From {
			cnt++
		}
	}
	if cnt >= maxSignalPerSender {
		return ErrSignalPerSenderCap
	}
	if len(inbox) >= maxSignalInbox {
		inbox = inbox[1:] // 单队列满丢弃最旧
		q.total--         // 补回被丢弃的一条的计数
	}
	q.inboxes[m.To] = append(inbox, m)
	q.total++
	// P1-8：close-broadcast 唤醒该 target 全部等待者（并发 poll 不再只有一个 waiter
	// 被唤醒、其余搁浅）。每通道只 close 一次：此处 close 后立即 delete，Wait 只在
	// map 无条目时新建——两次 Push 之间无 Wait 时不重复 close。
	if w := q.waiters[m.To]; w != nil {
		close(w)
		delete(q.waiters, m.To)
	}
	return nil
}

// Pop 非阻塞取走 target 收件箱里的一条消息（任意 kind）；无则返回 nil。
func (q *SignalQueue) Pop(target string) *SignalMsg {
	q.mu.Lock()
	defer q.mu.Unlock()
	inbox := q.compactExpiredLocked(target, time.Now())
	if len(inbox) == 0 {
		return nil
	}
	m := inbox[0]
	q.inboxes[target] = inbox[1:]
	if len(inbox) == 1 {
		delete(q.inboxes, target) // 清空即删 map 项，防空 inbox 长期占内存
	}
	q.total--
	return &m
}

// PopKind 非阻塞取走 target 收件箱里第一条指定 kind 的消息；无匹配返回 nil。
// 不匹配 kind 的消息保留在队列，交给对应 kind 的消费者（I9）——
// 破坏性消费不再吞掉其他 kind 的信令。
func (q *SignalQueue) PopKind(target string, kind SignalKind) *SignalMsg {
	q.mu.Lock()
	defer q.mu.Unlock()
	inbox := q.compactExpiredLocked(target, time.Now())
	for i, m := range inbox {
		if m.Kind == kind {
			q.inboxes[target] = append(inbox[:i], inbox[i+1:]...)
			if len(q.inboxes[target]) == 0 {
				delete(q.inboxes, target)
			}
			q.total--
			return &m
		}
	}
	return nil
}

// Peek 非阻塞查看 target 收件箱里第一条指定 kind 的消息（不删除，返回副本）。
// 配合 Confirm 实现「Encode 成功后才消费」的可靠 poll（I5）。
func (q *SignalQueue) Peek(target string, kind SignalKind) *SignalMsg {
	q.mu.Lock()
	defer q.mu.Unlock()
	inbox := q.compactExpiredLocked(target, time.Now())
	for i := range inbox {
		if kind == "" || inbox[i].Kind == kind {
			m := inbox[i]
			return &m
		}
	}
	return nil
}

// Confirm 删除 target 收件箱中 ID 匹配的消息（Peek 后确认消费）。
// 返回是否成功删除；未找到（已被并发 poll 取走）返回 false。
func (q *SignalQueue) Confirm(target, id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	inbox := q.inboxes[target]
	for i := range inbox {
		if inbox[i].ID == id {
			q.inboxes[target] = append(inbox[:i], inbox[i+1:]...)
			if len(q.inboxes[target]) == 0 {
				delete(q.inboxes, target)
			}
			q.total--
			return true
		}
	}
	return false
}

// Purge 清空 peer 的收件箱与 waiter（节点下线时调用，I6）。
// 幂等：peer 不存在时是 no-op。close-broadcast 唤醒阻塞中的 Wait（P1-8），
// 使其不再搁浅至 ctx 截止（最长 25s）；唤醒后 Pop 为空，调用方自行收尾。
// 每通道只 close 一次：此处 close 后立即 delete，后续 Wait 新建通道。
func (q *SignalQueue) Purge(peerID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if inbox := q.inboxes[peerID]; len(inbox) > 0 {
		q.total -= len(inbox)
		delete(q.inboxes, peerID)
	}
	if w := q.waiters[peerID]; w != nil {
		close(w)
		delete(q.waiters, peerID)
	}
}

// Total 返回当前全局积压消息总数（含所有收件箱）。
// 供上层（server 联动测试 / 运维观察）确认节点下线 Purge 后配额已释放。
func (q *SignalQueue) Total() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.total
}

// Wait 阻塞等待 target 收件箱出现新消息（长轮询语义）。
// 返回后调用方应再 Pop 一次；有超时保护。
// 同一 target 的并发 Wait 共享同一个开放通道，Push/Purge close 后**全部**被唤醒
// （P1-8）；成功唤醒与 ctx 取消两条路径都会清理 waiter 条目，防止 waiters 表
// 无界增长（I6/S10）。
func (q *SignalQueue) Wait(ctx context.Context, target string) error {
	q.mu.Lock()
	// 若已有积压消息（含过期清理后仍非空），立即返回
	if len(q.compactExpiredLocked(target, time.Now())) > 0 {
		q.mu.Unlock()
		return nil
	}
	w := q.waiters[target]
	if w == nil {
		// waiters 表已达上限：不再为新的 target 建 waiter，直接按超时处理
		if len(q.waiters) >= maxSignalWaiters {
			q.mu.Unlock()
			<-ctx.Done()
			return ctx.Err()
		}
		w = make(chan struct{})
		q.waiters[target] = w
	}
	q.mu.Unlock()

	select {
	case <-w:
		// 成功唤醒（channel 已被 Push/Purge close）：清理该 target 的 waiter 条目。
		// 若已有新 waiter 覆盖（q.waiters[target] != w），不误删。
		q.mu.Lock()
		if q.waiters[target] == w {
			delete(q.waiters, target)
		}
		q.mu.Unlock()
		return nil
	case <-ctx.Done():
		// 清理该 target 的 waiter 条目（若无新 waiter 覆盖）。
		q.mu.Lock()
		if q.waiters[target] == w {
			delete(q.waiters, target)
		}
		q.mu.Unlock()
		return ctx.Err()
	}
}

// PollTimeout 是 poll 长轮询的单次最长等待。
//
// 依赖约束：服务端写入超时（server_timeouts.write，默认 30s）必须大于
// PollTimeout，否则长轮询会被服务端在到达 PollTimeout 前掐断（I32/S9）。
// 客户端单次 poll 的 HTTP 超时（signaling_client.go 的 httpClient.Timeout，
// 60s）必须大于 PollTimeout + 网络余量，避免客户端先于服务端超时。
const PollTimeout = 25 * time.Second
