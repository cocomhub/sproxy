// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/cocomhub/sproxy/pkg/server/issues"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// ScanTarget 是一个待审查的卷（名字 + 可审查对象）。
//
// `Target` 传卷的底层对象（如 `*secretdata.SecretdataFS`）——本层用
// `volume.AssertStorageInspector` 做**能力探测**：不支持审查的卷被跳过（不报错），
// 因为审查是**可选能力**（仓内既有「可选能力 + Assert*」模式）。
type ScanTarget struct {
	Volume string
	Target any
}

// ScanStats 是一次启动扫描的统计（便于日志与 UI 呈现）。
type ScanStats struct {
	Volumes   int // 成功审查的卷数
	Skipped   int // 跳过（不支持审查能力 / 审查失败）的卷数
	Nodes     int // 扫描到的节点数
	Reported  int // 登记（或合并）的事项数
	Truncated bool
}

// ScanVolumesOnce 对给定卷做一次**只读**审查，把发现的存储差异登记为待处理事项。
//
// 为什么需要它（用户裁定 2026-11-10）：「特别是这种**启动时发现残留数据无法自主决策**的情况
// 必须能人工感知到问题并接入处理」。此前这些差异只能在人**主动**执行 `sclient volume explain`
// 时才被发现 ⇒ 长期潜伏。
//
// 语义与边界：
//   - **只读**：只调 `Trace`/`TraceTree`（不读内容、不修复、不删除）；
//   - `depth` 钳制到 `[1, volume.MaxTreeDepth]`（防误用打爆大目录/远端后端）；
//   - **只登记需要人处理或可自动重试的差异**（见 `issueAnomalyKind`）；提示类异常
//     （`dedup_shared`/`upload_in_progress`/`truncated` 等）**不登记**，避免噪声淹没真实问题；
//   - 单卷审查失败只记 `Skipped`，**不中断**其它卷。
func (c *IssueCenter) ScanVolumesOnce(ctx context.Context, targets []ScanTarget, depth int) ScanStats {
	var st ScanStats
	if c == nil || c.manager == nil {
		return st
	}
	if depth < 1 {
		depth = 1
	}
	if depth > volume.MaxTreeDepth {
		depth = volume.MaxTreeDepth
	}
	for _, t := range targets {
		if ctx.Err() != nil {
			return st
		}
		insp, ok := volume.AssertStorageInspector(t.Target)
		if !ok {
			st.Skipped++
			continue
		}
		tr, err := traceVolume(ctx, t.Target, insp, depth)
		if err != nil {
			st.Skipped++
			continue
		}
		st.Volumes++
		reportTrace(c, t.Volume, tr, &st)
	}
	return st
}

// traceVolume 选择 `TraceTree`（depth>1 且卷支持）或单节点 `Trace`。
func traceVolume(ctx context.Context, target any, insp volume.StorageInspector, depth int) (*volume.PathTrace, error) {
	if ti, ok := volume.AssertTreeInspector(target); ok && depth > 1 {
		return ti.TraceTree(ctx, "", depth, volume.InspectIndex)
	}
	return insp.Trace(ctx, "", volume.InspectIndex)
}

// reportTrace 递归遍历落点树，把异常登记为事项。
func reportTrace(c *IssueCenter, volName string, tr *volume.PathTrace, st *ScanStats) {
	if tr == nil {
		return
	}
	st.Nodes++
	for _, a := range tr.Anomalies {
		kind, retryable, report := issueAnomalyKind(a.Code)
		if !report {
			continue
		}
		if a.Code == volume.AnomalyTruncated {
			st.Truncated = true
		}
		loc := strings.TrimSpace(tr.Rel)
		if loc == "" {
			loc = "(卷根)"
		}
		if a.RealPath != "" {
			loc = loc + " [" + a.RealPath + "]"
		}
		c.manager.Report(issues.Item{
			Kind:      kind,
			Volume:    volName,
			Path:      loc,
			Detail:    scanDetail(a, tr),
			Source:    "startup-scan",
			Retryable: retryable,
		})
		st.Reported++
	}
	for i := range tr.Children {
		reportTrace(c, volName, &tr.Children[i], st)
	}
}

