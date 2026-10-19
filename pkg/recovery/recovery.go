// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package recovery 是**统一任务恢复协调器**（设计 docs/designs/2026-10-19-audit-service.md §6.3 +
// docs/designs/2026-10-19-audit-wal-program.md M3；用户裁定见
// docs/archive/architecture-task-recovery.md）。
//
// 定位：
//   - `pkg/audit` 提供**注册与重放原语**（`ListScopes` / `Resume` / `MarkDone`）；
//   - 本包在其上建**协调器 + 注册中心**：启动时统一扫描「未完成作用域」并按任务族分派重放，
//     结束后写**提交记录**（`MarkDone`）——**长任务不再各自扫自己的目录**。
//
// 契约（与 pkg/audit §6.1 一致，不可违反）：
//   - 作用域命名：长任务 `Open` 作用域时必须使用 `<Prefix>.<id>`（如 `task.cloud:<taskID>`）；
//   - `MarkDone` = **副作用完成之后**的提交记录：协调器在 `Resume`（= 执行副作用）成功**之后**
//     才调用 `MarkDone`；失败 ⇒ **保持未完成** ⇒ 下次启动可重试（绝不丢任务）；
//   - `Applier` **必须幂等**（同 (scope, Seq) 重复应用零副作用）—— 幂等是**使用方契约**；
//   - 未注册前缀的作用域**不静默丢弃**：计入报告 `Unknown` 且不 `MarkDone`（可观测、可补注册）。
//
// 两种恢复形态（**按 Kind 互斥**，同一任务族只能选一种 ⇒ 绝不一次恢复两遍）：
//   - 作用域恢复（新）：`Register(Registration{Prefix, Factory})` —— 任务写 WAL 作用域，协调器重放；
//   - legacy 兜底（既有子系统）：`RegisterLegacy(Legacy{Kind, Recover})` —— 子系统自有持久化，
//     只把**启动恢复的触发点**收敛到协调器（首版保留，迁移完成后删除注册）。
//
// 本包只依赖 `pkg/audit`（不依赖 cloud/syncexec 等业务包）⇒ 由装配层（pkg/server）注册各任务族，
// 避免包间循环依赖。
package recovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// ErrDuplicatePrefix 表示同一作用域前缀被重复注册（配置错误，必须启动期暴露）。
var ErrDuplicatePrefix = errors.New("recovery: 作用域前缀重复注册")

// ApplierFactory 为某作用域构造**幂等** ReplayApplier。
// 返回 false 表示该作用域不适配本任务族（跳过，不算失败）。
type ApplierFactory func(scope audit.ScopeID) (audit.ReplayApplier, bool)

// Registration 描述一个可恢复任务族。
type Registration struct {
	// Kind 是任务族名（便于运维/门禁，如 "cloud" / "syncexec" / "chunked" / "transfer"）。
	Kind string
	// Prefix 是作用域前缀（如 "task.cloud"）；匹配 `prefix + "."` 开头的作用域。
	Prefix string
	// Factory 为该作用域构造幂等 Applier。
	Factory ApplierFactory
}

// Legacy 是**既有子系统**的恢复入口（设计 M3「首版保留 legacy 兜底」）。
//
// 适用：子系统自有持久化（不写 audit 作用域），如 cloud 下载的任务台账、分块上传的 staging、
// syncexec 的冲突记录。收敛目标是**启动恢复只有一个编排点**（Coordinator.Run），而不是让每个
// 子系统在各自构造函数里自行扫描目录。
//
// 迁移路径：某子系统改为「作用域 + ReplayApplier」后，**删除**其 Legacy 注册（RegisterLegacy
// 与 Register 按 Kind 互斥 ⇒ 二者不会同时存在，避免一次恢复跑两遍）。
type Legacy struct {
	// Kind 是任务族名（与 Registration.Kind 同域，二者**互斥**）。
	Kind string
	// Recover 执行一次恢复，返回恢复的任务数（实现需**幂等**：重复调用不重复执行业务副作用）。
	Recover func(ctx context.Context) (int, error)
}

