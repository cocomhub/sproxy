// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// config_writer_acl_test.go 锁定 C4 CRITICAL 修复（2026-10-07 真实浏览器复测）：
// config 写回 `volumeEntryNode` 曾只写 name/type/vol_capacity/extra，不写 ACL——
// 运行时建卷（POST /api/volumes/user）注入 ModeAllow + 单 owner（键空间 `user/<rel>`），
// 但写回 config 再重启后，config 声明卷 ACL 零值（Mode=="" → parseVolumeACL 归 den）
// → Shared()==true → 键空间翻转成 `<owner>/user/<rel>` → 同一封装卷重启前后键分道，
// 重启前文件在重启后索引下不可见（复测：v2.txt 重启前落 root→"user"、重启后落
// root→"anonymous"→"user"，旧数据 404 但磁盘密文/meta 仍在）。

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
	"gopkg.in/yaml.v3"
)

// TestConfigWriter_VolumeEntry_IncludesACL 验证 volumeEntryNode 在 UserVolume 带
// owner-only ACL（ACL.Owners 非空）时补写 acl 段（mode allow + owners [<owner>]），保持
// config 声明卷装配后与运行时卷一致（Shared()==false，键空间不翻转）。**不手填 Owner 字段**
// （2026-10-07 复测：真实建卷链 UserVolume.Owner 恒空，手填会绕过写回触发条件）——
// 本测试故意以真实链形状（仅 ACL.Owners，无 Owner）构造，逐步回归 ACL-driven 写回；
// ACL.Owners 空（config 声明卷无 owner）→ 不写 acl 段（零回归）。
func TestConfigWriter_VolumeEntry_IncludesACL(t *testing.T) {
	t.Parallel()
	n, err := volumeEntryNode(UserVolume{
		Name: "vault", Type: "secretdata", Capacity: 100 << 20,
		Extra: map[string]any{"target": "main/videos"},
		ACL:   volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{"alice": {}}},
	})
	if err != nil {
		t.Fatalf("volumeEntryNode: %v", err)
	}
	body, err := yaml.Marshal(n)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	s := string(body)
	for _, want := range []string{"acl:", "mode: allow", "owners:", "alice"} {
		if !strings.Contains(s, want) {
			t.Fatalf("volumeEntryNode 写回应含 %q（ACL 丢失会导致封装卷重启键空间翻转）:\n%s", want, s)
		}
	}
	if strings.Contains(s, "mesh_readers") {
		t.Fatalf("写回不应带 mesh_readers 段（运行时卷无跨节点授权）:\n%s", s)
	}

	// Owner 空 → 不写 acl 段（config 声明卷无 owner，保持现状）。
	n2, err := volumeEntryNode(UserVolume{Name: "vault", Type: "secretdata"})
	if err != nil {
		t.Fatalf("volumeEntryNode(empty owner): %v", err)
	}
	b2, _ := yaml.Marshal(n2)
	if strings.Contains(string(b2), "acl:") {
		t.Fatalf("Owner 空时不应写 acl 段:\n%s", b2)
	}
}

// TestConfigWriter_UserVolume_KeyspaceStable 是键空间回归（关键）：模拟「运行时建卷写回
// config → 重新解析装配」后，同一封装卷键空间与写回前一致（Shared==false 且
// ResolveUserPath 不添 owner 前缀），杜绝重启翻转导致旧数据不可见。UserVolume 按真实
// 建卷链形状构造：**不填 Owner 字段**（复测证实现实链 Owner 恒空），仅 ACL.Owners 携带
// owner——写回须以 ACL 驱动，否则本测试红。
func TestConfigWriter_UserVolume_KeyspaceStable(t *testing.T) {
	t.Parallel()
	uv := UserVolume{
		Name: "vault", Type: "secretdata", Capacity: 100 << 20,
		Extra: map[string]any{"target": "main/videos"},
		ACL:   volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{"alice": {}}},
	}
	// 写回前（运行时卷）键空间：独享（Allow+单 owner）→ user/x（无 owner 前缀）。
	preVol := volume.Volume{Name: uv.Name, Type: uv.Type, Capacity: uv.Capacity, Extra: uv.Extra, ACL: uv.ACL}
	preKey, preErr := preVol.ResolveUserPath("alice", "x")
	if preErr != nil {
		t.Fatalf("写回前 ResolveUserPath: %v", preErr)
	}
	if preKey != "user/x" {
		t.Fatalf("写回前键应为独享 user/x，got %q（Shared=%v）", preKey, preVol.Shared())
	}

	// 写回 config → 重新解析装配。
	path := filepath.Join(t.TempDir(), "sproxy.yaml")
	if err := os.WriteFile(path, []byte("addr: :18083\nvolumes:\n  - name: main\n    type: local\n    root: ./storage\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	w := NewFileConfigWriter(path)
	if err := w.AppendVolume(uv); err != nil {
		t.Fatalf("AppendVolume: %v", err)
	}
	data, _ := os.ReadFile(path)
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal config: %v", err)
	}
	var vc *VolumeConfig
	for i := range cfg.Volumes {
		if cfg.Volumes[i].Name == "vault" {
			vc = &cfg.Volumes[i]
			break
		}
	}
	if vc == nil {
		t.Fatalf("写回 config 未含 vault 卷:\n%s", data)
	}
	if vc.ACL == nil {
		t.Fatalf("写回 config 的 vault 卷丢失 acl（应含 mode allow + owners [alice]）:\n%s", data)
	}
	// 重新装配（config → 纯域 Volume）走 parseVolumeACL 真路径。
	rebVol := volume.Volume{Name: vc.Name, Type: vc.Type, Capacity: int64(vc.VolCapacity), Extra: vc.Extra,
		ACL: parseVolumeACL(vc.ACL, slog.New(slog.NewTextHandler(io.Discard, nil)))}
	if rebVol.Shared() {
		t.Fatalf("重启装配后封装卷应 Shared()==false（写回 ACL 应保持独享），ACL=%+v", rebVol.ACL)
	}
	rebKey, err := rebVol.ResolveUserPath("alice", "x")
	if err != nil {
		t.Fatalf("重启装配后 ResolveUserPath: %v", err)
	}
	if rebKey != preKey {
		t.Fatalf("重启键空间翻转：写回前 %q，重启后 %q——旧数据将不可见（应保持 %q）",
			preKey, rebKey, preKey)
	}
}