// scanDetail 生成面向**人工**的详细说明（含异常码、位置、实例说明与建议处置）。
func scanDetail(a volume.Anomaly, tr *volume.PathTrace) string {
	var b strings.Builder
	fmt.Fprintf(&b, "存储审查发现差异 %s：位置 %q（kind=%s）", a.Code, tr.Rel, tr.Kind)
	if a.RealPath != "" {
		fmt.Fprintf(&b, "，真实路径 %q", a.RealPath)
	}
	if d := strings.TrimSpace(a.Detail); d != "" {
		fmt.Fprintf(&b, "；%s", d)
	}
	if advice := anomalyAdvice(a.Code); advice != "" {
		fmt.Fprintf(&b, "。建议：%s", advice)
	}
	return b.String()
}

// anomalyAdvice 给出每种差异的处置建议（写入事项 Detail，供人工判断）。
func anomalyAdvice(code string) string {
	switch code {
	case volume.AnomalyDirMetaAmbiguous:
		return "容器内有多个目录 meta（改名/移动时旧 meta 删除失败）。引擎按 Version 仲裁可继续" +
			"工作，但布局异常；应先确认哪个 meta 与逻辑路径一致（可用 `sclient volume explain` 核对），" +
			"再经 GC/清理路径删除旧 blob（**不要**直接手工删容器）"
	case volume.AnomalyContainerNotIndexed:
		return "容器存在但索引无对应逻辑目录（索引未重建或外部创建）。可自动重试：重建/刷新索引后复核"
	case volume.AnomalyParentUnresolved:
		return "目录 meta 的父引用在索引中缺失（父目录未加载或索引不完整）。需人工确认逻辑归属后再处置"
	case volume.AnomalyMetaUnreadable:
		return "meta 无法解密/损坏。需人工核对卷密钥是否与建卷时一致；**不要**删除 blob（可能仍可恢复）"
	case volume.AnomalyBlobMissing:
		return "索引声明存在但磁盘缺失（数据丢失）。需人工核对备份/副本后再决定恢复方式"
	case volume.AnomalySizeMismatch:
		return "磁盘大小与声明不符。需人工判断是截断还是声明过期"
	default:
		return ""
	}
}

// issueAnomalyKind 把审查异常码映射为（事项类型, 是否可自动重试, 是否登记）。
//
// 只登记「需要人处理」或「可自动重试」的差异；提示类（dedup_shared / upload_in_progress /
// probe_failed 的临时态等）不登记，避免把正常状态当成问题（噪声会淹没真实告警）。
func issueAnomalyKind(code string) (issues.Kind, bool, bool) {
	switch code {
	case volume.AnomalyDirMetaAmbiguous:
		// 需要人工仲裁（引擎不能自行决定保留哪一份 meta）⇒ 不自动重试。
		return issues.KindDirMetaAmbiguous, false, true
	case volume.AnomalyContainerNotIndexed:
		// 索引未重建 ⇒ 可自动重试（重建/刷新索引）。
		return issues.KindContainerNotIndexed, true, true
	case volume.AnomalyParentUnresolved, volume.AnomalyMetaUnreadable,
		volume.AnomalyBlobMissing, volume.AnomalySizeMismatch:
		return issues.KindOther, false, true
	default:
		// blob_orphan（疑似孤儿，GC 会复核）/ dedup_shared（提示）/ upload_in_progress（在途）/
		// probe_failed（无法判定，非缺失）/ truncated（本次扫描被截断，由 ScanStats 体现）/ not_found
		return "", false, false
	}
}

// IssueKindForAnomaly 暴露异常码 → 事项类型的映射（供 API/诊断复用，避免两处硬编码）。
func IssueKindForAnomaly(code string) (issues.Kind, bool, bool) { return issueAnomalyKind(code) }

// VolumeLister 是卷集的**最小读取接口**（便于测试注入替身；生产传 `*registry.Set`）。
type VolumeLister interface {
	All() []volume.Volume
	External(name string) registry.ExternalBackend
}

// ScanTargetsFromSet 从卷集提取**可审查目标**：只取「后端同步视图实现 StorageInspector」的卷。
//
// 为什么不报错也不登记"不支持审查"：审查是**可选能力**（仓内既有「可选能力 + Assert*」模式），
// 本地卷等不实现属正常（它们没有容器/meta 概念）⇒ 静默跳过。
func ScanTargetsFromSet(l VolumeLister) []ScanTarget {
	if l == nil {
		return nil
	}
	var out []ScanTarget
	for _, v := range l.All() {
		be := l.External(v.Name)
		if be == nil {
			continue
		}
		fs := be.FS()
		if _, ok := volume.AssertStorageInspector(fs); !ok {
			continue
		}
		out = append(out, ScanTarget{Volume: v.Name, Target: fs})
	}
	return out
}
