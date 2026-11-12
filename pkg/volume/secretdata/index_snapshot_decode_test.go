// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"testing"
)

// TestDecodeSnapshotInto_MatchesV1Decode 锁定「gzip 直连解码」与「物化解压 + v1 Unmarshal」
// 结果**完全一致**（P0 内存优化 #2：换解码路径不得改变语义）。
//
// 判据有决定性：逐字段比对 Version/Containers/Dirs/DirSegs/DirIDs/Entries，
// 而不是只看 len —— 后者对「字段错位但长度相同」的缺陷无感。
func TestDecodeSnapshotInto_MatchesV1Decode(t *testing.T) {
	t.Parallel()
	want := indexSnapshot{
		Version:      1,
		SecretFP:     "abcd1234",
		Algorithm:    "aes-gcm",
		CreatedAt:    "2026-11-12T00:00:00Z",
		Containers:   []string{"c1", "c2"},
		Dirs:         []string{"a/b", "a/b/c", "x"},
		DirSegs:      map[string]string{"a/b": "a/b", "x": "x"},
		DirIDs:       map[string]string{"a/b": "id-1", "x": "id-2"},
		DirPhysTotal: 42,
		Entries:      []indexSnapshotEntry{{Rel: "a/b/m1", DirSeg: "a/b", MetaName: "m1", Size: 11, MTime: 1700000000, Phys: 4096, BaseVersion: 3}},
	}
	body, err := json.Marshal(&want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestSpeed)
	if _, werr := zw.Write(body); werr != nil {
		t.Fatalf("gzip write: %v", werr)
	}
	if cerr := zw.Close(); cerr != nil {
		t.Fatalf("gzip close: %v", cerr)
	}

	cases := []struct {
		name   string
		plain  []byte
		isGzip bool
	}{
		{"gzip 压缩体（新格式）", gz.Bytes(), true},
		{"明文 JSON（旧格式兼容）", body, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got indexSnapshot
			if ok := decodeSnapshotInto(tc.plain, tc.isGzip, &got, maxSnapshotDecodedBytes); !ok {
				t.Fatal("解码应成功")
			}
			if !snapshotEqualForTest(&got, &want) {
				t.Fatalf("解码结果与 v1 路径不一致\n got=%+v\nwant=%+v", got, want)
			}
		})
	}
}

// TestDecodeSnapshotInto_RejectsOversizeBody 锁定防解压炸弹上限仍生效。
// 用小上限实测（上限经参数注入，不依赖 3GiB 常量，也不引入包级可变状态）。
func TestDecodeSnapshotInto_RejectsOversizeBody(t *testing.T) {
	t.Parallel()
	// 造一个解压后明显大于上限的 gzip 体。
	big := bytes.Repeat([]byte(`{"dirs":["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"],"x":1}`), 64)
	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestSpeed)
	if _, werr := zw.Write(big); werr != nil {
		t.Fatalf("gzip write: %v", werr)
	}
	if cerr := zw.Close(); cerr != nil {
		t.Fatalf("gzip close: %v", cerr)
	}
	var got indexSnapshot
	if ok := decodeSnapshotInto(gz.Bytes(), true, &got, 32); ok {
		t.Fatal("解压后超过上限必须被拒绝（防解压炸弹）")
	}
}

// TestDecodeSnapshotInto_RejectsCorruptBody 锁定异常输入的 fail-closed 语义。
func TestDecodeSnapshotInto_RejectsCorruptBody(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		plain  []byte
		isGzip bool
	}{
		{"gzip 头损坏", []byte("not-a-gzip-stream"), true},
		{"JSON 语法错误", []byte(`{"version":`), false},
		{"空体", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got indexSnapshot
			if ok := decodeSnapshotInto(tc.plain, tc.isGzip, &got, maxSnapshotDecodedBytes); ok {
				t.Fatal("损坏输入必须解码失败（回退全量扫描）")
			}
		})
	}
}

// TestDecodeSnapshotInto_ReadsStreamIncrementally 锁定「不物化解压 buffer」这一收益来源：
// 解码器只消费到 JSON 顶层结束即停，不应把整个流读完（老的物化实现必须读完）。
func TestDecodeSnapshotInto_ReadsStreamIncrementally(t *testing.T) {
	t.Parallel()
	// 顶层 JSON 之后追加 4MiB 垃圾：流式解码应在读完顶层结构后停止。
	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestSpeed)
	if _, werr := zw.Write([]byte(`{"version":1,"dirs":["a"]}`)); werr != nil {
		t.Fatalf("gzip write: %v", werr)
	}
	if _, werr := zw.Write(bytes.Repeat([]byte("x"), 4<<20)); werr != nil {
		t.Fatalf("gzip write junk: %v", werr)
	}
	if cerr := zw.Close(); cerr != nil {
		t.Fatalf("gzip close: %v", cerr)
	}

	var got indexSnapshot
	if ok := decodeSnapshotInto(gz.Bytes(), true, &got, maxSnapshotDecodedBytes); !ok {
		t.Fatal("顶层结构合法时应解码成功（尾部垃圾不影响）")
	}
	if got.Version != 1 || len(got.Dirs) != 1 || got.Dirs[0] != "a" {
		t.Fatalf("解码结果不符: %+v", got)
	}
	_ = io.Discard
}

// snapshotEqualForTest 逐字段比对两个快照（测试内 helper，不参与生产逻辑）。
func snapshotEqualForTest(a, b *indexSnapshot) bool {
	if a.Version != b.Version || a.SecretFP != b.SecretFP || a.Algorithm != b.Algorithm ||
		a.CreatedAt != b.CreatedAt || a.DirPhysTotal != b.DirPhysTotal {
		return false
	}
	if !sliceEqual(a.Containers, b.Containers) || !sliceEqual(a.Dirs, b.Dirs) {
		return false
	}
	if !mapEqual(a.DirSegs, b.DirSegs) || !mapEqual(a.DirIDs, b.DirIDs) {
		return false
	}
	if len(a.Entries) != len(b.Entries) {
		return false
	}
	for i := range a.Entries {
		ca, cb := a.Entries[i], b.Entries[i]
		if ca.DirSeg != cb.DirSeg || ca.MetaName != cb.MetaName || ca.Size != cb.Size ||
			ca.Rel != cb.Rel || ca.MTime != cb.MTime || ca.Phys != cb.Phys ||
			ca.BaseVersion != cb.BaseVersion {
			return false
		}
	}
	return true
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mapEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
