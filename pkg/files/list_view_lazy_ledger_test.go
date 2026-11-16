// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// list_view_lazy_ledger_test.go 钉住 list 视图路径的**台账惰性获取**（性能优化）：
//
// 背景：`checksumStoreReady` 在台账为空时会触发 `RebuildChecksums`——那是**全卷遍历 + 逐文件读
// sidecar** 级开销。S2/自报卷（secretdata/s3）已能在列举时给出 checksum，此时**不应**再触达台账
// （否则一次列表就白跑一次全卷重建）。
//
// 判据：用**计数型** FileMetaPolicy 观测「是否触达 sidecar 读」。
//   - 视图条目**全部自报** checksum ⇒ 计数为 0（未触发重建）；
//   - 视图条目**未自报** ⇒ 计数 > 0（走台账兜底并触发一次重建，语义不变）。
//
// 变异：把 `fileInfosFromViewEntries` 的台账获取改回**热路径无条件调用** ⇒ 第一例计数 > 0 ⇒ 红。

import (
	"context"
	"io"
	"os"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// countingMetaPolicy 只计数 `ReadMetaRaw`（重建期的 sidecar 读），不写任何东西。
type countingMetaPolicy struct {
	testMetaPolicy
	reads atomic.Int64
}

func (p *countingMetaPolicy) ReadMetaRaw(context.Context, *storage.Root, string) ([]byte, error) {
	p.reads.Add(1)
	return nil, os.ErrNotExist // 无 sidecar：仅用于观测「是否触达台账/自愈」
}

// 其余方法沿用 testMetaPolicy（值接收者）；此处显式断言接口满足。
var _ FileMetaPolicy = (*countingMetaPolicy)(nil)

// seedLocalFile 直接在租户根写文件（**不经服务**，故台账为空 ⇒ 逼出 checksumStoreReady 的自愈路径）。
func seedLocalFile(t *testing.T, env *dirsEnv, rel string) {
	t.Helper()
	tnt := env.tenantFor("alice")
	if tnt == nil || tnt.Root() == nil {
		t.Fatal("租户未装配")
	}
	if err := tnt.Root().MkdirAll("user/docs", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	f, err := tnt.Root().OpenFile(rel, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	_, _ = io.WriteString(f, "x")
	if cerr := f.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
}

// newLazyLedgerEnv 构造「本地卷 + 计数型 meta 策略 + 可配视图自报」的列表环境。
func newLazyLedgerEnv(t *testing.T, viewChecksum func(string) string) (*dirsEnv, *countingMetaPolicy) {
	t.Helper()
	env := newDirsEnv(t)
	env.enableVolumes(t, "default")
	env.enableWriteDefaults()
	pol := &countingMetaPolicy{}
	env.metaPolicy = pol
	env.viewList = &viewListSet{env: env, viewChecksum: viewChecksum}
	env.rebuild()
	return env, pol
}

// TestList_ViewSelfReportSkipsLedgerRebuild 全部条目自报 checksum ⇒ **不得**触达台账/自愈。
func TestList_ViewSelfReportSkipsLedgerRebuild(t *testing.T) {
	t.Parallel()
	env, pol := newLazyLedgerEnv(t, func(string) string { return "self-sha" })
	seedLocalFile(t, env, "user/docs/a.txt")

	files := listFileInfos(t, env, "docs")
	if len(files) != 1 || files[0].Name != "a.txt" {
		t.Fatalf("应列出 a.txt，got %+v", files)
	}
	if files[0].Checksum != "self-sha" {
		t.Fatalf("checksum 应取视图自报值，got %q", files[0].Checksum)
	}
	if n := pol.reads.Load(); n != 0 {
		t.Fatalf("条目已自报 checksum 时不得触达台账/自愈（实测 sidecar 读 %d 次）", n)
	}
}

// TestList_ViewMissFallsBackToLedger 条目未自报 ⇒ 仍走台账兜底（并可触发一次重建），语义不变。
func TestList_ViewMissFallsBackToLedger(t *testing.T) {
	t.Parallel()
	env, pol := newLazyLedgerEnv(t, nil) // 不自报 ⇒ 需要台账兜底
	seedLocalFile(t, env, "user/docs/a.txt")

	files := listFileInfos(t, env, "docs")
	if len(files) != 1 {
		t.Fatalf("应列出 1 条，got %+v", files)
	}
	if n := pol.reads.Load(); n == 0 {
		t.Fatal("未自报时应触达台账兜底（空台账 ⇒ 自愈重建会读 sidecar）")
	}
}
