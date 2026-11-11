// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

// inspect_test.go 锁审查输出的**JSON 契约**（字段名/结构稳定性）。
//
// 为什么需要：`trace`/`explain` 的 JSON 是 CLI、Web UI（P2）与运维脚本的共同契约；
// 字段重命名属破坏性变更，必须有测试在 CI 里拦住（否则只有用户侧爆炸）。

import (
	"encoding/json"
	"testing"
)

// marshalKeys 把对象序列化并取其**顶层字段名**（排序后由调用方比对）。
func marshalKeys(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func TestPathTrace_JSONContract(t *testing.T) {
	t.Parallel()
	idx := 0
	onDisk := true
	disk := int64(7)
	tr := PathTrace{
		SchemaVersion: SchemaVersion, Mode: "disk", Rel: "a/b.txt", Kind: "file", Size: 10, MTime: 1,
		Containers: []ContainerRef{{RealPath: "c1", LogicalDir: "d", DirID: "id"}},
		Meta:       &BlobRef{RealPath: "c1/m", Bytes: 3},
		Chunks:     []ChunkRef{{Index: idx, Blob: BlobRef{RealPath: "c1/x", Bytes: 4, Plain: 3, OnDisk: &onDisk, DiskBytes: &disk}, Blocklets: 1}},
		Parity:     &BlobRef{RealPath: "c1/p", Bytes: 5},
		Dedup:      &DedupRef{Container: "pool", Refs: 2},
		Upload:     &UploadProgress{Rel: "a/b.txt", Container: "c1", Total: 10, Committed: 5, Chunks: 1},
		Anomalies:  []Anomaly{{Code: AnomalyBlobMissing, RealPath: "c1/x", Detail: "d"}},
	}
	m := marshalKeys(t, tr)
	for _, k := range []string{
		"schema_version", "mode", "rel", "kind", "size", "mtime",
		"containers", "meta", "chunks", "parity", "dedup", "upload", "anomalies",
	} {
		if _, ok := m[k]; !ok {
			t.Fatalf("PathTrace 缺字段 %q（契约破坏）", k)
		}
	}
	chunk := m["chunks"].([]any)[0].(map[string]any)
	for _, k := range []string{"index", "blob", "blocklets"} {
		if _, ok := chunk[k]; !ok {
			t.Fatalf("ChunkRef 缺字段 %q", k)
		}
	}
	blob := chunk["blob"].(map[string]any)
	for _, k := range []string{"real_path", "bytes", "plain", "on_disk", "disk_bytes"} {
		if _, ok := blob[k]; !ok {
			t.Fatalf("BlobRef 缺字段 %q", k)
		}
	}
}

func TestRealExplanation_JSONContract(t *testing.T) {
	t.Parallel()
	ci := 1
	ex := RealExplanation{
		SchemaVersion: SchemaVersion, Mode: "index", RealPath: "c1/x", Kind: "chunk",
		Owner:     &LogicalRef{Rel: "a", Role: "file", ChunkIndex: &ci},
		Container: &ContainerRef{RealPath: "c1", LogicalDir: "d"},
		Evidence:  []string{"e"},
		Entries: []RealEntry{{
			Name: "x", Kind: "container", Bytes: 1,
			Owner:     &LogicalRef{Rel: "a", Role: "file"},
			Container: &ContainerRef{RealPath: "c1/sub", LogicalDir: "d/sub"},
			Note:      "n",
		}},
		Anomalies: []Anomaly{{Code: AnomalyUploadInProgress, RealPath: "c1/y", Detail: "d"}},
	}
	m := marshalKeys(t, ex)
	for _, k := range []string{
		"schema_version", "mode", "real_path", "kind", "owner", "container", "evidence", "entries", "anomalies",
	} {
		if _, ok := m[k]; !ok {
			t.Fatalf("RealExplanation 缺字段 %q（契约破坏）", k)
		}
	}
	entry := m["entries"].([]any)[0].(map[string]any)
	for _, k := range []string{"name", "kind", "bytes", "owner", "container", "note"} {
		if _, ok := entry[k]; !ok {
			t.Fatalf("RealEntry 缺字段 %q", k)
		}
	}
	if m["schema_version"].(float64) != float64(SchemaVersion) {
		t.Fatalf("schema_version = %v", m["schema_version"])
	}
}

// TestInspectMode_String 模式名是 JSON/CLI 共用契约。
func TestInspectMode_String(t *testing.T) {
	t.Parallel()
	if InspectIndex.String() != "index" || InspectDisk.String() != "disk" {
		t.Fatalf("mode string = %q/%q", InspectIndex.String(), InspectDisk.String())
	}
}
