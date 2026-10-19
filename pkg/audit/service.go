// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// service.go 是审计能力的**唯一门面**（设计 §4.3）：
//   - 结构体注入式（**零全局可变状态** ⇒ 测试隔离、不污染其他功能测试）；
//   - `Options` 在 `New` 时一次性 resolve 成**不可变 effective 配置**（消除每次写入的三级查表与 nil 分支）；
//   - 作用域强制：**`Emit` 定义在 `*Scope` 上** ⇒ 用法上无法绕开作用域（编译期强制）；
//   - 下游钩子 panic 一律 recover + 计数（E21）；落盘失败不阻断业务（best-effort，可选 fail-closed）。

// ErrInvalidScope 表示作用域标识非法（含路径穿越风险形态）。
var ErrInvalidScope = errors.New("audit: 非法作用域标识")

// ErrScopeClosed 表示作用域已结束（MarkDone/Close 之后不可再写）。
var ErrScopeClosed = errors.New("audit: 作用域已结束")

// ErrAuditUnavailable 表示 fail-closed 模式下审计不可写（调用方应拒绝业务操作）。
var ErrAuditUnavailable = errors.New("audit: 审计不可写（fail-closed）")

// ContextFields 是从 ctx 提取身份/上下文的**注入点**（pkg/audit 不 import 装配层 ⇒ 保持叶子包）。
// 未注入的字段返回空串（不影响写入，只影响过滤与展示）。
type ContextFields struct {
	Actor     func(context.Context) string
	Owner     func(context.Context) string
	Mesh      func(context.Context) string
	RequestID func(context.Context) string
	SourceIP  func(context.Context) string
}

// Options 是 Service 的构造参数（显式注入，无包级变量）。
type Options struct {
	Journal  Journal  // 必填（MemoryJournal 供测试；NullJournal 供禁用剖面）
	Index    Index    // 可选（nil ⇒ 由 Policy/CapabilitySet 决定是否建环形索引）
	Notifier Notifier // 可选（nil ⇒ NullNotifier）
	Clock    Clock    // 可选（默认 systemClock）
	Metrics  Metrics  // 可选（默认 NullMetrics）
	Logger   *slog.Logger
	Policy   Policy
	// Caps 为 nil ⇒ 全能力（零回归：既有调用方不声明也照常工作）；显式传 &DisabledCapabilities() 关闭。
	Caps *CapabilitySet
	// NodeID 是本节点标识（写入每行；单机模型下恒同值 ⇒ 全局序退化为节点 seq）。
	NodeID string
	// FailClosed=true 时审计不可写会返回错误（合规部署显式开启；默认 best-effort）。
	FailClosed bool
	// CtxFields 是 ctx 身份提取注入点。
	CtxFields ContextFields
}

// effective 是 resolve 后的不可变配置（写入路径只读它）。
type effective struct {
	journal  Journal
	index    Index
	notifier Notifier
	clock    Clock
	metrics  Metrics
	logger   *slog.Logger
	policy   Policy
	caps     CapabilitySet
	nodeID   string
	failFast bool
	ctx      ContextFields
}

// Service 是审计服务（唯一门面；并发安全）。
type Service struct {
	eff effective

	mu     sync.Mutex
	seq    map[ScopeID]uint64 // 每作用域单调 seq
	scopes map[ScopeID]*scopeRuntime
	last   time.Time // 最近一次轮转（T3 防抖）
	dirty  int       // 自上次轮转以来的条目数（T2/T3 判据）
	// unsynced 是「已进内存但落盘失败」的条目数（有界保留时优先计为**丢失**；设计 §5.4.1）。
	unsynced int
	closed   bool
}

// scopeRuntime 是作用域运行时状态。
type scopeRuntime struct {
	kv     map[string]any
	done   bool
	closed bool
}

