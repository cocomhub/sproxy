// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/integrity"
)

// RebuildOptions 是 rebuild（定点重建）参数（零值 = 默认）。
type RebuildOptions struct {
	ChunkSize int64           // 分块大小（0 → 下载器配置/默认 32MiB）
	ZeroScan  ZeroScanOptions // 零尾扫描参数（零值 = 默认：1MiB 窗口 / 64KiB 阈值）
	DryRun    bool            // 仅扫描报告，不改动任何文件
	// DisableEscalate 关闭「定位不到可疑块但权威校验失败 → 全量重建一次」的自动升级。
	// 默认 false = 自动升级（rebuild 的语义是把文件修好；升级只在校验失败时发生）。
	DisableEscalate bool
	SinkFactory     downloader.SinkFactory // 可选：与 DownloadWithWriter 一致（配额记账）
}

// RebuildReport 是 rebuild 的扫描/重建报告（CLI 输出与可观测用）。
type RebuildReport struct {
	DestPath         string            // 最终目标路径
	DataPath         string            // 扫描时定位到的数据文件（工作文件或成品名）
	Working          string            // 工作文件路径（destPath + .hybrid.downloading）
	Total            int64             // 文件总大小
	ChunkSize        int64             // 分块大小
	Normalized       bool              // 已把成品名数据移回工作文件（恢复续传状态）
	HashVerified     bool              // 现有数据已通过源权威哈希（快路径，无需下载）
	ChunksTotal      int               // 总分片数
	ChunksTrust      int               // 可信分片数（manifest 记录 ∩ 零尾扫描通过）
	RebuildSpans     []ChunkSpan       // 待重建分片（缺失 + 零尾可疑）
	ZeroSuspect      []ZeroTailSuspect // 零尾可疑块（块内尾部连续零 ≥ 阈值）
	DownloadedChunks int               // 本次实际补下的分片数
	FullRebuild      bool              // 触发了全量重建（非零损坏无法局部定位）
	Finalized        bool              // 已定稿（rename 到 destPath + 清 manifest）
	Result           *Result           // 在线重建的结果（DryRun=false 且成功）
}

// Rebuild 定点重建一个 PikPak 分享下载产物（用户实测故障的恢复入口）：
//
//	① 定位数据文件（工作文件 / 成品名），不做改动；
//	② 权威哈希快路径：现有数据已与源一致 → 直接定稿（不下载任何分片）；
//	③ 零尾扫描：可信集合 = manifest 记录（源一致时）∩ 零尾扫描通过；待重建 = 其余分片；
//	④ 重写 manifest（只保留可信分片）→ 复用 Download 续传补齐这些分片 → 权威校验 → 定稿；
//	⑤ 若补下后权威校验仍失败（非零损坏等扫描定位不到的情况）→ 自动升级全量重建一次。
//
// 覆盖的用户实测异常状态：
//   - 成品已改名 + manifest 残留（dest 有数据、工作文件缺失）→ 数据移回工作文件后定点修复；
//   - manifest 记录完成但磁盘是零洞（预分配 + 未写完）→ 剔除该块并只重下这些块；
//   - manifest 丢失/为空但数据基本完整 → 零尾扫描重建可信集合，避免全量重下；
//   - 数据已完整只是进度文件丢了 → 快路径直接定稿。
func (d *HybridDownloader) Rebuild(ctx context.Context, source, destPath string, opt RebuildOptions, onProgress downloader.ProgressFunc) (*RebuildReport, error) {
	s, err := d.newRebuildSession(ctx, source, destPath, opt, onProgress)
	if err != nil {
		return nil, err
	}
	if err := s.prepare(); err != nil {
		return s.rep, err
	}
	// 快路径已定稿（或 DryRun 只读）→ 无需 apply。
	if opt.DryRun || s.rep.Finalized {
		return s.rep, nil
	}
	if err := s.apply(ctx); err != nil {
		return s.rep, err
	}
	return s.rep, nil
}

// rebuildSession 是单次 rebuild 的私有状态（参数收敛，避免超长签名/高复杂度函数）。
type rebuildSession struct {
	d         *HybridDownloader
	source    string
	shareID   string
	destPath  string
	target    *ShareFile
	chunkSize int64
	opt       RebuildOptions
	onProg    downloader.ProgressFunc
	plan      *rebuildPlan
	rep       *RebuildReport
}

