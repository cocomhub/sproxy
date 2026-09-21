// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/cocomhub/sproxy/pkg/files"
)

// fileEvent 是推送给订阅者的文件变更事件（SSE 数据负载）。
type fileEvent struct {
	Cursor uint64 `json:"cursor"`
	Action string `json:"action"`
	Owner  string `json:"owner"`
	Rel    string `json:"rel"`
	Size   int64  `json:"size,omitempty"`
}

// eventRing 是每 owner 的事件环形缓冲 + 订阅者集合。
// 游标单调递增（Last-Event-ID 重连回放用）；缓冲满时最旧事件被覆盖。
type eventRing struct {
	mu     sync.Mutex
	cursor uint64
	buf    []fileEvent // 环形缓冲（容量 eventRingCap）
	subs   map[chan fileEvent]struct{}
}

// eventRingCap 是每 owner 事件缓冲容量（足够覆盖重连窗口）。
const eventRingCap = 1000

// EventBus 是文件变更事件总线：OnFileEvent 从领域层接收，SSE handler 订阅。
// 每 owner 独立环形缓冲（租户隔离）+ 单调游标。
type EventBus struct {
	mu    sync.Mutex
	rings map[string]*eventRing
}

// NewEventBus 构造事件总线。
func NewEventBus() *EventBus {
	return &EventBus{rings: map[string]*eventRing{}}
}

// OnFileEvent 实现 files.EventSink：接收领域事件并广播给 owner 订阅者。
func (b *EventBus) OnFileEvent(action, owner, rel string, size int64) {
	r := b.ring(owner)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cursor++
	ev := fileEvent{Cursor: r.cursor, Action: action, Owner: owner, Rel: rel, Size: size}
	if len(r.buf) < eventRingCap {
		r.buf = append(r.buf, ev)
	} else {
		// 环形覆盖最旧（保留最近 eventRingCap 条）。
		r.buf[len(r.buf)-1] = ev
	}
	for ch := range r.subs {
		select {
		case ch <- ev:
		default:
			// 订阅者慢（chan 满）：断开该订阅（SSE 客户端可重连回放）。
			delete(r.subs, ch)
			close(ch)
		}
	}
}

// ring 返回 owner 的环形缓冲（懒建）。
func (b *EventBus) ring(owner string) *eventRing {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.rings[owner]
	if !ok {
		r = &eventRing{subs: map[chan fileEvent]struct{}{}}
		b.rings[owner] = r
	}
	return r
}

// Subscribe 注册 owner 的订阅者：返回事件 chan、当前游标、取消函数。
// chan 容量 64；慢消费被断开（不阻塞 publish）。
func (b *EventBus) Subscribe(owner string) (<-chan fileEvent, uint64, func()) {
	r := b.ring(owner)
	r.mu.Lock()
	defer r.mu.Unlock()
	ch := make(chan fileEvent, 64)
	r.subs[ch] = struct{}{}
	return ch, r.cursor, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if _, ok := r.subs[ch]; ok {
			delete(r.subs, ch)
			close(ch)
		}
	}
}

// Replay 返回 lastCursor 之后的事件（重连回放）。
// 返回 (事件列表, 最新游标, 是否在缓冲范围内)；落后过多滚出缓冲时 ok=false。
func (b *EventBus) Replay(owner string, lastCursor uint64) ([]fileEvent, uint64, bool) {
	r := b.ring(owner)
	r.mu.Lock()
	defer r.mu.Unlock()
	if lastCursor >= r.cursor {
		return nil, r.cursor, true // 无新事件
	}
	if r.cursor-lastCursor > uint64(eventRingCap) {
		return nil, r.cursor, false // 落后过多，缓冲已滚出
	}
	var out []fileEvent
	for _, ev := range r.buf {
		if ev.Cursor > lastCursor {
			out = append(out, ev)
		}
	}
	return out, r.cursor, true
}

// eventsHandler 处理 GET /api/events?owner=<owner>——SSE 流。
// 支持 Last-Event-ID 头（重连回放）；事件格式：id:<cursor>\ndata:<json>\n\n。
func (h *Handlers) eventsHandler(w http.ResponseWriter, r *http.Request) {
	owner := r.URL.Query().Get("owner")
	if owner == "" {
		owner = ownerFromRequest(r)
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE 不支持", http.StatusInternalServerError)
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Accel-Buffering", "no")

	bus := h.eventBus()
	// 重连回放：Last-Event-ID 头指定游标起点。
	if leid := r.Header.Get("Last-Event-ID"); leid != "" {
		if last, err := strconv.ParseUint(leid, 10, 64); err == nil {
			if evs, _, ok := bus.Replay(owner, last); ok {
				for _, ev := range evs {
					writeSSEEvent(w, fl, ev)
				}
			}
		}
	}

	ch, _, cancel := bus.Subscribe(owner)
	defer cancel()

	// 首帧注释行（SSE 心跳，保持连接活跃）。
	fmt.Fprintf(w, ": connected\n\n")
	fl.Flush()

	ctx := r.Context()
	for {
		select {
		case ev := <-ch:
			writeSSEEvent(w, fl, ev)
		case <-ctx.Done():
			return
		}
	}
}

// writeSSEEvent 写一条 SSE 事件（id + data）。
func writeSSEEvent(w http.ResponseWriter, fl http.Flusher, ev fileEvent) {
	b, _ := json.Marshal(ev)
	fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Cursor, b)
	fl.Flush()
}

// eventBus 返回 Handlers 的事件总线（懒建）。
func (h *Handlers) eventBus() *EventBus {
	h.eventBusOnce.Do(func() {
		h.eventsBus = NewEventBus()
	})
	return h.eventsBus
}

// OnFileEvent 实现 files.EventSink（装配层：领域事件 → 事件总线）。
func (r filesRuntime) OnFileEvent(action, owner, rel string, size int64) {
	r.h.eventBus().OnFileEvent(action, owner, rel, size)
}

var _ files.EventSink = filesRuntime{}
