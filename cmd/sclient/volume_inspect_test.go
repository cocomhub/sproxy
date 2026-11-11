// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// volume_inspect_test.go 锁审查 CLI 的渲染与目标解析（不依赖服务端）。

import (
	"bytes"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/spf13/cobra"
)

func TestWriteTraceText_RendersChainAndAnomalies(t *testing.T) {
	t.Parallel()
	idx := 0
	onDisk := false
	var buf bytes.Buffer
	writeTraceText(&buf, &syncpkg.PathTrace{
		SchemaVersion: syncpkg.SchemaVersion, Mode: "index", Rel: "photos/a.jpg", Kind: "file",
		Size: 4096, Meta: &syncpkg.BlobRef{RealPath: "k3f9a/sub/7xQz", Bytes: 1024},
		Chunks: []syncpkg.ChunkRef{{Index: 0, Blob: syncpkg.BlobRef{RealPath: "k3f9a/sub/9ab", Bytes: 2048, Plain: 2000}, Blocklets: 1}},
		Parity: &syncpkg.BlobRef{RealPath: "k3f9a/sub/f5d", Bytes: 2048},
		Anomalies: []syncpkg.Anomaly{
			{Code: syncpkg.AnomalyBlobMissing, RealPath: "k3f9a/sub/x", Detail: "缺失"},
			{Code: syncpkg.AnomalySizeMismatch, RealPath: "k3f9a/sub/y", Detail: "大小不符"},
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

// TestWriteTraceText_ShowsContainerLogicalNames 树必须同时给真实容器名与逻辑目录名。
func TestWriteTraceText_ShowsContainerLogicalNames(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	writeTraceText(&buf, &syncpkg.PathTrace{
		SchemaVersion: syncpkg.SchemaVersion, Mode: "index", Rel: "photos/a.jpg", Kind: "file",
		Size: 2048,
		Containers: []syncpkg.ContainerRef{
			{RealPath: "OypEIk", LogicalDir: ""},
			{RealPath: "OypEIk/TmKKk", LogicalDir: "ak-6c/user"},
			{RealPath: "OypEIk/TmKKk/y78eK", LogicalDir: "ak-6c/user/photos"},
		},
		Meta:   &syncpkg.BlobRef{RealPath: "OypEIk/TmKKk/y78eK/7xQz", Bytes: 1024},
		Chunks: []syncpkg.ChunkRef{{Index: 0, Blob: syncpkg.BlobRef{RealPath: "OypEIk/TmKKk/y78eK/9ab", Bytes: 2048, Plain: 2000}, Blocklets: 1}},
	})
	out := buf.String()
	for _, want := range []string{
		"卷根容器 OypEIk", "容器 TmKKk   → ak-6c/user", "容器 y78eK   → ak-6c/user/photos",
		"文件meta 7xQz",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q:\n%s", want, out)
		}
	}
}

// TestWriteTraceText_Uploading 上传中视图：进度 + 明确说明 meta 未提交。
func TestWriteTraceText_Uploading(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	writeTraceText(&buf, &syncpkg.PathTrace{
		SchemaVersion: syncpkg.SchemaVersion, Mode: "uploading", Rel: "photos/a.mp4", Kind: "file",
		Size:       1000,
		Containers: []syncpkg.ContainerRef{{RealPath: "k3f9a", LogicalDir: ""}},
		Upload:     &syncpkg.UploadProgress{Rel: "photos/a.mp4", Container: "k3f9a", Total: 1000, Committed: 400, Chunks: 3},
	})
	out := buf.String()
	for _, want := range []string{
		"上传中（meta 尚未提交）", "已上传 400 B / 1000 B (40%)", "已完成 3 块",
		"索引中暂无该文件",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q:\n%s", want, out)
		}
	}
	t.Logf("uploading view:\n%s", out)
}

// TestWriteTraceText_RendersChildren 子树行需带名称/大小/类型/meta/分块数。
func TestWriteTraceText_RendersChildren(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	writeTraceText(&buf, &syncpkg.PathTrace{
		SchemaVersion: syncpkg.SchemaVersion, Mode: "index", Rel: "photos", Kind: "dir", Size: 0,
		Containers: []syncpkg.ContainerRef{{RealPath: "k3f9a", LogicalDir: ""}},
		Meta:       &syncpkg.BlobRef{RealPath: "k3f9a/dm", Bytes: 1024},
		Children: []syncpkg.PathTrace{
			{Rel: "photos/a.jpg", Kind: "file", Size: 2048,
				Meta:   &syncpkg.BlobRef{RealPath: "k3f9a/c1/m1", Bytes: 100},
				Chunks: []syncpkg.ChunkRef{{Index: 0, Blob: syncpkg.BlobRef{RealPath: "k3f9a/c1/k1", Bytes: 2048}}}},
			{Rel: "photos/up.bin", Kind: "file", Size: 4096, Mode: "uploading",
				Upload: &syncpkg.UploadProgress{Rel: "photos/up.bin", Total: 4096, Committed: 1024, Chunks: 1}},
		},
	})
	out := buf.String()
	for _, want := range []string{
		"├─ a.jpg   2.0 KiB  [file]", "meta=m1", "chunks=1",
		"└─ up.bin", "上传中 1.0 KiB / 4.0 KiB",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q: %s", want, out)
		}
	}
}

func TestWriteExplainText_RendersOwnerParityAndEntries(t *testing.T) {
	t.Parallel()
	chunk := 2
	var buf bytes.Buffer
	writeExplainText(&buf, &syncpkg.RealExplanation{
		SchemaVersion: syncpkg.SchemaVersion, Mode: "disk", RealPath: "k3f9a/sub/9ab", Kind: "chunk",
		Owner:     &syncpkg.LogicalRef{Rel: "photos/a.jpg", Role: "file", ChunkIndex: &chunk},
		Container: &syncpkg.ContainerRef{RealPath: "k3f9a/sub", LogicalDir: "photos", DirID: "d1", ParentID: "p0"},
		Evidence:  []string{"索引反查命中（容器 + 分块名）"},
		Entries: []syncpkg.RealEntry{
			{Name: "9ab", Kind: "chunk", Bytes: 2048, Owner: &syncpkg.LogicalRef{Rel: "photos/a.jpg", Role: "file", ChunkIndex: &chunk}},
			{Name: "f5d", Kind: "parity", Bytes: 2048, Owner: &syncpkg.LogicalRef{Rel: "photos/a.jpg", Role: "file", Parity: true}},
		},
		Anomalies: []syncpkg.Anomaly{{Code: syncpkg.AnomalyBlobOrphan, RealPath: "k3f9a/sub/zz", Detail: "疑似孤儿"}},
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
