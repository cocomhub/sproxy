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
}

func (f *fakeExternalOps) ExternalDelete(_ context.Context, _, volName, remotePath, expectedChecksum string) (bool, error) {
	f.deletes = append(f.deletes, extDeleteCall{vol: volName, path: remotePath, checksum: expectedChecksum})
	return f.delOK, f.delErr
}

func (f *fakeExternalOps) ExternalList(context.Context, string, string, string) ([]FileInfo, bool, error) {
	return nil, false, nil
}

func (f *fakeExternalOps) ExternalStat(context.Context, string, string, string) (int64, string, error) {
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
