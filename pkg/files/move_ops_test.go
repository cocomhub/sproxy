// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// move_ops_test.go 钉住跨卷移动的域侧组合语义（外部/加密卷源）：
//   - 复制成功 → **才**删源，且删除调用携带复制时校验过的明文 checksum（TOCTOU 防丢数据）；
//   - 删源失败 → 报错**不谎报成功**（文案标明「已复制到目标，源删除失败」），目标已在；
//   - 源已不存在（并发删除）→ 幂等成功（移动目标已达成）。

// extDeleteCall 记录一次 ExternalDelete 调用（断言卷/路径/checksum）。
type extDeleteCall struct{ vol, path, checksum string }

// fakeExternalOps 是 ExternalVolumeOps 的测试替身：本族用例只触达 ExternalDelete，
// 其余方法返回「未命中」零值（与真实装配的未命中语义一致）。
type fakeExternalOps struct {
	deletes []extDeleteCall
	delOK   bool
	delErr  error
	// renOK 让 ExternalRename 命中（默认 false = 未命中，保持既有用例语义）。
	renOK bool
	// statSize > 0 时 ExternalStat 返回该逻辑大小（默认 -1 = 不可用，保持既有用例语义）。
	statSize int64
}

func (f *fakeExternalOps) ExternalDelete(_ context.Context, _, volName, remotePath, expectedChecksum string) (bool, error) {
	f.deletes = append(f.deletes, extDeleteCall{vol: volName, path: remotePath, checksum: expectedChecksum})
	return f.delOK, f.delErr
}

func (f *fakeExternalOps) ExternalList(context.Context, string, string, string) ([]FileInfo, bool, error) {
	return nil, false, nil
}

func (f *fakeExternalOps) ExternalStat(context.Context, string, string, string) (int64, string, error) {
	if f.statSize > 0 {
		return f.statSize, "", nil
	}
	return -1, "", nil
}

func (f *fakeExternalOps) ExternalCopy(context.Context, string, string, string, string, string, string, bool) (ExternalCopyResult, error) {
	return ExternalCopyResult{}, nil
}

func (f *fakeExternalOps) ExternalMkdir(context.Context, string, string, string) (bool, error) {
	return false, nil
}

func (f *fakeExternalOps) ExternalRemoveDir(context.Context, string, string, string, bool) (bool, error) {
	return false, nil
}

func (f *fakeExternalOps) ExternalRename(context.Context, string, string, string, string, string) (bool, error) {
	return f.renOK, nil
}

// ExternalMeta / ExternalMetaUpdate：本族用例默认未命中（meta 读写在 files/meta_update 用例覆盖）。
func (f *fakeExternalOps) ExternalMeta(context.Context, string, string, string) (map[string]any, bool, error) {
	return nil, false, nil
}

func (f *fakeExternalOps) ExternalMetaUpdate(context.Context, string, string, string, map[string]any) (bool, error) {
	return false, nil
}

// moveEnv 构造带外部卷接缝替身的域环境（单卷默认视图 + 假接缝）。
func moveEnv(t *testing.T, ops *fakeExternalOps) *dirsEnv {
	t.Helper()
	env := newDirsEnv(t)
	env.externalOps = ops
	env.routeUpload = env.routeUploadDefault // 真实账本路由（空 route 会被 WriteFile 判 400）
	env.rebuild()
	return env
}

// targetFile 读回默认租户根下的 user 桶文件（<owner> 根由夹具懒建）。
func targetFile(t *testing.T, env *dirsEnv, owner, rel string) ([]byte, error) {
	t.Helper()
	return os.ReadFile(filepath.Join(env.root, owner, "user", rel))
}

