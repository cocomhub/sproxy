// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

// muxQualitySource 是质量数据源（由 mux 实例/聚合层实现）。
// 接口化而非直接依赖 mux.Mux：测试注入 fake、装配层可接聚合（跨 mux 汇总）。
type muxQualitySource interface {
	// QualityMetrics 返回该源的 mux 质量指标（重传等计数器）。
	// 返回 nil 表示无数据（该源尚未有统计）。
	QualityMetrics() *mux.Metrics
}

// QualitySnapshot 是单个传输候选的质量快照（QualityOf 返回）。
type QualitySnapshot struct {
	// RetransmitRate 是重传率（重传成功次数 / 发送帧数，0-1；无数据为 0）。
	RetransmitRate float64
	// Latency 是最近一次建连耗时（无历史为 0）。
	Latency time.Duration
	// Score 是 0-1 质量分数（1 = 最佳；无历史 = qualityNeutralScore 中性，不歧视）。
	Score float64
}

// qualityNeutralScore 是无历史候选的中性分数（0.5）：
// 不歧视首次候选（不因无历史而降权，也不虚高），开启加权时与有历史候选可比。
const qualityNeutralScore = 0.5

// qualityStaggerDelay 是劣化候选的延迟启动时长（QualityRouting 开启时）：
// 远小于 RaceWindow（5s）不误伤慢建连；大于普通候选竞速间隔使健康候选先胜出。
const qualityStaggerDelay = 100 * time.Millisecond

// qualityRegistry 是质量源注册表：候选 ID → 质量源。
// 装配层（pkg/server aggregate 或 mesh 建连处）注册；SmartDial 加权查询。
// 包级全局（与 SmartPathRegistry 同构）——测试需 qualityRegistryClear 隔离。
var qualityRegistry = struct {
	mu sync.RWMutex
	m  map[string]muxQualitySource
}{m: make(map[string]muxQualitySource)}

// RegisterQualitySource 注册候选的质量源（候选 ID = Candidate.ID）。
// 重复注册覆盖（更新质量数据源）；nil 源忽略（防御）。
func RegisterQualitySource(candidateID string, src muxQualitySource) {
	if src == nil {
		return
	}
	qualityRegistry.mu.Lock()
	defer qualityRegistry.mu.Unlock()
	qualityRegistry.m[candidateID] = src
}

// qualityRegistryDelete 删除候选质量源（测试清理/源销毁时）。
func qualityRegistryDelete(candidateID string) {
	qualityRegistry.mu.Lock()
	defer qualityRegistry.mu.Unlock()
	delete(qualityRegistry.m, candidateID)
}

// qualityRegistryClear 清空质量注册表（仅测试用）。
func qualityRegistryClear() {
	qualityRegistry.mu.Lock()
	defer qualityRegistry.mu.Unlock()
	qualityRegistry.m = make(map[string]muxQualitySource)
}

// QualityOf 查询候选的历史质量快照。
// 无注册源/无指标 → 返回中性快照（Score=qualityNeutralScore，不歧视首次候选）。
func QualityOf(candidateID string) QualitySnapshot {
	qualityRegistry.mu.RLock()
	src, ok := qualityRegistry.m[candidateID]
	qualityRegistry.mu.RUnlock()
	if !ok || src == nil {
		return QualitySnapshot{Score: qualityNeutralScore}
	}
	mm := src.QualityMetrics()
	if mm == nil {
		return QualitySnapshot{Score: qualityNeutralScore}
	}
	// 重传率：重传成功次数 /（发送帧数 + 重传次数）——重传也是实际发送（失败后重发），
	// 分母用总传输帧数保证 0-1 归一；分母为 0（无任何传输） = 健康零重传。
	sent := mm.FramesSent.Load()
	rt := mm.Retransmits.Load()
	total := sent + rt
	var rate float64
	if total > 0 {
		rate = float64(rt) / float64(total)
		if rate > 1 {
			rate = 1 // 钳制（防御异常计数）
		}
	}
	// 分数：1 - 重传率，映射到 (0,1]（重传可自愈不归零；高重传趋近 0）。
	// 用 (1-rate)*0.5 + 0.5：rate=0 → 1.0；rate=1 → 0.5（比无历史中性略低，仍可竞争）；
	// 健康候选始终优于无历史（1.0 > 0.5），劣化候选低于中性（0.5-）被降权。
	score := 1.0 - rate*0.5
	if score < 0 {
		score = 0
	}
	return QualitySnapshot{
		RetransmitRate: rate,
		Score:          score,
	}
}

// qualitySortKey 把质量快照转成排序键（SmartDial 加权预排序用）：
// 有历史候选按 Score 降序（健康优先）；无历史（Score=中性）与健康同权放行。
// 返回 (hasHistory, score)：hasHistory=true 且 score 高者先启动。
func qualitySortKey(candidateID string) (hasHistory bool, score float64) {
	s := QualityOf(candidateID)
	return s.Score != qualityNeutralScore, s.Score
}

// logQualityWeighting 输出选路加权结果（可观测：哪些候选被质量降权）。
// 开启 QualityRouting 且候选数 >1 时调用；日志含每个候选的质量分。
func logQualityWeighting(cands []Candidate, enabled bool) {
	if !enabled || len(cands) < 2 {
		return
	}
	attrs := make([]any, 0, len(cands)*2)
	for _, c := range cands {
		s := QualityOf(c.ID)
		attrs = append(attrs, c.ID, fmt.Sprintf("%.2f", s.Score))
	}
	slog.Debug("smart dial 质量加权选路", attrs...)
}
