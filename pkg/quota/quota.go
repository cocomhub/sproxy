// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package quota 提供通用配额池（Pool/Scope/Reservation）。
// 子作用域通过路径化叠加挂载，子池操作自动向父链聚合；预留通过 Reservation 句柄对账。
// 本包不引入租户/存储概念，是通用的配额领域模型。
package quota

import (
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cocomhub/sproxy/pkg/waitq"
)

// ErrStorageFull 表示配额超限（子作用域上限或全局兜底上限）。
var ErrStorageFull = errors.New("storage quota exceeded")

// Pool 是配额账本节点（根池，或某个 Scope 的底层账本）。
// 账本记录已确认占用 committed 与预留中 reserved；可用额度 = maxBytes − (committed + reserved)。
type Pool struct {
	// waitMu 是**根等待锁**（仅根池实例有效）：等待侧在 `root.waitq.Queue.AcquireUnder` 的
	// lock 参数（即本锁）下完成「快速尝试 + 入队」，释放/放宽侧在**同一把锁**下变更状态并
	// NotifyAll（withWaitLock）⇒ 唤醒不可能落在「已判定不足 → 尚未入队」的窗口内
	// （独立复审 P2 根因修复 2026-10-18；2026-10-19 迁移到 pkg/waitq 时以 AcquireUnder 的
	// 锁协作形式**保持**该保证 —— 设计 §3.1 I1）。
	// **全局锁序：waitMu → allocMu → node.mu；任何路径不得反向获取**（否则 ABBA 死锁）。
	waitMu sync.Mutex
	// allocMu 串行化**跨层**分配账本更新（EnsureChild 一次调用会改动多级节点的 allocated ⇒
	// 读旧值/记账/写新上限必须整体原子）。锁序：allocMu → mu；不得反向获取。
	allocMu   sync.Mutex
	mu        sync.RWMutex
	maxBytes  int64 // 0 = 不限制
	committed int64
	reserved  int64
	// allocated 是**已分配给子节点的额度**（配额模型 v3 的分配账本）：与 committed/reserved
	// 严格分离——分配是「策略额度」（允许谁用多少），用量是「实际占用」。把分配记成用量会让
	// 「已分配但未使用」的空间被误判为已用。不变式：allocated ≤ 可用上限（maxBytes>0 时）。
	allocated int64
	// reserve 是**运维余量**（2026-10-14 用户裁定）：为 staging/孤儿 blob/索引等**非配额物理
	// 字节**保留，配额（用量 + 分配）只能用到 `maxBytes - reserve` ⇒「绝对禁止磁盘打满」由
	// 预留保证而非事后对账。仅卷根设置；子节点为 0。
	reserve int64
	// queue 是**通用等待队列**（pkg/waitq；仅根池实例有效）：配额不足时排队重试。
	// 收敛自原私有 sync.Cond 实现（设计 docs/designs/2026-10-19-waitq.md §8），
	// 能力不变（严格上界 / 沿父链 / 不可能容纳立即失败 / 超时取消 / 等待轮次可观测）。
	queue *waitq.Queue
	// holderMu/holders 暂存「已预留但尚未被调用方接管」的 Reservation（键 = 请求 holder），
	// 由 waitq 适配器写入、WaitReserve 取回（见 waitq_adapter.go）。
	holderMu sync.Mutex
	holders  map[string]*Reservation
	// waitSeq 生成请求 holder 唯一键。
	waitSeq atomic.Uint64
	// waitRounds 是**进入等待的轮次计数**（仅用于可观测性/测试确定性同步：轮询它即可知道
	// 「等待者确实进入了等待」，避免固定 time.Sleep；与 staging_tracker 的 waitRounds 同型）。
	waitRounds atomic.Int64
	parent     *Pool // 非 nil 时向父链聚合
	children   []*Scope
	path       string // 根池为 ""；子池为对应 Scope 的完整路径
}

// NewPool 创建根配额池。maxBytes<=0 表示不限制。
func NewPool(maxBytes int64) *Pool {
	p := &Pool{maxBytes: maxBytes}
	// 等待队列（根池）：MaxWaiters=0 ⇒ 无界（保持既有能力：此前 sync.Cond 队列亦无界；
	// 有界是可选的后续开关，不在此处默认收紧）。AgeBoost 提供反饿死提升（同级请求仍按
	// 入队序 FIFO ⇒ 与既有行为一致）。
	p.queue = waitq.New(waitq.Options{
		DefaultWait: DefaultWaitTimeout,
		AgeBoost:    DefaultWaitTimeout,
		Metrics:     waitqMetrics{root: p},
	})
	return p
}