func TestMoveFile_ExternalSource_CopiesThenDeletesSource(t *testing.T) {
	t.Parallel()
	ops := &fakeExternalOps{delOK: true}
	env := moveEnv(t, ops)
	content := []byte("move-me-payload-0123456789")
	sum := sha256Hex(content)

	res, err := env.svc.MoveFile(context.Background(), MoveFileInput{
		Owner: "alice", From: "moved.bin", To: "moved.bin", Src: &fakeSource{data: content, checksum: sum},
	})
	if err != nil {
		t.Fatalf("MoveFile: %v", err)
	}
	if res.From != "moved.bin" || res.To != "moved.bin" || res.Size != int64(len(content)) {
		t.Fatalf("结果异常: %+v", res)
	}
	// 目标已写入且内容一致。
	got, rerr := targetFile(t, env, "alice", "moved.bin")
	if rerr != nil {
		t.Fatalf("目标应存在: %v", rerr)
	}
	if string(got) != string(content) {
		t.Fatalf("目标内容不一致: %q", got)
	}
	// 删源恰一次，携带复制时校验过的明文 checksum（源并发改写 → 删源被 checksum 判据拦下）。
	if len(ops.deletes) != 1 {
		t.Fatalf("ExternalDelete 调用 %d 次, want 1", len(ops.deletes))
	}
	if d := ops.deletes[0]; d.path != "moved.bin" || d.checksum != sum {
		t.Fatalf("删源参数异常: %+v（want path=moved.bin checksum=%s）", d, sum)
	}
}

func TestMoveFile_SourceDeleteFailure_ReportsPartial(t *testing.T) {
	t.Parallel()
	ops := &fakeExternalOps{delErr: errors.New("远端删除失败")}
	env := moveEnv(t, ops)
	content := []byte("partial-move-payload")

	_, err := env.svc.MoveFile(context.Background(), MoveFileInput{
		Owner: "alice", From: "p.bin", To: "p.bin", Src: &fakeSource{data: content, checksum: sha256Hex(content)},
	})
	if err == nil {
		t.Fatal("删源失败必须报错（不得谎报成功）")
	}
	if !strings.Contains(err.Error(), "已复制到目标，源删除失败") {
		t.Fatalf("错误文案应标明半完成状态, got: %v", err)
	}
	// 目标已经写入（复制先于删源）——用户据此知道只需清理源。
	if _, rerr := targetFile(t, env, "alice", "p.bin"); rerr != nil {
		t.Fatalf("目标应已写入: %v", rerr)
	}
}

func TestMoveFile_SourceGone_IdempotentSuccess(t *testing.T) {
	t.Parallel()
	ops := &fakeExternalOps{delOK: false} // 源已被并发删除 → 未命中
	env := moveEnv(t, ops)
	content := []byte("concurrent-delete-payload")

	res, err := env.svc.MoveFile(context.Background(), MoveFileInput{
		Owner: "alice", From: "c.bin", To: "c.bin", Src: &fakeSource{data: content, checksum: sha256Hex(content)},
	})
	if err != nil {
		t.Fatalf("源已不存在应按幂等成功, got: %v", err)
	}
	if res.Size != int64(len(content)) {
		t.Fatalf("结果大小异常: %+v", res)
	}
	if _, rerr := targetFile(t, env, "alice", "c.bin"); rerr != nil {
		t.Fatalf("目标应已写入: %v", rerr)
	}
}

// fakeRenamePhys 是 files.VolumePhysFootprinter 的测试替身：**按路径**返回物理足迹，用来模拟
// 「外部卷（secretdata）改名 = 只重写 meta blob ⇒ meta pad 随机 ⇒ 物理足迹变化」。
type fakeRenamePhys struct {
	perPath map[string]int64
	bound   int64
}

func (f fakeRenamePhys) PhysFootprint(_, _, remotePath string) (int64, bool) {
	n, ok := f.perPath[remotePath]
	if !ok || n <= 0 {
		return 0, false
	}
	return n, true
}

func (f fakeRenamePhys) PhysBound(_ string, logical int64) int64 {
	if f.bound > 0 {
		return f.bound
	}
	return logical
}

