// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"errors"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

func (m *Mux) writeLoop() {
	for {
		select {
		case <-m.done:
			return
		case msg := <-m.writeCh:
			m.sendFrame(msg)
		case <-time.After(50 * time.Millisecond):
		}
		m.scanRetransmitQ()
	}
}

func (m *Mux) readLoop() {
	retries := 0
	for {
		raw, err := m.conn.Receive(m.Context())
		if err != nil {
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

func (m *Mux) pingLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			frame := EncodeFrame(0, FramePing, nil)
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