// New 构造 Service（`Options` 一次性 resolve）。
func New(opts Options) (*Service, error) {
	if opts.Journal == nil {
		return nil, errors.New("audit: New 需要 Journal（测试可用 NewMemoryJournal）")
	}
	eff := effective{
		journal:  opts.Journal,
		index:    opts.Index,
		notifier: opts.Notifier,
		clock:    opts.Clock,
		metrics:  opts.Metrics,
		logger:   opts.Logger,
		policy:   opts.Policy,
		caps:     FullCapabilities(), // 由下方 resolve 覆盖（nil ⇒ 全能力）
		nodeID:   opts.NodeID,
		failFast: opts.FailClosed,
		ctx:      opts.CtxFields,
	}
	if eff.clock == nil {
		eff.clock = systemClock{}
	}
	if eff.metrics == nil {
		eff.metrics = NullMetrics{}
	}
	if eff.notifier == nil {
		eff.notifier = NullNotifier{}
	}
	// 能力集：nil ⇒ 全能力（零回归）；显式传指针 ⇒ 严格按声明（含「禁用剖面」）。
	if opts.Caps != nil {
		eff.caps = *opts.Caps
	} else {
		eff.caps = FullCapabilities()
	}
	// 索引：能力开启且未注入 ⇒ 按 Policy 上限建环形索引。
	if eff.index == nil {
		switch {
		case !eff.caps.Index:
			eff.index = NullIndex{}
		case eff.policy.MaxMemoryEntries > 0:
			eff.index = NewRingIndex(eff.policy.MaxMemoryEntries)
		default:
			eff.index = NullIndex{}
		}
	}
	return &Service{
		eff:    eff,
		seq:    make(map[ScopeID]uint64),
		scopes: make(map[ScopeID]*scopeRuntime),
		last:   eff.clock.Now(),
	}, nil
}

// Capabilities 返回生效能力集（使用方断言用）。
func (s *Service) Capabilities() CapabilitySet { return s.eff.caps }

// Disabled 报告是否为禁用剖面（no-op）。
func (s *Service) Disabled() bool { return s.eff.caps.disabled() }

// svcCtxKey / scCtxKey 是审计服务与作用域在 ctx 中的键（替代旧 WithContext/From 的语义）。
type svcCtxKey struct{}

type scCtxKey struct{}

// WithScope 把作用域句柄注入 ctx：**过程 span（Begin/BeginSpan）的作用域来源**。
// 与旧 API「NewScope 绑定 scopeID 的 logger 经 ctx 传递」语义等效（这里拆成 Service + Scope）。
func WithScope(ctx context.Context, sc *Scope) context.Context {
	if sc == nil {
		return ctx
	}
	return context.WithValue(ctx, scCtxKey{}, sc)
}

// ScopeFrom 返回 ctx 中的作用域句柄（未注入 ⇒ nil）。
func ScopeFrom(ctx context.Context) *Scope {
	if ctx == nil {
		return nil
	}
	sc, _ := ctx.Value(scCtxKey{}).(*Scope)
	return sc
}

// WithService 把审计服务注入 ctx（供下游 span/事件取用；nil 服务 ⇒ 原样返回）。
func WithService(ctx context.Context, s *Service) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, svcCtxKey{}, s)
}

// ServiceFrom 返回 ctx 中的审计服务（未注入 ⇒ nil；调用方须 nil-safe —— 链式安全）。
func ServiceFrom(ctx context.Context) *Service {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(svcCtxKey{}).(*Service)
	return s
}

// BeginSpan 在 ctx 上开始一个过程步骤：**nil-safe**（无服务/能力关闭 ⇒ 返回 nil，
// End/Fail 均为 no-op）—— 与旧 `From(ctx).Begin(step)` 的链式安全语义等效。
func BeginSpan(ctx context.Context, step string, kv ...any) *Step {
	s := ServiceFrom(ctx)
	if s == nil || s.eff.caps.disabled() || !s.eff.caps.Spans {
		return nil
	}
	return s.Begin(ctx, step, kv...)
}

// Log 写一条通用事件行（等价旧 Logger.Log(Type, Level, step, kv...)；kv 键值映射到 Entry）。
func (s *Service) Log(ctx context.Context, t Type, lvl Level, step string, kv ...any) error {
	if s == nil {
		return nil
	}
	e := Entry{Kind: KindEvent, Type: t, Level: lvl, Step: step}
	applyEntryKV(&e, kv)
	return s.write(ctx, e)
}

