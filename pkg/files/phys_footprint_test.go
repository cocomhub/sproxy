// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
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
type fakePhysFootprint struct {
	foot    int64
	dirFoot int64
	root    *storage.Root // 真实本地卷根（目录存在性判定用；nil ⇒ 目录足迹不可用）
}

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

// DirChainFootprint / DirPhysBound：目录足迹（本替身默认无目录成本，专项用例用 dirFoot 覆盖）。
//
// 为让「链差分」在测试里真实生效，替身按**真实本地卷的目录存在性**计数：已存在的每一级目录
// 贡献 dirFoot 字节（模拟 secretdata 每级一个 1–2MB 的 dir meta blob）。这样 mkdir 前后/rmdir
// 前后的差分就等于「本次新建/移除的目录级数 × dirFoot」——与生产语义同构。
func (f fakePhysFootprint) DirChainFootprint(volName, owner, remotePath string) (int64, bool) {
	if f.dirFoot <= 0 || f.root == nil {
		return 0, false
	}
	key := "user/" + strings.Trim(remotePath, "/")
	var n int64
	for _, seg := range chainSegments(key) {
		if e, err := f.root.Stat(seg); err == nil && e != nil {
			n += f.dirFoot
		}
	}
	return n, true
}

// chainSegments 返回 key 及其全部祖先（从 user 桶起）。
func chainSegments(key string) []string {
	parts := strings.Split(key, "/")
	out := make([]string, 0, len(parts))
	for i := range parts {
		out = append(out, strings.Join(parts[:i+1], "/"))
	}
	return out
}

func (f fakePhysFootprint) DirPhysBound(volName string) int64 { return f.dirFoot }

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

// TestMakeDir_ChargesDirFootprintAndRemoveReleases：目录按**物理足迹**记账——
// mkdir 收（含递归创建的祖先链）、rmdir 放（同口径），无能力 ⇒ 免费（零回归）。
func TestMakeDir_ChargesDirFootprintAndRemoveReleases(t *testing.T) {
	t.Parallel()
	e := writeTargetEnv(t)
	const dirFoot = int64(1 << 20) // 每级目录 meta blob ≈1MB（模拟 secretdata）
	e.svc.rt.phys = fakePhysFootprint{dirFoot: dirFoot, root: e.tenantFor("alice").Root()}
	ctx := context.Background()

	// 直接预建 `user/` 桶目录（绕过 service ⇒ 不产生记账）⇒ 让「本次新建级数」确定 = 1。
	if err := e.tenantFor("alice").Root().MkdirAll("user", 0o755); err != nil {
		t.Fatalf("预建 user 桶: %v", err)
	}
	// 单级 mkdir ⇒ 记账 1 × dirFoot（`user/` 已存在 ⇒ 差分只含 user/videos）。
	if _, err := e.svc.MakeDir(ctx, "alice", "videos", ""); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	sc := e.quotaScopeFor("alice", "user/videos")
	if sc == nil {
		t.Fatal("目录 Scope 不可用")
	}
	if got := sc.Usage(); got != dirFoot {
		t.Fatalf("mkdir 后 Usage=%d want %d（目录不免费）", got, dirFoot)
	}

	// 多级 mkdir（a/b/c）⇒ 新增 3 级（a, a/b, a/b/c）⇒ 增量必须是 3 × dirFoot（祖先链计入）。
	// 注：未配置目录级配额时 `quotaScopeFor` 对同 owner 的各路径解析到**同一** scope ⇒ 断言用增量。
	before2 := sc.Usage()
	if _, err := e.svc.MakeDir(ctx, "alice", "a/b/c", ""); err != nil {
		t.Fatalf("MakeDir a/b/c: %v", err)
	}
	if got := sc.Usage() - before2; got != 3*dirFoot {
		t.Fatalf("多级 mkdir 增量=%d want %d（祖先链必须计入）", got, 3*dirFoot)
	}

	// rmdir ⇒ 释放该目录自身（videos，1 × dirFoot）；祖先与其它目录不受影响（用增量断言）。
	before3 := sc.Usage()
	if _, err := e.svc.RemoveDir(ctx, "alice", "videos", "", true); err != nil {
		t.Fatalf("RemoveDir: %v", err)
	}
	if got := before3 - sc.Usage(); got != dirFoot {
		t.Fatalf("rmdir 释放=%d want %d（释放必须与写入同口径）", got, dirFoot)
	}
}
