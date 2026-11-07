// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// core.go 是**统一审计条目模型**（设计 docs/designs/2026-10-19-audit-service.md §4.1）。
//
// 与既有 legacy API（audit.go 的 Row/Sink/Logger/Span）的关系：同一份实现、同一个模型 ——
// 新 API 是唯一实现，legacy 是**门面**（见 legacy.go，门禁 R35-d）。本文件只放**数据与策略**，
// 不含落盘/索引/服务实现（分别在 journal.go / index.go / service.go）。

// Type 是审计行的事件类型（一类业务动作）—— 与旧 Row.Type 同名同值（数据兼容，非第二实现）。
type Type string

const (
	TypeAPI       Type = "api"       // HTTP API 调用
	TypeDownload  Type = "download"  // 下载
	TypeTransfer  Type = "transfer"  // 转存
	TypeEncrypt   Type = "encrypt"   // 加密/解密阶段
	TypeResource  Type = "resource"  // CPU/内存采样行（专用：采样类遥测）
	TypeSensitive Type = "sensitive" // 敏感操作
	TypeError     Type = "error"     // 错误
	TypeLog       Type = "log"       // 通用事件行（无专属类型的 Log/未知步骤）
)

// Level 是审计行的严重级别。
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Dimension 用于按观测维度划分审计行（任务 / 系统）。
type Dimension string

const (
	DimTask   Dimension = "task"
	DimSystem Dimension = "system"
)

// Kind 是条目类别（统一旧 Row.Type 与合规事件）。
type Kind string

const (
	// KindEvent 是合规事件（谁/什么/结果）。
	KindEvent Kind = "event"
	// KindSpan 是过程步骤（含耗时/错误）。
	KindSpan Kind = "span"
	// KindState 是作用域状态（WAL：Save 落盘；恢复重放依赖它）。
	KindState Kind = "state"
	// KindDone 是作用域终态（**提交记录**：副作用完成之后写入；恢复时跳过）。
	KindDone Kind = "done"
	// KindResource 是采样行（CPU/内存/带宽）。
	KindResource Kind = "resource"
	// KindRotate 是轮转标记（审计自身的可审计性）。
	KindRotate Kind = "rotate"
)

// Result 是合规结果。
type Result string

const (
	// ResultSuccess 表示操作成功完成。
	ResultSuccess Result = "success"
	// ResultDenied 表示被拒绝（校验失败/权限不足）。
	ResultDenied Result = "denied"
	// ResultError 表示因错误失败。
	ResultError Result = "error"
	// ResultPending 表示进行中（长任务中间态）。
	ResultPending Result = "pending"
)

// ScopeID 是一次可审计工作单元（请求/任务/后台作业）的标识。
// 合法形态复用 legacy 白名单 scopeIDPattern（字母数字开头，其后 [A-Za-z0-9._-]，长度 ≤128）
// —— scopeID 参与文件名拼接，是路径安全第一道闸。
type ScopeID string

// validScopeID 报告 id 是否可作为作用域标识（复用 legacy 白名单，避免两套规则）。
func validScopeID(id ScopeID) bool {
	return scopeIDPattern.MatchString(string(id))
}

