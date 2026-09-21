// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

// TestQualityOf_ScoreBounds 验证质量分数边界：无历史中性、健康 1.0、劣化递减。
func TestQualityOf_ScoreBounds(t *testing.T) {
	// sproxy:serial: 包级质量注册表全局状态（qualityRegistry 注入/清理），不可并行
	qualityRegistryClear()

	// 无历史：中性（不歧视首次候选）。
	if s := QualityOf("ghost-transport"); s.Score != qualityNeutralScore {
		t.Fatalf("无历史候选 Score = %v, want 中性 %v", s.Score, qualityNeutralScore)
	}
	if s := QualityOf("ghost-transport"); s.RetransmitRate != 0 {
		t.Fatalf("无历史候选 RetransmitRate = %v, want 0", s.RetransmitRate)
	}

	// 健康实例：零重传 → Score 1.0。
	RegisterQualitySource("healthy", &fakeQualityMux{mm: mux.Metrics{}})
	defer qualityRegistryDelete("healthy")
	if s := QualityOf("healthy"); s.Score != 1.0 {
		t.Fatalf("零重传候选 Score = %v, want 1.0", s.Score)
	}

	// 劣化实例：重传率高 → Score 下降（< 1.0）。
	RegisterQualitySource("bad", &fakeQualityMux{mm: *muxCount(50)})
	defer qualityRegistryDelete("bad")
	s := QualityOf("bad")
	if s.Score >= 1.0 {
		t.Fatalf("高重传候选 Score = %v, want < 1.0（重传率应拉低分数）", s.Score)
	}
	if s.Score <= 0 {
		t.Fatalf("高重传候选 Score = %v, want > 0（重传可自愈，不应归零）", s.Score)
	}
}

// TestQualityRouting_PicksBetterQuality 验证加权生效：同 Priority 两候选
// （**快但高重传** vs **慢但零重传**）——默认关时快者胜（纯延迟零回归）；
// 开启 QualityRouting 时健康慢者胜（质量加权覆盖纯延迟）。
func TestQualityRouting_PicksBetterQuality(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突 + 质量注册表全局状态
	qualityRegistryClear()
	fastBad := &fakePath{name: "qb", kind: "qb-kind", delay: 30 * time.Millisecond, priority: 100, enabled: true}
	slowGood := &fakePath{name: "qa", kind: "qa-kind", delay: 50 * time.Millisecond, priority: 100, enabled: true}
	smartWithProviders(t, fastBad, slowGood)
	smartCacheClear()
	RegisterQualitySource("qb", &fakeQualityMux{mm: *muxCount(999)}) // 快但劣化（高重传）
	RegisterQualitySource("qa", &fakeQualityMux{mm: mux.Metrics{}})  // 慢但健康（零重传）
	defer qualityRegistryDelete("qb")
	defer qualityRegistryDelete("qa")

	// 默认关（QualityRouting=false）：纯延迟竞速 → 快劣化候选 qb 胜（零回归）。
	res, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "local", DialOptions{})
	if err != nil {
		t.Fatalf("DialSmart err: %v", err)
	}
	if res.Kind != "qb-kind" {
		t.Fatalf("默认关应选快候选 qb（30ms）, got %s（延迟竞速被意外改变）", res.Kind)
	}

	// 开启 QualityRouting：质量加权 → 健康慢候选 qa 胜（劣化 qb 延迟启动被降权）。
	smartCacheClear() // 清缓存重新竞速
	so := SmartOptions{QualityRouting: true}
	res2, err := DialSmartWithOptions(context.Background(), nil, nil,
		&client.MeshService{Node: "n", Addr: "a:1"}, "local", DialOptions{}, so)
	if err != nil {
		t.Fatalf("DialSmartWithOptions err: %v", err)
	}
	if res2.Kind != "qa-kind" {
		t.Fatalf("开启加权应选健康候选 qa（50ms 慢但零重传）, got %s（质量加权未生效或快劣化候选 qb 抢先）", res2.Kind)
	}
}

// TestQualityRouting_NeutralWhenNoHistory 验证无历史候选不歧视（开启加权也正常竞速）。
func TestQualityRouting_NeutralWhenNoHistory(t *testing.T) {
	// sproxy:serial: 全局注册表注入冲突
	qualityRegistryClear()
	a := &fakePath{name: "qa", kind: "qa-kind", delay: 30 * time.Millisecond, priority: 100, enabled: true}
	b := &fakePath{name: "qb", kind: "qb-kind", delay: 30 * time.Millisecond, priority: 100, enabled: true}
	smartWithProviders(t, a, b)
	smartCacheClear()
	// 两个候选都无历史 → 开启加权也不偏袒（保持原竞速，先完成者胜）。
	so := SmartOptions{QualityRouting: true}
	res, err := DialSmartWithOptions(context.Background(), nil, nil,
		&client.MeshService{Node: "n", Addr: "a:1"}, "local", DialOptions{}, so)
	if err != nil {
		t.Fatalf("DialSmartWithOptions err: %v", err)
	}
	if res.Kind != "qa-kind" && res.Kind != "qb-kind" {
		t.Fatalf("无历史候选应正常竞速（任一胜）, got %s", res.Kind)
	}
}

// ---- 测试辅助 ----

// fakeQualityMux 实现 muxQualitySource（测试注入质量源，避免起真 mux）。
type fakeQualityMux struct {
	mm mux.Metrics
}

func (f *fakeQualityMux) QualityMetrics() *mux.Metrics { return &f.mm }

// muxCount 构造带指定重传数的 Metrics 简写（保持测试行短）。
func muxCount(n int64) *mux.Metrics {
	m := &mux.Metrics{}
	m.Retransmits.Store(n)
	return m
}
