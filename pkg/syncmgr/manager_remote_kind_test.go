// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncmgr

// manager_remote_kind_test.go 钉住 **P1-e**：`validateRemote` 必须**按载体分支**校验
// （`kind=mesh` 的远端没有 URL/凭据，但有 node/volume/pins）。
//
// 回归背景：写批次之前只有 direct 载体，`validateRemote` 无条件要求 `http(s)://` URL 与
// access_key/secret ⇒ 即便 `syncexec` 已支持 mesh，`kind=mesh` 的远端也会在**创建任务**时被拒。
// 本用例组是「配置层 Validate 已按 kind 分支」之外的另一半：**任务创建路径**同样要按 kind。

import (
	"strings"
	"testing"
)

const meshPin = "sha256:3f2a1b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708"

// meshRemote 返回一条完整可用的 mesh 远端配置（无 URL、无凭据）。
func meshRemote(name string) RemoteConfig {
	return RemoteConfig{
		Name: name, Kind: RemoteKindMesh,
		Node: "nodeB", Volume: "main", PeerPins: []string{meshPin},
	}
}

// baidupcsRemote 返回一条完整可用的 baidupcs 远端配置（无 URL、无凭据；本机网盘卷）。
func baidupcsRemote(name string) RemoteConfig {
	return RemoteConfig{
		Name: name, Kind: RemoteKindBaidupcs,
		Volume: "mydisk",
	}
}

// volumeRemote 返回一条完整可用的通用本机卷远端配置（kind=volume，无 URL、无凭据）。
func volumeRemote(name string) RemoteConfig {
	return RemoteConfig{
		Name: name, Kind: RemoteKindVolume,
		Volume: "any-volume",
	}
}

func TestValidateRemote_ByKind(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	cases := []struct {
		name    string
		rc      RemoteConfig
		wantErr string // 空 = 期望通过
	}{
		// ---- mesh：按 node/volume/pins 校验，**不要求** URL/凭据 ----
		{"mesh 完整可用（无 URL/凭据）", meshRemote("r-mesh"), ""},
		{"mesh 带显式 relay", func() RemoteConfig { r := meshRemote("r-mesh"); r.Transport = "relay"; return r }(), ""},
		{"mesh 带 auto", func() RemoteConfig { r := meshRemote("r-mesh"); r.Transport = "auto"; return r }(), ""},
		{"mesh 缺 node", func() RemoteConfig { r := meshRemote("r-mesh"); r.Node = ""; return r }(), "node"},
		{"mesh 缺 volume", func() RemoteConfig { r := meshRemote("r-mesh"); r.Volume = ""; return r }(), "volume"},
		{"mesh 缺 peer_pins（不 TOFU）", func() RemoteConfig { r := meshRemote("r-mesh"); r.PeerPins = nil; return r }(), "peer_pins"},
		{"mesh 未知 transport", func() RemoteConfig { r := meshRemote("r-mesh"); r.Transport = "quic"; return r }(), "transport"},

		// ---- baidupcs：本机网盘卷（无网络对端）；按 volume 校验，**不要求** URL/凭据 ----
		{"baidupcs 完整可用（无 URL/凭据）", baidupcsRemote("r-bd"), ""},
		{"baidupcs 缺 volume", func() RemoteConfig { r := baidupcsRemote("r-bd"); r.Volume = ""; return r }(), "volume"},

		// ---- volume：通用本机卷（WebDAV/baidupcs 统一；kind=baidupcs 归一同构）----
		{"volume 完整可用（无 URL/凭据）", volumeRemote("r-vol"), ""},
		{"volume 缺 volume", func() RemoteConfig { r := volumeRemote("r-vol"); r.Volume = ""; return r }(), "volume"},

		// ---- direct：既有语义逐字保留（零回归）----
		{"direct 完整可用", testRemote("r1", "http://127.0.0.1:1"), ""},
		{"direct 非法 URL", testRemote("r1", "not-a-url"), "URL"},
		{"direct 缺凭据", RemoteConfig{Name: "r1", URL: "http://127.0.0.1:1"}, "access_key"},
		{"direct 非法 scheme", testRemote("r1", "ftp://127.0.0.1:1"), "URL"},

		// ---- 未知载体：拒绝（不猜、不回落）----
		{"未知 kind", RemoteConfig{Name: "r-x", Kind: RemoteKind("quic"), URL: "http://127.0.0.1:1"}, "载体"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newTestManager(t, nil, []RemoteConfig{tc.rc}, nil, nil)
			_, _, err := mgr.CreateTask(CreateRequest{Direction: "push", Remote: tc.rc.Name})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("应通过校验, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应被拒绝（错误需提及 %q）", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误应提及 %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestValidateRemote_MeshDoesNotRequireDirectFields 强调「按 kind 分支」而非「放宽校验」：
// mesh 远端即使带上了 URL/凭据也不校验它们（载体已定，多余字段不参与）；而 direct 远端
// **仍然**必须齐备（上表已钉）。
func TestValidateRemote_MeshDoesNotRequireDirectFields(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	r := meshRemote("r-mesh")
	r.URL = "说好的不是 URL" // 非法 URL 也不影响 mesh 校验（字段不属于该载体）
	r.AccessKey = ""
	r.AccessKeySecret = ""
	mgr := newTestManager(t, nil, []RemoteConfig{r}, nil, nil)
	if _, _, err := mgr.CreateTask(CreateRequest{Direction: "push", Remote: "r-mesh"}); err != nil {
		t.Fatalf("mesh 载体不应因 URL/凭据字段被拒: %v", err)
	}
}