// Scope 是作用域句柄（**唯一可写入口**：Emit 定义在它上面）。
type Scope struct {
	svc *Service
	id  ScopeID
}

// ID 返回作用域标识。
func (sc *Scope) ID() ScopeID { return sc.id }

// Open 打开（或复用）作用域。scopeID 必须为合法段名形态（复用 legacy 白名单）。
func (s *Service) Open(ctx context.Context, id ScopeID, _ ...any) (*Scope, error) {
	if !validScopeID(id) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidScope, id)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	if _, ok := s.scopes[id]; !ok {
		s.scopes[id] = &scopeRuntime{kv: make(map[string]any)}
	}
	s.mu.Unlock()
	return &Scope{svc: s, id: id}, nil
}

// Emit 写一条合规/过程条目（**作用域必填：接收者即作用域**）。
func (sc *Scope) Emit(ctx context.Context, e Entry) error {
	if sc == nil || sc.svc == nil {
		return errors.New("audit: nil scope")
	}
	e.Scope = sc.id
	return sc.svc.write(ctx, e)
}

// Emit 是 Service 上的便捷入口（等价于 Open 后 Emit；用于无显式作用域句柄的调用方）。
func (s *Service) Emit(ctx context.Context, e Entry) error {
	if e.Scope == "" {
		return fmt.Errorf("%w: 空作用域（Emit 必须在作用域上进行）", ErrInvalidScope)
	}
	return s.write(ctx, e)
}

// Begin 开始一个步骤（过程审计；CapabilitySet.Spans 关闭时返回 no-op 步骤）。
// 注意：**不在 Step 里保存 ctx**（containedctx 门禁）⇒ Begin 时即把 ctx 身份字段解析进 base。
func (s *Service) Begin(ctx context.Context, step string, kv ...any) *Step {
	base := Entry{}
	s.fillContext(ctx, &base)
	// 作用域来源：ctx 注入的 Scope；未注入 ⇒ **合成作用域** system（设计 E1：后台/周期作业
	// 必须自开合成作用域 ⇒ 绝不出现「无作用域」条目）。
	base.Scope = ScopeID("system")
	if sc := ScopeFrom(ctx); sc != nil {
		base.Scope = sc.ID()
	}
	return &Step{svc: s, step: step, start: s.eff.clock.Now(), kv: kv, base: base}
}

// Step 是一个过程步骤（End/Fail 收口）。
type Step struct {
	svc   *Service
	step  string
	start time.Time
	kv    []any
	base  Entry // Begin 时已解析的身份字段（Actor/Owner/Mesh/RequestID/SourceIP）
	ended bool
}

// End 结束步骤（写一条 KindSpan）。
func (st *Step) End(kv ...any) {
	if st == nil || st.ended {
		return
	}
	st.ended = true
	st.svc.writeStep(st.base, st.step, st.start, "", append(st.kv, kv...)...)
}

// Fail 结束步骤并记录错误（写一条 KindSpan + Err）。
func (st *Step) Fail(err error, kv ...any) {
	if st == nil || st.ended {
		return
	}
	st.ended = true
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	st.svc.writeStep(st.base, st.step, st.start, msg, append(st.kv, kv...)...)
}

// writeStep 写过程步骤（内部统一入口；base 已含身份字段）。
func (s *Service) writeStep(base Entry, step string, start time.Time, errMsg string, kv ...any) {
	if s == nil || s.eff.caps.disabled() || !s.eff.caps.Spans {
		return
	}
	e := base
	e.Kind = stepKind(step)
	e.Type = stepType(step)
	e.Step = step
	e.DurMS = s.eff.clock.Since(start).Milliseconds()
	e.StartMS = e.DurMS
	e.Err = errMsg
	e.Level = LevelInfo
	if errMsg != "" {
		e.Level = LevelError
	}
	applyEntryKV(&e, kv)
	_ = s.write(context.Background(), e)
}