// newRebuildSession 解析分享并建立重建会话（resolve 在 rebuild 中是必需的：
// 没有源就无法判定「哪些分片算完整」）。
func (d *HybridDownloader) newRebuildSession(ctx context.Context, source, destPath string, opt RebuildOptions, onProgress downloader.ProgressFunc) (*rebuildSession, error) {
	shareID, err := parseShareID(source)
	if err != nil {
		return nil, err
	}
	meta, err := d.resolver.Resolve(ctx, source)
	if err != nil {
		return nil, fmt.Errorf("pikpak rebuild resolve %s: %w", shareID, err)
	}
	target := pickLargestShareFile(meta.Files)
	if target == nil || target.Size <= 0 {
		return nil, fmt.Errorf("%w: rebuild no target file in share %s", ErrShareNotFound, shareID)
	}
	chunkSize := d.chunkSize
	if opt.ChunkSize > 0 {
		chunkSize = opt.ChunkSize
	}
	if chunkSize <= 0 {
		chunkSize = defaultHybridChunkSize
	}
	return &rebuildSession{
		d: d, source: source, shareID: shareID, destPath: destPath,
		target: target, chunkSize: chunkSize, opt: opt, onProg: onProgress,
		rep: &RebuildReport{DestPath: destPath, Working: WorkingPath(destPath), Total: target.Size, ChunkSize: chunkSize},
	}, nil
}

// prepare 只读阶段：定位数据文件 → 权威哈希快路径 → 零尾扫描规划（DryRun 到此为止）。
func (s *rebuildSession) prepare() error {
	s.rep.DataPath, _ = locateDataFile(s.destPath, s.target.Size)
	if err := s.fastPath(); err != nil {
		return err
	}
	if s.rep.HashVerified {
		return nil // 快路径已定稿（或 DryRun 报告）
	}
	plan, err := s.d.planRebuild(s.destPath, s.shareID, s.target, s.rep.DataPath, s.chunkSize, s.opt.ZeroScan)
	if err != nil {
		return err
	}
	s.plan = plan
	s.rep.ChunksTotal, s.rep.ChunksTrust = plan.total, len(plan.trust)
	s.rep.RebuildSpans, s.rep.ZeroSuspect = plan.rebuild, plan.suspect
	return nil
}

// fastPath 权威哈希命中即定稿（数据已完整只是进度文件丢了 → 不下载任何分片）。
func (s *rebuildSession) fastPath() error {
	if s.target.Hash == "" {
		return nil
	}
	matched, err := integrity.VerifyGCIDStats(s.rep.DataPath, integrity.GCIDCandidates, s.target.Hash, nil)
	if err != nil {
		return fmt.Errorf("pikpak rebuild verify: %w", err)
	}
	if !matched {
		return nil
	}
	s.rep.HashVerified = true
	if s.opt.DryRun {
		return nil
	}
	if err := finalizeExisting(s.destPath, s.rep.DataPath); err != nil {
		return err
	}
	s.rep.Finalized = true
	return nil
}

// apply 落盘阶段：归一化到工作文件 → 重写可信 manifest → 复用 Download 续传补齐 →（必要时）全量重建。
func (s *rebuildSession) apply(ctx context.Context) error {
	if err := normalizeWorkingFile(s.destPath, s.target.Size, s.rep.DataPath); err != nil {
		return err
	}
	s.rep.Normalized = s.rep.DataPath != s.rep.Working
	writeManifest(s.destPath, newRebuildManifest(s.d, s.shareID, s.source, s.target, s.plan.trust))
	res, err := s.d.DownloadWithWriter(ctx, s.source, s.destPath, s.onProg, s.opt.SinkFactory)
	if err == nil {
		s.rep.Finalized, s.rep.Result = true, res
		s.rep.DownloadedChunks = len(s.plan.rebuild)
		return nil
	}
	// 补下后权威校验仍失败（扫描定位不到的非零损坏/旧源数据）→ 全量重建一次。
	if !s.opt.DisableEscalate && s.target.Hash != "" && errors.Is(err, ErrIntegrityMismatch) {
		return s.fullRebuild(ctx, err)
	}
	return err
}

// fullRebuild 丢弃全部可信记录后整文件重建一次（rebuild 的语义是把文件修好；
// 只在「局部修复后权威校验仍失败」时触发，且只触发一次）。
func (s *rebuildSession) fullRebuild(ctx context.Context, cause error) error {
	s.d.log.Warn("hybrid rebuild: integrity still failing after repair, full rebuild",
		"dest", s.destPath, "cause", cause)
	writeManifest(s.destPath, newRebuildManifest(s.d, s.shareID, s.source, s.target, nil))
	res, err := s.d.DownloadWithWriter(ctx, s.source, s.destPath, s.onProg, s.opt.SinkFactory)
	if err != nil {
		return err
	}
	s.rep.FullRebuild, s.rep.Finalized, s.rep.Result = true, true, res
	s.rep.DownloadedChunks = s.plan.total
	return nil
}

// rebuildPlan 是重建规划结果。
type rebuildPlan struct {
	total   int               // 总分片数
	trust   map[int64]int64   // 可信分片（offset → length）
	rebuild []ChunkSpan       // 待重建分片
	suspect []ZeroTailSuspect // 零尾可疑块
}

