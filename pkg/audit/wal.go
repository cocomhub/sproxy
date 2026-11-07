// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

// wal.go 是 **WAL 恢复原语**（设计 docs/designs/2026-10-19-audit-service.md §6 / M2）。
//
// 状态机与**提交语义**（设计 §6.1，审核 P1）：
//
//	Open(scope) ─▶ active ─(Save 落盘 KindState, Seq++)─▶ active
//	                 ├─(崩溃)─▶ 重启后 ListScopes ⇒ 未完成 ⇒ Resume(ReplayApplier)
//	                 └─(MarkDone)─▶ done（KindDone 落盘 ⇒ 恢复**跳过**）
//
// **契约（必须遵守）**：`MarkDone` 是**副作用完成之后**的提交记录（commit record）。
//   - 正确顺序：**先做业务副作用 → 再 MarkDone**。两步之间崩溃 ⇒ 重启后重放该作用域；
//   - **禁止**先 MarkDone 再做副作用 —— 那会让恢复「跳过未完成的任务」⇒ **真丢任务**；
//   - 因此 `ReplayApplier.Apply` **必须幂等**（同 (scope, Seq) 重复应用零副作用）——
//     幂等是**使用方契约**，库不代为保证（设计 §6 审核修订 P1）。
//
// 两级序（设计 §4.1）：**同一 scope 内 `Seq` 是权威顺序**（Resume 依赖它）；跨 scope 不排序。
//
// 段族边界（设计 E27）：WAL/任务族 Journal **不配置** `MaxTotalBytes` ⇒ 永不丢弃
// （容量不足表现为拒绝新的 Open，绝不丢已有任务状态）。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// ReplayApplier 接收 `KindState` 状态重放（设计 §6.2 / §4.3）。
//
// **幂等是使用方契约**：同一 (scope, Seq) 可能被重复应用（如「副作用完成但 MarkDone 前崩溃」
// 后再次恢复），实现必须保证重复应用零副作用（例如按 Seq 去重、或副作用本身幂等）。
type ReplayApplier interface {
	// Apply 应用一条状态。state 是 `StateRecord` 的 JSON 编码。
	Apply(ctx context.Context, scope ScopeID, seq uint64, state []byte) error
}

// StateRecord 是 `KindState` 条目的载荷（`Scope.Save` 写入 `Meta{key,value}`）。
type StateRecord struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// ScopeState 是 `Resume` 的结果：从 WAL 重建的作用域状态。
type ScopeState struct {
	Scope   ScopeID        // 作用域
	LastSeq uint64         // 该作用域最后一条条目的 Seq（无条目为 0）
	Done    bool           // 已提交（末条为 KindDone ⇒ 恢复时跳过重放）
	States  int            // 重放的 KindState 条数
	LastTS  time.Time      // 末条时间
	KV      map[string]any // 重建后的状态（按 Seq 覆盖语义）
}

// ScopeInfo 是 `ListScopes` 的作用域摘要（扫描段目录得到，无内存依赖 ⇒ 重启可用）。
type ScopeInfo struct {
	Scope   ScopeID   // 作用域
	LastSeq uint64    // 最后 Seq
	LastTS  time.Time // 最后条目时间
	Done    bool      // 是否已提交（末条 KindDone）
	States  int       // KindState 条数
	Summary string    // 摘要（末条 Action/Step，便于运维判断）
}

// ScopeStateFilter 过滤 `ListScopes` 结果。
type ScopeStateFilter struct {
	// UnfinishedOnly 只返回**未完成**（末条非 KindDone）的作用域 —— 恢复协调器的主用入口。
	UnfinishedOnly bool
	// Since 只返回 LastTS 不早于该时刻的作用域（零值不过滤）。
	Since time.Time
	// Limit >0 时最多返回该条数（按 LastTS **倒序**，最近的优先）。
	Limit int
}