// LegacyResult 是单个 legacy 恢复入口的结果。
type LegacyResult struct {
	Kind      string
	Recovered int
	Err       error
}

// Options 控制一次协调运行。
type Options struct {
	// Limit >0 时本次最多处理该数量的未完成作用域（分批启动，避免同时压垮下游）。
	Limit int
	// Since 只处理 LastTS 不早于该时刻的作用域（零值不过滤）。
	Since time.Time
	// FailFast 首个失败即中止（默认 false：一个任务坏掉不得拖住其它任务）。
	FailFast bool
}

// ScopeResult 是单个作用域的处理结果。
type ScopeResult struct {
	Scope   audit.ScopeID
	Kind    string
	Resumed bool  // 是否执行了重放并写了提交记录
	States  int   // 重放的 KindState 条数
	Unknown bool  // 无匹配注册（未处理，**未**写提交记录）
	Err     error // 处理错误（作用域保持未完成）
}

// Report 是一次协调运行的汇总（可观测：日志/监控/CLI 共用）。
type Report struct {
	Total   int           // 扫描到的未完成作用域数
	Resumed int           // 成功重放并提交的数量
	Skipped int           // 无需处理（已提交/未注册/工厂返回 false）
	Failed  int           // 失败（保持未完成，可重试）
	Unknown int           // 未注册前缀的数量（⚠ 需补注册）
	Results []ScopeResult // 逐个作用域结果

	// LegacyRecovered 是 legacy 入口恢复的任务总数；Legacy 是逐个入口结果。
	LegacyRecovered int
	Legacy          []LegacyResult
}

// Err 返回首个错误（用于调用方决定是否告警）。
func (r *Report) Err() error {
	for _, lg := range r.Legacy {
		if lg.Err != nil {
			return lg.Err
		}
	}
	for _, res := range r.Results {
		if res.Err != nil {
			return res.Err
		}
	}
	return nil
}

// String 生成一行摘要（日志友好）。
func (r *Report) String() string {
	return fmt.Sprintf("recovery: total=%d resumed=%d skipped=%d failed=%d unknown=%d legacy=%d/%d",
		r.Total, r.Resumed, r.Skipped, r.Failed, r.Unknown, r.LegacyRecovered, len(r.Legacy))
}

// Coordinator 统一恢复协调器（**结构体**，无全局态；并发安全）。
type Coordinator struct {
	svc    *audit.Service
	logger *slog.Logger

	mu     sync.RWMutex
	regs   []Registration
	legacy []Legacy
}

// New 构造协调器。svc 为 nil ⇒ Run 返回错误（fail-closed：恢复不可静默失效）。
func New(svc *audit.Service, logger *slog.Logger) *Coordinator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Coordinator{svc: svc, logger: logger}
}

