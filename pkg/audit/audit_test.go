// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestLogger_BeginEnd_Dur(t *testing.T) {
	t.Parallel()
	var got []Row
	sink := &memSink{append: func(r Row) { got = append(got, r) }}
	l, err := NewScope("task-1", ScopeOpts{Sink: sink, NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	s := l.Begin("download")
	time.Sleep(2 * time.Millisecond)
	kb, vb := ByteKV(1024)
	kbw, vbw := BandwidthKV(4096)
	s.End(kb, vb, kbw, vbw)
	if err := l.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	r := got[0]
	if r.Type != TypeDownload || r.Step != "download" || r.Dim != DimTask {
		t.Fatalf("bad row: %+v", r)
	}
	if r.DurMS < 1 {
		t.Fatalf("dur not recorded: %+v", r)
	}
	// 量级上界：Begin 必须初始化 span.start（buggy 版本未初始化 → time.Since(zero)
	// 溢出为 ~292 年量级 DurMS，此处必红锁定回归）。
	if r.DurMS >= 60000 {
		t.Fatalf("dur magnitude out of range (Begin start not initialized?): %+v", r)
	}
	if r.Bytes != 1024 || r.BandwidthBps != 4096 {
		t.Fatalf("kv lost: %+v", r)
	}
}

func TestLogger_SaveGet_CrossFunc(t *testing.T) {
	t.Parallel()
	l := mustLogger(t)
	ctx := WithContext(context.Background(), l)
	l.Save(ctx, "plain_bytes", int64(1000))
	v, ok := l.Get(ctx, "plain_bytes")
	if !ok || v != int64(1000) {
		t.Fatalf("get failed: %v ok=%v", v, ok)
	}
}

func TestLogger_From_NoLogger_Nil(t *testing.T) {
	t.Parallel()
	if From(context.Background()) != nil {
		t.Fatal("From on empty ctx must be nil")
	}
}

func TestLogger_With_Dimension(t *testing.T) {
	t.Parallel()
	l := mustLogger(t)
	sys := l.With(DimSystem)
	if sys == nil || sys == l {
		t.Fatal("With must return distinct logger")
	}
	if sys.dim != DimSystem {
		t.Fatalf("dim not set: %v", sys.dim)
	}
}

func TestLogger_Err_LevelError(t *testing.T) {
	t.Parallel()
	var got []Row
	l := mustLoggerSink(t, &memSink{append: func(r Row) { got = append(got, r) }})
	l.Err(errors.New("boom"), "encrypt")
	if len(got) != 1 || got[0].Level != LevelError || got[0].Err != "boom" {
		t.Fatalf("bad error row: %+v", got)
	}
}

// memSink 是 Sink 的内存测试实现：append 字段（可选）逐行回调，rows 保存全部已追加行。
type memSink struct {
	mu     sync.Mutex
	rows   []Row
	append func(r Row)
}

func (m *memSink) Append(r Row) error {
	if m.append != nil {
		m.append(r)
	}
	m.mu.Lock()
	m.rows = append(m.rows, r)
	m.mu.Unlock()
	return nil
}

func (m *memSink) Recent(f Filter) []Row {
	m.mu.Lock()
	defer m.mu.Unlock()
	if f.Type == "" && f.Level == "" && f.Dim == "" && f.Step == "" {
		out := make([]Row, len(m.rows))
		copy(out, m.rows)
		return out
	}
	var out []Row
	for _, r := range m.rows {
		if (f.Type == "" || f.Type == r.Type) &&
			(f.Level == "" || f.Level == r.Level) &&
			(f.Dim == "" || f.Dim == r.Dim) &&
			(f.Step == "" || f.Step == r.Step) {
			out = append(out, r)
		}
	}
	return out
}

func (m *memSink) Close() error { return nil }

// mustLogger 构造带内存 Sink 的默认作用域 Logger。
func mustLogger(t *testing.T) *Logger {
	t.Helper()
	return mustLoggerSink(t, &memSink{})
}

// mustLoggerSink 用给定 Sink 构造作用域 Logger，失败即终止测试。
func mustLoggerSink(t *testing.T, s Sink) *Logger {
	t.Helper()
	l, err := NewScope("test", ScopeOpts{Sink: s})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	return l
}

// ByteKV 构造字节数键值对（"bytes"）。
func ByteKV(bytes int64) (string, any) { return "bytes", bytes }

// BandwidthKV 构造带宽键值对（"bw_bps"）。
func BandwidthKV(bandwidthBps int64) (string, any) { return "bw_bps", bandwidthBps }