// NewPoolWithParent 创建**挂到父池之下**的根池（封装卷配额继承源卷）：
//   - 默认（maxBytes<=0）无自身上限，只受父池约束——即「默认从源卷共享」；
//   - 配了 maxBytes 则同时受「自身上限」与「父池上限」双重约束（reserveUp 沿父链逐层校验）；
//   - 占用经 adjustUp 沿父链聚合（同一字节只在父池计一次——子池 committed 是父池的子集，
//     故子池不再是独立根池：父池不再单独为同一字节记账）。
//
// 约束（与 Pool.Adjust / ReleaseCommitted 同）：父链在装配期一次性确定，之后不可变；
// 调用方不得对同一父池同时用 Scope 与 NewPoolWithParent 表达同一区间（否则双计）。
func NewPoolWithParent(parent *Pool, maxBytes int64) *Pool {
	return &Pool{maxBytes: maxBytes, parent: parent}
}

// Parent 返回父池（根池为 nil）。诊断/装配校验用。
func (p *Pool) Parent() *Pool { return p.parent }

// TryReserve 在池上预留 estimate（独立根池语义，不挂子层；卷容量池等直接入账）。
// 失败返回 ErrStorageFull。返回的 Reservation 对池本身 Commit/Release。
func (p *Pool) TryReserve(estimate int64) (*Reservation, error) {
	amt := nonNeg(estimate)
	if err := p.reserveUp(amt); err != nil {
		return nil, err
	}
	return &Reservation{pool: p, amount: amt}, nil
}

// Scope 返回挂载到根池上的新子作用域（路径化叠加，maxBytes<=0 不限制）。
//
// **与 EnsureChild 同一记账实现**（`setChildLimit`；独立复审 B-P2-4 根因修复，2026-10-18）：
// 此前只建节点**不记分配账** ⇒ 生产环境 `Allocated()/Unallocated()` 为 0/失真、I1 无强制点。
// 超额（Σ 子树上限 > 父可用上限）⇒ **原子失败并返回 nil**（调用方必须判空；fail-closed）。
func (p *Pool) Scope(path string, maxBytes int64) *Scope {
	sc, _ := p.newChildWithLimit(path, maxBytes)
	return sc
}

// newChildWithLimit 建（或复用）path 子节点并把上限设为 maxBytes，**同步分配账本**。
// 超额 ⇒ 回滚新建节点并返回错误（路径语义同 newScope：路径化叠加，joinPath 保留前导 "/"）。
func (p *Pool) newChildWithLimit(path string, maxBytes int64) (*Scope, error) {
	child := p.childWithSegment(lastSegment(path))
	var created []*Pool
	if child == nil {
		child = p.newScope(path, 0)
		created = append(created, child.pool)
	}
	var serr error
	p.withWaitLock(func() { serr = p.setChildLimit(child.pool, maxBytes, created) })
	if serr != nil {
		return nil, serr
	}
	return child, nil
}

// Usage 返回本池聚合占用（含子孙作用域传播）。
func (p *Pool) Usage() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.committed
}

// Reserved 返回本池聚合预留。
// WaitStats 返回该池**根池**等待队列的统计快照（S4「等待可观测」）。
//
// 语义：只有根池持有 waitq 队列（子池共享根池的排队能力）⇒ 这里沿 parent 上溯；
// 未配置队列/nil 池 ⇒ 零值（表示「无等待观测面」，不是「没有等待」的另一种说法 —— 调用方
// 用 Pool 是否存在来判断观测面是否可用）。
func (p *Pool) WaitStats() waitq.Stats {
	if p == nil {
		return waitq.Stats{}
	}
	root := p
	for root.parent != nil {
		root = root.parent
	}
	if root.queue == nil {
		return waitq.Stats{}
	}
	return root.queue.Stats()
}

func (p *Pool) Reserved() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.reserved
}

// MaxBytes 返回本池上限（0 = 不限制）。
// 持读锁读取：SetMaxBytes 锁写时并发读不构成 data race（PUT /api/storage/config 热调
// 上限与并发 MaxBytes 读同时发生时）。
func (p *Pool) MaxBytes() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.maxBytes
}

