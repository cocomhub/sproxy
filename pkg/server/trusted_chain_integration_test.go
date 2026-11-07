// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// trusted_chain_integration_test.go 是**生产组合链**的运行期贯穿测试（M7，第 7 轮 MAJOR）：
// 此前只有单装饰器测试（各包内），而真实装配是
//
//	Gate(StagingQuota) ∘ Guard ∘ Wrap(可信卷) ∘ CapacityFS(LocalFS)
//
// 任意一层漏转发可选能力（Provider/Updater/RangeReader/WriteIfAbsent/…）编译仍过、单测仍绿，
// 但上层断言恒 false → **静默降级**（本分支已发生过 3 次同类：staging 门卫恒被跳过、Provider
// 探测失败、RangeReader/DirectURL 落空）。本测试钉住「整链可用」这一最终事实：
// 写 → 读 sidecar（Provider，经 Innermost 下探）→ UpdateMetaExtra → 读回逐分块校验。

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	volcap "github.com/cocomhub/sproxy/pkg/volume/capacity"
	"github.com/cocomhub/sproxy/pkg/volume/trusted"
)

// recordingTracker 是 staging 配额钩子的观测替身（断言整链确实预留/释放）。
type recordingTracker struct {
	reserved int64
	released int64
}

func (r *recordingTracker) ReserveUsage(_ context.Context, size int64) error {
	r.reserved += size
	return nil
}
func (r *recordingTracker) ReleaseUsage(size int64) { r.released += size }

// TestTrustedChain_FullStackWriteMetaVerify 整链贯穿：Gate ∘ Guard ∘ Wrap ∘ CapacityFS。
// 变异（任一层去掉能力转发）→ 对应断言红：Provider 读不到 sidecar / UpdateMetaExtra 失败 /
// RangeReader 断言落空 / staging 预留为 0。
func TestTrustedChain_FullStackWriteMetaVerify(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })

	// 生产链路（顺序与装配一致）：CapacityFS 在卷侧，Wrap 生成 sidecar，Guard 隔离 meta 桶，
	// Gate 是装配层最外层的 staging 强制门卫。
	base := syncpkg.NewLocalFS(filepath.ToSlash(filepath.Join(dir, "anonymous")), testLogger())
	led, lerr := volcap.OpenLedger(t.TempDir(), "test")
	if lerr != nil {
		t.Fatal(lerr)
	}
	capFS := volcap.Wrap(base, volcap.NewLedgerCounter(led, 0))
	wrapped := trusted.Wrap(capFS, trusted.Options{Logger: testLogger()})
	guarded := trusted.Guard(wrapped)
	tracker := &recordingTracker{}
	chain := syncpkg.WrapStagingQuota(guarded, tracker)

	ctx := context.Background()
	// ① 可选能力断言（经 Innermost 下探：装饰层无条件回显会让裸断言恒真）。
	if syncpkg.AssertRangeReader(chain) == nil {
		t.Fatal("整链应暴露 RangeReader（CapacityFS/Guard/Wrap/Gate 逐层转发）")
	}
	if _, ok := syncpkg.Innermost(chain).(syncpkg.RangeReader); !ok {
		t.Fatal("最内层原始卷应实现 RangeReader（下探未被装饰层污染）")
	}
	rel := "user/docs/a.bin"
	body := bytes.Repeat([]byte("chain-"), 200)
	// ② 写整链（Gate 预留 → Wrap 写 sidecar → CapacityFS 记账）。
	if werr := chain.WriteFile(ctx, syncpkg.LogicalKey{}, rel, bytes.NewReader(body), int64(len(body)), 0); werr != nil {
		t.Fatalf("整链 WriteFile: %v", werr)
	}
	if tracker.reserved != int64(len(body)) || tracker.released != int64(len(body)) {
		t.Fatalf("staging 保留/释放应对称（M7：Gate 被跳过则恒 0）: reserved=%d released=%d",
			tracker.reserved, tracker.released)
	}
	// ③ Provider 能力经 Guard/Gate 可用（meta.Provider 断言 + 真实读到写入的 sidecar）。
	pv, ok := any(chain).(meta.Provider)
	if !ok {
		t.Fatal("整链应暴露 meta.Provider（Guard/Wrap 转发）")
	}
	fm, ferr := pv.FileMeta(ctx, rel)
	if ferr != nil || fm == nil {
		t.Fatalf("应读到 sidecar（Provider 转发）: fm=%v err=%v", fm, ferr)
	}
	if fm.TotalSHA256 == "" || fm.Size != int64(len(body)) {
		t.Fatalf("sidecar 内容不符: %+v", fm)
	}
	// ④ Updater 能力可用：写入 extra 标记并读回确认。
	ue, ok := any(chain).(interface {
		UpdateMetaExtra(context.Context, string, map[string]any) error
	})
	if !ok {
		t.Fatal("整链应暴露 metaExtraUpdater（UpdateMetaExtra 转发）")
	}
	if uerr := ue.UpdateMetaExtra(ctx, rel, map[string]any{"chain_probe": "ok"}); uerr != nil {
		t.Fatalf("UpdateMetaExtra 经整链应成功（Updater 转发）: %v", uerr)
	}
	fm2, ferr2 := pv.FileMeta(ctx, rel)
	if ferr2 != nil || fm2 == nil || fm2.Extra["chain_probe"] != "ok" {
		t.Fatalf("extra 标记应可读回: fm=%+v err=%v", fm2, ferr2)
	}
	// ⑤ 读回逐分块校验：VerifyReadSeeker 全量读通过，且篡改后 fail-closed。
	rc, oerr := chain.OpenRead(ctx, rel)
	if oerr != nil {
		t.Fatalf("OpenRead: %v", oerr)
	}
	raw, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil || !bytes.Equal(raw, body) {
		t.Fatalf("读回内容不符: len=%d err=%v", len(raw), rerr)
	}
	verifier := meta.VerifyReadSeeker(bytes.NewReader(raw), fm2)
	if _, verr := io.Copy(io.Discard, verifier); verr != nil {
		t.Fatalf("整链写出的 sidecar 应可通过逐分块校验（写出即可读）: %v", verr)
	}
	tampered := append([]byte(nil), raw...)
	tampered[0] ^= 0xFF
	vbad := meta.VerifyReadSeeker(bytes.NewReader(tampered), fm2)
	if _, verr := io.Copy(io.Discard, vbad); verr == nil {
		t.Fatal("篡改内容必须 fail-closed")
	}
}

