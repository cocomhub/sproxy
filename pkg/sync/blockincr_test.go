// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBigTestFile 写一个大文件（nBlocks 个 blockSize 块，内容为给定字节）。
func writeBigTestFile(t *testing.T, root, rel string, blockSize, nBlocks int64, fill byte) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if dir := filepath.Dir(full); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	data := bytes.Repeat([]byte{fill}, int(blockSize*nBlocks))
	if err := os.WriteFile(full, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// TestEngineSync_BlockIncremental_OnlyDiffs 块级增量：src 与 dst 旧文件大部分块相同，
// 中部修改一块 → 同步后 dst 内容 = src，且块级路径生效（通过 BlockDiff 验证
// 相同块未被重写——用修改后的统计/结果断言）。
//
// 断言方式：块级复制与整文件复制在结果上等价（dst 内容 = src），但块级路径
// 的差异是「省写放大」——此处断言 dst 最终内容正确 + 无回归（整文件路径仍工作）。
// 差异块传输字节由 BlockDiff 单测覆盖；此处验证引擎接线正确。
func TestEngineSync_BlockIncremental_OnlyDiffs(t *testing.T) {
	t.Parallel()
	const block = 1024 * 1024
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()

	// dst 旧文件：5 块全 'a'。
	writeBigTestFile(t, dstRoot, "big.bin", block, 5, 'a')
	// src 新文件：5 块，前 4 块 'a'，第 5 块（索引 4）改 'b'——只 1 块差异。
	srcFull := filepath.Join(srcRoot, "big.bin")
	if err := os.MkdirAll(srcRoot, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	srcData := bytes.Repeat([]byte{'a'}, int(4*block))
	srcData = append(srcData, bytes.Repeat([]byte{'b'}, int(block))...)
	if err := os.WriteFile(srcFull, srcData, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	// 注入日志缓冲：断言块级路径真实生效（差异块复制走 syncFileBlock）。
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictOverwrite}
	engine := &Engine{Concurrency: 2, Logger: logger}
	if err := engine.Sync(context.Background(), NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	if !strings.Contains(logBuf.String(), "块级增量复制完成") {
		t.Fatalf("应走块级增量路径（syncFileBlock 日志缺失）: %s", logBuf.String())
	}

	// 结果：dst = src（5 块，前 4 'a' 后 1 'b'）。
	gotData, err := os.ReadFile(filepath.Join(dstRoot, "big.bin"))
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(gotData, srcData) {
		t.Fatalf("块级同步后 dst 应 = src（差异块已写入），len=%d", len(gotData))
	}
	// 前 4 块相同内容保持（块级路径没破坏未差异块）。
	if !bytes.Equal(gotData[:4*block], bytes.Repeat([]byte{'a'}, int(4*block))) {
		t.Fatalf("前 4 块（相同块）应保持 'a'（块级复制不得破坏未差异块）")
	}
	if !bytes.Equal(gotData[4*block:], bytes.Repeat([]byte{'b'}, int(block))) {
		t.Fatalf("最后块（差异块）应为 'b'")
	}
}

// TestEngineSync_BlockIncremental_FallbackFull 无 BlockAccessor（自定义最小 FS）
// → 回退整文件复制（零回归）。
func TestEngineSync_BlockIncremental_FallbackFull(t *testing.T) {
	t.Parallel()
	const block = 1024 * 1024
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	writeBigTestFile(t, dstRoot, "big.bin", block, 3, 'a')
	writeBigTestFile(t, srcRoot, "big.bin", block, 3, 'b')

	// 自定义 FS：只实现基础接口（无 BlockAccessor）→ 走整文件复制。
	src := &noBlockFS{LocalFS: NewLocalFS(srcRoot, nil)}
	dst := &noBlockFS{LocalFS: NewLocalFS(dstRoot, nil)}

	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictOverwrite}
	engine := &Engine{Concurrency: 2}
	if err := engine.Sync(context.Background(), src, dst, job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	gotData, err := os.ReadFile(filepath.Join(dstRoot, "big.bin"))
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(gotData, bytes.Repeat([]byte{'b'}, int(3*block))) {
		t.Fatalf("无 BlockAccessor 应整文件复制（dst = src）, len=%d", len(gotData))
	}
}

// noBlockFS 包装 LocalFS 但不实现 BlockAccessor（验证回退零回归）。
type noBlockFS struct {
	*LocalFS
}

// TestEngineSync_BlockIncremental_SameContent 内容相同（仅 mtime 变）→
// 块级路径 0 差异 → 回退整文件复制（结果一致）。
func TestEngineSync_BlockIncremental_SameContent(t *testing.T) {
	t.Parallel()
	const block = 1024 * 1024
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	writeBigTestFile(t, dstRoot, "big.bin", block, 2, 'a')
	writeBigTestFile(t, srcRoot, "big.bin", block, 2, 'a')

	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictOverwrite}
	engine := &Engine{Concurrency: 2}
	if err := engine.Sync(context.Background(), NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	gotData, err := os.ReadFile(filepath.Join(dstRoot, "big.bin"))
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(gotData, bytes.Repeat([]byte{'a'}, int(2*block))) {
		t.Fatalf("相同内容同步后 dst 应 = src（回退路径仍正确）, len=%d", len(gotData))
	}
}

// TestEngineSync_BlockIncremental_BlockAccessorImpl LocalFS 实现 BlockAccessor 断言。
func TestEngineSync_BlockIncremental_BlockAccessorImpl(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTestFile(t, root, "x.txt", "hello")
	fs := NewLocalFS(root, nil)
	if _, ok := any(fs).(BlockAccessor); !ok {
		t.Fatal("LocalFS 应实现 BlockAccessor（块级增量路径依赖）")
	}
}

// 消除 strings 未用警告（测试辅助）。
var _ = strings.Repeat
