// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal/policy"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// memSettingsStore 是 SettingsStore 的内存替身（记录调用，便于断言「落盘先于热生效」）。
type memSettingsStore struct {
	payload        json.RawMessage
	version        int64
	saveCalls      int
	saveErr        error
	loadErr        error
	lastSaveExpVer int64
	// raceOnLoad 模拟「读到版本后、CAS 之前另一节点写入」的并发窗口：Load 返回后再把
	// version 推进 1，使随后的 Save(expected=读到的版本) 必然 CAS 失配。
	raceOnLoad bool
}

func (m *memSettingsStore) Load(_ context.Context, _ string) (json.RawMessage, int64, error) {
	if m.loadErr != nil {
		return nil, 0, m.loadErr
	}
	v := m.version
	if m.raceOnLoad {
		m.version++ // 并发写者抢先提交
	}
	return m.payload, v, nil
}

func (m *memSettingsStore) Save(_ context.Context, _ string, payload json.RawMessage, expectedVersion int64) (int64, error) {
	m.saveCalls++
	m.lastSaveExpVer = expectedVersion
	if m.saveErr != nil {
		return 0, m.saveErr
	}
	if expectedVersion != m.version {
		return 0, registry.ErrSettingsVersionConflict
	}
	m.payload = payload
	m.version++
	return m.version, nil
}

func (m *memSettingsStore) Delete(context.Context, string) error {
	m.payload, m.version = nil, 0
	return nil
}

// newBackendForSettings 构造一个 backend 与其 FS（小块策略 + mock KDF，加速）。
func newBackendForSettings(t *testing.T, opts Options) (*backend, *SecretdataFS) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	opts.Secret = []byte("test-secret-key-000")
	opts.Algorithm = testAlgo
	// 卷块上限取 1MiB：UpdateSettings 用 b.fs.opts.Block.Max 校验规则的 max_size
	// （规则块不得超过卷级上界，否则 PhysBound 会低估物理上界）。Min 保持小值以加速。
	opts.Block = shardseal.BlockPolicy{Mode: shardseal.BlockModeRandom, Min: 64, Max: 1 << 20}
	if opts.TempDir == "" {
		opts.TempDir = t.TempDir()
	}
	fs, err := NewFS(syncpkg.NewLocalFS(root, nil), opts)
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return &backend{fs: fs}, fs
}

// TestBackend_Settings_ReportsBlockPolicySection 锁定分区回显（Key/Label/Fields/Value/ReadOnly）。
func TestBackend_Settings_ReportsBlockPolicySection(t *testing.T) {
	t.Parallel()
	c, err := policy.Compile(policy.Policy{
		Rules:   []policy.Rule{{PathRegex: `^archive/`, MaxSize: 1 << 20, Mode: shardseal.BlockModeSingle}},
		Version: 3,
	}, 0)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	sections, ferr := func() ([]registry.VolumeSettings, error) {
		b, _ := newBackendForSettings(t, Options{Policy: c})
		return b.Settings(context.Background())
	}()
	if ferr != nil {
		t.Fatalf("Settings: %v", ferr)
	}
	if len(sections) != 1 {
		t.Fatalf("应有 1 个分区，得到 %d", len(sections))
	}
	s := sections[0]
	if s.Key != settingsSectionBlockPolicy {
		t.Fatalf("分区 Key = %q", s.Key)
	}
	if len(s.Fields) == 0 {
		t.Fatal("Fields 不应为空")
	}
	var got policy.Policy
	if uerr := json.Unmarshal(s.Value, &got); uerr != nil {
		t.Fatalf("Value 应可解析为 policy.Policy: %v", uerr)
	}
	if got.Version != 3 || len(got.Rules) != 1 {
		t.Fatalf("回显规则不符: %+v", got)
	}
}

// TestBackend_Settings_NoStoreIsReadOnly 锁定未注入持久化后端 ⇒ ReadOnly=true。
func TestBackend_Settings_NoStoreIsReadOnly(t *testing.T) {
	t.Parallel()
	b, _ := newBackendForSettings(t, Options{})
	sections, err := b.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if len(sections) != 1 || !sections[0].ReadOnly {
		t.Fatalf("无 store 时应只读，得到 %+v", sections)
	}
}