// Register 注册任务族（Kind/Prefix/Factory 必填；前缀唯一）。
func (c *Coordinator) Register(r Registration) error {
	if r.Kind == "" {
		return fmt.Errorf("recovery: Registration.Kind 不能为空")
	}
	r.Prefix = strings.TrimSuffix(strings.TrimSpace(r.Prefix), ".")
	if r.Prefix == "" {
		return fmt.Errorf("recovery: Registration.Prefix 不能为空（kind=%s）", r.Kind)
	}
	if r.Factory == nil {
		return fmt.Errorf("recovery: Registration.Factory 不能为空（kind=%s）", r.Kind)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, old := range c.regs {
		if old.Prefix == r.Prefix {
			return fmt.Errorf("%w: %q（kind=%s 与 %s 冲突）", ErrDuplicatePrefix, r.Prefix, r.Kind, old.Kind)
		}
	}
	for _, lg := range c.legacy {
		if lg.Kind == r.Kind {
			return fmt.Errorf("%w: kind %q 已注册为 legacy 恢复，不能再注册作用域恢复", ErrDuplicatePrefix, r.Kind)
		}
	}
	c.regs = append(c.regs, r)
	return nil
}

// RegisterLegacy 注册既有子系统的恢复入口。
//
// **与作用域注册按 Kind 互斥**（同一任务族只能有一种恢复形态）：否则一次 Run 会把同一批任务
// 恢复两遍（legacy 一遍 + 作用域重放一遍）⇒ 业务副作用重复执行。
func (c *Coordinator) RegisterLegacy(l Legacy) error {
	if l.Kind == "" {
		return fmt.Errorf("recovery: Legacy.Kind 不能为空")
	}
	if l.Recover == nil {
		return fmt.Errorf("recovery: Legacy.Recover 不能为空（kind=%s）", l.Kind)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, old := range c.legacy {
		if old.Kind == l.Kind {
			return fmt.Errorf("%w: legacy kind %q 重复注册", ErrDuplicatePrefix, l.Kind)
		}
	}
	for _, reg := range c.regs {
		if reg.Kind == l.Kind {
			return fmt.Errorf("%w: kind %q 已注册为作用域恢复，不能再注册 legacy", ErrDuplicatePrefix, l.Kind)
		}
	}
	c.legacy = append(c.legacy, l)
	return nil
}

// Registrations 返回注册快照（按前缀长度**降序** ⇒ 最长前缀优先匹配）。
func (c *Coordinator) Registrations() []Registration {
	c.mu.RLock()
	out := make([]Registration, len(c.regs))
	copy(out, c.regs)
	c.mu.RUnlock()
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].Prefix) > len(out[j].Prefix) })
	return out
}

// Run 扫描未完成作用域并逐个恢复（幂等：重复运行对已提交作用域零动作）。
func (c *Coordinator) Run(ctx context.Context, opts Options) (*Report, error) {
	if c == nil || c.svc == nil {
		return nil, fmt.Errorf("recovery: 协调器未配置 audit.Service")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	infos, err := c.svc.ListScopes(ctx, audit.ScopeStateFilter{
		UnfinishedOnly: true,
		Since:          opts.Since,
		Limit:          opts.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("recovery: 扫描未完成作用域失败: %w", err)
	}
	rep := &Report{Total: len(infos), Results: make([]ScopeResult, 0, len(infos))}
	// **顺序**：legacy 子系统先恢复（存储/下载器/会话状态就绪），再做作用域重放（业务副作用）。
	c.runLegacy(ctx, rep)
	if opts.FailFast {
		if err := rep.Err(); err != nil {
			return rep, err
		}
	}
	for _, info := range infos {
		if cerr := ctx.Err(); cerr != nil {
			return rep, cerr
		}
		res := c.recoverOne(ctx, info)
		rep.Results = append(rep.Results, res)
		switch {
		case res.Err != nil:
			rep.Failed++
		case res.Unknown:
			rep.Unknown++
			rep.Skipped++
		case res.Resumed:
			rep.Resumed++
		default:
			rep.Skipped++
		}
		if res.Err != nil && opts.FailFast {
			return rep, res.Err
		}
	}
	c.logReport(rep)
	return rep, nil
}

// runLegacy 依次执行 legacy 恢复入口（**隔离**：一个子系统失败不影响其它子系统与作用域重放）。
func (c *Coordinator) runLegacy(ctx context.Context, rep *Report) {
	c.mu.RLock()
	items := make([]Legacy, len(c.legacy))
	copy(items, c.legacy)
	c.mu.RUnlock()
	for _, l := range items {
		if err := ctx.Err(); err != nil {
			rep.Legacy = append(rep.Legacy, LegacyResult{Kind: l.Kind, Err: err})
			rep.Failed++
			continue
		}
		n, err := safeRecover(ctx, l)
		rep.Legacy = append(rep.Legacy, LegacyResult{Kind: l.Kind, Recovered: n, Err: err})
		if err != nil {
			rep.Failed++
			c.logger.Error("recovery: legacy 恢复失败（其它子系统继续）", "kind", l.Kind, "err", err)
			continue
		}
		rep.LegacyRecovered += n
		c.logger.Info("recovery: legacy 恢复完成", "kind", l.Kind, "recovered", n)
	}
}

// safeRecover 调用 legacy 入口并**兜住 panic**（E21 同类：恢复路径不得把启动打崩）。
func safeRecover(ctx context.Context, l Legacy) (n int, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovery: legacy %s panic: %v", l.Kind, r)
		}
	}()
	return l.Recover(ctx)
}