// Resume 按 Seq 重放某作用域的全部 `KindState`，返回重建状态（设计 §6.2）。
//
// 语义：
//   - 末条为 `KindDone` ⇒ `Done=true` 且**不调用** ap（作用域已提交 ⇒ 跳过）；
//   - 否则按 **Seq 升序**逐条调用 `ap.Apply`（同 Seq 重复出现只应用一次）；
//   - 完全基于 Journal ⇒ **可在进程重启后调用**（无需任何内存中的运行态）。
func (s *Service) Resume(ctx context.Context, id ScopeID, ap ReplayApplier) (*ScopeState, error) {
	if s == nil {
		return nil, fmt.Errorf("audit: nil service")
	}
	if id == "" {
		return nil, fmt.Errorf("audit: Resume 需要作用域")
	}
	entries, err := s.readScope(id)
	if err != nil {
		return nil, err
	}
	st := &ScopeState{Scope: id, KV: map[string]any{}}
	if len(entries) == 0 {
		return st, nil
	}
	fillScopeTail(st, entries[len(entries)-1])
	if st.Done {
		return st, nil // 提交记录之后 ⇒ 跳过重放（设计 §6.1）
	}
	if rerr := s.replayStates(ctx, id, entries, ap, st); rerr != nil {
		return st, rerr
	}
	return st, nil
}

// fillScopeTail 从末条条目填充作用域尾部信息（LastSeq/LastTS/Done）。
func fillScopeTail(st *ScopeState, last Entry) {
	st.LastSeq = last.Seq
	st.LastTS = last.TS
	st.Done = last.Kind == KindDone
}

// replayStates 按 Seq 升序重放 KindState（同 Seq 去重；apply 失败向上传播）。
func (s *Service) replayStates(ctx context.Context, id ScopeID, entries []Entry, ap ReplayApplier, st *ScopeState) error {
	var applied uint64
	for _, e := range entries {
		next, err := s.replayOne(ctx, id, e, applied, ap, st)
		if err != nil {
			return err
		}
		if next != 0 {
			applied = next
		}
	}
	return nil
}

// replayOne 处理单条：跳过非 KindState、重复 Seq 与无法解析的载荷；成功时返回其 Seq。
// （拆出以把重放的认知复杂度留在门禁阈值内；判定语义与注释见 replayStates。）
func (s *Service) replayOne(ctx context.Context, id ScopeID, e Entry, applied uint64, ap ReplayApplier, st *ScopeState) (uint64, error) {
	if e.Kind != KindState {
		return 0, nil
	}
	if e.Seq != 0 && e.Seq <= applied {
		return 0, nil // 同 Seq 去重（库侧配合使用方的幂等契约）
	}
	rec, ok := stateRecordOf(e)
	if !ok {
		return 0, nil // 无法解析的载荷：跳过（不阻断其它状态）
	}
	raw, merr := json.Marshal(rec)
	if merr != nil {
		return 0, fmt.Errorf("audit: 状态序列化失败: %w", merr)
	}
	if ap != nil {
		if aerr := ap.Apply(ctx, id, e.Seq, raw); aerr != nil {
			return 0, fmt.Errorf("audit: 重放 Seq=%d 失败: %w", e.Seq, aerr)
		}
	}
	st.States++
	kvSet(st.KV, rec.Key, rec.Value)
	return e.Seq, nil
}