// Save 把作用域状态落盘（WAL：写一条 KindState）并写入内存 kv。
func (sc *Scope) Save(ctx context.Context, key string, v any) error {
	if sc == nil || sc.svc == nil {
		return errors.New("audit: nil scope")
	}
	sc.svc.mu.Lock()
	st := sc.svc.scopes[sc.id]
	if st == nil {
		st = &scopeRuntime{kv: make(map[string]any)}
		sc.svc.scopes[sc.id] = st
	}
	if st.done {
		sc.svc.mu.Unlock()
		return ErrScopeClosed
	}
	st.kv[key] = v
	sc.svc.mu.Unlock()
	if !sc.svc.eff.caps.WAL {
		return nil // 能力关闭：仅内存（显式选择）
	}
	return sc.svc.write(ctx, Entry{
		Scope: sc.id,
		Kind:  KindState,
		Step:  "state",
		Meta:  map[string]any{"key": key, "value": v},
	})
}

// Get 读取作用域状态（内存快照；重放语义见 M2）。
func (sc *Scope) Get(_ context.Context, key string) (any, bool) {
	if sc == nil || sc.svc == nil {
		return nil, false
	}
	sc.svc.mu.Lock()
	defer sc.svc.mu.Unlock()
	st := sc.svc.scopes[sc.id]
	if st == nil {
		return nil, false
	}
	v, ok := st.kv[key]
	return v, ok
}

// MarkDone 写作用域**终态**（提交记录：必须在外部副作用完成之后调用；恢复时据此跳过）。
func (sc *Scope) MarkDone(ctx context.Context) error {
	if sc == nil || sc.svc == nil {
		return errors.New("audit: nil scope")
	}
	sc.svc.mu.Lock()
	st := sc.svc.scopes[sc.id]
	if st == nil {
		st = &scopeRuntime{kv: make(map[string]any)}
		sc.svc.scopes[sc.id] = st
	}
	if st.done {
		sc.svc.mu.Unlock()
		return nil
	}
	st.done = true
	sc.svc.mu.Unlock()
	if !sc.svc.eff.caps.Compliance {
		return nil
	}
	return sc.svc.write(ctx, Entry{Scope: sc.id, Kind: KindDone, Step: "done", Result: ResultSuccess})
}

// Close 结束作用域（不再接受写入）。
func (sc *Scope) Close() error {
	if sc == nil || sc.svc == nil {
		return nil
	}
	sc.svc.mu.Lock()
	defer sc.svc.mu.Unlock()
	if st := sc.svc.scopes[sc.id]; st != nil {
		st.closed = true
	}
	return nil
}

// write 是**唯一写入路径**：能力门 → 补字段 → 落盘 → 索引 → 通知 → 轮转判定。
// 拆成 prepare/persist/observe/maybeRotate 四步（复杂度合规 + 职责单一）。
func (s *Service) write(ctx context.Context, e Entry) error {
	prepared, err := s.prepare(ctx, e)
	if err != nil || prepared == nil {
		return err
	}
	if err := s.persist(*prepared); err != nil {
		return err
	}
	s.observe(ctx, *prepared)
	s.maybeRotate()
	return nil
}

// prepare 分配 seq、补全字段并做能力门；返回 nil 表示「能力未启用 ⇒ 静默跳过」。
func (s *Service) prepare(ctx context.Context, e Entry) (*Entry, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	if st := s.scopes[e.Scope]; st != nil && st.closed && e.Kind != KindDone {
		s.mu.Unlock()
		return nil, ErrScopeClosed
	}
	if !s.eff.caps.enabled(e.Kind) {
		s.mu.Unlock()
		return nil, nil // 能力未启用：静默成功（不阻断业务）
	}
	s.seq[e.Scope]++
	e.Seq = s.seq[e.Scope]
	s.mu.Unlock()

	s.fillContext(ctx, &e)
	if e.TS.IsZero() {
		e.TS = s.eff.clock.Now()
	}
	if e.Node == "" {
		e.Node = s.eff.nodeID
	}
	if e.Level == "" {
		e.Level = LevelInfo
	}
	return &e, nil
}