// SetMaxBytes 运行时调整本池上限（0 = 不限制）。仅影响后续 TryReserve/Reservation 校验，
// 不回溯既有 committed/reserved。
func (p *Pool) SetMaxBytes(n int64) {
	p.withWaitLock(func() {
		p.mu.Lock()
		p.maxBytes = n
		p.mu.Unlock()
	})
}

// UsageByBucket 从本池向下递归收集所有 Scope 的 committed 占用，key 为完整路径。
// 本池自身计入其路径 key（根池计入空路径 ""）。
func (p *Pool) UsageByBucket() map[string]int64 {
	m := make(map[string]int64)
	p.collect(m)
	return m
}

// ReleaseCommittedAndUnmount 释放本池 committed 到父链并摘除本池对应的子 Scope（删封装卷时
// 归还配额给底层池 + 清 children 留桩；C5/M8，2026-10-06）。根池（parent nil）只释放不摘除；
// 已卸载/无占用 → 幂等空操作。释放先于摘除：父池 committed 含本子池已传播的占用，先 release
// 到父链再摘除使父池账本不虚增。
func (p *Pool) ReleaseCommittedAndUnmount() {
	p.withWaitLock(func() {
		if used := p.Usage(); used > 0 {
			p.releaseCommittedUp(used)
		}
		if p.parent == nil {
			return
		}
		(&Scope{pool: p}).Unmount()
	})
}

// Adjust 调整本池已确认占用（reconcile 收敛用，diff 语义）：committed += (next − prev)，
// 可正可负（防下溢归零），不经过 reserved。
// 与 Scope.Adjust 的差异：Scope.Adjust 沿父链向上传播（子桶 diff 聚合到根池）；本方法
// **只调本池自身**（根池无父链）。调用方须保证本池无挂载子层 Scope（或已把子层占用聚合进
// 参数），否则直接校准本池 committed 会与子层 adjustUp 传播产生双计——server 侧每卷容量池
// （pkg/volume/registry.Set 的 pools）为独立根池、T3 未挂子层，reconcile 用它把池收敛到磁盘实际。
func (p *Pool) Adjust(prev, next int64) {
	p.withWaitLock(func() { p.adjustUp(next - prev) })
}

// ReleaseCommitted 释放本池已确认占用 n（文件/版本删除按实际删除字节从 committed 扣减）。
// 语义与 Adjust 同域但面向「已知删除量」：Adjust(prev,next) 传新旧两值差分，本方法直接
// 扣减 n（等价 Adjust(Usage, Usage−n) 的**原子单锁**版本——替换 delete/rmdir/版本删除
// 原先「读 Usage 两次 + Adjust」非原子序列，防并发写路径与释放交错时读-改-写竞态欠校）。
// 只调本池自身（根池无父链），不触碰 reserved，防下溢归零（绝不反负）。n<=0 为空操作。
// 调用方须保证本池无挂载子层 Scope（与 Adjust 同约束——server 侧每卷容量池为独立根池）。
func (p *Pool) ReleaseCommitted(n int64) {
	p.withWaitLock(func() { p.releaseCommittedUp(nonNeg(n)) })
}

// newScope 创建路径为父路径拼接 path 的新子作用域，其底层账本挂到本池之下。
func (p *Pool) newScope(path string, maxBytes int64) *Scope {
	child := &Pool{
		maxBytes: maxBytes,
		parent:   p,
		path:     joinPath(p.path, path),
	}
	s := &Scope{pool: child}
	p.mu.Lock()
	p.children = append(p.children, s)
	p.mu.Unlock()
	return s
}

// Resolve 沿 children 段树按路径段逐级下探，返回**最深匹配**子作用域（http route 式）。
// 段序查找：每个匹配子层继续向下，直到某段无匹配子层为止；返回的节点即最长前缀命中点。
// 若路径上的层未装配（如 /a/b/c 仅装配到 /a/b），返回已命中的最深层（文件仍受该层上限
// 约束、committed 归集到该层）。对返回值调用 TryReserve 时，reserveUp 沿 parent 链逐层
// 各自独立检查 maxBytes——所有层级的配额约束自动完成，调用方无需理会层级。
// segs 为空时返回 p 自身对应的 Scope（供"桶内顶级文件"回落）。
func (p *Pool) Resolve(segs []string) *Scope {
	cur := p
	for _, seg := range segs {
		if seg == "" {
			continue
		}
		child := cur.childWithSegment(seg)
		if child == nil {
			break
		}
		cur = child.pool
	}
	return &Scope{pool: cur}
}

