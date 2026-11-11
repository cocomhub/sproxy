// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server/issues"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// fakeInspector 是审查能力测试替身（同时实现 StorageInspector + TreeInspector）。
// 记录被调用的 rel/depth，便于锁定"扫描走只读入口且深度被钳制"。
type fakeInspector struct {
	trace     *volume.PathTrace
	traceErr  error
	tree      *volume.PathTrace
	treeErr   error
	callsTree int
	lastDepth int
}

func (f *fakeInspector) Trace(_ context.Context, _ string, _ volume.InspectMode) (*volume.PathTrace, error) {
	return f.trace, f.traceErr
}

func (f *fakeInspector) Explain(_ context.Context, _ string, _ volume.InspectMode) (*volume.RealExplanation, error) {
	return nil, nil
}

func (f *fakeInspector) TraceTree(_ context.Context, _ string, depth int, _ volume.InspectMode) (*volume.PathTrace, error) {
	f.callsTree++
	f.lastDepth = depth
	return f.tree, f.treeErr
}

// TestScanVolumesOnce_ReportsNeedsHumanAndRetryable 锁定用户核心要求：
// **启动扫描发现的残留数据必须被登记**，且按"能否自主决策"分流 ——
// `dir_meta_ambiguous` ⇒ needs_human（不自动重试）；`container_not_indexed` ⇒ 可自动重试。
func TestScanVolumesOnce_ReportsNeedsHumanAndRetryable(t *testing.T) {
	t.Parallel()
	c := NewIssueCenter(filepath.Join(t.TempDir(), "issues.json"))
	insp := &fakeInspector{trace: &volume.PathTrace{
		Rel: "", Kind: "dir",
		Anomalies: []volume.Anomaly{
			{Code: volume.AnomalyDirMetaAmbiguous, RealPath: "OypE/abc", Detail: "容器内有 2 个 dir-meta"},
			{Code: volume.AnomalyContainerNotIndexed, RealPath: "OypE/def", Detail: "dir_id 不在索引"},
		},
	}}
	st := c.ScanVolumesOnce(context.Background(), []ScanTarget{{Volume: "baidu", Target: insp}}, 1)
	if st.Volumes != 1 || st.Reported != 2 {
		t.Fatalf("统计不符：%+v", st)
	}

	got := c.Manager().List(issues.ListFilter{Volume: "baidu"})
	if len(got) != 2 {
		t.Fatalf("应登记 2 条，got %d", len(got))
	}
	byKind := map[issues.Kind]issues.Item{}
	for _, it := range got {
		byKind[it.Kind] = it
	}
	amb, ok := byKind[issues.KindDirMetaAmbiguous]
	if !ok || amb.Status != issues.StatusNeedsHuman || amb.Retryable {
		t.Fatalf("dir_meta_ambiguous 应为 needs_human 且不可自动重试：%+v", amb)
	}
	nidx, ok := byKind[issues.KindContainerNotIndexed]
	if !ok || nidx.Status != issues.StatusOpen || !nidx.Retryable {
		t.Fatalf("container_not_indexed 应为 open 且可自动重试：%+v", nidx)
	}
	// Detail 必须面向人工（含异常码、位置、处置建议）
	for _, want := range []string{"dir_meta_ambiguous", "建议：", "不要"} {
		if !containsSub(amb.Detail, want) {
			t.Errorf("Detail 应含 %q，got %q", want, amb.Detail)
		}
	}
}

// TestScanVolumesOnce_IgnoresAdvisoryAnomalies 锁定：**提示类**异常不得登记
// （dedup_shared / upload_in_progress / blob_orphan / truncated 等），否则噪声淹没真实问题。
func TestScanVolumesOnce_IgnoresAdvisoryAnomalies(t *testing.T) {
	t.Parallel()
	c := NewIssueCenter(filepath.Join(t.TempDir(), "issues.json"))
	insp := &fakeInspector{trace: &volume.PathTrace{
		Rel: "x", Kind: "file",
		Anomalies: []volume.Anomaly{
			{Code: volume.AnomalyDedupShared, Detail: "在去重池"},
			{Code: volume.AnomalyUploadInProgress, Detail: "在途上传"},
			{Code: volume.AnomalyBlobOrphan, Detail: "疑似孤儿（GC 复核）"},
		},
	}}
	st := c.ScanVolumesOnce(context.Background(), []ScanTarget{{Volume: "v", Target: insp}}, 1)
	if st.Reported != 0 {
		t.Fatalf("提示类异常不应登记，got %d", st.Reported)
	}
	if n := len(c.Manager().List(issues.ListFilter{})); n != 0 {
		t.Fatalf("不应有事项，got %d", n)
	}
}

