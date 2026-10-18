// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// fakeStagingQuota 是 syncpkg.StagingQuotaTracker 的测试替身（按上限预留、记账用量与峰值）。
type fakeStagingQuota struct {
	mu    sync.Mutex
	limit int64
	used  int64
	peak  int64
	calls int
}

func (f *fakeStagingQuota) ReserveUsage(_ context.Context, n int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.used+n > f.limit {
		return fmt.Errorf("fake: staging 配额不足（used=%d + n=%d > limit=%d）", f.used, n, f.limit)
	}
	f.used += n
	if f.used > f.peak {
		f.peak = f.used
	}
	return nil
}

func (f *fakeStagingQuota) ReleaseUsage(n int64) {
	f.mu.Lock()
	f.used -= n
	if f.used < 0 {
		f.used = 0
	}
	f.mu.Unlock()
}

func (f *fakeStagingQuota) snapshot() (used, peak int64, calls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.used, f.peak, f.calls
}

// TestSecretdata_StagingQuota_SelfManaged 钉住「本地临时物由本卷**自管** staging 配额」
// （2026-10-18，复用装配层既有的独立 staging Scope 与 StagingQuotaTracker，不另造机制）：
//   - 未注入配额 ⇒ 零回归（写成功、不记账）；
//   - 注入且**不足以容纳**本地临时物 ⇒ 写 **fail-closed**（不落半态、不写坏数据）；
//   - 注入且足够 ⇒ 写成功且**预留被释放**（多次写不泄漏 ⇒ 不会把磁盘配额长期占死）。
func TestSecretdata_StagingQuota_SelfManaged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	payload := bytes.Repeat([]byte("staging-quota-"), 4096) // 56KiB

	// ① 未注入 ⇒ 直通（零回归）。
	raw1 := syncpkg.NewLocalFS(t.TempDir(), nil)
	fs1, err := NewFS(raw1, Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if err := fs1.WriteFile(ctx, syncpkg.LogicalKey{}, "a.bin", bytes.NewReader(payload), int64(len(payload)), 0); err != nil {
		t.Fatalf("未注入配额时写应成功: %v", err)
	}

	// ② 配额过小 ⇒ fail-closed。
	raw2 := syncpkg.NewLocalFS(t.TempDir(), nil)
	fs2, err2 := NewFS(raw2, Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir()})
	if err2 != nil {
		t.Fatalf("NewFS: %v", err2)
	}
	tight := &fakeStagingQuota{limit: 1} // 1 字节：任何写都放不下
	fs2.WithStagingQuota(tight)
	if err := fs2.WriteFile(ctx, syncpkg.LogicalKey{}, "b.bin", bytes.NewReader(payload), int64(len(payload)), 0); err == nil {
		t.Fatal("本地 staging 配额不足时写必须 fail-closed（防打爆磁盘）✗")
	}
	if tightUsed, _, calls := tight.snapshot(); tightUsed != 0 {
		t.Errorf("预留失败后不得留下占用（used=%d）", tightUsed)
	} else if calls == 0 {
		t.Error("探针未生效：ReserveUsage 未被调用（判据无效）")
	}

	// ③ 配额足够 ⇒ 成功且释放；连续多次写不泄漏。
	raw3 := syncpkg.NewLocalFS(t.TempDir(), nil)
	fs3, err3 := NewFS(raw3, Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir()})
	if err3 != nil {
		t.Fatalf("NewFS: %v", err3)
	}
	q := &fakeStagingQuota{limit: 8 << 20} // 8MiB：够一次写
	fs3.WithStagingQuota(q)
	for i := range 3 {
		rel := fmt.Sprintf("c%d.bin", i)
		if err := fs3.WriteFile(ctx, syncpkg.LogicalKey{}, rel, bytes.NewReader(payload), int64(len(payload)), 0); err != nil {
			t.Fatalf("第 %d 次写失败（配额应足够）: %v", i, err)
		}
		if leaked, _, _ := q.snapshot(); leaked != 0 {
			t.Fatalf("第 %d 次写后预留未释放（used=%d）⇒ 配额被长期占死 ✗", i, leaked)
		}
	}
	if _, peak, _ := q.snapshot(); peak <= int64(len(payload)) {
		t.Errorf("峰值 %d 应覆盖「明文 + meta pad 上界」（判据过弱）", peak)
	}
}