// persist 落盘（best-effort；fail-closed 时向上传播）。
// E16：带 no_audit 标记的条目（审计/归档自身写入）**不再产生审计** ⇒ 直接跳过。
func (s *Service) persist(e Entry) error {
	if e.isNoAudit() {
		return nil
	}
	if err := s.eff.journal.Append(e); err != nil {
		s.eff.metrics.IncrWriteFailed()
		s.mu.Lock()
		s.unsynced++
		s.mu.Unlock()
		if s.eff.failFast {
			return fmt.Errorf("%w: %v", ErrAuditUnavailable, err)
		}
		if s.eff.logger != nil {
			s.eff.logger.Warn("audit: 落盘失败（best-effort 继续）", "scope", e.Scope, "kind", e.Kind, "err", err)
		}
	}
	return nil
}

// observe 更新内存活跃索引（有界保留）并派发下游通知。
func (s *Service) observe(ctx context.Context, e Entry) {
	if s.eff.caps.Index {
		s.evictIfAtCapacity()
		s.eff.index.Add(e)
		s.enforceMemoryBound()
	}
	if s.eff.caps.Notifier {
		s.notify(ctx, e)
	}
}

// maybeRotate 惰性过时轮转（T2）；KindRotate 自身不触发（防递归 E22）。
func (s *Service) maybeRotate() {
	s.mu.Lock()
	s.dirty++
	need := s.staleLocked()
	s.mu.Unlock()
	if need {
		_ = s.Rotate(context.Background(), "age")
	}
}

// fillContext 用注入的提取器补 Actor/Owner/Mesh/RequestID/SourceIP（**只补空值**）。
func (s *Service) fillContext(ctx context.Context, e *Entry) {
	if ctx == nil {
		return
	}
	if e.Actor == "" && s.eff.ctx.Actor != nil {
		e.Actor = s.eff.ctx.Actor(ctx)
	}
	if e.Owner == "" && s.eff.ctx.Owner != nil {
		e.Owner = s.eff.ctx.Owner(ctx)
	}
	if e.Mesh == "" && s.eff.ctx.Mesh != nil {
		e.Mesh = s.eff.ctx.Mesh(ctx)
	}
	if e.RequestID == "" && s.eff.ctx.RequestID != nil {
		e.RequestID = s.eff.ctx.RequestID(ctx)
	}
	if e.SourceIP == "" && s.eff.ctx.SourceIP != nil {
		e.SourceIP = s.eff.ctx.SourceIP(ctx)
	}
}

// notify 派发通知（recover 兜底 + 计数）。
func (s *Service) notify(ctx context.Context, e Entry) {
	defer func() {
		if r := recover(); r != nil {
			s.eff.metrics.IncrHookPanic()
			if s.eff.logger != nil {
				s.eff.logger.Error("audit: Notifier panic（已隔离）", "scope", e.Scope, "panic", r)
			}
		}
	}()
	s.eff.notifier.Notify(ctx, e)
}

// evictIfAtCapacity 在「索引已达条数上限」时先逐出 1 条（环形覆盖前的显式记账）。
// 为何必须前置：环形索引自身会覆盖最旧 ⇒ `Len()` 永不超上限 ⇒ 事后判定永不触发。
func (s *Service) evictIfAtCapacity() {
	p := s.eff.policy
	if p.MaxMemoryEntries <= 0 || s.eff.index.Len() < p.MaxMemoryEntries {
		return
	}
	s.evictAndAccount(1)
}

// enforceMemoryBound 执行字节上限（T1 的字节维度）：超限则逐出最旧。
func (s *Service) enforceMemoryBound() {
	p := s.eff.policy
	if p.MaxMemoryBytes <= 0 {
		return
	}
	for s.eff.index.ApproxBytes() > p.MaxMemoryBytes {
		if !s.evictAndAccount(1) {
			return
		}
	}
}

