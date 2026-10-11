// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// TestMeta_ReservedFields_RoundtripPaved 验证全量预留字段（压缩/KeyID/Extra/审计/版本/
// 去重/墓碑/签名）的 JSON roundtrip：全部预留字段非零 → marshal → unmarshal 逐字段
// 相等（json tag 正确映射、含子结构/Extra map 的 reflect.DeepEqual 全等）。
// 预留字段是「未来用、现在不写值」——有值 roundtrip 保证 tag 名/编码正确，待 9c 写值
// 时可直接复用。
func TestMeta_ReservedFields_RoundtripPaved(t *testing.T) {
	t.Parallel()
	m := Meta{
		Compressed:   true,
		Compression:  "zstd",
		KeyID:        "sk-2026-10-02-a",
		Extra:        map[string]any{"media_type": "video/mp4", "acl": "rw", "priority": "high"},
		WriterID:     "pikpak-job-42",
		SourceURL:    "https://example.com/foo.mp4",
		AccessCount:  7,
		LastAccess:   "2026-10-02T21:50:00Z",
		BaseVersion:  3,
		VersionSeq:   2,
		Supersedes:   "v2",
		VClock:       "node-1:1000",
		RefCount:     4,
		Deleted:      true,
		ExportedFrom: "pikpak://backup/2026",
		Signature:    "0123456789abcdef",
	}
	data, err := json.Marshal(&m)
	if err != nil {
		t.Fatalf("marshal 有值 meta: %v", err)
	}
	var back Meta
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal 有值 meta: %v", err)
	}
	if !reflect.DeepEqual(back, m) {
		t.Errorf("有值预留字段 roundtrip 不一致：\n got=%+v\nwant=%+v", back, m)
	}
}

// TestMeta_ReservedFields_EmptyOmit 全量预留字段**零值被 omitempty 省略**（空 meta 不
// 携带冗余键；旧 meta 解密后这些字段为零值，语义即「未启用」）。逐键断言不在输出中。
func TestMeta_ReservedFields_EmptyOmit(t *testing.T) {
	t.Parallel()
	m := Meta{} // 全部预留字段零值。
	data, err := json.Marshal(&m)
	if err != nil {
		t.Fatalf("marshal 空 meta: %v", err)
	}
	for _, key := range []string{
		"compressed", "compression", "key_id", "extra",
		"writer_id", "source_url", "access_count", "last_access",
		"base_version", "version_seq", "supersedes", "vclock",
		"ref_count", "deleted", "exported_from", "signature",
	} {
		if bytes.Contains(data, []byte(`"`+key+`"`)) {
			t.Errorf("空 meta 不应携带预留键 %q（omitempty 失效），json=%s", key, data)
		}
	}
	// roundtrip 后仍为零值（缺失键不导致非零）。
	var back Meta
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal 空 meta: %v", err)
	}
	if back.Compressed || back.AccessCount != 0 || back.BaseVersion != 0 ||
		back.VersionSeq != 0 || back.RefCount != 0 || back.Deleted || back.Extra != nil {
		t.Errorf("空 meta roundtrip 应保持零值：%+v", back)
	}
}

// TestBlockletType_ConstantsPaved 锁定 type 常量表完整性：Data/Padding/Extra/Boot/Index/
// Ref/Parity 七个类型值互不相同；Ref=0x12、Parity=0x13 精确；序 reserved(0x11) <
// ref < parity（0x11+ 预留域），且 Ref/Parity 严格大于既有已消费的 Index(0x10)——去重/
// 纠错预留绝不被误当成既有数据段。
func TestBlockletType_ConstantsPaved(t *testing.T) {
	t.Parallel()
	types := []BlockletType{
		BlockletTypeData, BlockletTypePadding, BlockletTypeExtra,
		BlockletTypeBoot, BlockletTypeIndex, BlockletTypeRef, BlockletTypeParity,
	}
	// 全部互不相同（唯一性）。
	seen := map[BlockletType]bool{}
	for _, ty := range types {
		if seen[ty] {
			t.Errorf("type 重复：0x%02x", byte(ty))
		}
		seen[ty] = true
	}
	// 精确取值。
	if BlockletTypeRef != 0x12 || BlockletTypeParity != 0x13 {
		t.Errorf("Ref/Parity 取值错误：ref=0x%02x parity=0x%02x（期望 0x12/0x13）", byte(BlockletTypeRef), byte(BlockletTypeParity))
	}
	// 序关系：reserved(0x11) < ref < parity。
	if BlockletTypeReserved >= BlockletTypeRef || BlockletTypeRef >= BlockletTypeParity {
		t.Errorf("类型序错误：reserved(0x%02x) < ref(0x%02x) < parity(0x%02x) 不成立",
			byte(BlockletTypeReserved), byte(BlockletTypeRef), byte(BlockletTypeParity))
	}
	// 不与既有已消费类型重叠（strict 大于 Index 0x10）。
	if BlockletTypeRef <= BlockletTypeIndex || BlockletTypeParity <= BlockletTypeIndex {
		t.Errorf("Ref/Parity 应严格大于 Index(0x10)：ref=0x%02x parity=0x%02x", byte(BlockletTypeRef), byte(BlockletTypeParity))
	}
}
