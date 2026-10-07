// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package audit 提供通用审计日志核心：作用域 Logger（NewScope）承载跨层 span
// （Begin/End/Fail）、即时单行计量（Log/Err）、跨函数数据粘合（Save/Get）与多维度观测
// （With）。行由 Sink 落盘（后续 FileSink 每任务独立文件），行本身是纯 JSON 可序列化结构。
//
// 线程安全：Logger 的方法可在并发下调用；rows 经 Sink.Append 同步落底。nil Logger（以及
// nil Span）上所有方法均安全 no-op，故 From(ctx) 返回 nil 时可放心链式调用。
package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Type 是审计行的事件类型（一类业务动作）。
type Type string

const (
	TypeAPI       Type = "api"       // HTTP API 调用
	TypeDownload  Type = "download"  // 下载
	TypeTransfer  Type = "transfer"  // 转存
	TypeEncrypt   Type = "encrypt"   // 加密/解密阶段
	TypeResource  Type = "resource"  // CPU/内存采样行
	TypeSensitive Type = "sensitive" // 敏感操作（预迁移现有 server/audit）
	TypeError     Type = "error"     // 错误
)

// Level 是审计行的严重级别。
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Dimension 用于按观测维度划分审计行（任务 / 系统 / …）。
type Dimension string

const (
	DimTask   Dimension = "task"
	DimSystem Dimension = "system"
)

// Row 是一行审计日志（落盘一行 JSON）。
type Row struct {
	Type         Type      `json:"type"`
	Level        Level     `json:"level"`
	Dim          Dimension `json:"dim"`
	Step         string    `json:"step"`
	StartMS      int64     `json:"start_ms"`         // 距作用域起点（毫秒）
	DurMS        int64     `json:"dur_ms"`           // 步骤耗时（毫秒）
	Err          string    `json:"err,omitempty"`    // 错误信息
	BandwidthBps int64     `json:"bw_bps,omitempty"` // 带宽（字节/秒）
	Bytes        int64     `json:"bytes,omitempty"`  // 字节数
	Meta         any       `json:"meta,omitempty"`   // 灵活附加（如算法、明密文对比）
	NodeID       string    `json:"node,omitempty"`   // 产生该行的节点
}

// Sink 是审计行的落盘目标：每行追加、可筛选查询最近行、关闭。
type Sink interface {
	Append(r Row) error
	Recent(f Filter) []Row
	Close() error
}

// Filter 用于筛选 Recent 结果；空字段表示不筛选。
type Filter struct {
	Type  Type
	Level Level
	Dim   Dimension
	Step  string
}

// Archiver 是归档钩子接口（预留，默认不归档）：把作用域已记录的行归档到所选卷/路径。
type Archiver interface {
	Archive(ctx context.Context, scopeID string, rows []Row) error
}

// ScopeOpts 是 NewScope 的构造参数。
type ScopeOpts struct {
	Sink    Sink      // 必填；为 nil 时 NewScope 返回 ErrNoSink
	Archive Archiver  // 预留：默认 nil = 不归档
	Start   time.Time // 作用域起点；零值 = 构造时刻
	NodeID  string    // 本节点标识，写入每行
}

// ErrNoSink 表示构造作用域时未提供 Sink。
var ErrNoSink = errors.New("audit: no sink configured")

// scopeState 是同一作用域内所有 Logger（含 With 派生子）共享的可变状态：
// kvStore（Save/Get 粘合数据）与最近一次 append 失败错误（Flush 消费）。
type scopeState struct {
	mu   sync.Mutex
	kv   map[string]any
	aerr error
}

// Logger 是作用域审计记录器。With 派生子共享同一 scopeState/sink，仅维度不同。
type Logger struct {
	scopeID string
	sink    Sink
	start   time.Time
	nodeID  string
	dim     Dimension
	state   *scopeState
}