// planRebuild 规划可信/待重建分片：
//   - 数据文件不存在 → 全部待重建；
//   - 全文件零尾扫描（一次扫描同时服务「manifest 校验」与「manifest 缺失时的可信集合重建」）；
//   - 可信 = 零尾扫描通过 ∩（manifest 源身份一致时：manifest 记录完成）。
//
// manifest 源身份不一致/为空时也用「扫描通过」做可信集合——低成本避免全量重下；
// 若数据其实是旧版本内容（非零但不对），最终权威校验失败会触发全量重建兜底。
func (d *HybridDownloader) planRebuild(destPath, shareID string, target *ShareFile, dataPath string, chunkSize int64, scanOpt ZeroScanOptions) (*rebuildPlan, error) {
	spans := PlanChunkSpans(target.Size, chunkSize)
	plan := &rebuildPlan{total: len(spans), trust: map[int64]int64{}}
	if !fileExists(dataPath) {
		plan.rebuild = spans
		return plan, nil
	}
	suspect, err := ScanZeroTail(dataPath, spans, scanOpt)
	if err != nil {
		return nil, fmt.Errorf("pikpak rebuild scan: %w", err)
	}
	plan.suspect = suspect
	zeroTail := make(map[int64]bool, len(suspect))
	for _, s := range suspect {
		zeroTail[s.Offset] = true
	}
	recorded := d.recordedChunks(destPath, shareID, target)
	for _, span := range spans {
		if zeroTail[span.Offset] {
			continue // 尾部零洞：未写完 → 待重建
		}
		if recorded != nil {
			if _, ok := recorded[span.Offset]; !ok {
				continue // manifest 未记录 → 待重建
			}
		}
		plan.trust[span.Offset] = span.Length
	}
	for _, span := range spans {
		if _, ok := plan.trust[span.Offset]; !ok {
			plan.rebuild = append(plan.rebuild, span)
		}
	}
	return plan, nil
}

// recordedChunks 返回 manifest 记录完成的分片（源身份一致时）；不一致/缺失 → nil（= 不看 manifest，
// 可信集合退化为「零尾扫描通过」）。
func (d *HybridDownloader) recordedChunks(destPath, shareID string, target *ShareFile) map[int64]int64 {
	m := loadManifest(destPath)
	if m == nil || !m.SourceMatches(shareID, target) {
		return nil
	}
	return m.Chunks
}

// newRebuildManifest 构造重建后的 manifest（trust 为 nil → 空分片集合 = 全量重建）。
func newRebuildManifest(d *HybridDownloader, shareID, sourceURL string, target *ShareFile, trust map[int64]int64) *hybridManifest {
	chunks := trust
	if chunks == nil {
		chunks = map[int64]int64{}
	}
	shareEnd := min(int64(float64(target.Size)*d.shareRatio), target.Size)
	return &hybridManifest{
		Total:    target.Size,
		ShareEnd: shareEnd,
		Chunks:   chunks,
		Source: manifestSource{
			ShareID: shareID, FileID: target.ID, Hash: target.Hash, Size: target.Size, URL: sourceURL,
		},
	}
}

// locateDataFile 只读定位当前数据文件：工作文件优先（大小一致），其次成品名（大小一致）；
// 都不一致时返回工作文件路径（由调用方预分配）。
func locateDataFile(destPath string, total int64) (string, error) {
	work := WorkingPath(destPath)
	if matchesSize(work, total) {
		return work, nil
	}
	if matchesSize(destPath, total) {
		return destPath, nil
	}
	return work, nil
}

// matchesSize 判断路径存在且大小等于 total。
func matchesSize(path string, total int64) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Size() == total
}

// normalizeWorkingFile 把数据归一化到工作文件：数据在成品名 → rename 移回（保留已下数据）；
// 缺失/大小不符 → 预分配工作文件（大小 = total）。
func normalizeWorkingFile(destPath string, total int64, dataPath string) error {
	work := WorkingPath(destPath)
	if dataPath == destPath && fileExists(destPath) {
		if err := os.Rename(destPath, work); err != nil {
			return fmt.Errorf("pikpak rebuild move to working: %w", err)
		}
		return nil
	}
	if matchesSize(work, total) {
		return nil
	}
	if err := preallocate(work, total); err != nil {
		return fmt.Errorf("pikpak rebuild preallocate: %w", err)
	}
	return nil
}

// finalizeExisting 数据已在成品路径时定稿：仅清进度文件（无需 rename）。
func finalizeExisting(destPath, dataPath string) error {
	if dataPath == destPath {
		removeManifest(destPath)
		return nil
	}
	return finalizeWorking(destPath)
}

// finalizeWorking 把工作文件 rename 为成品并清 manifest。
func finalizeWorking(destPath string) error {
	if err := os.Rename(WorkingPath(destPath), destPath); err != nil {
		return fmt.Errorf("pikpak rebuild finalize: %w", err)
	}
	removeManifest(destPath)
	return nil
}
