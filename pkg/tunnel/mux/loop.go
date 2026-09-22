// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"context"
	"errors"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

func (m *Mux) writeLoop() {
	// 常驻 ticker 而非每次循环 time.After：避免空闲时每 50ms 分配一个新 timer
	// （timer 不 Stop 会滞留到触发，多 mux 时是持续的 GC 压力）。defer Stop 保证
	// 退出路径不残留计时器。
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case msg := <-m.writeCh:
			m.sendFrame(msg)
		case <-ticker.C:
			// 补送被 writeCh 打满挤掉的窗口信用（信用不得静默丢失，见 retransmit.go）。
			m.flushPendingWindowUpdates()
			// 补送被 writeCh 打满挤掉的 Pong（同样不得静默丢失，见 frame_handler.go）。
			m.flushPendingPong()
		}
		m.scanRetransmitQ()
	}
}

func (m *Mux) readLoop() {
	retries := 0
	for {
		raw, err := m.conn.Receive(m.Context())
		if err != nil {
			// context.Canceled 是本 mux 被关闭的信号（Close→done→ctx cancel），不是瞬时
			// 传输错误 ⇒ 立即退出，不重试不退避（否则关闭阶段每个 mux 都打
			// 「recv transient error, retrying」并以 1s/2s/4s… 退避拖延，CI 实证：
			// pkg/tunnel benchmark 收尾卡 44s + 367 条 mux error 日志风暴）。
			if errors.Is(err, context.Canceled) {
				return
			}
			if errors.Is(err, xfer.ErrConnClosed) || retries >= maxRecvRetries {
				m.metrics.Errors.Add(1)
				m.logger.Error("mux: recv error, closing", "err", err, "retries", retries)
				m.Close()
				return
			}
			retries++
			m.metrics.RecvRetries.Add(1)
			m.logger.Warn("mux: recv transient error, retrying", "err", err, "retries", retries)
			backoff := retryBaseDelay << min(retries-1, 3)
			select {
			case <-m.done:
				return
			case <-m.ctx.Done():
				m.Close()
				return
			case <-time.After(backoff):
			}
			continue
		}
		retries = 0
		m.handleFrame(raw)
	}
}

// paddingLoop 空闲填充循环（roadmap §5.3 P1 被动伪装层；仅 WithIdlePadding 开启时启动）。
// 周期发送 FramePadding 帧（负载 nil），与 pingLoop 30s 心跳独立共存——填充只保持
// 连接活跃形态（DPI 难判断空闲），不参与心跳超时判定。
func (m *Mux) paddingLoop() {
	ticker := time.NewTicker(m.paddingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			frame, encErr := EncodeFrame(0, FramePadding, nil)
			if encErr != nil {
				continue
			}
			if err := m.conn.Send(m.Context(), frame); err != nil {
				m.metrics.Errors.Add(1)
				return
			}
			m.metrics.PaddingSent.Add(1)
		}
	}
}

func (m *Mux) pingLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			frame, encErr := EncodeFrame(0, FramePing, nil)
			if encErr != nil { // 不可达：负载为 nil
				continue
			}
			m.metrics.PingsSent.Add(1)
			if err := m.conn.Send(m.Context(), frame); err != nil {
				m.metrics.Errors.Add(1)
				m.logger.Error("mux: ping send error, closing", "err", err)
				m.Close()
				return
			}
			lastPong := time.Unix(0, m.lastPongNano.Load())
			if time.Since(lastPong) > 90*time.Second {
				m.metrics.Errors.Add(1)
				m.logger.Warn("mux: heartbeat timeout, closing")
				m.Close()
				return
			}
		}
	}
}