// NewScope 以 scopeID 构造新的作用域审计记录器；opts.Sink 必填。
func NewScope(scopeID string, opts ScopeOpts) (*Logger, error) {
	if opts.Sink == nil {
		return nil, fmt.Errorf("audit: NewScope(%q): %w", scopeID, ErrNoSink)
	}
	start := opts.Start
	if start.IsZero() {
		start = time.Now()
	}
	return &Logger{
		scopeID: scopeID,
		sink:    opts.Sink,
		start:   start,
		nodeID:  opts.NodeID,
		dim:     DimTask, // 默认任务维度
		state:   &scopeState{kv: make(map[string]any)},
	}, nil
}

// With 返回同作用域、换维度的派生子 Logger（拷贝，共享 sink/scope state）。kv 键值对写入
// 作用域共享 kvStore，供跨维度 Save/Get 读取。nil Logger 返回 nil。
func (l *Logger) With(d Dimension, kv ...any) *Logger {
	if l == nil {
		return nil
	}
	nl := *l
	nl.dim = d
	if len(kv) > 1 {
		nl.state.mu.Lock()
		for i := 0; i+1 < len(kv); i += 2 {
			if k, ok := kv[i].(string); ok {
				nl.state.kv[k] = kv[i+1]
			}
		}
		nl.state.mu.Unlock()
	}
	return &nl
}

// Span 是单个步骤的计时跨度：Begin 创建，End/Fail 收口并落行。
type Span struct {
	l     *Logger
	step  string
	typ   Type
	level Level
	start time.Time
	kv    []any // Begin 时预置的键值对（End/Fail 时先于 End 参数应用）
}

// Begin 开启一个步骤 span，step 拟定为 Type（step 命中已知 Type 值则用之，否则回落
// TypeResource）。kv 为起始附加键值对。nil Logger 返回可安全调用但落空的 Span。
func (l *Logger) Begin(step string, kv ...any) *Span {
	if l == nil {
		return &Span{}
	}
	return &Span{
		l:     l,
		step:  step,
		typ:   stepType(step),
		level: LevelInfo,
		start: time.Now(),
		kv:    kv,
	}
}

// End 收尾 span：填 DurMS，应用 Begin/End 键值对，落一行到 Sink。
func (s *Span) End(kv ...any) {
	if s == nil || s.l == nil {
		return
	}
	l := s.l
	r := Row{
		Type:    s.typ,
		Level:   s.level,
		Dim:     l.dim,
		Step:    s.step,
		StartMS: s.start.Sub(l.start).Milliseconds(),
		DurMS:   time.Since(s.start).Milliseconds(),
	}
	applyKV(&r, s.kv)
	applyKV(&r, kv)
	l.appendRow(&r)
}

// Fail 以错误收尾 span：级别置为 LevelError 并携带 err，落库。kv 为附加键值对
// （与 End 同语义：bytes/bw_bps 映射到对应字段，其余汇聚进 Meta；可为空）。
func (s *Span) Fail(err error, kv ...any) {
	if s == nil || s.l == nil {
		return
	}
	l := s.l
	r := Row{
		Type:    s.typ,
		Level:   LevelError,
		Dim:     l.dim,
		Step:    s.step,
		Err:     errText(err),
		StartMS: s.start.Sub(l.start).Milliseconds(),
		DurMS:   time.Since(s.start).Milliseconds(),
	}
	applyKV(&r, s.kv)
	applyKV(&r, kv)
	l.appendRow(&r)
}

// Log 记一行无计时的事件（DurMS=0）。
func (l *Logger) Log(t Type, lvl Level, step string, kv ...any) {
	if l == nil {
		return
	}
	r := Row{Type: t, Level: lvl, Dim: l.dim, Step: step}
	applyKV(&r, kv)
	l.appendRow(&r)
}

// Err 记一条错误审计行（Type=TypeError，Level=LevelError）。
func (l *Logger) Err(err error, step string, kv ...any) {
	if l == nil {
		return
	}
	r := Row{Type: TypeError, Level: LevelError, Dim: l.dim, Step: step, Err: errText(err)}
	applyKV(&r, kv)
	l.appendRow(&r)
}

// Save 把跨函数计算值写入作用域共享 kvStore（幂等）。ctx 携带 Logger 时写入 ctx 的
// Logger，否则写接收者。nil Logger 安全 no-op。
func (l *Logger) Save(ctx context.Context, key string, v any) error {
	lg := loggerFromContext(ctx, l)
	if lg == nil {
		return nil
	}
	lg.state.mu.Lock()
	lg.state.kv[key] = v
	lg.state.mu.Unlock()
	return nil
}