// TestBackend_UpdateSettings_RejectsInvalid 锁定校验失败不改内存策略。
func TestBackend_UpdateSettings_RejectsInvalid(t *testing.T) {
	t.Parallel()
	st := &memSettingsStore{}
	b, _ := newBackendForSettings(t, Options{})
	if err := b.SetSettingsStore(st); err != nil {
		t.Fatalf("SetSettingsStore: %v", err)
	}
	// single 缺 max_size
	bad := json.RawMessage(`{"rules":[{"path_regex":"x","mode":"single"}]}`)
	err := b.UpdateSettings(context.Background(), settingsSectionBlockPolicy, bad)
	if err == nil {
		t.Fatal("缺 max_size 应报错")
	}
	if st.saveCalls != 0 {
		t.Fatalf("校验失败不应落盘，saveCalls=%d", st.saveCalls)
	}
	sections, _ := b.Settings(context.Background())
	var got policy.Policy
	_ = json.Unmarshal(sections[0].Value, &got)
	if len(got.Rules) != 0 {
		t.Fatalf("校验失败不应改变内存策略: %+v", got)
	}
}

// TestBackend_UpdateSettings_UnknownSection 锁定未知分区映射 405 哨兵。
func TestBackend_UpdateSettings_UnknownSection(t *testing.T) {
	t.Parallel()
	b, _ := newBackendForSettings(t, Options{})
	err := b.UpdateSettings(context.Background(), "other", json.RawMessage(`{}`))
	if !errors.Is(err, registry.ErrSettingsSectionUnsupported) {
		t.Fatalf("未知分区应映射哨兵，得到: %v", err)
	}
}

// TestBackend_UpdateSettings_PersistsThenApplies 锁定「先落盘（CAS）再热生效」的顺序与效果。
func TestBackend_UpdateSettings_PersistsThenApplies(t *testing.T) {
	t.Parallel()
	// 内存快照版本 = 0（无静态规则）⇒ store 也必须是 0，否则首次写就被 CAS 判为冲突。
	st := &memSettingsStore{version: 0}
	b, fs := newBackendForSettings(t, Options{})
	if err := b.SetSettingsStore(st); err != nil {
		t.Fatalf("SetSettingsStore: %v", err)
	}
	payload := json.RawMessage(`{"rules":[{"path_regex":"^archive/","max_size":"1MiB","mode":"single"}]}`)
	if err := b.UpdateSettings(context.Background(), settingsSectionBlockPolicy, payload); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if st.saveCalls != 1 {
		t.Fatalf("应落盘 1 次，得到 %d", st.saveCalls)
	}
	if st.lastSaveExpVer != 0 {
		t.Fatalf("CAS 期望版本应为当前快照版本 0，得到 %d", st.lastSaveExpVer)
	}
	if st.version != 1 {
		t.Fatalf("落盘后 store 版本应为 1，得到 %d", st.version)
	}
	// 热生效：写一个命中文件 ⇒ 单块
	content := data(600)
	if err := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "alice/user/archive/a.bin",
		bytes.NewReader(content), int64(len(content)), 1700000000); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fs.mu.RLock()
	e := fs.index["alice/user/archive/a.bin"]
	fs.mu.RUnlock()
	if e == nil || e.meta == nil || len(e.meta.Chunks) != 1 {
		t.Fatalf("规则应已热生效（单块），得到 %+v", e)
	}
}