// Entry 是统一审计条目。契约要点（设计 §4.1）：
//   - 合规语义字段**不带 omitempty**（/api/audit 字段出现性与现状一致）；
//   - `Owner` 是**权限过滤的唯一权威字段**（不是 Actor；来源规则见设计 §5.7）；
//   - `Seq` 是 **scope 内**权威顺序（全局序由 (Node, 节点级 seq) 决定，见 §4.1）。
type Entry struct {
	// 身份与顺序
	Scope ScopeID `json:"scope"`
	Seq   uint64  `json:"seq"`
	Kind  Kind    `json:"kind"`
	// 兼容旧 Row 的维度（旧文件可 round-trip）
	Type  Type      `json:"type,omitempty"`
	Level Level     `json:"level,omitempty"`
	Dim   Dimension `json:"dim,omitempty"`
	// 合规语义（契约字段）
	Action  string `json:"action"`
	Actor   string `json:"actor"`
	Owner   string `json:"owner"`
	Mesh    string `json:"mesh"`
	ObjType string `json:"object_type"`
	Object  string `json:"object"`
	Result  Result `json:"result"`
	Detail  string `json:"detail"`
	// 结构化上下文（纯增量）
	Volume    string `json:"volume,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	SourceIP  string `json:"source_ip,omitempty"`
	// 过程 / 资源
	Step    string `json:"step,omitempty"`
	StartMS int64  `json:"start_ms,omitempty"`
	DurMS   int64  `json:"dur_ms,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
	BwBps   int64  `json:"bw_bps,omitempty"`
	Err     string `json:"err,omitempty"`
	// 关联
	Node string         `json:"node,omitempty"`
	TS   time.Time      `json:"ts"`
	Meta map[string]any `json:"meta,omitempty"`
}