// ResolvePath 把路径按 "/" 拆段后调用 Resolve（便捷版）。
func (p *Pool) ResolvePath(path string) *Scope {
	return p.Resolve(strings.Split(path, "/"))
}

// EnsureScope 拆段逐级下探挂载：已存在的子层复用，不存在的现建（maxBytes 应用于最终层，
// 中间层 0 = 不限制）。返回最终段的 Scope。用于装配期把配置路径（如 user/videos/hd）
// 展开为多层嵌套子作用域。
// EnsureScope 拆段逐级下探挂载到本池下（已存在复用，不存在现建），返回最终段 Scope。
//
// **与 EnsureChild 同一记账实现**（见 Pool.Scope 注释）：末段上限走 `setChildLimit`（同步分配
// 账本 + I1 强制）；超额 ⇒ 原子失败并返回 nil（fail-closed）。
func (p *Pool) EnsureScope(segs []string, maxBytes int64) *Scope {
	sc, _ := p.ensureScopeWithLimit(segs, maxBytes)
	return sc
}

// ensureScopeWithLimit 是 EnsureScope 的实现：逐段下探（中间层 0），末段设上限并记账。
func (p *Pool) ensureScopeWithLimit(segs []string, maxBytes int64) (*Scope, error) {
	cur := p
	var created []*Pool
	descended := false
	for _, seg := range segs {
		if seg == "" {
			continue
		}
		child := cur.childWithSegment(seg)
		if child == nil {
			child = cur.newScope(seg, 0)
			created = append(created, child.pool)
		}
		cur = child.pool
		descended = true
	}
	if !descended {
		return nil, errors.New("quota: EnsureScope 需要非空路径段")
	}
	var serr error
	p.withWaitLock(func() { serr = p.setChildLimit(cur, maxBytes, created) })
	if serr != nil {
		return nil, serr
	}
	return &Scope{pool: cur}, nil
}

// childWithSegment 返回 path 末段恰为 seg 的直接子层 Scope（children 段序查找）。
// p 持有读锁时调用须注意：此处仅遍历快照读取，无并发下的结构性变更。
func (p *Pool) childWithSegment(seg string) *Scope {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, c := range p.children {
		if lastSegment(c.pool.path) == seg {
			return c
		}
	}
	return nil
}

