// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/cocomhub/sproxy/pkg/state"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// newTestSettingsStore 构造基于本地 StateStore 的持久化实现（每测试独立临时目录）。
func newTestSettingsStore(t *testing.T) registry.SettingsStore {
	t.Helper()
	return registry.NewStateSettingsStore(state.NewLocalStateStore(t.TempDir(), nil))
}

// TestSettingsStore_SaveIncrementsVersion 锁定版本自增（CAS 成功路径）。
func TestSettingsStore_SaveIncrementsVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestSettingsStore(t)

	v1, err := st.Save(ctx, "vault", json.RawMessage(`{"rules":[]}`), 0)
	if err != nil {
		t.Fatalf("首次 Save: %v", err)
	}
	if v1 != 1 {
		t.Fatalf("首版应为 1，得到 %d", v1)
	}
	v2, err := st.Save(ctx, "vault", json.RawMessage(`{"rules":[1]}`), v1)
	if err != nil {
		t.Fatalf("二次 Save: %v", err)
	}
	if v2 != 2 {
		t.Fatalf("第二版应为 2，得到 %d", v2)
	}
}

// TestSettingsStore_CASConflict 锁定并发写不覆盖（旧版本号写入必冲突）。
func TestSettingsStore_CASConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestSettingsStore(t)

	v1, err := st.Save(ctx, "vault", json.RawMessage(`{"rules":[]}`), 0)
	if err != nil {
		t.Fatalf("首次 Save: %v", err)
	}
	if _, err = st.Save(ctx, "vault", json.RawMessage(`{"rules":[1]}`), v1); err != nil {
		t.Fatalf("二次 Save: %v", err)
	}
	if _, err = st.Save(ctx, "vault", json.RawMessage(`{"rules":[2]}`), v1); !errors.Is(err, registry.ErrSettingsVersionConflict) {
		t.Fatalf("旧版本号写入应冲突，得到: %v", err)
	}
}

// TestSettingsStore_LoadMissing 锁定无记录 ⇒ (nil, 0, nil)，不是错误。
func TestSettingsStore_LoadMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestSettingsStore(t)

	raw, ver, err := st.Load(ctx, "no-such-volume")
	if err != nil {
		t.Fatalf("Load 未命中不应报错: %v", err)
	}
	if raw != nil || ver != 0 {
		t.Fatalf("未命中应为 (nil,0)，得到 (%q,%d)", raw, ver)
	}
}

// TestSettingsStore_LoadRoundTrip 锁定存进去的载荷原样读回（不重编码）。
func TestSettingsStore_LoadRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestSettingsStore(t)

	const payload = `{"rules":[{"path_regex":"^archive/","mode":"single"}]}`
	v, err := st.Save(ctx, "vault", json.RawMessage(payload), 0)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, gotV, err := st.Load(ctx, "vault")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if gotV != v {
		t.Fatalf("版本不一致：%d vs %d", gotV, v)
	}
	if string(raw) != payload {
		t.Fatalf("载荷应原样读回，得到 %s", raw)
	}
}

// TestSettingsStore_DeleteIdempotent 锁定删不存在的键不报错，删后回到未命中。
func TestSettingsStore_DeleteIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestSettingsStore(t)

	if err := st.Delete(ctx, "ghost"); err != nil {
		t.Fatalf("删不存在的键应幂等成功: %v", err)
	}
	if _, err := st.Save(ctx, "vault", json.RawMessage(`{"rules":[]}`), 0); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := st.Delete(ctx, "vault"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	raw, ver, err := st.Load(ctx, "vault")
	if err != nil {
		t.Fatalf("删后 Load: %v", err)
	}
	if raw != nil || ver != 0 {
		t.Fatalf("删后应为 (nil,0)，得到 (%q,%d)", raw, ver)
	}
}

// TestSettingsStore_PerVolumeIsolation 锁定不同卷互不影响。
func TestSettingsStore_PerVolumeIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestSettingsStore(t)

	if _, err := st.Save(ctx, "vault-a", json.RawMessage(`{"rules":["a"]}`), 0); err != nil {
		t.Fatalf("Save a: %v", err)
	}
	if _, err := st.Save(ctx, "vault-b", json.RawMessage(`{"rules":["b"]}`), 0); err != nil {
		t.Fatalf("Save b: %v", err)
	}
	rawA, _, err := st.Load(ctx, "vault-a")
	if err != nil {
		t.Fatalf("Load a: %v", err)
	}
	if string(rawA) != `{"rules":["a"]}` {
		t.Fatalf("vault-a 载荷被污染: %s", rawA)
	}
}
