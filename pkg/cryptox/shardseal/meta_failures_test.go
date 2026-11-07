// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"encoding/json"
	"testing"
)

// TestMetaFailures_SerializeOmitEmpty：新增失败字段全部 omitempty——无失败时旧 meta
// JSON 不含这些键（旧 meta 双向兼容，metaVersion 保持 1）。
func TestMetaFailures_SerializeOmitEmpty(t *testing.T) {
	t.Parallel()
	meta := &Meta{
		Version:     metaVersion,
		Algorithm:   AlgorithmName,
		AlgoVersion: AlgoV1GCM,
		Original:    OriginalInfo{Name: "a.mp4", Size: 10, SHA256: "abcd"},
		Chunks:      []ChunkInfo{{Index: 0, FileName: "f0", OrigSize: 10, Blocklets: []BlockletInfo{{Offset: 0, Size: 10, EncSize: 38}}}},
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// meta 顶层不应有 failures/keyframes 键（omitempty）。
	for _, k := range []string{"failures", "keyframes"} {
		if _, ok := m[k]; ok {
			t.Errorf("meta 顶层不应有键 %q（omitempty）", k)
		}
	}
	chunks := m["chunks"].([]any)
	first := chunks[0].(map[string]any)
	if _, ok := first["failures"]; ok {
		t.Errorf("ChunkInfo 不应有 failures 键（omitempty）")
	}
	bls := first["blocklets"].([]any)
	bl := bls[0].(map[string]any)
	if _, ok := bl["failures"]; ok {
		t.Errorf("BlockletInfo 不应有 failures 键（omitempty）")
	}
}

// TestMetaFailures_Roundtrip：写入失败信息 → JSON 往返保留（ChunkInfo.Failures /
// BlockletInfo.Failures / Meta.Keyframes）。
func TestMetaFailures_Roundtrip(t *testing.T) {
	t.Parallel()
	meta := &Meta{
		Version:     metaVersion,
		Algorithm:   AlgorithmName,
		AlgoVersion: AlgoV1GCM,
		Original:    OriginalInfo{Name: "a.mp4", Size: 10, SHA256: "abcd"},
		Keyframes:   []int64{0, 1000},
		Chunks: []ChunkInfo{{
			Index: 0, FileName: "f0", OrigSize: 10,
			Failures: []BlockErrorMsg{{Code: "keyframe_parse", Msg: "视频截断"}},
			Blocklets: []BlockletInfo{{
				Offset: 0, Size: 10, EncSize: 38,
				Failures: []BlockErrorMsg{{Code: "keyframe_parse", Msg: "截断"}},
			}},
		}},
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Meta
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Keyframes) != 2 || got.Keyframes[1] != 1000 {
		t.Errorf("Keyframes 往返=%v", got.Keyframes)
	}
	if len(got.Chunks[0].Failures) != 1 || got.Chunks[0].Failures[0].Code != "keyframe_parse" {
		t.Errorf("ChunkInfo.Failures 往返=%+v", got.Chunks[0].Failures)
	}
	if len(got.Chunks[0].Blocklets[0].Failures) != 1 {
		t.Errorf("BlockletInfo.Failures 往返=%+v", got.Chunks[0].Blocklets[0].Failures)
	}
}

// TestValidateMeta_WithFailures：带失败信息的 meta 校验通过（新增字段不破坏 validateMeta）。
func TestValidateMeta_WithFailures(t *testing.T) {
	t.Parallel()
	meta := &Meta{
		Version:     metaVersion,
		Algorithm:   AlgorithmName,
		AlgoVersion: AlgoV1GCM,
		Original:    OriginalInfo{Name: "a.mp4", Size: 10, SHA256: "abcd"},
		Keyframes:   []int64{0},
		Chunks: []ChunkInfo{{
			Index: 0, FileName: "f0", OrigSize: 10,
			Failures: []BlockErrorMsg{{Code: "kf", Msg: "x"}},
			Blocklets: []BlockletInfo{{
				Offset: 0, Size: 10, EncSize: 38,
				Failures: []BlockErrorMsg{{Code: "kf", Msg: "y"}},
			}},
		}},
	}
	if err := validateMeta(meta); err != nil {
		t.Fatalf("带失败信息 meta 应校验通过: %v", err)
	}
}