// lastSegment 取路径最后一段（如 "/tenant/a/user/videos" → "videos"）。
func lastSegment(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// collect 以快照方式递归收集各层 committed 到 m。
func (p *Pool) collect(m map[string]int64) {
	p.mu.RLock()
	committed, path, children := p.committed, p.path, p.children
	p.mu.RUnlock()
	m[path] += committed
	for _, c := range children {
		c.pool.collect(m)
	}
}

// reserveUp 沿父链从自身向上预留 n：每层在锁内检查上限并累加，父链失败则回滚自身累加。
func (p *Pool) reserveUp(n int64) error {
	p.mu.Lock()
	if p.maxBytes > 0 && p.committed+p.reserved+n > p.usableLocked() {
		p.mu.Unlock()
		return ErrStorageFull
	}
	p.reserved += n
	p.mu.Unlock()
	if p.parent != nil {
		if err := p.parent.reserveUp(n); err != nil {
			p.mu.Lock()
			p.reserved -= n
			p.mu.Unlock()
			return err
		}
	}
	return nil
}

// commitUp 沿父链把预留 amount 对账为实际占用 actual（diff=actual−amount 可正可负）。
//
// 安全前提：amount 恒 ≤ 本层 reserved——由「reserveUp 在同一节点记账 + Reservation.done 的 CAS
// 只允许一次 Commit/Release + QuotaWriter.mu 串行化同一任务的写入」共同保证。
// 此处**刻意不做钳制**：reserved 变负即代表调用方破坏了该前提（而非本层需要兜底），
// 静默 clamp 会掩盖真实缺陷。新增绕过上述三条的 commit 路径前，请先复核该前提。
func (p *Pool) commitUp(amount, actual int64) {
	p.mu.Lock()
	p.reserved -= amount
	p.committed += actual
	p.mu.Unlock()
	if p.parent != nil {
		p.parent.commitUp(amount, actual)
	}
}

// commitReservation 把一笔预留对账为实际占用（评审 R14 C3）。
//
// 为什么需要：`commitUp` 在 actual < amount 时**释放了可用额度**（reserved 减少多于 committed
// 增加）⇒ 必须像其它释放入口一样在根等待锁下变更并广播，否则等待者睡到超时才 507
// （丢唤醒）。actual ≥ amount 时不增加可用额度 ⇒ 走快路（不抢 waitMu，避免写路径被排队影响）。
func (p *Pool) commitReservation(amount, actual int64) {
	if actual >= amount {
		p.commitUp(amount, actual)
		return
	}
	p.withWaitLock(func() { p.commitUp(amount, actual) })
}

// releaseReserve 归还预留（放弃）——统一走根等待锁 + 广播（丢唤醒的唯一收口）。
func (p *Pool) releaseReserve(n int64) {
	p.withWaitLock(func() { p.releaseUp(n) })
}

// releaseUp 沿父链归还预留 n（放弃预留）。
//
// 只向上传播**本层实际归还量**：本层 reserved 不足 n 时（调用方重复归还/重复结算的形态——
// 今日各调用点都有 CAS 或归零守卫，属防御性），本层钳到 0 即止。此前按 n 全额上传会给祖先层
// 制造**负 reserved**，而 available = max − (committed+reserved) ⇒ 负值把祖先可用额度**凭空
// 放大**，可能放行超额写入。
//
// 取舍：调用方的重复归还因此被**静默吸收**（本包是纯领域模型，无 logger 可告警）——换来的是
// 「账本恒不为负」「祖先额度不被放大」两条硬不变量；非正数视为空操作。
func (p *Pool) releaseUp(n int64) {
	if n <= 0 {
		return
	}
	p.mu.Lock()
	actual := min(n, p.reserved)
	p.reserved -= actual
	p.mu.Unlock()
	if p.parent != nil && actual > 0 {
		p.parent.releaseUp(actual)
	}
}

// releaseCommittedUp 沿父链释放已确认占用 n（文件删除按文件大小释放）。
//
// 只向上传播**本层实际扣减量**（与 releaseUp 同口径）：当 n 超过本层 committed（调用方高估
// 本桶份额，或并发窗口里本桶已被别的路径先释放过），本层钳到 0 即止。此前按 n 全额上传会把
// 祖先层中**其它子桶**的占用一并抹掉（破坏「祖先 ≥ 其子树之和」这条不变量），且 reconcile 未必能修回。
func (p *Pool) releaseCommittedUp(n int64) {
	if n <= 0 {
		return
	}
	p.mu.Lock()
	actual := min(n, p.committed)
	p.committed -= actual
	p.mu.Unlock()
	if p.parent != nil && actual > 0 {
		p.parent.releaseCommittedUp(actual)
	}
}

// adjustUp 沿父链传播已确认占用增量 diff（可正可负，防下溢归零）。
//
// 只向上传播**本层实际生效的增量**：负 diff 超过本层 committed 时本层钳到 0，超出部分不再向
// 祖先传播（否则会连带扣掉祖先层里其它子桶的占用，与 releaseCommittedUp 同一条不变量）。
// 正 diff 原样上传（本层与祖先同步增加）；实际增量为 0 时不再触达祖先。
func (p *Pool) adjustUp(diff int64) {
	if diff == 0 {
		return
	}
	p.mu.Lock()
	actual := diff
	if p.committed+actual < 0 {
		actual = -p.committed
	}
	p.committed += actual
	p.mu.Unlock()
	if p.parent != nil && actual != 0 {
		p.parent.adjustUp(actual)
	}
}

// available 计算可用额度：maxBytes<=0 返回 MaxInt64，否则 maxBytes−(committed+reserved)，负值归 0。
func (p *Pool) available() int64 {
	p.mu.RLock()
	maxBytes, used := p.maxBytes, p.committed+p.reserved
	p.mu.RUnlock()
	if maxBytes <= 0 {
		return math.MaxInt64
	}
	usable := p.usable()
	if used >= usable {
		return 0
	}
	return usable - used
}

// usableLocked / usable 返回**可用上限**（= maxBytes − 运维余量）。语义：配额（用量 + 分配）只能
// 用到可用上限；余量留给非配额物理字节（staging/孤儿/索引），从而「磁盘绝不打满」。
func (p *Pool) usableLocked() int64 {
	if p.maxBytes <= 0 {
		return 0
	}
	u := max(p.maxBytes-p.reserve, 0)
	return u
}

// Usable 返回可用上限（卷容量 − 运维余量；maxBytes<=0 ⇒ 0 表示不限制/不适用）。
func (p *Pool) Usable() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.usableLocked()
}

