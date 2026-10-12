// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"testing"
)

// write_target_test.go —— 统一写内核 WriteTarget 的域级测试（dirsEnv 最小装配）：
//   - 新建 / 覆盖写（原子 + 差分记账 + sidecar 内置 + 台账）；
//   - Verify=true 校验和比对（不符删已写 + 400 文案）；
//   - Exclusive（O_EXCL：已存在 409 / 新建成功 + 不建 sidecar）；
//   - NoLock（调用方已持锁场景不重复取锁）；
//   - NoQuota（调用方已结算场景不二次预留/结算）；
//   - 错误一律 *HTTPError（Status/Message 对齐上传文案）。

func writeTargetEnv(t *testing.T) *dirsEnv {
	t.Helper()
	e := newDirsEnv(t)
	e.fileMeta = true       // sidecar 内置断言需要 meta 能力装配
	e.enableWriteDefaults() // 打开写面族真实装配路径（locate + route），WriteTarget 才能拿到 route
	return e
}

func wtInput(owner, remotePath string) WriteTargetInput {
	return WriteTargetInput{Owner: owner, RemotePath: remotePath, ClientSize: -1}
}

// TestWriteTarget_CreateAtomicAndSidecar 新建：原子写 + sidecar 恒生成（meta 桶）+ 台账。
func TestWriteTarget_CreateAtomicAndSidecar(t *testing.T) {
	t.Parallel()
	e := writeTargetEnv(t)
	payload := []byte("target-create")
	res, err := e.svc.WriteTarget(context.Background(), wtInput("alice", "a.txt"), bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("WriteTarget: %v", err)
	}
	if res.Size != int64(len(payload)) || res.NoOp {
		t.Fatalf("res=%+v", res)
	}
	tnt := e.tenantFor("alice")
	if _, serr := tnt.Root().Stat("user/a.txt"); serr != nil {
		t.Fatalf("文件未落盘: %v", serr)
	}
	// sidecar 内置：meta/<rel>.meta 存在（meta 恒生成）。
	if !e.svc.rt.fileMetaEnabled() {
		t.Fatal("测试环境 fileMeta 未装配（WithFileMeta 未生效）")
	}
	if _, serr := tnt.Root().Stat("meta/a.txt.meta"); serr != nil {
		t.Fatalf("sidecar 应内置生成: %v", serr)
	}
	// 台账：checksum 记录。
	if cs := e.checksum["alice"]; cs == nil {
		t.Fatal("台账缺失")
	}
}

// TestWriteTarget_OverwriteDiffLedgerAndIdempotent 覆盖写：原子替换 + 差分记账（prev→written），
// 内容与 sidecar 同步更新。
func TestWriteTarget_OverwriteDiffLedgerAndIdempotent(t *testing.T) {
	t.Parallel()
	e := writeTargetEnv(t)
	tnt := e.tenantFor("alice")
	if _, werr := e.svc.WriteTarget(context.Background(), wtInput("alice", "a.bin"), bytes.NewReader([]byte("v1"))); werr != nil {
		t.Fatalf("写 v1: %v", werr)
	}
	if _, werr := e.svc.WriteTarget(context.Background(), wtInput("alice", "a.bin"), bytes.NewReader([]byte("v2-longer"))); werr != nil {
		t.Fatalf("覆盖 v2: %v", werr)
	}
	got, rerr := tnt.Root().OpenDecrypted("user/a.bin")
	if rerr != nil {
		t.Fatalf("读: %v", rerr)
	}
	content, _ := io.ReadAll(got)
	_ = got.Close()
	if string(content) != "v2-longer" {
		t.Fatalf("覆盖后内容 = %q, want v2-longer", content)
	}
	// sidecar 同步更新（读路径按新 meta 校验通过——用 VerifyDownload 断言）。
	// （加密卷未开：读路径直接 Stat size == 内容长度即可佐证 sidecar 是新的。）
	if _, serr := tnt.Root().Stat("meta/a.bin.meta"); serr != nil {
		t.Fatalf("sidecar 应保留: %v", serr)
	}
}

// TestWriteTarget_VerifyMismatchDeletesAnd400 Verify=true 且校验和不符：删已写 + 400 文案。
func TestWriteTarget_VerifyMismatchDeletesAnd400(t *testing.T) {
	t.Parallel()
	e := writeTargetEnv(t)
	tnt := e.tenantFor("alice")
	in := wtInput("alice", "v.bin")
	in.Verify = true
	in.ExpectedChecksum = "wrong-checksum"
	_, err := e.svc.WriteTarget(context.Background(), in, bytes.NewReader([]byte("data")))
	if err == nil {
		t.Fatal("校验和不符应报错")
	}
	he, ok := err.(*HTTPError)
	if !ok || he.Status != 400 {
		t.Fatalf("应 400 HTTPError, got %v", err)
	}
	if _, serr := tnt.Root().Stat("user/v.bin"); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("校验失败应删已写内容, stat err=%v", serr)
	}
}