// TestTrustedChain_StagingQuotaIsIndependentRootPool 回归（M5）：staging Scope 挂**独立根池**
// ——受 staging_quota_bytes 约束，不牵连 globalPool（max_storage_bytes）；否则慢速外部卷上传
// 长期占暂存会抬高全局 usage、误拒无关 owner 的正常写。
// 变异：sc 改回 h.globalPool.Scope → 全局 pool 的 usage 随 staging 变化 → 红。
func TestTrustedChain_StagingQuotaIsIndependentRootPool(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(100) // 全局 100B
	h := &Handlers{globalPool: pool}
	// staging_quota_bytes 未配置（0）→ staging 池不设上限，但**不侵入全局账本**。
	cfg := &Config{}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	h.cfgPtr = &cfgPtr

	sc := h.stagingQuotaScope("alice")
	if sc == nil {
		t.Fatal("staging scope 应可用")
	}
	res, err := sc.TryReserve(80)
	if err != nil {
		t.Fatalf("独立 staging 池（上限 0=不限）应可预留: %v", err)
	}
	res.Commit(80)
	// 全局池不得因 staging 记账而抬高（M5：此前沿父链累加 → 无关 owner 被 507）。
	if used := pool.Usage(); used != 0 {
		t.Fatalf("staging 记账不得侵入全局池（M5）: globalPool.Usage()=%d want 0", used)
	}
	// 无关 owner 的正常写（90B ≤ 全局 100B）不得被拒。
	other, oerr := pool.Scope("/tenant/bob", 0).TryReserve(90)
	if oerr != nil {
		t.Fatalf("无关 owner 的写不得被 staging 占用误拒（M5）: %v", oerr)
	}
	other.Release()
}