func (p *Pool) usable() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.usableLocked()
}

// SetOperationalReserve 设置**运维余量**（非配额物理字节保留；<0 视为 0）。只应在卷根池设置。
func (p *Pool) SetOperationalReserve(n int64) {
	if n < 0 {
		n = 0
	}
	p.withWaitLock(func() {
		p.mu.Lock()
		p.reserve = n
		p.mu.Unlock()
	})
}

// OperationalReserve 返回运维余量。
func (p *Pool) OperationalReserve() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.reserve
}

// rootPool 返回根池（parent 构造后不可变 ⇒ 无需加锁读取）。
func (p *Pool) rootPool() *Pool {
	cur := p
	for cur.parent != nil {
		cur = cur.parent
	}
	return cur
}

// withWaitLock 在**根等待锁**下执行 fn（fn 内部做状态变更 + 广播）。**所有释放/放宽可用额度的
// 公开入口必须走它**：与等待侧的 double-check/Wait 互斥 ⇒ 结构上消除丢唤醒（见 waitMu 注释）。
// 锁序：waitMu → allocMu → node.mu（fn 内只可向内获取，不得反向）。
func (p *Pool) withWaitLock(fn func()) {
	if p == nil {
		return
	}
	root := p.rootPool()
	root.waitMu.Lock()
	fn()
	root.waitMu.Unlock()
	// **释放侧唤醒的唯一收口**（设计 §3.1 I1）。两点要点：
	//  1. 唤醒在 **fn() 之后**：此时状态变更已完成 ⇒ 不会漏掉「刚入队的等待者」；
	//  2. 唤醒在 **释放 waitMu 之后**：fn() 内部可能持有 node.mu，而队列的
	//     「额度是否够」判据要读账本（node.mu）⇒ 持锁唤醒会**自死锁**（RWMutex 不可重入）。
	// 丢唤醒窗口仍闭合：等待侧的「查 + 入队」在 waitMu 内原子于本处的状态变更 ⇒
	// 等待者要么在状态变更前查到容量（不入队），要么已入队（本次唤醒必达）。
	if root.queue != nil {
		root.queue.NotifyAll()
	}
}

// joinPath 拼接父子路径，处理空段与多余分隔符。
func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	if child == "" {
		return parent
	}
	return strings.TrimRight(parent, "/") + "/" + strings.TrimLeft(child, "/")
}

// Scope 是配额子作用域（路径化叠加）。持有底层账本 *Pool，子池操作自动向父链聚合。
type Scope struct {
	pool *Pool
}

// Mount 返回该作用域下的子作用域（路径继续叠加；maxBytes 省略时 0 = 不限制）。
// 命名避开与类型同名（Scope.Scope 阅读歧义），语义即"挂载子作用域"。
// Mount 在本作用域下挂载路径化子 Scope（maxBytes 省略/0 = 不限制）。
//
// **与 EnsureChild 同一记账实现**（见 Pool.Scope 注释）：超额 ⇒ 原子失败并返回 nil（fail-closed）。
func (s *Scope) Mount(path string, maxBytes ...int64) *Scope {
	var mb int64
	if len(maxBytes) > 0 {
		mb = maxBytes[0]
	}
	sc, _ := s.pool.newChildWithLimit(path, mb)
	return sc
}

// Resolve 沿本作用域 children 段树按路径段逐级下探（http route 式），返回最深匹配子作用域。
// 等价于 Pool.Resolve 在本层进行；segs 为空时返回自身。
func (s *Scope) Resolve(segs []string) *Scope {
	return s.pool.Resolve(segs)
}

// ResolvePath 便捷版：把路径按 "/" 拆段后 Resolve。
func (s *Scope) ResolvePath(path string) *Scope {
	return s.Resolve(strings.Split(path, "/"))
}

// EnsureScope 拆段逐级下探挂载到本作用域下（已存在复用，不存在现建），返回最终段 Scope。
func (s *Scope) EnsureScope(segs []string, maxBytes int64) *Scope {
	return s.pool.EnsureScope(segs, maxBytes)
}

// TryReserve 预留 estimate（仅增加 reserved，不落 committed）。失败返回 ErrStorageFull。
func (s *Scope) TryReserve(estimate int64) (*Reservation, error) {
	amt := nonNeg(estimate)
	if err := s.pool.reserveUp(amt); err != nil {
		return nil, err
	}
	return &Reservation{pool: s.pool, amount: amt}, nil
}