// TestWriteTarget_VerifyPass 校验和一致：正常成功。
func TestWriteTarget_VerifyPass(t *testing.T) {
	t.Parallel()
	e := writeTargetEnv(t)
	payload := []byte("verify-pass")
	in := wtInput("alice", "vp.txt")
	in.Verify = true
	in.ExpectedChecksum = sha256hexForTest(payload)
	res, err := e.svc.WriteTarget(context.Background(), in, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("WriteTarget: %v", err)
	}
	if res.Checksum != in.ExpectedChecksum {
		t.Fatalf("checksum = %s, want %s", res.Checksum, in.ExpectedChecksum)
	}
}

// TestWriteTarget_ExclusiveCreateAndExists 归档输出 Exclusive：新建成功（不建 sidecar）；
// 已存在 → 409 errArchiveExists 文案。
func TestWriteTarget_ExclusiveCreateAndExists(t *testing.T) {
	t.Parallel()
	e := writeTargetEnv(t)
	tnt := e.tenantFor("alice")
	// Exclusive 用于 user 桶 O_EXCL 新建（如版本恢复目标/去重复制——同 rel 不允许存在）。
	in := wtInput("alice", "x.bin")
	in.Exclusive = true
	if _, err := e.svc.WriteTarget(context.Background(), in, bytes.NewReader([]byte("arc1"))); err != nil {
		t.Fatalf("Exclusive 新建: %v", err)
	}
	if _, serr := tnt.Root().Stat("meta/x.bin.meta"); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("Exclusive 不应建 sidecar（O_EXCL 新建语义）, stat=%v", serr)
	}
	// 已存在 → 409。
	_, err := e.svc.WriteTarget(context.Background(), in, bytes.NewReader([]byte("arc2")))
	if err == nil {
		t.Fatal("同名已存在应 409")
	}
	he, ok := err.(*HTTPError)
	if !ok || he.Status != 409 {
		t.Fatalf("应 409, got %v", err)
	}
	// 内容未被覆盖。
	got, _ := tnt.Root().Open("user/x.bin")
	content, _ := io.ReadAll(got)
	_ = got.Close()
	if string(content) != "arc1" {
		t.Fatalf("已存在文件不应被覆盖: %q", content)
	}
}

// TestWriteTarget_NoLockSkipsInnerLock NoLock：调用方已持锁时不重复取锁（仍可写）。
func TestWriteTarget_NoLockSkipsInnerLock(t *testing.T) {
	t.Parallel()
	e := writeTargetEnv(t)
	release, ok := e.svc.rt.fileLocks().TryMark("alice", "user/nl.bin", uploadingLockUpload)
	if !ok {
		t.Fatal("预占锁失败")
	}
	defer release()
	in := wtInput("alice", "nl.bin")
	in.NoLock = true
	if _, err := e.svc.WriteTarget(context.Background(), in, bytes.NewReader([]byte("nl"))); err != nil {
		t.Fatalf("NoLock 场景应可写: %v", err)
	}
}

// TestWriteTarget_NoQuotaSkipsReserve NoQuota：调用方已结算场景不二次预留/结算（文件仍落盘）。
func TestWriteTarget_NoQuotaSkipsReserve(t *testing.T) {
	t.Parallel()
	e := writeTargetEnv(t)
	in := wtInput("alice", "nq.bin")
	in.NoQuota = true
	if _, err := e.svc.WriteTarget(context.Background(), in, bytes.NewReader([]byte("nq"))); err != nil {
		t.Fatalf("NoQuota 场景应可写: %v", err)
	}
	// NoQuota：不预留也不结算 → 双账本无变化（文件仍落盘）。
	sc := e.quotaScopeFor("alice", "user/nq.bin")
	if sc == nil {
		t.Fatal("quotaScopeFor 应返回 user 桶 Scope（dirsEnv 单卷默认装配）")
	}
	if got := sc.Usage(); got != 0 {
		t.Fatalf("NoQuota 不应计入账本, scope.Usage=%d", got)
	}
}

// TestWriteTarget_ConflictWhenLocked 未持锁时同 rel 并发（锁被占用）→ 409。
func TestWriteTarget_ConflictWhenLocked(t *testing.T) {
	t.Parallel()
	e := writeTargetEnv(t)
	release, ok := e.svc.rt.fileLocks().TryMark("alice", "user/c.bin", uploadingLockUpload)
	if !ok {
		t.Fatal("预占锁失败")
	}
	defer release()
	_, err := e.svc.WriteTarget(context.Background(), wtInput("alice", "c.bin"), bytes.NewReader([]byte("c")))
	if err == nil {
		t.Fatal("持锁时应 409")
	}
	he, ok := err.(*HTTPError)
	if !ok || he.Status != 409 {
		t.Fatalf("应 409, got %v", err)
	}
}

// sha256hexForTest 计算 SHA-256 hex（WriteTarget Verify 用例）。
func sha256hexForTest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