// isNoAudit 报告该条目是否带「不再产生审计」标记（防递归：审计/归档写入自身不得再触发审计）。
// 见设计 E16。
func (e Entry) isNoAudit() bool {
	v, ok := e.Meta["no_audit"]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// EntryFilter 是读取/查询的过滤条件（空字段不过滤；多字段 AND）。
type EntryFilter struct {
	Scope  ScopeID
	Kind   Kind
	Type   Type
	Level  Level
	Dim    Dimension
	Action string
	Actor  string
	Owner  string
	Mesh   string
	Object string
	Result Result
	// Volume 按**卷维度**筛选（空 = 不过滤）。评审 R13：Entry.Volume 此前从未赋值、
	// 过滤器也没有该维度 ⇒ 即便写了也筛不了。
	Volume string
	Since  time.Time // 保留 TS.After(Since) 的条目（零值不过滤）
	Limit  int       // >0 时最多返回该条数（按序从最早开始）
}

// Match 报告条目是否命中过滤条件（供 Journal.Read / Index.Query 共用，避免两套语义）。
// 拆成三个小判定以保持可读性与复杂度合规（身份 / 主体 / 时间）。
func (f EntryFilter) Match(e Entry) bool {
	return f.matchIdentity(e) && f.matchSubject(e) && f.matchTime(e)
}

// matchIdentity 判定身份类字段（作用域/类别/旧维度）。
func (f EntryFilter) matchIdentity(e Entry) bool {
	if f.Scope != "" && e.Scope != f.Scope {
		return false
	}
	if f.Kind != "" && e.Kind != f.Kind {
		return false
	}
	if f.Type != "" && e.Type != f.Type {
		return false
	}
	if f.Level != "" && e.Level != f.Level {
		return false
	}
	if f.Dim != "" && e.Dim != f.Dim {
		return false
	}
	return true
}

// matchSubject 判定主体类字段（动作/主体/归属/对象/结果）。
func (f EntryFilter) matchSubject(e Entry) bool {
	if f.Action != "" && !actionMatches(e.Action, f.Action) {
		return false
	}
	if f.Actor != "" && e.Actor != f.Actor {
		return false
	}
	if f.Owner != "" && e.Owner != f.Owner {
		return false
	}
	if f.Mesh != "" && e.Mesh != f.Mesh {
		return false
	}
	if f.Object != "" && e.Object != f.Object {
		return false
	}
	if f.Volume != "" && e.Volume != f.Volume {
		return false
	}
	if f.Result != "" && e.Result != f.Result {
		return false
	}
	return true
}

// matchTime 判定时间下界（Since 为零值则不过滤）。
func (f EntryFilter) matchTime(e Entry) bool {
	return f.Since.IsZero() || e.TS.After(f.Since)
}

// Clock 是时间来源（可注入 ⇒ 过时/老化判定可确定性测试，禁 time.Sleep）。
// Since 使用单调语义（防时钟回拨导致老化/过时失真）。
type Clock interface {
	Now() time.Time
	Since(time.Time) time.Duration
}

// systemClock 是默认时钟。
type systemClock struct{}

func (systemClock) Now() time.Time                  { return time.Now() }
func (systemClock) Since(t time.Time) time.Duration { return time.Since(t) }

// Metrics 是审计自身的可观测面（设计 §5.4.1 / E3 / E21）。实现必须并发安全且**不得 panic**。
type Metrics interface {
	// IncrDrop 累计**未持久化即被逐出**的条目数（真正的审计丢失；有界保留的代价，设计 §5.4.1）。
	IncrDrop(n int)
	// IncrEvict 累计**已持久化**条目的热索引逐出数（不是丢失：Journal 仍是权威）。
	IncrEvict(n int)
	// IncrWriteFailed 累计落盘失败次数。
	IncrWriteFailed()
	// IncrHookPanic 累计下游钩子（Notifier/Index 使用方实现）panic 次数。
	IncrHookPanic()
	// IncrRotate 按原因累计轮转次数。
	IncrRotate(reason string)
}

// NullMetrics 是 Metrics 的 null-object（零分支调用）。
type NullMetrics struct{}

func (NullMetrics) IncrDrop(int)      {}
func (NullMetrics) IncrEvict(int)     {}
func (NullMetrics) IncrWriteFailed()  {}
func (NullMetrics) IncrHookPanic()    {}
func (NullMetrics) IncrRotate(string) {}

// Notifier 是审计条目的下游钩子（通知中心/告警/SIEM）。
// 实现**不得 panic**；Service 仍会 recover 兜底并计数（E21）。
type Notifier interface {
	Notify(ctx context.Context, e Entry)
}

// NullNotifier 是 Notifier 的 null-object。
type NullNotifier struct{}

func (NullNotifier) Notify(context.Context, Entry) {}

// Layout 决定段文件的组织方式（设计 §4.1/§5.4.1：合规=全局段族、任务/WAL=按 scope 段族）。
type Layout string

const (
	// LayoutGlobal 是全局段（合规时间线：便于合规导出/检索）。
	LayoutGlobal Layout = "global"
	// LayoutPerScope 是按作用域段（任务/WAL：便于 Resume 与单任务归档）。
	LayoutPerScope Layout = "per_scope"
)

// Policy 是轮转与有界保留策略（设计 §5.1/§5.4.1）。
// 全部字段零值 = 不触发（除 MaxMemoryEntries 的显式设置），由 Options 归一为**不可变 effective**。
type Policy struct {
	// T1：内存活跃索引上限（条数 / 近似字节）。0 = 不限制。
	MaxMemoryEntries int
	MaxMemoryBytes   int64
	// T2：内容过时轮转（最早未落盘条目超龄）。0 = 不启用。
	MaxAge time.Duration
	// T3：读取触发轮转。DisableRotateOnRead 默认 false ⇒ **默认开启**（bool 默认 false 纪律）。
	DisableRotateOnRead bool
	// T3 防抖：最小轮转间隔。0 = 不防抖。
	MinRotateInterval time.Duration
	// T7/T8：段大小与热段硬上限。
	MaxSize       int64
	MaxArchives   int
	MaxTotalBytes int64
}

// CapabilitySet 声明**使用方按场景选择性启用**的能力（设计 §2.6）。
// 包提供全部能力，使用方声明所需 —— 不做一刀切。
type CapabilitySet struct {
	Compliance bool // 合规事件形态（Emit）
	Spans      bool // 过程 span（Begin/End/Fail）
	WAL        bool // 作用域状态落盘（Save/Get 持久化；重放语义见 M2）
	Resource   bool // 采样行
	Index      bool // 内存活跃索引
	Notifier   bool // 下游通知钩子
}

// FullCapabilities 是全能力（默认）。
func FullCapabilities() CapabilitySet {
	return CapabilitySet{Compliance: true, Spans: true, WAL: true, Resource: true, Index: true, Notifier: true}
}

// ComplianceOnly 是仅合规剖面（最小写入面）。
func ComplianceOnly() CapabilitySet {
	return CapabilitySet{Compliance: true, Index: true}
}

// TelemetryOnly 是仅诊断剖面（span/采样，不记合规事件）。
func TelemetryOnly() CapabilitySet {
	return CapabilitySet{Spans: true, Resource: true, Index: true}
}

// DisabledCapabilities 是禁用剖面（no-op Service：Emit 静默成功 + 计数，零回归）。
func DisabledCapabilities() CapabilitySet { return CapabilitySet{} }

// disabled 报告是否禁用全部能力。
func (c CapabilitySet) disabled() bool {
	return !c.Compliance && !c.Spans && !c.WAL && !c.Resource
}

// enabled 报告某类条目是否被能力集允许写入。
func (c CapabilitySet) enabled(k Kind) bool {
	switch k {
	case KindEvent, KindDone, KindRotate:
		return c.Compliance
	case KindSpan:
		return c.Spans
	case KindState:
		return c.WAL
	case KindResource:
		return c.Resource
	default:
		// 未知类别：保守放行（调用方已显式构造 Entry；门禁保证类别受控）。
		return true
	}
}

// stepKind 由步骤名推断条目类别（过程步骤 ⇒ KindSpan；采样 ⇒ KindResource）。
func stepKind(step string) Kind {
	if strings.HasPrefix(step, "resource") {
		return KindResource
	}
	return KindSpan
}

// stepType 由步骤名推断事件类型（Type）—— 等价旧 stepType 的职责：
// 下载/转存/加解密/采样各自成类，其余归 TypeLog。**必须设置**，否则按 Type 过滤的
// 查询（云任务审计报告、加密卷 encrypt span 断言）会全部落空。
func stepType(step string) Type {
	switch {
	case strings.HasPrefix(step, "download"):
		return TypeDownload
	case strings.HasPrefix(step, "transfer"):
		return TypeTransfer
	case strings.HasPrefix(step, "encrypt"), strings.HasPrefix(step, "decrypt"):
		return TypeEncrypt
	case strings.HasPrefix(step, "resource"):
		return TypeResource
	default:
		return TypeLog
	}
}

// applyEntryKV 把键值对映射到 Entry：bytes/bw_bps 直填数值，其余汇聚进 Meta。
// （与 legacy `applyKV(*Row, …)` 同语义；Entry 是统一模型，legacy 行经门面转换。）
func applyEntryKV(e *Entry, kv []any) {
	for i := 0; i+1 < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			continue
		}
		v := kv[i+1]
		switch k {
		case "bytes":
			if n, ok := asInt64(v); ok {
				e.Bytes = n
			} else {
				putEntryMeta(e, k, v)
			}
		case "bw_bps":
			if n, ok := asInt64(v); ok {
				e.BwBps = n
			} else {
				putEntryMeta(e, k, v)
			}
		default:
			putEntryMeta(e, k, v)
		}
	}
}

// putEntryMeta 把键值写入 Entry.Meta（懒建 map）。
func putEntryMeta(e *Entry, k string, v any) {
	if e.Meta == nil {
		e.Meta = make(map[string]any, 4)
	}
	e.Meta[k] = v
}

// asInt64 宽容地把数值类型转 int64；**不可识别返回 false**（调用方决定处置，不静默写 0 ——
// 审计里「0 字节」是有效语义，把类型写错记成 0 会污染数据）。
func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint:
		return int64(n), true
	case uint8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		return int64(n), true
	case float32:
		return int64(n), true
	case float64:
		return int64(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
		if f, err := n.Float64(); err == nil {
			return int64(f), true
		}
		return 0, false
	default:
		return 0, false
	}
}

// actionMatches 保留既有 action 过滤语义：过滤器**以 "." 结尾** ⇒ 前缀族匹配
// （如 "ai." 命中 "ai.summarize"/"ai.tag"）；否则精确相等。
// 契约来源：pkg/server 原 AuditFilter.actionMatches（前缀族用例见 TestActionMatches_PrefixFamily）。
func actionMatches(action, filter string) bool {
	if strings.HasSuffix(filter, ".") {
		return strings.HasPrefix(action, filter)
	}
	return action == filter
}