// ReleaseUsage 释放已确认占用 n（文件删除时按文件大小释放）。
//
// 超额释放只扣**本层实际持有量**：n 超过本层 committed 时本层归零，且只向上传播本层实际生效的
// 增量——祖先层中其它子桶的占用不会被连带扣减（详见 releaseCommittedUp）。非正数视为空操作。
func (s *Scope) ReleaseUsage(n int64) {
	s.pool.withWaitLock(func() { s.pool.releaseCommittedUp(nonNeg(n)) })
}

// Adjust 调整已确认占用（覆盖写同文件尺寸变化场景，diff 语义）。
// committed += (next − prev)，diff 可正可负，沿父链同步；不经过 reserved。
// 多文件桶下仅修正被覆盖文件的尺寸变化，不丢弃其它文件占用。
//
// 负向传播同样按**本层实际生效的增量**：降幅超过本层 committed 时本层归零，只向上传播本层
// 实际扣掉的量，不连带扣减祖先层中其它子桶的占用（详见 releaseCommittedUp）。
func (s *Scope) Adjust(prev, next int64) {
	s.pool.withWaitLock(func() { s.pool.adjustUp(next - prev) })
}

// Pool 返回该作用域的底层配额账本（引用场景：把子作用域当作 *Pool 上交上层统一入账——
// 嵌套封装配额委托（方案B）经它把封装卷容量 Pool 挂到底层卷池的子 Scope，Set.Pool(wrapper)
// 返回该底池让消费方直接 TryReserve/Adjust/释放，父链聚合天然保障整体不超底层配额）。
func (s *Scope) Pool() *Pool { return s.pool }

// Unmount 从父池 children 摘除本作用域（删封装卷时清「子 Scope 留桩」，M8，2026-10-06）。
// 摘除后本 Scope 不再被父池 Resolve/UsageByBucket 看到；其自身账本独立存在（committed 残留
// 由调用方先归零——见 Set.ClearDelegatedPool：先释放 committed 到父链再摘除）。根池自身
// （parent nil）或已卸载 → 空操作（幂等）。并发安全：父池 children 锁内摘除。
func (s *Scope) Unmount() {
	p := s.pool
	if p.parent == nil {
		return
	}
	parent := p.parent
	parent.mu.Lock()
	defer parent.mu.Unlock()
	for i, c := range parent.children {
		if c.pool == p {
			parent.children = append(parent.children[:i], parent.children[i+1:]...)
			return
		}
	}
}

// Usage 返回已确认占用。
func (s *Scope) Usage() int64 { return s.pool.Usage() }

// Reserved 返回预留中。
func (s *Scope) Reserved() int64 { return s.pool.Reserved() }

// MaxBytes 返回上限（0 = 不限制）。
func (s *Scope) MaxBytes() int64 { return s.pool.MaxBytes() }

// Available 返回可用额度 = max − (committed+reserved)；max<=0 时返回 MaxInt64。
func (s *Scope) Available() int64 { return s.pool.available() }

// UsageByBucket 从该作用域向下按路径归集 committed 占用。
func (s *Scope) UsageByBucket() map[string]int64 {
	m := make(map[string]int64)
	s.pool.collect(m)
	return m
}

// Reservation 是预留句柄。Commit(actual) 按实际对账；Release() 放弃预留。
// 保证 Commit/Release 至多生效一次（重复调用忽略）。
// pool 是预留所在的底层账本节点（Scope 预留 = Scope.pool；Pool 预留 = 池自身）。
type Reservation struct {
	pool   *Pool
	amount int64
	done   atomic.Bool
}

// Commit 按实际落地大小对账：预留 amount → 实际 actual（diff=actual−amount 同步父链）。
func (r *Reservation) Commit(actual int64) {
	if !r.done.CompareAndSwap(false, true) {
		return
	}
	r.pool.commitReservation(r.amount, nonNeg(actual))
}

// Release 放弃预留（归还 reserved）。
func (r *Reservation) Release() {
	if !r.done.CompareAndSwap(false, true) {
		return
	}
	r.pool.withWaitLock(func() { r.pool.releaseUp(r.amount) })
}

// nonNeg 把负数归零（estimate/actual/释放量不允许为负）。
func nonNeg(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}
