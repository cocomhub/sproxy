// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// files_meta_stale_test.go —— 「崩溃窗口留陈旧 sidecar」的**分类 + 可修复**回归
// （第 5 轮对抗评审 ③）。
//
// 场景：主文件与 sidecar 是两次独立原子写；崩溃落在「主文件已落新内容、sidecar 未更新」之间，
// sidecar 的 Size 与实测内容长度不一致。此前该文件**永久 fail-closed 且无提示**；现在必须：
//  1. 仍**拒绝下发未校验内容**（C2 口径不变，不静默降级为直通——否则截断/追加会被洗白成可信态）；
//  2. 归类为 `meta.ErrStaleMeta`（可与「sidecar 损坏」区分）+ 专用 Warn（含重建指引）；
//  3. 重建 sidecar（重写文件 / 调 WriteMeta）后恢复「可校验读」，而非需要人工猜。
func TestFilesMetaPolicy_StaleSidecarClassifiedAndRepairable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = dir
	cfg.Volumes = []VolumeConfig{{Name: "default", Root: dir}}
	h := buildVolSetHandlers(t, cfg)
	var logBuf bytes.Buffer
	h.logger = slog.New(slog.NewTextHandler(&logBuf, nil))

	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })

	p := filesMetaPolicy{h: h}
	ctx := context.Background()
	const rel = "alice/user/stale.bin"
	if merr := root.MkdirAll("alice/user", 0o755); merr != nil {
		t.Fatalf("建目录: %v", merr)
	}

	write := func(data []byte) {
		t.Helper()
		f, werr := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if werr != nil {
			t.Fatalf("写内容: %v", werr)
		}
		if _, werr := f.Write(data); werr != nil {
			t.Fatalf("写内容: %v", werr)
		}
		if werr := f.Close(); werr != nil {
			t.Fatalf("关内容: %v", werr)
		}
	}
	openSeek := func() files.SeekReadCloser {
		t.Helper()
		f, oerr := root.OpenFile(rel, os.O_RDONLY, 0)
		if oerr != nil {
			t.Fatalf("打开内容: %v", oerr)
		}
		sr, ok := any(f).(files.SeekReadCloser)
		if !ok {
			t.Fatalf("OpenFile 结果不满足 SeekReadCloser: %T", f)
		}
		return sr
	}
	verifyOK := func(want []byte) {
		t.Helper()
		sr, verr := p.VerifyDownload(ctx, root, rel, openSeek())
		if verr != nil {
			t.Fatalf("VerifyDownload 应通过，got %v", verr)
		}
		if sr == nil {
			t.Fatal("VerifyDownload 应返回校验流")
		}
		got, rerr := io.ReadAll(sr)
		_ = sr.Close()
		if rerr != nil {
			t.Fatalf("校验流读取失败（应逐分块校验通过）: %v", rerr)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("内容 = %d B, want %d B", len(got), len(want))
		}
	}

	long := bytes.Repeat([]byte("new-content-after-crash-"), 40)
	write(long)
	if werr := p.WriteMeta(ctx, "alice", root, rel, nil); werr != nil {
		t.Fatalf("WriteMeta: %v", werr)
	}
	verifyOK(long) // 基线：sidecar 与内容一致 → 校验通过

	// 模拟崩溃窗口：主文件被替换为**更长**内容，sidecar 仍是旧的（未更新）。
	longer := append(append([]byte(nil), long...), bytes.Repeat([]byte("X"), 512)...)
	write(longer)
	_, verr := p.VerifyDownload(ctx, root, rel, openSeek())
	if !errors.Is(verr, meta.ErrStaleMeta) {
		t.Fatalf("应分类为 ErrStaleMeta（陈旧 sidecar），got %v", verr)
	}
	if logs := logBuf.String(); !strings.Contains(logs, "sidecar 陈旧") {
		t.Fatalf("应输出专用告警（含重建指引），log=%s", logs)
	}

	// 重建 sidecar（等价于重写文件 / 运维调 WriteMeta）→ 恢复可校验读。
	if werr := p.WriteMeta(ctx, "alice", root, rel, nil); werr != nil {
		t.Fatalf("重建 WriteMeta: %v", werr)
	}
	verifyOK(longer)

	// 截断（更短）同样归类为陈旧，而不是「损坏」或静默直通。
	write(longer[:len(longer)-7])
	_, verr = p.VerifyDownload(ctx, root, rel, openSeek())
	if !errors.Is(verr, meta.ErrStaleMeta) {
		t.Fatalf("截断也应分类为 ErrStaleMeta，got %v", verr)
	}
	if verr == nil || !strings.Contains(verr.Error(), "重建") {
		t.Fatalf("错误文案应含可操作修复指引，got %v", verr)
	}
}

// TestSeekEndSize 探测内容长度后必须把读写位置复原（否则后续 Read 从末尾开始，静默空读）。
func TestSeekEndSize(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := dir + "/x.bin"
	if err := os.WriteFile(p, []byte("0123456789"), 0o644); err != nil {
		t.Fatalf("写: %v", err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("打开: %v", err)
	}
	defer f.Close()
	n, serr := seekEndSize(f)
	if serr != nil || n != 10 {
		t.Fatalf("seekEndSize = %d, %v；want 10, nil", n, serr)
	}
	buf := make([]byte, 4)
	if _, rerr := f.Read(buf); rerr != nil {
		t.Fatalf("复原后应可从头读: %v", rerr)
	}
	if string(buf) != "0123" {
		t.Fatalf("复原后读到 %q, want 0123（位置未复原）", string(buf))
	}
}