func (f fakeRenamePhys) DirChainFootprint(_, _, _ string) (int64, bool) { return 0, false }
func (f fakeRenamePhys) DirPhysBound(_ string) int64                    { return 0 }

// renameExternalEnv 构造「含一个无本地 Root 的卷 = 外部卷」的环境（外部判据与生产同源）。
func renameExternalEnv(t *testing.T, ops *fakeExternalOps) *dirsEnv {
	t.Helper()
	env := newDirsEnv(t)
	env.externalOps = ops
	env.routeUpload = env.routeUploadDefault
	env.pools = map[string]*quota.Pool{"vault": quota.NewPool(0)}
	env.volSet = registry.NewSet(
		[]volume.Volume{{Name: "vault", RootDir: t.TempDir()}},
		map[string]*storage.Root{}, // 无本地 Root ⇒ 外部卷
		nil, env.pools, "vault")
	env.rebuild()
	return env
}

// TestRenameFile_ExternalVolume_SettlesByMeasuredFootprint：外部卷改名必须按**前后实测足迹**
// 调整配额（源释放 + 目标结算 + 卷池差分）——改名只重写 meta blob 而 meta pad 随机 ⇒ 物理足迹
// 变化（本用例 ±2MB），不调整即账本与磁盘漂移（少记 = 幽灵占用/假 507）。
func TestRenameFile_ExternalVolume_SettlesByMeasuredFootprint(t *testing.T) {
	t.Parallel()
	const (
		footBefore = int64(1 << 20)
		footAfter  = int64(3 << 20)
		bound      = int64(8 << 20)
	)
	ops := &fakeExternalOps{renOK: true, statSize: 4096}
	env := renameExternalEnv(t, ops)
	env.svc.rt.phys = fakeRenamePhys{
		perPath: map[string]int64{"a.bin": footBefore, "b.bin": footAfter},
		bound:   bound,
	}
	ctx := context.Background()
	sc := env.quotaScopeFor("alice", "user/a.bin")
	if sc == nil {
		t.Fatal("Scope 不可用")
	}
	pool := env.pools["vault"]
	if pool == nil {
		t.Fatal("卷池不可用")
	}
	before, beforePool := sc.Usage(), pool.Usage()

	if _, err := env.svc.RenameFile(ctx, RenameFileInput{
		Owner: "alice", From: "a.bin", To: "b.bin", ExplicitVol: "vault", ExpectedChecksum: "cs",
	}); err != nil {
		t.Fatalf("RenameFile(外部卷): %v", err)
	}
	// 净变化 = 改名后实测 − 改名前进账（目标结算 footAfter、源释放 footBefore）。
	if got := sc.Usage() - before; got != footAfter-footBefore {
		t.Fatalf("owner 配额净变化=%d want %d（应按实测差调整）", got, footAfter-footBefore)
	}
	if got := pool.Usage() - beforePool; got != footAfter-footBefore {
		t.Fatalf("卷池净变化=%d want %d（差分必须双向反映）", got, footAfter-footBefore)
	}
}

// TestRenameFile_ExternalVolume_NoCapabilityNoQuotaChange：无该能力 ⇒ 零回归（不动账）。
func TestRenameFile_ExternalVolume_NoCapabilityNoQuotaChange(t *testing.T) {
	t.Parallel()
	ops := &fakeExternalOps{renOK: true, statSize: 4096}
	env := renameExternalEnv(t, ops)
	ctx := context.Background()
	sc := env.quotaScopeFor("alice", "user/a.bin")
	before := sc.Usage()
	if _, err := env.svc.RenameFile(ctx, RenameFileInput{
		Owner: "alice", From: "a.bin", To: "b.bin", ExplicitVol: "vault", ExpectedChecksum: "cs",
	}); err != nil {
		t.Fatalf("RenameFile(外部卷): %v", err)
	}
	if got := sc.Usage(); got != before {
		t.Fatalf("无能力时不应动账: %d → %d", before, got)
	}
}