// TestScanVolumesOnce_RecursesChildren 锁定：`TraceTree` 的**子节点**也要扫（否则深层残留漏报）。
func TestScanVolumesOnce_RecursesChildren(t *testing.T) {
	t.Parallel()
	c := NewIssueCenter(filepath.Join(t.TempDir(), "issues.json"))
	insp := &fakeInspector{tree: &volume.PathTrace{
		Rel: "", Kind: "dir",
		Children: []volume.PathTrace{{
			Rel: "jav/makers", Kind: "dir",
			Anomalies: []volume.Anomaly{{Code: volume.AnomalyDirMetaAmbiguous, RealPath: "OypE/z"}},
		}},
	}}
	st := c.ScanVolumesOnce(context.Background(), []ScanTarget{{Volume: "baidu", Target: insp}}, 2)
	if insp.callsTree != 1 {
		t.Fatalf("depth>1 且支持时应走 TraceTree，got %d 次", insp.callsTree)
	}
	if st.Reported != 1 || st.Nodes < 2 {
		t.Fatalf("应递归扫到子节点并登记 1 条：%+v", st)
	}
	got := c.Manager().List(issues.ListFilter{})
	if len(got) != 1 || !containsSub(got[0].Path, "jav/makers") {
		t.Fatalf("应记录子节点逻辑路径：%+v", got)
	}
}

// TestScanVolumesOnce_ClampsDepthAndSkipsUnsupported 锁定：
// depth 被钳制到 [1, MaxTreeDepth]；不支持审查能力的卷**跳过而不报错**；审查失败也只记 Skipped。
func TestScanVolumesOnce_ClampsDepthAndSkipsUnsupported(t *testing.T) {
	t.Parallel()
	c := NewIssueCenter(filepath.Join(t.TempDir(), "issues.json"))

	insp := &fakeInspector{tree: &volume.PathTrace{Rel: "", Kind: "dir"}}
	st := c.ScanVolumesOnce(context.Background(), []ScanTarget{{Volume: "v", Target: insp}}, 99)
	if insp.lastDepth != volume.MaxTreeDepth {
		t.Fatalf("depth 应钳制到 %d，got %d", volume.MaxTreeDepth, insp.lastDepth)
	}
	if st.Volumes != 1 {
		t.Fatalf("统计不符：%+v", st)
	}

	// 不支持审查能力 ⇒ skipped，不 panic 不报错
	st2 := c.ScanVolumesOnce(context.Background(), []ScanTarget{{Volume: "v2", Target: struct{}{}}}, 1)
	if st2.Skipped != 1 || st2.Volumes != 0 {
		t.Fatalf("不支持审查的卷应跳过：%+v", st2)
	}

	// 审查失败 ⇒ skipped
	failing := &fakeInspector{traceErr: errors.New("后端不可读")}
	st3 := c.ScanVolumesOnce(context.Background(), []ScanTarget{{Volume: "v3", Target: failing}}, 1)
	if st3.Skipped != 1 {
		t.Fatalf("审查失败应记 skipped：%+v", st3)
	}
}

// TestScanVolumesOnce_IdempotentMerge 锁定：重复扫描同一残留**合并为一条**（累加 Occurrences），
// 否则每次启动/定期扫描都会新增条目，UI 会被重复项淹没。
func TestScanVolumesOnce_IdempotentMerge(t *testing.T) {
	t.Parallel()
	c := NewIssueCenter(filepath.Join(t.TempDir(), "issues.json"))
	insp := &fakeInspector{trace: &volume.PathTrace{
		Rel: "", Kind: "dir",
		Anomalies: []volume.Anomaly{{Code: volume.AnomalyDirMetaAmbiguous, RealPath: "OypE/same"}},
	}}
	targets := []ScanTarget{{Volume: "baidu", Target: insp}}
	_ = c.ScanVolumesOnce(context.Background(), targets, 1)
	_ = c.ScanVolumesOnce(context.Background(), targets, 1)

	got := c.Manager().List(issues.ListFilter{})
	if len(got) != 1 {
		t.Fatalf("重复扫描应合并为 1 条，got %d", len(got))
	}
	if got[0].Occurrences != 2 {
		t.Fatalf("Occurrences 应为 2，got %d", got[0].Occurrences)
	}
}

// TestIssueKindForAnomaly_StableMapping 锁定映射契约（API/诊断复用，避免两处硬编码漂移）。
func TestIssueKindForAnomaly_StableMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code      string
		kind      issues.Kind
		retryable bool
		report    bool
	}{
		{volume.AnomalyDirMetaAmbiguous, issues.KindDirMetaAmbiguous, false, true},
		{volume.AnomalyContainerNotIndexed, issues.KindContainerNotIndexed, true, true},
		{volume.AnomalyParentUnresolved, issues.KindOther, false, true},
		{volume.AnomalyMetaUnreadable, issues.KindOther, false, true},
		{volume.AnomalyBlobOrphan, "", false, false},
		{volume.AnomalyDedupShared, "", false, false},
	}
	for _, tc := range cases {
		k, r, rep := IssueKindForAnomaly(tc.code)
		if k != tc.kind || r != tc.retryable || rep != tc.report {
			t.Errorf("%s ⇒ (%q,%v,%v)，期望 (%q,%v,%v)", tc.code, k, r, rep, tc.kind, tc.retryable, tc.report)
		}
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
