// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// manifest_api.go 把「进度文件（manifest）只读分析 + 工作文件归位 + 离线零尾扫描」的
// **领域逻辑**收敛到 sproxy（用户裁定 2026-10-28：pikget 只负责 CLI 交互）。
//
// 这些能力此前散落在 CLI（pikget rebuild）里：读 manifest 摘要、从 manifest 恢复分享 URL、
// 离线扫描的「可信/待重建」推导、异常命名文件归位。放在领域包后，任何调用方（CLI/服务端/
// 运维脚本）共用同一实现，避免双份逻辑漂移。
package pikpak

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ManifestSummary 是进度文件（manifest）的只读摘要（供扫描/展示/续传基准）。
type ManifestSummary struct {
	Path      string          // 实际命中的 manifest 路径
	Total     int64           // 记录的总字节数
	ChunkSize int64           // 记录的最大分片长度（历史字段，仅作参考）
	Done      int             // 已记录分片数
	Chunks    map[int64]int64 // offset → length
}

// OfflineReport 是离线零尾扫描报告（不联网、不修改文件）。
type OfflineReport struct {
	DataPath     string            // 数据文件路径（工作文件）
	Total        int64             // 文件大小（manifest 有 total 时以其为准）
	ChunkSize    int64             // 分片大小
	ChunksTotal  int               // 分片总数
	ChunksTrust  int               // 可信分片数（manifest 记录 ∩ 零尾扫描通过）
	Manifest     string            // manifest 路径（空 = 无）
	ManifestDone int               // manifest 记录的分片数
	Suspects     []ZeroTailSuspect // 零尾可疑分片
	Rebuild      []ChunkSpan       // 待重建分片（零尾可疑）
}

// NormalizeWorkDest 把「工作文件路径」（*.hybrid.downloading）归一为最终文件路径。
func NormalizeWorkDest(path string) string {
	return strings.TrimSuffix(path, hybridWorkingSuffix)
}

// ManifestCandidates 返回 path 对应的进度文件候选（顺序：归一后的最终名 → stale 归档）。
func ManifestCandidates(path string) []string {
	base := NormalizeWorkDest(path)
	out := []string{ManifestPath(base), StaleManifestPath(base)}
	if base != path {
		out = append(out, ManifestPath(path), StaleManifestPath(path))
	}
	return out
}

// ReadManifestSummary 读取进度文件摘要（候选顺序尝试；损坏/缺失 → false）。
func ReadManifestSummary(path string) (ManifestSummary, bool) {
	for _, p := range ManifestCandidates(path) {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var m hybridManifest
		if json.Unmarshal(raw, &m) != nil || m.Chunks == nil {
			continue
		}
		sum := ManifestSummary{Path: p, Total: m.Total, Done: len(m.Chunks), Chunks: m.Chunks}
		for _, ln := range m.Chunks {
			if ln > sum.ChunkSize {
				sum.ChunkSize = ln
			}
		}
		return sum, true
	}
	return ManifestSummary{}, false
}

// SourceURLFromManifest 从 manifest 记录的源 URL 恢复分享地址（rebuild --repair 免重输 URL）。
func SourceURLFromManifest(path string) (string, bool) {
	for _, p := range ManifestCandidates(path) {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var m hybridManifest
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if u := strings.TrimSpace(m.Source.URL); u != "" {
			return u, true
		}
	}
	return "", false
}

// ResumeBytes 返回续传基准（manifest 已记录分片字节合计；无 manifest → 0）。
func ResumeBytes(dest string) int64 {
	sum, ok := ReadManifestSummary(dest)
	if !ok {
		return 0
	}
	var total int64
	for _, ln := range sum.Chunks {
		total += ln
	}
	return total
}

