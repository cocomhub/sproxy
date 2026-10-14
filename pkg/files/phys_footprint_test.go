// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"bytes"
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// phys_footprint_test.go 钉住**卷自报物理足迹**的记账口径（配额模型 v3：只按物理占用记账）。
//
// 背景：物理量含**逻辑不可推**的部分——secretdata 的 meta blob pad 到 [base, 2·base)
// （默认 1–2MB/文件，尺寸混淆设计，与逻辑大小无关）、XOR parity（可达逻辑 2×）⇒ 按逻辑长度
// 记账在小文件场景下物理可达逻辑的**千倍**。故写内核必须采信**卷自报的实测足迹**。
//
// 本文件用假能力替身（不依赖 secretdata 包，锁定的是**写内核的口径**）：
//   - 写入 ⇒ owner 配额用量 == 卷自报足迹（而非逻辑长度）；
//   - 删除 ⇒ 释放**同一量**（同口径）⇒ 归零（少释放 = 幽灵占用/假 507）。
//
// 变异：settleSize 去掉能力分支（回落逻辑）⇒ 第一条红；删除回落逻辑 ⇒ 第二条红。

// fakePhysFootprint 是 files.VolumePhysFootprinter 的测试替身：固定报告 foot 字节。
type fakePhysFootprint struct{ foot int64 }

func (f fakePhysFootprint) PhysFootprint(volName, owner, remotePath string) (int64, bool) {
	if f.foot <= 0 {
		return 0, false
	}
	return f.foot, true
}

func (f fakePhysFootprint) PhysBound(volName string, logical int64) int64 {
	if f.foot <= 0 {
		return logical
	}
	return logical + f.foot
}

// TestWriteTarget_VolumeReportedFootprintWins：写内核按卷自报足迹记账（逻辑长度不参与）。
func TestWriteTarget_VolumeReportedFootprintWins(t *testing.T) {
	t.Parallel()
	e := writeTargetEnv(t)
	const foot = int64(3 << 20) // 1KB 文件在 secretdata 上物理 ≈3MB（meta pad + 内容）
	e.svc.rt.phys = fakePhysFootprint{foot: foot}
	ctx := context.Background()
	payload := bytes.Repeat([]byte("z"), 1024)

	if _, err := e.svc.WriteTarget(ctx, WriteTargetInput{
		Owner: "alice", RemotePath: "tiny.bin", ClientSize: 1024,
	}, bytes.NewReader(payload)); err != nil {
		t.Fatalf("WriteTarget: %v", err)
	}
	sc := e.quotaScopeFor("alice", "user/tiny.bin")
	if sc == nil {
		t.Fatal("user 桶 Scope 不可用")
	}
	if got := sc.Usage(); got != foot {
		t.Fatalf("写入后 Usage=%d want %d（应采信卷自报物理足迹，而非逻辑 1024）", got, foot)
	}

	// 删除：必须释放**同一量**（同口径）⇒ 归零。少释放会让配额永久虚高（幽灵占用/假 507）。
	if _, err := e.svc.DeleteFile(ctx, DeleteFileInput{
		Owner: "alice", RemotePath: "tiny.bin", SoftDelete: false,
		ExpectedChecksum: testutil.SHA256Hex(payload),
	}); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if got := sc.Usage(); got != 0 {
		t.Fatalf("删除后 Usage=%d want 0（释放必须与写入同口径）", got)
	}
}

// TestWriteTarget_NoCapabilityFallsBackLogical：无该能力（明文卷）⇒ 零回归（按逻辑长度记账）。
func TestWriteTarget_NoCapabilityFallsBackLogical(t *testing.T) {
	t.Parallel()
	e := writeTargetEnv(t)
	ctx := context.Background()
	payload := bytes.Repeat([]byte("y"), 2048)
	if _, err := e.svc.WriteTarget(ctx, WriteTargetInput{
		Owner: "alice", RemotePath: "plain.bin", ClientSize: 2048,
	}, bytes.NewReader(payload)); err != nil {
		t.Fatalf("WriteTarget: %v", err)
	}
	sc := e.quotaScopeFor("alice", "user/plain.bin")
	if got := sc.Usage(); got != 2048 {
		t.Fatalf("明文卷 Usage=%d want 2048（无能力 ⇒ 恒等，零回归）", got)
	}
}