// Get 从作用域共享 kvStore 读取键值。ctx 携带 Logger 时读 ctx 的，否则读接收者。
// nil Logger 返回 (nil, false)。
func (l *Logger) Get(ctx context.Context, key string) (any, bool) {
	lg := loggerFromContext(ctx, l)
	if lg == nil {
		return nil, false
	}
	lg.state.mu.Lock()
	v, ok := lg.state.kv[key]
	lg.state.mu.Unlock()
	return v, ok
}

// Flush 返回并清空自上次 Flush 以来 append 失败的聚合错误。行均同步追加，故本方法主要
// 作为错误收口与调用方同步点。nil Logger 返回 nil。
func (l *Logger) Flush() error {
	if l == nil {
		return nil
	}
	l.state.mu.Lock()
	err := l.state.aerr
	l.state.aerr = nil
	l.state.mu.Unlock()
	return err
}

// Close 关闭底层 Sink。nil Logger 安全 no-op。
func (l *Logger) Close() error {
	if l == nil || l.sink == nil {
		return nil
	}
	return l.sink.Close()
}

// WithContext 把 Logger 放入 ctx，供深层代码经 From 取用。nil Logger 返回原 ctx。
func WithContext(ctx context.Context, l *Logger) context.Context {
	if ctx == nil || l == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, l)
}

// From 从 ctx 取 Logger；无则返回 nil（安全，nil Logger 上所有方法均 no-op）。
func From(ctx context.Context) *Logger {
	if ctx == nil {
		return nil
	}
	l, _ := ctx.Value(ctxKey{}).(*Logger)
	return l
}

type ctxKey struct{}

// loggerFromContext 优先取 ctx 携带的 Logger，缺省回落接收者。
func loggerFromContext(ctx context.Context, fallback *Logger) *Logger {
	if l := From(ctx); l != nil {
		return l
	}
	return fallback
}

// appendRow 同步落一行到 Sink，并把失败的 error 聚合进 Flush；按 scopeID+step 记一条 slog
// 告警，避免 append 失败被静默吞掉。
func (l *Logger) appendRow(r *Row) {
	if l.sink == nil {
		return
	}
	r.NodeID = l.nodeID
	if err := l.sink.Append(*r); err != nil {
		l.state.mu.Lock()
		if l.state.aerr == nil {
			l.state.aerr = err
		}
		l.state.mu.Unlock()
		slog.Warn("audit: append row failed", "scope", l.scopeID, "step", r.Step, "err", err)
	}
}

// applyKV 把键值对映射到 Row：key "bytes"→Bytes、"bw_bps"→BandwidthBps，其余汇聚进
// Meta（map）。无法成对的 kv（奇数项或非 string key）忽略。
func applyKV(r *Row, kv []any) {
	for i := 0; i+1 < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			continue
		}
		v := kv[i+1]
		switch k {
		case "bytes":
			r.Bytes = toInt64(v)
		case "bw_bps":
			r.BandwidthBps = toInt64(v)
		default:
			m, _ := r.Meta.(map[string]any)
			if m == nil {
				m = make(map[string]any, len(kv)/2)
				r.Meta = m
			}
			m[k] = v
		}
	}
}

// toInt64 宽容地数值化为 int64；无法识别返回 0。
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int8:
		return int64(n)
	case int16:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case uint:
		return int64(n)
	case uint8:
		return int64(n)
	case uint16:
		return int64(n)
	case uint32:
		return int64(n)
	case uint64:
		return int64(n)
	case float32:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

// stepType 把步骤名映射为审计类型：步骤名命中已知类型值则用之，否则回落 TypeResource。
func stepType(step string) Type {
	switch Type(step) {
	case TypeAPI, TypeDownload, TypeTransfer, TypeEncrypt, TypeResource, TypeSensitive:
		return Type(step)
	default:
		return TypeResource
	}
}

// errText 把 error 转文本；nil 返回空串。
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