// StageWorkFiles 把用户指定的异常命名数据/进度文件移入标准工作文件位置
// （*.hybrid.downloading / *.hybrid），使后续重建可直接续用已下数据。
// 传空路径 = 不动；目标已存在时不覆盖（返回错误，避免静默丢失）。
func StageWorkFiles(dest, dataPath, manifestPath string) error {
	if err := stageOneWorkFile(dataPath, WorkingPath(dest), "数据文件"); err != nil {
		return err
	}
	return stageOneWorkFile(manifestPath, ManifestPath(dest), "进度文件")
}

// stageOneWorkFile 把 src 归位到 want（空 src = 不动；want 已存在则拒绝覆盖）。
func stageOneWorkFile(src, want, label string) error {
	if src == "" {
		return nil
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("%s %s 不存在: %w", label, src, err)
	}
	if src == want {
		return nil
	}
	if _, err := os.Stat(want); err == nil {
		return fmt.Errorf("目标%s已存在，拒绝覆盖: %s", label, want)
	}
	if err := os.Rename(src, want); err != nil {
		return fmt.Errorf("%s移入标准位置失败（%s → %s）: %w", label, src, want, err)
	}
	return nil
}

// OfflineScan 离线扫描本地（工作）文件：manifest 对照 + 零尾可疑分片，只报告不改动。
// chunkSize<=0 时用 manifest 记录的分片长度；无 manifest 时以整文件为单片。
func OfflineScan(dest string, chunkSize, zeroRun int64) (OfflineReport, error) {
	sum, hasManifest := ReadManifestSummary(dest)
	path := dest
	if hasManifest && sum.Total > 0 {
		path = WorkingPath(NormalizeWorkDest(dest))
		if _, err := os.Stat(path); err != nil {
			path = dest // 工作文件不存在 → 退回到用户给的路径
		}
	}
	total := fileSizeOrZero(path)
	if 0 < sum.Total {
		total = sum.Total
	}
	if total <= 0 {
		return OfflineReport{}, fmt.Errorf("文件为空或不存在，无法扫描: %s", path)
	}
	if chunkSize <= 0 {
		chunkSize = sum.ChunkSize
	}
	if chunkSize <= 0 {
		chunkSize = defaultHybridChunkSize
	}
	suspects, err := ScanZeroTailSpans(path, total, chunkSize, ZeroScanOptions{ZeroRun: zeroRun})
	if err != nil {
		return OfflineReport{}, fmt.Errorf("扫描失败: %w", err)
	}
	rep := OfflineReport{
		DataPath: path, Total: total, ChunkSize: chunkSize,
		ChunksTotal: len(PlanChunkSpans(total, chunkSize)),
		Suspects:    suspects, ManifestDone: sum.Done,
	}
	if hasManifest {
		rep.Manifest = sum.Path
	}
	rep.Rebuild = rebuildSpans(suspects)
	if hasManifest {
		rep.ChunksTrust = countTrustedChunks(total, chunkSize, sum.Chunks, suspects)
	}
	return rep, nil
}

// rebuildSpans 把零尾可疑块转为待重建分片。
func rebuildSpans(suspects []ZeroTailSuspect) []ChunkSpan {
	out := make([]ChunkSpan, 0, len(suspects))
	for _, s := range suspects {
		out = append(out, ChunkSpan{Offset: s.Offset, Length: s.Length})
	}
	return out
}

// countTrustedChunks 统计「manifest 记录完成 ∩ 零尾扫描通过」的分片数。
func countTrustedChunks(total, chunkSize int64, recorded map[int64]int64, suspects []ZeroTailSuspect) int {
	suspectAt := make(map[int64]bool, len(suspects))
	for _, s := range suspects {
		suspectAt[s.Offset] = true
	}
	trust := 0
	for _, span := range PlanChunkSpans(total, chunkSize) {
		if suspectAt[span.Offset] {
			continue
		}
		if _, ok := recorded[span.Offset]; ok {
			trust++
		}
	}
	return trust
}

// fileSizeOrZero 返回文件大小（不存在/错误 → 0）。
func fileSizeOrZero(path string) int64 {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return 0
	}
	return st.Size()
}
