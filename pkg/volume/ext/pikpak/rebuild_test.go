// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildPartial 构造「分片部分缺失」的文件：real = 完整数据，holes = 需要清零的块索引。
// 返回 (数据文件路径, 完整数据)。
func buildPartial(t *testing.T, dir string, payload []byte, chunkSize int64, holes ...int) (string, []byte) {
	t.Helper()
	data := make([]byte, len(payload))
	copy(data, payload)
	for _, idx := range holes {
		off := int64(idx) * chunkSize
		end := min(off+chunkSize, int64(len(data)))
		for i := off; i < end; i++ {
			data[i] = 0
		}
	}
	path := filepath.Join(dir, "partial.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	return path, payload
}

// TestScanZeroTail_DetectsHoleAndPartialTail 零尾扫描：整块零洞 + 块尾部分零都判可疑；
// 尾部为真实数据（含尾部零 run 低于阈值）不判可疑。
func TestScanZeroTail_DetectsHoleAndPartialTail(t *testing.T) {
	t.Parallel()
	const chunk = 1 << 20
	payload := recoveryPayload(4 * chunk)
	data := make([]byte, len(payload))
	copy(data, payload)
	// chunk1：整块零洞
	for i := chunk; i < 2*chunk; i++ {
		data[i] = 0
	}
	// chunk2：仅最后 128KiB 为未写零（尾部连续零）
	for i := 3*chunk - 128<<10; i < 3*chunk; i++ {
		data[i] = 0
	}
	path := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	suspect, err := ScanZeroTailSpans(path, int64(len(data)), chunk, ZeroScanOptions{})
	if err != nil {
		t.Fatalf("ScanZeroTailSpans: %v", err)
	}
	byOffset := map[int64]ZeroTailSuspect{}
	for _, s := range suspect {
		byOffset[s.Offset] = s
	}
	if _, ok := byOffset[0]; ok {
		t.Error("chunk0 数据完整，不应可疑")
	}
	if _, ok := byOffset[3*chunk]; ok {
		t.Error("chunk3 数据完整，不应可疑")
	}
	s1, ok := byOffset[chunk]
	if !ok {
		t.Fatalf("chunk1 整块零洞应可疑，实际 %+v", suspect)
	}
	if s1.Zeros < defaultZeroRun {
		t.Errorf("chunk1 零尾长度 = %d, want >= %d", s1.Zeros, defaultZeroRun)
	}
	if _, ok := byOffset[2*chunk]; !ok {
		t.Errorf("chunk2 尾部 128KiB 零应可疑，实际 %+v", suspect)
	}
}

// TestScanZeroTail_Threshold 阈值可调：把阈值抬到大于零 run → 不再可疑。
func TestScanZeroTail_Threshold(t *testing.T) {
	t.Parallel()
	const chunk = 1 << 20
	data := make([]byte, chunk)
	// 仅最后 8KiB 为零
	for i := chunk - 8<<10; i < chunk; i++ {
		data[i] = 0
	}
	for i := range chunk - 8<<10 {
		data[i] = 0xAB
	}
	path := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	yes, err := ScanZeroTailSpans(path, chunk, chunk, ZeroScanOptions{ZeroRun: 4 << 10})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(yes) != 1 {
		t.Fatalf("4KiB 阈值应判可疑: %+v", yes)
	}
	no, err := ScanZeroTailSpans(path, chunk, chunk, ZeroScanOptions{ZeroRun: 32 << 10})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(no) != 0 {
		t.Fatalf("32KiB 阈值不应判可疑: %+v", no)
	}
}

// TestRebuild_FastPathFinalizes 数据已完整（权威哈希命中）→ 直接定稿，不下载任何分片。
func TestRebuild_FastPathFinalizes(t *testing.T) {
	t.Parallel()
	payload := recoveryPayload(4 << 20)
	h := newRecoveryHarness(t, payload, 1<<20, "", 0)
	hd := h.newDownloader(t, 0.5)
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.mp4")
	// 工作文件已含完整数据；manifest 丢失（模拟只能靠内容判断的状态）
	if err := os.WriteFile(WorkingPath(dest), payload, 0o644); err != nil {
		t.Fatalf("seed working: %v", err)
	}

	rep, err := hd.Rebuild(context.Background(), "https://mypikpak.com/s/EXAMPLE-001123", dest, RebuildOptions{}, nil)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if !rep.HashVerified || !rep.Finalized {
		t.Fatalf("应走权威校验快路径并定稿: %+v", rep)
	}
	if rep.DownloadedChunks != 0 {
		t.Errorf("快路径不应下载分片: %+v", rep)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("定稿内容不正确: err=%v size=%d", err, len(got))
	}
	if fileExists(WorkingPath(dest)) {
		t.Error("定稿后不应残留工作文件")
	}
	if fileExists(ManifestPath(dest)) {
		t.Error("定稿后不应残留 manifest")
	}
}

// TestRebuild_RepairsZeroTailChunk 用户实测状态：manifest 记录全部完成，但磁盘上有一块零洞
// → 零尾扫描剔除该块并只重下这一块（不全量重下）。
func TestRebuild_RepairsZeroTailChunk(t *testing.T) {
	t.Parallel()
	const chunk = 1 << 20
	payload := recoveryPayload(4 * chunk)
	h := newRecoveryHarness(t, payload, chunk, "", 0)
	hd := h.newDownloader(t, 0.5)
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.mp4")
	// 数据文件：chunk2（账号区）为空洞
	src, _ := buildPartial(t, dir, payload, chunk, 2)
	if err := os.Rename(src, WorkingPath(dest)); err != nil {
		t.Fatalf("rename working: %v", err)
	}
	// 残留 manifest：声称 4 块全部完成（与磁盘不一致 → 必须被零尾扫描纠正）
	target := &ShareFile{ID: "share-f1", Size: int64(len(payload)), Hash: payloadGCID(payload)}
	chunks := map[int64]int64{}
	for off := int64(0); off < target.Size; off += chunk {
		chunks[off] = min(chunk, target.Size-off)
	}
	writeManifestFile(t, dest, &hybridManifest{
		Total: target.Size, Chunks: chunks,
		Source: manifestSource{ShareID: "EXAMPLE-001123", FileID: target.ID, Hash: target.Hash, Size: target.Size},
	})

	rep, err := hd.Rebuild(context.Background(), "https://mypikpak.com/s/EXAMPLE-001123", dest, RebuildOptions{ChunkSize: chunk}, nil)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if len(rep.ZeroSuspect) != 1 || rep.ZeroSuspect[0].Offset != 2*chunk {
		t.Fatalf("应恰有 1 个零尾可疑块（offset=%d）: %+v", 2*chunk, rep.ZeroSuspect)
	}
	if rep.DownloadedChunks != 1 {
		t.Errorf("应只重建 1 块，实际 %d", rep.DownloadedChunks)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("重建后内容不正确: err=%v", err)
	}
	if !rep.Finalized {
		t.Error("应已定稿")
	}
}

// TestRebuild_NormalizesRenamedFinalFile 用户实测状态（成品已改名 + manifest 残留 + 一块零洞）
// → 自动把成品移回工作文件、定点修复、再定稿。
func TestRebuild_NormalizesRenamedFinalFile(t *testing.T) {
	t.Parallel()
	const chunk = 1 << 20
	payload := recoveryPayload(4 * chunk)
	h := newRecoveryHarness(t, payload, chunk, "", 0)
	hd := h.newDownloader(t, 0.5)
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.mp4")
	src, _ := buildPartial(t, dir, payload, chunk, 3) // 最后一块为空洞
	if err := os.Rename(src, dest); err != nil {      // 成品名（工作文件缺失）
		t.Fatalf("rename dest: %v", err)
	}
	target := &ShareFile{ID: "share-f1", Size: int64(len(payload)), Hash: payloadGCID(payload)}
	writeManifestFile(t, dest, &hybridManifest{
		Total: target.Size, Chunks: map[int64]int64{0: chunk, 1: chunk},
		Source: manifestSource{ShareID: "EXAMPLE-001123", FileID: target.ID, Hash: target.Hash, Size: target.Size},
	})

	rep, err := hd.Rebuild(context.Background(), "https://mypikpak.com/s/EXAMPLE-001123", dest, RebuildOptions{ChunkSize: chunk}, nil)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if !rep.Normalized || !rep.Finalized {
		t.Fatalf("应已归一化 + 定稿: %+v", rep)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("重建后内容不正确: err=%v", err)
	}
	if fileExists(WorkingPath(dest)) || fileExists(ManifestPath(dest)) {
		t.Error("定稿后不应残留工作文件/manifest")
	}
}

// TestRebuild_DryRunReportsOnly 仅扫描报告：不改文件、不下载。
func TestRebuild_DryRunReportsOnly(t *testing.T) {
	t.Parallel()
	const chunk = 1 << 20
	payload := recoveryPayload(4 * chunk)
	h := newRecoveryHarness(t, payload, chunk, "", 0)
	hd := h.newDownloader(t, 0.5)
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.mp4")
	src, _ := buildPartial(t, dir, payload, chunk, 1)
	if err := os.Rename(src, WorkingPath(dest)); err != nil {
		t.Fatalf("rename: %v", err)
	}

	rep, err := hd.Rebuild(context.Background(), "https://mypikpak.com/s/EXAMPLE-001123", dest, RebuildOptions{ChunkSize: chunk, DryRun: true}, nil)
	if err != nil {
		t.Fatalf("Rebuild dry-run: %v", err)
	}
	if rep.Finalized || rep.DownloadedChunks != 0 {
		t.Fatalf("dry-run 不应下载/定稿: %+v", rep)
	}
	if len(rep.RebuildSpans) == 0 {
		t.Fatalf("dry-run 应报告待重建分片: %+v", rep)
	}
	if !fileExists(WorkingPath(dest)) {
		t.Error("dry-run 不应移动数据文件")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("dry-run 不应产出成品")
	}
}

// TestRebuild_NoSuspectButIntegrityFails_Escalates 非零损坏（扫描定位不到）→ 权威校验失败后
// 自动升级为全量重建（否则会陷入「manifest 说全完成 + 校验恒失败」的死循环）。
func TestRebuild_NoSuspectButIntegrityFails_Escalates(t *testing.T) {
	t.Parallel()
	const chunk = 1 << 20
	payload := recoveryPayload(4 * chunk)
	h := newRecoveryHarness(t, payload, chunk, "", 0)
	hd := h.newDownloader(t, 0.5)
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.mp4")
	// 数据完整但中间一个字节被改坏（非零损坏，零尾扫描查不出）
	corrupted := make([]byte, len(payload))
	copy(corrupted, payload)
	corrupted[chunk+1234] ^= 0xFF
	if err := os.WriteFile(WorkingPath(dest), corrupted, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	target := &ShareFile{ID: "share-f1", Size: int64(len(payload)), Hash: payloadGCID(payload)}
	chunks := map[int64]int64{}
	for off := int64(0); off < target.Size; off += chunk {
		chunks[off] = min(chunk, target.Size-off)
	}
	writeManifestFile(t, dest, &hybridManifest{
		Total: target.Size, Chunks: chunks,
		Source: manifestSource{ShareID: "EXAMPLE-001123", FileID: target.ID, Hash: target.Hash, Size: target.Size},
	})

	rep, err := hd.Rebuild(context.Background(), "https://mypikpak.com/s/EXAMPLE-001123", dest, RebuildOptions{ChunkSize: chunk}, nil)
	if err != nil {
		t.Fatalf("Rebuild 应在升级全量重建后成功: %v", err)
	}
	if !rep.FullRebuild {
		t.Errorf("应标记全量重建: %+v", rep)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("全量重建后内容不正确: err=%v", err)
	}
}

// TestRebuild_ResolveFailureIsExplicit rebuild 是显式修复命令：resolve 失败如实报错，
// 不静默降级（避免「以为修好了」）。
func TestRebuild_ResolveFailureIsExplicit(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t, recoveryPayload(1<<20), 1<<20, "", 0)
	hd := h.newDownloader(t, 0.5)
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Rebuild(context.Background(), "https://example.com/not-pikpak", dest, RebuildOptions{}, nil); err == nil {
		t.Fatal("err = nil, want resolve error")
	}
}

// TestRebuild_NotPikpakURL URL 形态不支持 → 明确报错。
func TestRebuild_NotPikpakURL(t *testing.T) {
	t.Parallel()
	hd := &HybridDownloader{log: apiTestLogger()}
	_, err := hd.Rebuild(context.Background(), "https://example.com/x.zip", "/tmp/x", RebuildOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("err = %v, want unsupported url", err)
	}
}

// TestChunkErrorUnwrap ChunkError 可被 errors.Is/As 追溯（CLI 据此区分 ctx/网络/哨兵）。
func TestChunkErrorUnwrap(t *testing.T) {
	t.Parallel()
	ce := &ChunkError{Offset: 3, Length: 9, Err: context.Canceled}
	if !strings.Contains(ce.Error(), "@3") {
		t.Errorf("Error() = %q, want 含 offset", ce.Error())
	}
	if !errors.Is(ce, context.Canceled) {
		t.Error("ChunkError 应可 errors.Is 追溯底层错误")
	}
}