// recoverOne 处理单个作用域：匹配任务族 ⇒ Resume（执行副作用）⇒ **成功后** MarkDone。
func (c *Coordinator) recoverOne(ctx context.Context, info audit.ScopeInfo) ScopeResult {
	res := ScopeResult{Scope: info.Scope}
	reg, ok := c.match(info.Scope)
	if !ok {
		// 未注册前缀：**不静默丢弃**也不写提交记录（保持未完成，补注册后可恢复）。
		res.Unknown = true
		c.logger.Warn("recovery: 未注册的作用域前缀（保持未完成）", "scope", info.Scope)
		return res
	}
	res.Kind = reg.Kind
	ap, ok := reg.Factory(info.Scope)
	if !ok {
		c.logger.Warn("recovery: 任务族不认领该作用域（保持未完成）", "kind", reg.Kind, "scope", info.Scope)
		return res
	}
	st, err := c.svc.Resume(ctx, info.Scope, ap)
	if err != nil {
		res.Err = fmt.Errorf("恢复 %s（kind=%s）失败: %w", info.Scope, reg.Kind, err)
		c.logger.Error("recovery: 作用域恢复失败（保持未完成，下次重试）",
			"kind", reg.Kind, "scope", info.Scope, "err", err)
		return res
	}
	res.States = st.States
	if st.Done {
		// 竞态：另一实例已提交 ⇒ 本次无需动作。
		return res
	}
	// **提交语义**：副作用（Resume 的重放）已完成 ⇒ 才能写提交记录（设计 §6.1）。
	if merr := c.svc.MarkDone(ctx, info.Scope); merr != nil {
		res.Err = fmt.Errorf("写提交记录 %s（kind=%s）失败: %w", info.Scope, reg.Kind, merr)
		c.logger.Error("recovery: 提交记录写入失败", "kind", reg.Kind, "scope", info.Scope, "err", merr)
		return res
	}
	res.Resumed = true
	c.logger.Info("recovery: 作用域已恢复并提交", "kind", reg.Kind, "scope", info.Scope, "states", st.States)
	return res
}

// match 按**最长前缀**匹配任务族（`task.cloud:x` 优先匹配 `task.cloud` 而非 `task`）。
func (c *Coordinator) match(scope audit.ScopeID) (Registration, bool) {
	s := string(scope)
	for _, reg := range c.Registrations() {
		if s == reg.Prefix || strings.HasPrefix(s, reg.Prefix+".") {
			return reg, true
		}
	}
	return Registration{}, false
}

// logReport 汇总日志（可观测：恢复是启动期关键路径，必须留痕）。
func (c *Coordinator) logReport(rep *Report) {
	if rep.Total == 0 {
		c.logger.Debug("recovery: 无未完成作用域")
		return
	}
	c.logger.Info("recovery: 协调完成", "total", rep.Total, "resumed", rep.Resumed,
		"skipped", rep.Skipped, "failed", rep.Failed, "unknown", rep.Unknown)
	for _, res := range rep.Results {
		if res.Err != nil {
			c.logger.Warn("recovery: 失败明细", "scope", res.Scope, "kind", res.Kind, "err", res.Err)
		}
	}
}
