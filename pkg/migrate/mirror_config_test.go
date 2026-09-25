// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrate

import (
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
	"gopkg.in/yaml.v3"
)

// mirrorConfigVolumes 是生成 YAML 的可反解结构（断言片段语义）。
type mirrorConfigVolumes struct {
	Volumes []struct {
		Name     string `yaml:"name"`
		MirrorTo string `yaml:"mirror_to,omitempty"`
	} `yaml:"volumes"`
}

type mirrorConfigSync struct {
	SyncRemotes []struct {
		Name string `yaml:"name"`
		Kind string `yaml:"kind"`
		URL  string `yaml:"url"`
	} `yaml:"sync_remotes"`
}

type mirrorConfigFed struct {
	Federation struct {
		Enabled bool `yaml:"enabled"`
		Peers   []struct {
			ID  string `yaml:"id"`
			URL string `yaml:"url"`
		} `yaml:"peers"`
	} `yaml:"federation"`
}

// TestMirrorConfig_GeneratesAllSections 断言：给定 manifest（源机）+ 目标机卷列表，
// 生成 YAML 含 volumes[].mirror_to（多卷冗余）、sync_remotes（源机为同步远端）、
// federation.peers（源机为联邦对端）三段。
func TestMirrorConfig_GeneratesAllSections(t *testing.T) {
	t.Parallel()
	m := &Manifest{
		Schema: SchemaVersion,
		Server: ServerRef{URL: "https://src.example:18083"},
		Files:  []FileEntry{{Name: "a.txt", Size: 5, Checksum: "abc", Volume: "disk2"}},
	}
	vols := []client.VolumeInfo{
		{Name: "default", Allowed: true},
		{Name: "mirror-vol", Allowed: true},
	}
	out, err := GenerateMirrorConfig(m, vols, MirrorOptions{
		TargetURL:       "https://dst.example:18083",
		AccessKey:       "ak-src",
		AccessKeySecret: "sk-src",
		AccessKeyID:     "skid-src",
	})
	if err != nil {
		t.Fatalf("GenerateMirrorConfig: %v", err)
	}
	// 结构断言：volumes 段 mirror_to 生成（多卷冗余）
	var vc mirrorConfigVolumes
	if err := yaml.Unmarshal(out, &vc); err != nil {
		t.Fatalf("解析 volumes 段: %v\n%s", err, out)
	}
	if len(vc.Volumes) == 0 || vc.Volumes[0].MirrorTo != "mirror-vol" {
		t.Errorf("volumes[0].mirror_to 未生成: %+v\n%s", vc.Volumes, out)
	}
	// sync_remotes 段：源机为 direct 远端（含 URL）
	var sc mirrorConfigSync
	if err := yaml.Unmarshal(out, &sc); err != nil {
		t.Fatalf("解析 sync_remotes 段: %v", err)
	}
	if len(sc.SyncRemotes) != 1 || sc.SyncRemotes[0].URL != "https://src.example:18083" {
		t.Errorf("sync_remotes 未含源机远端: %+v\n%s", sc.SyncRemotes, out)
	}
	if sc.SyncRemotes[0].Kind != "direct" {
		t.Errorf("sync_remotes.kind 应为 direct，got %q", sc.SyncRemotes[0].Kind)
	}
	// federation 段：源机为对端 hub
	var fc mirrorConfigFed
	if err := yaml.Unmarshal(out, &fc); err != nil {
		t.Fatalf("解析 federation 段: %v", err)
	}
	if !fc.Federation.Enabled || len(fc.Federation.Peers) != 1 || fc.Federation.Peers[0].URL != "https://src.example:18083" {
		t.Errorf("federation 未含源机对端: %+v\n%s", fc.Federation, out)
	}
	// 文本断言：键存在
	for _, key := range []string{"mirror_to", "sync_remotes", "federation"} {
		if !strings.Contains(string(out), key) {
			t.Errorf("生成 YAML 缺 %q 键:\n%s", key, out)
		}
	}
}

// TestMirrorConfig_SingleVolume 断言：目标机仅一卷时不生成 mirror_to（无多卷冗余目标）。
func TestMirrorConfig_SingleVolume(t *testing.T) {
	t.Parallel()
	m := &Manifest{Schema: SchemaVersion, Server: ServerRef{URL: "https://src.example:18083"}}
	vols := []client.VolumeInfo{{Name: "default", Allowed: true}}
	out, err := GenerateMirrorConfig(m, vols, MirrorOptions{TargetURL: "https://dst.example:18083"})
	if err != nil {
		t.Fatalf("GenerateMirrorConfig: %v", err)
	}
	var vc mirrorConfigVolumes
	if err := yaml.Unmarshal(out, &vc); err != nil {
		t.Fatalf("解析 volumes 段: %v", err)
	}
	if len(vc.Volumes) > 0 && vc.Volumes[0].MirrorTo != "" {
		t.Errorf("单卷目标机不应生成 mirror_to: %+v\n%s", vc.Volumes, out)
	}
}

// TestMirrorConfig_NoServer 断言：manifest 无源机 URL → 不生成 sync_remotes/federation。
func TestMirrorConfig_NoServer(t *testing.T) {
	t.Parallel()
	m := &Manifest{Schema: SchemaVersion}
	vols := []client.VolumeInfo{{Name: "default", Allowed: true}, {Name: "b", Allowed: true}}
	out, err := GenerateMirrorConfig(m, vols, MirrorOptions{TargetURL: "https://dst.example:18083"})
	if err != nil {
		t.Fatalf("GenerateMirrorConfig: %v", err)
	}
	for _, key := range []string{"sync_remotes", "federation"} {
		if strings.Contains(string(out), key) {
			t.Errorf("无源机 URL 不应生成 %q: %s", key, out)
		}
	}
	if !strings.Contains(string(out), "mirror_to") {
		t.Errorf("卷冗余与源机无关，仍应生成 mirror_to: %s", out)
	}
}