// TestBackend_UpdateSettings_CASConflict 锁定并发写冲突映射。
func TestBackend_UpdateSettings_CASConflict(t *testing.T) {
	t.Parallel()
	st := &memSettingsStore{version: 5}
	b, _ := newBackendForSettings(t, Options{})
	if err := b.SetSettingsStore(st); err != nil {
		t.Fatalf("SetSettingsStore: %v", err)
	}
	// 模拟并发窗口：Load 读到 5 之后、Save 之前另一写者把版本推到 6 ⇒ CAS 必然失配。
	st.raceOnLoad = true
	payload := json.RawMessage(`{"rules":[]}`)
	err := b.UpdateSettings(context.Background(), settingsSectionBlockPolicy, payload)
	if !errors.Is(err, registry.ErrSettingsVersionConflict) {
		t.Fatalf("应映射版本冲突，得到: %v", err)
	}
}

// TestBackend_SetSettingsStore_LoadFailureKeepsBaseline 锁定载入失败不阻断、保留基线。
func TestBackend_SetSettingsStore_LoadFailureKeepsBaseline(t *testing.T) {
	t.Parallel()
	c, err := policy.Compile(policy.Policy{Rules: []policy.Rule{
		{PathRegex: `^baseline/`, MaxSize: 1 << 20, Mode: shardseal.BlockModeSingle},
	}}, 0)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	b, _ := newBackendForSettings(t, Options{Policy: c})
	st := &memSettingsStore{loadErr: errors.New("backend down")}
	if serr := b.SetSettingsStore(st); serr != nil {
		t.Fatalf("载入失败不应阻断装配，得到: %v", serr)
	}
	sections, _ := b.Settings(context.Background())
	var got policy.Policy
	_ = json.Unmarshal(sections[0].Value, &got)
	if len(got.Rules) != 1 || got.Rules[0].PathRegex != `^baseline/` {
		t.Fatalf("应保留静态基线，得到 %+v", got)
	}
}

// TestBackend_SetSettingsStore_LoadsPersistedRules 锁定载入成功即覆盖基线。
func TestBackend_SetSettingsStore_LoadsPersistedRules(t *testing.T) {
	t.Parallel()
	b, _ := newBackendForSettings(t, Options{})
	payload := json.RawMessage(`{"version":4,"rules":[{"path_regex":"^persisted/","max_size":"1MiB","mode":"single"}]}`)
	st := &memSettingsStore{payload: payload, version: 4}
	if err := b.SetSettingsStore(st); err != nil {
		t.Fatalf("SetSettingsStore: %v", err)
	}
	sections, _ := b.Settings(context.Background())
	var got policy.Policy
	_ = json.Unmarshal(sections[0].Value, &got)
	if len(got.Rules) != 1 || got.Rules[0].PathRegex != `^persisted/` || got.Version != 4 {
		t.Fatalf("应载入持久化规则，得到 %+v", got)
	}
}

// TestBackend_SettingsStore_NilDisablesPersistence 锁定注入 nil store = 回到只读。
func TestBackend_SettingsStore_NilDisablesPersistence(t *testing.T) {
	t.Parallel()
	b, _ := newBackendForSettings(t, Options{})
	if err := b.SetSettingsStore(nil); err != nil {
		t.Fatalf("注入 nil 不应报错: %v", err)
	}
	sections, _ := b.Settings(context.Background())
	if !sections[0].ReadOnly {
		t.Fatal("nil store 应只读")
	}
	err := b.UpdateSettings(context.Background(), settingsSectionBlockPolicy, json.RawMessage(`{"rules":[]}`))
	if err == nil {
		t.Fatal("无持久化后端时更新应返回明确错误（不得静默只改内存）")
	}
}

