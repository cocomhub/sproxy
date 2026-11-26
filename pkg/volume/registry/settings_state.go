// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cocomhub/sproxy/pkg/state"
)

// settingsKeyPrefix 是卷设置在 StateStore 中的键前缀（`volume_policy/<卷名>`）。
//
// 为什么用 state 而不是卷的 Extra：Extra 承载后端凭据（bduss/access_key_secret 等），
// 有自己的加密信封语义；把可调配置混进去会让两者生命周期纠缠。StateStore 还天然支持
// 可选 mongo 后端 ⇒ 多节点部署下规则一致。
const settingsKeyPrefix = "volume_policy/"

// envelope 是落盘的封装：外层 version 是 CAS 版本，value 是后端自有的 JSON。
type envelope struct {
	Version int64           `json:"version"`
	Value   json.RawMessage `json:"value"`
}

// stateSettingsStore 是 SettingsStore 的**唯一实现**（基于 pkg/state.StateStore）。
type stateSettingsStore struct {
	st state.StateStore
}

// NewStateSettingsStore 构造 StateStore 后端的卷设置存储（st 为 nil ⇒ 返回 nil，调用方按
// 「无持久化」处理）。
func NewStateSettingsStore(st state.StateStore) SettingsStore {
	if st == nil {
		return nil
	}
	return &stateSettingsStore{st: st}
}

// settingsKey 拼出 StateStore 键（卷名为空由调用方拒绝）。
func settingsKey(volume string) string { return settingsKeyPrefix + volume }

// Load 实现 SettingsStore。
func (s *stateSettingsStore) Load(ctx context.Context, volume string) (json.RawMessage, int64, error) {
	key := settingsKey(volume)
	if volume == "" {
		return nil, 0, fmt.Errorf("registry: 卷名为空")
	}
	raw, err := s.st.Get(ctx, key)
	if err != nil {
		if errors.Is(err, state.ErrKeyNotFound) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("registry: 读卷设置 %q: %w", volume, err)
	}
	var env envelope
	if uerr := json.Unmarshal(raw, &env); uerr != nil {
		return nil, 0, fmt.Errorf("registry: 卷设置 %q 载荷损坏: %w", volume, uerr)
	}
	return env.Value, env.Version, nil
}

// Save 实现 SettingsStore（CAS：expectedVersion 不符 ⇒ ErrSettingsVersionConflict）。
//
// 实现走 state.StateStore.CAS：old 取「当前落盘字节」，new 取「version+1 的新封装」。
// expectedVersion == 0 表示期望无记录 ⇒ old 必须为 nil（create-only）。这样两个并发写者
// 恰好一个成功、另一个拿到 ErrCASMismatch ⇒ 映射为 ErrSettingsVersionConflict。
func (s *stateSettingsStore) Save(ctx context.Context, volume string, payload json.RawMessage, expectedVersion int64) (int64, error) {
	key := settingsKey(volume)
	if volume == "" {
		return 0, fmt.Errorf("registry: 卷名为空")
	}
	if len(payload) == 0 {
		return 0, fmt.Errorf("registry: 卷设置 %q 载荷为空", volume)
	}
	// 读当前值以构造 CAS 的 old 并对齐版本（CAS 的 old 语义要求逐字节匹配）。
	var old []byte
	curRaw, curVer, err := s.Load(ctx, volume)
	if err != nil {
		return 0, err
	}
	if curRaw != nil {
		env := envelope{Version: curVer, Value: curRaw}
		if old, err = json.Marshal(env); err != nil {
			return 0, fmt.Errorf("registry: 序列化当前卷设置: %w", err)
		}
	}
	if curVer != expectedVersion {
		return 0, fmt.Errorf("%w: 期望版本 %d，当前 %d", ErrSettingsVersionConflict, expectedVersion, curVer)
	}
	newVer := curVer + 1
	newRaw, err := json.Marshal(envelope{Version: newVer, Value: payload})
	if err != nil {
		return 0, fmt.Errorf("registry: 序列化卷设置: %w", err)
	}
	if cerr := s.st.CAS(ctx, key, old, newRaw); cerr != nil {
		if errors.Is(cerr, state.ErrCASMismatch) {
			return 0, fmt.Errorf("%w（并发写）: %w", ErrSettingsVersionConflict, cerr)
		}
		return 0, fmt.Errorf("registry: 写卷设置 %q: %w", volume, cerr)
	}
	return newVer, nil
}

// Delete 实现 SettingsStore（幂等）。
func (s *stateSettingsStore) Delete(ctx context.Context, volume string) error {
	if volume == "" {
		return fmt.Errorf("registry: 卷名为空")
	}
	if err := s.st.Delete(ctx, settingsKey(volume)); err != nil {
		return fmt.Errorf("registry: 删卷设置 %q: %w", volume, err)
	}
	return nil
}

// 编译期断言。
var _ SettingsStore = (*stateSettingsStore)(nil)