// TestCreateUserVolume_ConfigWriteback_KeyspaceStable 是 C4 CRITICAL 的**端到端回归**
// （2026-10-07 复测第二回）：走 createUserVolumeHandler 全真实调用链——POST
// /api/volumes/user 建封装卷 → handler 构造 UserVolume（本测试不得手工填 Owner，必须由
// create 路径产出）→ FileConfigWriter.AppendVolume 写回 config → yaml 读回 → 经
// parseVolumeACL 真路径重新装配 → Shared()==false 且 ResolveUserPath 键与写回前一致
// （user/x）——重启不翻转键空间，旧数据重启后仍可见。
//
// 本用例在修复前必红：旧 config_writer 以 UserVolume.Owner 驱动写回 ACL，而真实链
// uv.Owner 恒空 → acl 段不落 → 重启装配 Shared()==true → 键空间翻转。既有单测手填 Owner
// 绕过了该缺陷，故此处必须走真实 create 链（数据经 handler 产出），不给手填 Owner 留路。
func TestCreateUserVolume_ConfigWriteback_KeyspaceStable(t *testing.T) {
	t.Parallel()
	h, _ := newUserVolumeAPIHandlers(t, false)
	// 注入真实 FileConfigWriter（指向一个 base config 文件），使 handler 侧
	// `if h.configWriter != nil { h.configWriter.AppendVolume(uv) }` 真路径落盘。
	cfgPath := filepath.Join(t.TempDir(), "sproxy.yaml")
	if err := os.WriteFile(cfgPath, []byte("addr: :18083\nvolumes:\n  - name: main\n    type: local\n    root: ./storage\n"), 0o600); err != nil {
		t.Fatalf("write base config: %v", err)
	}
	h.SetConfigWriter(NewFileConfigWriter(cfgPath))
	mux := userVolWrap(h, "alice")

	rec := postUserVolume(t, mux, map[string]any{
		"name": "userdisk1", "type": userVolumeTestType, "capacity": 1000,
		"extra": map[string]any{"bduss": "test"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/volumes/user = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	// 键空间（写回前）：独享 Allow+单 owner → user/x（无 owner 前缀）。
	var preVol *volume.Volume
	for _, v := range h.volSet.All() {
		if v.Name == "userdisk1" {
			preVol = &v
			break
		}
	}
	if preVol == nil {
		t.Fatalf("volSet 未注册 userdisk1 卷")
	}
	preKey, err := preVol.ResolveUserPath("alice", "x")
	if err != nil {
		t.Fatalf("写回前 ResolveUserPath: %v", err)
	}
	if preKey != "user/x" {
		t.Fatalf("写回前键应为独享 user/x，got %q（Shared=%v）", preKey, preVol.Shared())
	}

	// 读回写好的 config 文件，重新装配（真路径 parseVolumeACL → 纯域 Volume）。
	data, _ := os.ReadFile(cfgPath)
	var cfg Config
	err = yaml.Unmarshal(data, &cfg)
	if err != nil {
		t.Fatalf("yaml.Unmarshal(写回 config): %v", err)
	}
	var vc *VolumeConfig
	for i := range cfg.Volumes {
		if cfg.Volumes[i].Name == "userdisk1" {
			vc = &cfg.Volumes[i]
			break
		}
	}
	if vc == nil {
		t.Fatalf("写回 config 未含 userdisk1 卷:\n%s", data)
	}
	// 重启装配 → Shared()==false（独享键空间，不翻转）。
	rebVol := volume.Volume{Name: vc.Name, Type: vc.Type, Capacity: int64(vc.VolCapacity), Extra: vc.Extra,
		ACL: parseVolumeACL(vc.ACL, slog.New(slog.NewTextHandler(io.Discard, nil)))}
	if rebVol.Shared() {
		t.Fatalf("重启装配后封装卷应 Shared()==false（真实 create 链写回必须落 owner-only ACL），ACL=%+v", rebVol.ACL)
	}
	rebKey, err := rebVol.ResolveUserPath("alice", "x")
	if err != nil {
		t.Fatalf("重启装配后 ResolveUserPath: %v", err)
	}
	if rebKey != preKey {
		t.Fatalf("重启键空间翻转：写回前 %q，重启后 %q——旧数据将不可见（应保持 %q）",
			preKey, rebKey, preKey)
	}
}