// TestBackend_ClearingRulesFallsBackToBaseline 锁定「清空运行期规则 ⇒ 回落静态基线」
// （设计 §7 / §12 回滚路径）：不能因内存里留了一个空快照就永久压制基线。
func TestBackend_ClearingRulesFallsBackToBaseline(t *testing.T) {
	t.Parallel()
	baseline, err := policy.Compile(policy.Policy{Rules: []policy.Rule{
		{PathRegex: `^baseline/`, MaxSize: 1 << 20, Mode: shardseal.BlockModeSingle},
	}}, 0)
	if err != nil {
		t.Fatalf("Compile baseline: %v", err)
	}
	st := &memSettingsStore{}
	b, fs := newBackendForSettings(t, Options{Policy: baseline})
	if serr := b.SetSettingsStore(st); serr != nil {
		t.Fatalf("SetSettingsStore: %v", serr)
	}
	// 先写入一条运行期规则（覆盖基线）
	if uerr := b.UpdateSettings(context.Background(), settingsSectionBlockPolicy,
		json.RawMessage(`{"rules":[{"path_regex":"^runtime/","max_size":"1MiB","mode":"single"}]}`)); uerr != nil {
		t.Fatalf("UpdateSettings(runtime): %v", uerr)
	}
	if _, hit := fs.effectivePolicy().Resolve("runtime/a.bin", 1024); !hit {
		t.Fatal("运行期规则应生效")
	}
	// 清空 ⇒ 回落基线（baseline/ 命中、runtime/ 不再命中）
	if uerr := b.UpdateSettings(context.Background(), settingsSectionBlockPolicy,
		json.RawMessage(`{"rules":[]}`)); uerr != nil {
		t.Fatalf("UpdateSettings(empty): %v", uerr)
	}
	if fs.policySnapshot() != nil {
		t.Fatal("清空运行期规则后不应遗留空快照（否则会永久压制基线）")
	}
	if _, hit := fs.effectivePolicy().Resolve("baseline/a.bin", 1024); !hit {
		t.Fatal("清空运行期规则后应回落到静态基线")
	}
	if _, hit := fs.effectivePolicy().Resolve("runtime/a.bin", 1024); hit {
		t.Fatal("清空后不应再命中运行期规则")
	}
}

// TestBackend_CASUsesPersistedVersion 锁定 CAS 期望版本取自**持久化**（而非内存快照）：
// 多节点部署下另一节点已写入 version=3、本节点内存快照仍是 0 时，本节点仍应能追加写入
// （版本推进到 4），而不是用陈旧内存版本永久 409。
func TestBackend_CASUsesPersistedVersion(t *testing.T) {
	t.Parallel()
	st := &memSettingsStore{} // 本节点启动时无记录（version 0）
	b, _ := newBackendForSettings(t, Options{})
	if err := b.SetSettingsStore(st); err != nil {
		t.Fatalf("SetSettingsStore: %v", err)
	}
	// 模拟「另一节点已写入 version=3」，本节点内存快照仍是 0
	st.payload = json.RawMessage(`{"rules":[]}`)
	st.version = 3

	if err := b.UpdateSettings(context.Background(), settingsSectionBlockPolicy,
		json.RawMessage(`{"rules":[{"path_regex":"^x/","max_size":"1MiB","mode":"single"}]}`)); err != nil {
		t.Fatalf("应以持久化版本为基准写入（不得因内存陈旧而失败）: %v", err)
	}
	if st.lastSaveExpVer != 3 {
		t.Fatalf("CAS 期望版本应取自持久化（3），得到 %d", st.lastSaveExpVer)
	}
	if st.version != 4 {
		t.Fatalf("写入后版本应为 4，得到 %d", st.version)
	}
}

// TestBackend_InvalidPayloadSentinel 锁定载荷非法映射到 registry.ErrSettingsPayloadInvalid
// （装配层据此回 400，而不是把客户端输入错误报成 500）。
func TestBackend_InvalidPayloadSentinel(t *testing.T) {
	t.Parallel()
	st := &memSettingsStore{}
	b, _ := newBackendForSettings(t, Options{})
	if err := b.SetSettingsStore(st); err != nil {
		t.Fatalf("SetSettingsStore: %v", err)
	}
	for _, payload := range []string{`{`, `{"rules":[{"path_regex":"x","mode":"single"}]}`, `{"rules":"nope"}`} {
		err := b.UpdateSettings(context.Background(), settingsSectionBlockPolicy, json.RawMessage(payload))
		if !errors.Is(err, registry.ErrSettingsPayloadInvalid) {
			t.Fatalf("payload=%s 应映射 ErrSettingsPayloadInvalid，得到 %v", payload, err)
		}
	}
}