// ListScopes 扫描 Journal 得到各作用域摘要（设计 §6.2「恢复原语」）。
// 与 Resume 一样**只依赖 Journal** ⇒ 进程重启后即可用于发现未完成作用域。
func (s *Service) ListScopes(ctx context.Context, f ScopeStateFilter) ([]ScopeInfo, error) {
	if s == nil {
		return nil, fmt.Errorf("audit: nil service")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := s.eff.journal.Read(EntryFilter{})
	if err != nil {
		return nil, err
	}
	by, order := accumulateScopes(entries)
	out := filterScopes(by, order, f)
	// 最近的优先（LastTS 倒序；稳定排序保证同 TS 时输出确定）
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastTS.After(out[j].LastTS) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// accumulateScopes 把条目按作用域聚合（同一作用域取 Seq 最大者为「末条」）。
func accumulateScopes(entries []Entry) (map[ScopeID]ScopeInfo, []ScopeID) {
	by := make(map[ScopeID]ScopeInfo, 16)
	order := make([]ScopeID, 0, 16)
	lastSeq := make(map[ScopeID]uint64, 16)
	for _, e := range entries {
		if e.Scope == "" {
			continue
		}
		info, ok := by[e.Scope]
		if !ok {
			order = append(order, e.Scope)
		}
		if e.Kind == KindState {
			info.States++
		}
		// 以 Seq 取最大（Seq==0 的旧条目按读取顺序累积）
		if !ok || e.Seq >= lastSeq[e.Scope] {
			lastSeq[e.Scope] = e.Seq
			info.LastSeq = e.Seq
			info.LastTS = e.TS
			info.Done = e.Kind == KindDone
			info.Summary = summaryOf(e)
		}
		info.Scope = e.Scope
		by[e.Scope] = info
	}
	return by, order
}

// filterScopes 按过滤条件产出结果（保持首次出现顺序；排序由调用方负责）。
func filterScopes(by map[ScopeID]ScopeInfo, order []ScopeID, f ScopeStateFilter) []ScopeInfo {
	out := make([]ScopeInfo, 0, len(order))
	for _, id := range order {
		info := by[id]
		if f.UnfinishedOnly && info.Done {
			continue
		}
		if !f.Since.IsZero() && info.LastTS.Before(f.Since) {
			continue
		}
		out = append(out, info)
	}
	return out
}

// MarkDone 写作用域**终态提交记录**（Service 级入口，等价于 `(*Scope).MarkDone`）。
//
// **调用顺序契约**：必须在业务副作用**完成之后**调用（先副作用、后 MarkDone）；
// 反序会让恢复跳过未完成任务（设计 §6.1）。
func (s *Service) MarkDone(ctx context.Context, id ScopeID) error {
	if s == nil {
		return fmt.Errorf("audit: nil service")
	}
	if id == "" {
		return fmt.Errorf("audit: MarkDone 需要作用域")
	}
	sc := &Scope{svc: s, id: id}
	return sc.MarkDone(ctx)
}

// readScope 读取某作用域的全部条目并按 **Seq 升序** 排序（同 Seq 保持读取顺序）。
func (s *Service) readScope(id ScopeID) ([]Entry, error) {
	entries, err := s.eff.journal.Read(EntryFilter{Scope: id})
	if err != nil {
		return nil, err
	}
	// Read 的排序语义是「先 Node 后 Seq」；Resume 需要**纯 Seq 序**（scope 内权威），
	// 因此这里显式重排（稳定排序：同 Seq 保持文件顺序）。
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Seq < entries[j].Seq })
	return entries, nil
}

// stateRecordOf 从 KindState 条目的 Meta 还原载荷（Save 写入 {"key","value"}）。
func stateRecordOf(e Entry) (StateRecord, bool) {
	if e.Meta == nil {
		return StateRecord{}, false
	}
	key, _ := e.Meta["key"].(string)
	if key == "" {
		return StateRecord{}, false
	}
	raw, err := json.Marshal(e.Meta["value"])
	if err != nil {
		return StateRecord{}, false
	}
	return StateRecord{Key: key, Value: raw}, true
}

// kvSet 把 JSON 值放回 KV（数字/对象/数组/字符串按解码后的 Go 形态）。
func kvSet(kv map[string]any, key string, raw json.RawMessage) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		v = string(raw)
	}
	kv[key] = v
}

// summaryOf 生成作用域摘要（优先 Action，其次 Step）。
func summaryOf(e Entry) string {
	if e.Action != "" {
		return e.Action
	}
	return e.Step
}
