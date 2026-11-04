// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// volume_inspect_test.go 锁审查 CLI 的渲染与目标解析（不依赖服务端）。

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/spf13/cobra"
)

func TestWriteTraceText_RendersChainAndAnomalies(t *testing.T) {
	t.Parallel()
	idx := 0
	onDisk := false
	var buf bytes.Buffer
	writeTraceText(&buf, &volume.PathTrace{
		SchemaVersion: volume.SchemaVersion, Mode: "index", Rel: "photos/a.jpg", Kind: "file",
		Size: 4096, Meta: &volume.BlobRef{RealPath: "k3f9a/sub/7xQz", Bytes: 1024},
		Chunks: []volume.ChunkRef{{Index: 0, Blob: volume.BlobRef{RealPath: "k3f9a/sub/9ab", Bytes: 2048, Plain: 2000}, Blocklets: 1}},
		Parity: &volume.BlobRef{RealPath: "k3f9a/sub/f5d", Bytes: 2048},
		Anomalies: []volume.Anomaly{
			{Code: volume.AnomalyBlobMissing, RealPath: "k3f9a/sub/x", Detail: "缺失"},
			{Code: volume.AnomalySizeMismatch, RealPath: "k3f9a/sub/y", Detail: "大小不符"},
		},
	})
	out := buf.String()
	for _, want := range []string{"photos/a.jpg", "卷根容器 k3f9a", "容器 sub", "文件meta 7xQz", "分块#0", "parity", "blob_missing", "size_mismatch", "不构成删除建议"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q:\n%s", want, out)
		}
	}
	_ = idx
	_ = onDisk
}

func TestWriteExplainText_RendersOwnerParityAndEntries(t *testing.T) {
	t.Parallel()
	chunk := 2
	var buf bytes.Buffer
	writeExplainText(&buf, &volume.RealExplanation{
		SchemaVersion: volume.SchemaVersion, Mode: "disk", RealPath: "k3f9a/sub/9ab", Kind: "chunk",
		Owner:     &volume.LogicalRef{Rel: "photos/a.jpg", Role: "file", ChunkIndex: &chunk},
		Container: &volume.ContainerRef{RealPath: "k3f9a/sub", LogicalDir: "photos", DirID: "d1", ParentID: "p0"},
		Evidence:  []string{"索引反查命中（容器 + 分块名）"},
		Entries: []volume.RealEntry{
			{Name: "9ab", Kind: "chunk", Bytes: 2048, Owner: &volume.LogicalRef{Rel: "photos/a.jpg", Role: "file", ChunkIndex: &chunk}},
			{Name: "f5d", Kind: "parity", Bytes: 2048, Owner: &volume.LogicalRef{Rel: "photos/a.jpg", Role: "file", Parity: true}},
		},
		Anomalies: []volume.Anomaly{{Code: volume.AnomalyBlobOrphan, RealPath: "k3f9a/sub/zz", Detail: "疑似孤儿"}},
	})
	out := buf.String()
	for _, want := range []string{"用途     chunk", "归属     photos/a.jpg", "分块#2", "逻辑目录 photos", "dir_id=d1", "parity", "blob_orphan", "GC"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "分块#0") {
		t.Fatalf("不应出现默认分块序号:\n%s", out)
	}
}

func TestResolveInspectTarget_VolumePrefixAndFlag(t *testing.T) {
	t.Parallel()
	st := &state.State{CurrentDir: "work"}
	cases := []struct {
		name    string
		arg     string
		flagVol string
		logical bool
		wantVol string
		wantRel string
	}{
		{"卷前缀 + 绝对路径", "data:/dir/a.txt", "", true, "data", "dir/a.txt"},
		{"flag 优先于前缀", "data:/dir/a.txt", "other", true, "other", "dir/a.txt"},
		{"真实路径原样", "data:/k3/sub/blob", "", false, "data", "k3/sub/blob"},
	}
	for _, c := range cases {
		cmd := &cobra.Command{}
		cmd.Flags().String(flagInspectVolume, c.flagVol, "")
		vol, rel, err := resolveInspectTarget(cmd, c.arg, st, c.logical)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if vol != c.wantVol || rel != c.wantRel {
			t.Fatalf("%s: got (%q,%q), want (%q,%q)", c.name, vol, rel, c.wantVol, c.wantRel)
		}
	}
}

func TestResolveInspectTarget_NoVolume(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	cmd.Flags().String(flagInspectVolume, "", "")
	if _, _, err := resolveInspectTarget(cmd, "dir/a.txt", &state.State{}, true); err == nil {
		t.Fatal("无卷时应报错（不得静默用默认卷）")
	}
}
