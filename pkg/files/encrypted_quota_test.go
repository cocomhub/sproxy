// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// encrypted_quota_test.go 钉住 **加密卷按物理占用记账**（2026-10-14 容量口径收口）：
// 加密块每 64KiB 多 32B（4B 长度前缀 + nonce + GCM tag）+ 22B 文件头 ⇒ 按逻辑长度记账会低估；
// 卷池靠周期对账收敛，**owner 配额侧永不收敛** ⇒ 短时可超限。判据 = owner 配额 Usage 与
// **磁盘物理字节数**逐字节相等（不是「大约」）。
//
// 变异：PhysSize 不装配 ⇒ Usage 退回逻辑长度（本用例红）；Commit 不折算 ⇒ 同上。

// encryptedEnv 建加密卷环境（alice 的租户根注入 at-rest 加密）。
func encryptedEnv(t *testing.T) *dirsEnv {
	t.Helper()
	e := writeTargetEnv(t)
	if serr := e.tenantFor("alice").Root().SetEncryption(make([]byte, 32)); serr != nil {
		t.Fatalf("SetEncryption: %v", serr)
	}
	if !e.tenantFor("alice").Root().IsEncrypted() {
		t.Fatal("应报告为加密卷")
	}
	return e
}

// TestWriteTarget_EncryptedVolumeChargesPhysical：加密卷写入 ⇒ 配额与磁盘**物理**字节一致。
func TestWriteTarget_EncryptedVolumeChargesPhysical(t *testing.T) {
	t.Parallel()
	e := encryptedEnv(t)
	payload := bytes.Repeat([]byte("x"), 100_000) // 跨 2 个 64KiB 块
	if _, err := e.svc.WriteTarget(context.Background(), WriteTargetInput{
		Owner: "alice", RemotePath: "enc.bin", ClientSize: int64(len(payload)),
	}, bytes.NewReader(payload)); err != nil {
		t.Fatalf("WriteTarget: %v", err)
	}
	want := storage.EncryptedPhysicalSize(int64(len(payload)))
	sc := e.quotaScopeFor("alice", "user/enc.bin")
	if sc == nil {
		t.Fatal("quotaScopeFor 应返回 Scope")
	}
	if got := sc.Usage(); got != want {
		t.Fatalf("加密卷配额 Usage=%d want %d（物理口径未生效 ⇒ 低估 %d 字节）", got, want, want-got)
	}
	// 磁盘真值：物理文件字节数必须等于记账值（口径同源）。
	phys, serr := os.Stat(filepath.Join(e.tenantFor("alice").Root().AbsPath(), "user", "enc.bin"))
	if serr != nil {
		t.Fatalf("Stat 物理文件: %v", serr)
	}
	if phys.Size() != want {
		t.Fatalf("磁盘物理 %d != 记账 %d", phys.Size(), want)
	}
}

// TestWriteTarget_PlainVolumeChargesLogical：非加密卷零回归——按逻辑长度记账（恒等口径）。
func TestWriteTarget_PlainVolumeChargesLogical(t *testing.T) {
	t.Parallel()
	e := writeTargetEnv(t)
	payload := bytes.Repeat([]byte("y"), 100_000)
	if _, err := e.svc.WriteTarget(context.Background(), WriteTargetInput{
		Owner: "alice", RemotePath: "plain.bin", ClientSize: int64(len(payload)),
	}, bytes.NewReader(payload)); err != nil {
		t.Fatalf("WriteTarget: %v", err)
	}
	if got := e.quotaScopeFor("alice", "user/plain.bin").Usage(); got != int64(len(payload)) {
		t.Fatalf("非加密卷 Usage=%d want %d（不应引入物理折算）", got, len(payload))
	}
}

// TestTenantPhysSizer_OnlyEncrypted：口径装配判据（加密 ⇒ 折算函数；否则 nil = 恒等）。
func TestTenantPhysSizer_OnlyEncrypted(t *testing.T) {
	t.Parallel()
	e := writeTargetEnv(t)
	tnt := e.tenantFor("alice")
	if f := TenantPhysSizer(tnt); f != nil {
		t.Fatal("非加密卷不应装配物理折算")
	}
	if f := TenantPhysSizer(nil); f != nil {
		t.Fatal("nil 租户不应装配物理折算")
	}
	if serr := tnt.Root().SetEncryption(make([]byte, 32)); serr != nil {
		t.Fatalf("SetEncryption: %v", serr)
	}
	f := TenantPhysSizer(tnt)
	if f == nil {
		t.Fatal("加密卷必须装配物理折算")
	}
	if got, want := f(1000), storage.EncryptedPhysicalSize(1000); got != want {
		t.Fatalf("折算函数 = %d want %d", got, want)
	}
}

// TestUploadRoute_PhysicalSizeBoundaries：折算口径边界（nil/非正值恒等；加密卷跨块）。
func TestUploadRoute_PhysicalSizeBoundaries(t *testing.T) {
	t.Parallel()
	plain := UploadRoute{}
	if plain.PhysicalSize(123) != 123 {
		t.Fatal("nil 折算应恒等")
	}
	enc := UploadRoute{PhysSize: storage.EncryptedPhysicalSize}
	for _, n := range []int64{0, -5} {
		if got := enc.PhysicalSize(n); got != n {
			t.Fatalf("非正值 %d 应原样返回，got %d", n, got)
		}
	}
	if got, want := enc.PhysicalSize(65536), storage.EncryptedPhysicalSize(65536); got != want {
		t.Fatalf("跨块折算 = %d want %d", got, want)
	}
}
