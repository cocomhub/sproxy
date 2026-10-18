// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// tempProbeFS 包装底层 FS，在**每次分块 OpenRead 之前**采样 temp 目录的**条目构成**。
//
// 为什么用「条目数 + 是否出现 view-* 目录」而不是「字节数」：本仓实测（Windows）**打开中的
// 文件**对 `os.ReadDir` 不可见或 `Size()` 为 0 ⇒ 字节判据会假绿 ✗；而**目录条目**始终可见
// ⇒ 条目构成判据在两端一致且确定性（不依赖并发时序）：
//   - 旧实现：进入分块循环**之前**创建 `view-*` staging 目录，随后才写 `decrypt-*` 明文
//     ⇒ 采样必然看到 `view-*` 目录，且条目数 ≥2；
//   - 去 staging 的实现：**从不创建**目录，只有 1 个明文文件 ⇒ 条目数恒 1、无 view-*。
type tempProbeFS struct {
	syncpkg.FS
	tempDir string
	mu      sync.Mutex
	samples int
	// maxEntries 是采样到的 temp 目录条目数最大值。
	maxEntries int
	// sawStaging 记录采样时是否已存在密文 staging 目录（`view-*`）。
	sawStaging bool
}

func (p *tempProbeFS) sample() {
	ents, _ := os.ReadDir(p.tempDir)
	staging := false
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), "view-") {
			staging = true
		}
	}
	p.mu.Lock()
	p.samples++
	if len(ents) > p.maxEntries {
		p.maxEntries = len(ents)
	}
	if staging {
		p.sawStaging = true
	}
	p.mu.Unlock()
}

func (p *tempProbeFS) OpenRead(ctx context.Context, rel string) (io.ReadCloser, error) {
	p.sample()
	return p.FS.OpenRead(ctx, rel)
}

// TestOpenRead_TempPeakBoundedByPlaintextSize 钉住「读路径不得把**全部分块密文** staging 到
// temp（⇒ 峰值 ≈2× 文件）」。
//
// 背景（2026-10-18 用户裁定「架构级根治、正确性完整性可靠性优先、允许破坏性变更」）：
// `openRead` 此前先把全部分块密文复制进 `s.temp/view-*`，再经 `shardseal.DecryptFile` 把整份
// 明文写进 `s.temp/decrypt-*` ⇒ 峰值临时盘 ≈ **2× 文件** ✗。根因是 `DecryptFile` 的
// 「本地目录 → 本地文件」路径型 API（调用方被迫先落密文）。
//
// 判据（确定性）：payload = 512KiB、块 32KiB（16 块）⇒ 逐块采样 temp 条目构成，断言
// ① **从未出现 `view-*` staging 目录** ② 条目数恒 ≤1（只有那份明文）③ 读回内容逐字节正确
// ④ 篡改一块 ⇒ 读必须失败（GCM 认证 + 整文件 SHA-256 校验仍生效）。
// 变异验证：在 openRead 里恢复 staging ⇒ ①② 必红。
func TestOpenRead_TempPeakBoundedByPlaintextSize(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	payload := bytes.Repeat([]byte("temp-peak-probe-"), 32*1024) // 512KiB

	storageDir := t.TempDir()
	tempDir := t.TempDir()
	raw := syncpkg.NewLocalFS(storageDir, nil)
	probe := &tempProbeFS{FS: raw, tempDir: tempDir}
	opts := Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		TempDir:   tempDir,
		Block:     shardseal.BlockPolicy{Min: 32 << 10, Max: 32 << 10},
	}
	sfs, err := NewFS(probe, opts)
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if sfs.temp != tempDir {
		t.Fatalf("夹具失效：s.temp=%q，期望 %q", sfs.temp, tempDir)
	}
	if err := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "big.bin",
		bytes.NewReader(payload), int64(len(payload)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rc, rerr := sfs.OpenRead(ctx, "big.bin")
	if rerr != nil {
		t.Fatalf("OpenRead: %v", rerr)
	}
	got, gerr := io.ReadAll(rc)
	_ = rc.Close()
	if gerr != nil {
		t.Fatalf("读回失败: %v", gerr)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("读回内容与原文不一致（%d vs %d 字节）", len(got), len(payload))
	}

	probe.mu.Lock()
	sawStaging, maxEntries, samples := probe.sawStaging, probe.maxEntries, probe.samples
	probe.mu.Unlock()
	if samples == 0 {
		t.Fatal("探针未采样（判据无效）")
	}
	if sawStaging {
		t.Fatalf("读路径仍在把全部分块密文 staging 到 temp（view-*）⇒ 峰值 ≈2× 文件 ✗（采样 %d 次）", samples)
	}
	// 目标已是「按需解密、**不落任何临时文件**」（temp → 0，见 read_seeker.go）：阈值取 0，
	// 否则「读路径退化为写一份明文临时文件」这种回归会绿过（判据选在无法区分 0/1 的位置）。
	if maxEntries > 0 {
		t.Fatalf("读期间 temp 存在 %d 个条目（应为 0：按需解密不落临时文件）⇒ 读路径退化 ✗", maxEntries)
	}

	// ④ 整文件校验仍必须生效：篡改**最后一个**分块的密文 ⇒ 读必须失败（不得返回坏内容）。
	sfs.mu.RLock()
	e := sfs.index["big.bin"]
	sfs.mu.RUnlock()
	if e == nil || len(e.meta.Chunks) < 2 {
		t.Fatalf("夹具异常：块数 %d", len(e.meta.Chunks))
	}
	last := e.meta.Chunks[len(e.meta.Chunks)-1]
	blobPath := path.Join(sfs.dataSeg(e), last.FileName)
	blob, berr := raw.OpenRead(ctx, blobPath)
	if berr != nil {
		t.Fatalf("读分块 blob 失败: %v", berr)
	}
	blobBytes, _ := io.ReadAll(blob)
	_ = blob.Close()
	// 破坏 blob **中段**的密文（尾部可能落在 GCM 覆盖区之外 ⇒ 判据会假绿 ✗）。
	blobBytes[len(blobBytes)/2] ^= 0xff
	if werr := raw.WriteFile(ctx, syncpkg.LogicalKey{}, blobPath,
		bytes.NewReader(blobBytes), int64(len(blobBytes)), 0); werr != nil {
		t.Fatalf("写回被篡改分块失败: %v", werr)
	}
	rc2, rerr2 := sfs.OpenRead(ctx, "big.bin")
	if rerr2 == nil {
		_, readErr := io.ReadAll(rc2)
		_ = rc2.Close()
		if readErr == nil {
			t.Fatal("篡改分块后读**不应成功**（GCM 认证/整文件校验必须 fail-closed）✗")
		}
	}
}