// evictAndAccount 逐出 n 条并**区分记账**：未持久化的计入 Drop（真丢失），已持久化的计入 Evict。
// 返回是否确实逐出了条目。
func (s *Service) evictAndAccount(n int) bool {
	evicted := s.eff.index.Evict(n)
	if len(evicted) == 0 {
		return false
	}
	s.mu.Lock()
	drops := min(len(evicted), s.unsynced)
	s.unsynced -= drops
	s.mu.Unlock()
	if drops > 0 {
		s.eff.metrics.IncrDrop(drops)
	}
	if rest := len(evicted) - drops; rest > 0 {
		s.eff.metrics.IncrEvict(rest)
	}
	return true
}

// staleLocked 报告「内容过时」（T2）—— 惰性判定，无后台 goroutine。
func (s *Service) staleLocked() bool {
	if s.eff.policy.MaxAge <= 0 || s.dirty == 0 {
		return false
	}
	return s.eff.clock.Since(s.last) >= s.eff.policy.MaxAge
}

// Query 读取审计（**读取可触发轮转**：T3，人工触发场景；可用 DisableRotateOnRead 关闭）。
// 排序语义：内存索引存在时**最新在前**（与 /api/audit 契约一致）；否则读 Journal（按 (Node,Seq) 升序）。
func (s *Service) Query(ctx context.Context, f EntryFilter) ([]Entry, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	shouldRotate := !s.eff.policy.DisableRotateOnRead && s.dirty > 0 &&
		s.eff.clock.Since(s.last) >= s.eff.policy.MinRotateInterval
	s.mu.Unlock()
	if shouldRotate {
		_ = s.Rotate(ctx, "read")
	}
	if s.eff.caps.Index && s.eff.index.Len() > 0 {
		return s.eff.index.Query(f), nil
	}
	return s.eff.journal.Read(f)
}

// Recent 返回**最新在前**的审计条目（HTTP 读面用）。
// 与 Query 的区别：Recent **不做**「读取触发轮转」（避免读请求反复触发轮转），
// 只读内存活跃索引；无索引时读 Journal 并反转为最新在前。
func (s *Service) Recent(f EntryFilter) []Entry {
	if s == nil {
		return nil
	}
	if s.eff.caps.Index && s.eff.index.Len() > 0 {
		return s.eff.index.Query(f)
	}
	es, err := s.eff.journal.Read(f)
	if err != nil {
		return nil
	}
	for i, j := 0, len(es)-1; i < j; i, j = i+1, j-1 {
		es[i], es[j] = es[j], es[i]
	}
	if f.Limit > 0 && len(es) > f.Limit {
		es = es[:f.Limit]
	}
	return es
}

// Rotate 显式轮转（T4；reason 记入轮转标记与指标）。
func (s *Service) Rotate(ctx context.Context, reason string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	s.dirty = 0
	s.last = s.eff.clock.Now()
	s.mu.Unlock()
	if err := s.eff.journal.Rotate(reason); err != nil {
		s.eff.metrics.IncrWriteFailed()
		return err
	}
	// 审计的审计：写一条轮转标记（**不触发再次轮转** ⇒ 防递归 E22）。
	return s.write(ctx, Entry{Kind: KindRotate, Step: "rotate", Result: ResultSuccess,
		Meta: map[string]any{"reason": reason}})
}

// Prune 按谓词剔行（TTL/清理的唯一入口：内存与磁盘在同一 Journal 事务内完成 —— 设计 E25/M1-13）。
// 调用方负责「只对合规族调用」（任务/WAL 族不受 TTL 约束）。
func (s *Service) Prune(keep func(Entry) bool) error {
	if s == nil {
		return nil
	}
	if err := s.eff.journal.Rewrite(keep); err != nil {
		return err
	}
	if s.eff.caps.Index {
		s.eff.index.Prune(keep)
	}
	return nil
}

// Flush 落盘缓冲（停服前调用）。
func (s *Service) Flush() error {
	if s == nil {
		return nil
	}
	return s.eff.journal.Flush()
}

// Close 关闭服务（幂等）。
func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.eff.journal.Close()
}
