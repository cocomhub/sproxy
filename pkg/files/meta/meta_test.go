// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package meta

// meta_test.go 钉住 FileMeta 计算器与校验语义：一次流式同时算整文件 sha256+md5、
// 按分块大小切分逐块 sha256+md5；Validate 覆盖连续性；分块自适应档位。
// 断言铁律：正例落到真实校验值（与独立重算比对），不只断非零。

import (
	"bytes"
	"crypto/md5" //nolint:gosec // 测试双算法校验
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
)

// refSHA/refMD5 独立重算（不与实现共享 hash 逻辑，防同源错误）。
func refSHA(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func refMD5(b []byte) string { s := md5.Sum(b); return hex.EncodeToString(s[:]) }

// TestCalculator_Streaming_SinglePass 一次流式算整文件 + 分块双算法，与独立重算比对。
func TestCalculator_Streaming_SinglePass(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("hello trusted volume 可信卷内容 "), 100) // ~3.3KB
	size := int64(len(data))
	chunkSize := int64(1024)
	c, err := NewCalculator(size, chunkSize)
	if err != nil {
		t.Fatalf("NewCalculator: %v", err)
	}
	// 流式分段喂入（模拟读写访问时顺便计算），分多次以验证跨 chunk 边界累计。
	for i := 0; i < len(data); i += 333 {
		end := min(i+333, len(data))
		if _, err := c.Write(data[i:end]); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if c.curSize > 0 {
		c.flushChunk()
	}
	m := c.Finish()

	if m.TotalSHA256 != refSHA(data) {
		t.Fatalf("整文件 SHA-256 不一致: got %s want %s", m.TotalSHA256, refSHA(data))
	}
	if m.TotalMD5 != refMD5(data) {
		t.Fatalf("整文件 MD5 不一致: got %s want %s", m.TotalMD5, refMD5(data))
	}
	if m.ChunkSize != chunkSize {
		t.Fatalf("ChunkSize = %d, want %d", m.ChunkSize, chunkSize)
	}
	// 分块数与覆盖校验（1000B×3 + 尾块）：每块内容独立重算比对。
	wantChunks := (len(data) + int(chunkSize) - 1) / int(chunkSize)
	if len(m.Chunks) != wantChunks {
		t.Fatalf("分块数 = %d, want %d", len(m.Chunks), wantChunks)
	}
	for i, cm := range m.Chunks {
		off := i * int(chunkSize)
		ln := chunkSize
		if off+int(ln) > len(data) {
			ln = int64(len(data) - off)
		}
		part := data[off : off+int(ln)]
		if cm.SHA256 != refSHA(part) {
			t.Fatalf("分块 %d SHA-256 不一致", i)
		}
		if cm.MD5 != refMD5(part) {
			t.Fatalf("分块 %d MD5 不一致", i)
		}
		if cm.Offset != int64(off) {
			t.Fatalf("分块 %d Offset = %d, want %d", i, cm.Offset, off)
		}
	}
	if err := Validate(m); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestCalculator_ReadFrom_Reader 经 ReadFrom 从 reader 一次性计算（FromFile 同路径）。
func TestCalculator_ReadFrom_Reader(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("abc"), 1000)    // 3KB
	c, err := NewCalculator(int64(len(data)), 0) // chunkSize 自适应
	if err != nil {
		t.Fatalf("NewCalculator: %v", err)
	}
	if n, err := c.ReadFrom(bytes.NewReader(data)); err != nil || n != int64(len(data)) {
		t.Fatalf("ReadFrom: n=%d err=%v", n, err)
	}
	m := c.Finish()
	if m.TotalSHA256 != refSHA(data) {
		t.Fatalf("整文件 SHA-256 不一致")
	}
	if err := Validate(m); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestCalculator_ZeroSize 零大小文件：无分块（调用方特判不写 meta 或空 meta）。
func TestCalculator_ZeroSize(t *testing.T) {
	t.Parallel()
	c, err := NewCalculator(0, 1024)
	if err != nil {
		t.Fatalf("NewCalculator: %v", err)
	}
	m := c.Finish()
	if m.Size != 0 || len(m.Chunks) != 0 {
		t.Fatalf("零文件应无分块: chunks=%d", len(m.Chunks))
	}
}

// TestChunkSizeForSize 分块自适应档位（用户裁定：大文件分块大一点，6G 家常便饭）。
func TestChunkSizeForSize(t *testing.T) {
	t.Parallel()
	const (
		miB = 1 << 20
		giB = 1 << 30
	)
	cases := []struct {
		size int64
		want int64
	}{
		{0, miB}, {15 * miB, miB}, {16 * miB, 4 * miB}, {255 * miB, 4 * miB},
		{256 * miB, 8 * miB}, {1*giB - 1, 8 * miB}, {1 * giB, 16 * miB},
		{4 * giB, 32 * miB}, {6 * giB, 32 * miB},
	}
	for _, tc := range cases {
		if got := ChunkSizeForSize(tc.size); got != tc.want {
			t.Errorf("ChunkSizeForSize(%d) = %d, want %d", tc.size, got, tc.want)
		}
	}
	// 6GiB 分块数合理（192 块），逐分片校验流量可控。
	if n := (6 * giB) / ChunkSizeForSize(6*giB); n != 192 {
		t.Errorf("6GiB 分块数 = %d, want 192", n)
	}
}

// TestValidate_FailClosed 残缺 meta 必须 fail-closed：缺 SHA、分块空洞、Offset 不连续。
func TestValidate_FailClosed(t *testing.T) {
	t.Parallel()
	valid := &FileMeta{
		Version: metaVersion, Size: 1024, TotalSHA256: strings.Repeat("a", 64),
		ChunkSize: 1024, Chunks: []ChunkMeta{{Index: 0, Offset: 0, Size: 1024, SHA256: strings.Repeat("b", 64)}},
	}
	if err := Validate(valid); err != nil {
		t.Fatalf("valid 应通过: %v", err)
	}
	// 缺 TotalSHA256。
	noTotal := *valid
	noTotal.TotalSHA256 = ""
	if Validate(&noTotal) == nil {
		t.Error("缺 TotalSHA256 应 fail-closed")
	}
	// 分块覆盖空洞（offset 起点错）。
	gap := *valid
	gap.Chunks[0].Offset = 100
	if Validate(&gap) == nil {
		t.Error("Offset 不连续应 fail-closed")
	}
	// 分块覆盖不等于 Size。
	short := *valid
	short.Chunks[0].Size = 100
	if Validate(&short) == nil {
		t.Error("分块覆盖 != Size 应 fail-closed")
	}
	// 缺分块 SHA。
	noChunkSHA := *valid
	noChunkSHA.Chunks[0].SHA256 = ""
	if Validate(&noChunkSHA) == nil {
		t.Error("分块缺 SHA-256 应 fail-closed")
	}
}

// TestFixSizeFromChunks m1 修复：Size 声明失真 → 重设分块覆盖和（自愈产出正确 meta）。
func TestFixSizeFromChunks(t *testing.T) {
	t.Parallel()
	// 覆盖和 == Size（无需修正，幂等）。
	fm := &FileMeta{Size: 2048, Chunks: []ChunkMeta{{Size: 1024}, {Size: 1024}}}
	if ok := FixSizeFromChunks(fm); !ok || fm.Size != 2048 {
		t.Fatalf("覆盖和=Size 应幂等, ok=%v size=%d", ok, fm.Size)
	}
	// Size 声明失真（< 覆盖和）→ 修正为覆盖和。
	fm2 := &FileMeta{Size: 1024, Chunks: []ChunkMeta{{Size: 1024}, {Size: 1024}}}
	if ok := FixSizeFromChunks(fm2); !ok || fm2.Size != 2048 {
		t.Fatalf("Size 失真应修正为覆盖和, ok=%v size=%d", ok, fm2.Size)
	}
	// 无分块（零字节）→ 不修正（ok=false）。
	fm3 := &FileMeta{Size: 0}
	if ok := FixSizeFromChunks(fm3); ok {
		t.Fatal("无分块应 ok=false（零字节不修正）")
	}
}

// TestMarshalUnmarshal Roundtrip：JSON 序列化 + 反序列化校验（Extra map[string]any 保真）。
func TestMarshalUnmarshal(t *testing.T) {
	t.Parallel()
	m := &FileMeta{
		Version: metaVersion, Size: 100, TotalSHA256: strings.Repeat("c", 64), TotalMD5: strings.Repeat("d", 32),
		ChunkSize: 100, Chunks: []ChunkMeta{{Index: 0, Offset: 0, Size: 100, SHA256: strings.Repeat("e", 64)}},
		Name: "a.bin", CTime: "2026-10-07T00:00:00Z",
		Extra: map[string]any{"creator": "alice", "priority": 3, "tags": []string{"x", "y"}},
	}
	data, err := Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.TotalSHA256 != m.TotalSHA256 || got.Extra["creator"] != "alice" {
		t.Fatalf("roundtrip 保真失败: %+v", got)
	}
	if _, ok := got.Extra["priority"].(float64); !ok {
		t.Fatalf("Extra int 应以 float64 反序列化: %T", got.Extra["priority"])
	}
}

// TestFromFile 本地文件直接计算（含 CTime/Name sanitize）。
func TestFromFile(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("from-file 内容"), 100)
	c := 0
	nowRFC3339 = func() string { c++; return fmt.Sprintf("2026-10-07T%02d:00:00Z", c) }
	t.Cleanup(func() { nowRFC3339 = func() string { return "" } })
	m, err := FromFile(writeTemp(t, data), 0, map[string]any{"creator": "bob"})
	if err != nil {
		t.Fatalf("FromFile: %v", err)
	}
	if m.TotalSHA256 != refSHA(data) {
		t.Fatalf("FromFile 整文件 SHA-256 不一致")
	}
	if m.Name != "file.bin" {
		t.Fatalf("Name = %q, want file.bin（裸名）", m.Name)
	}
	if m.CTime == "" {
		t.Fatal("CTime 应填充")
	}
	if err := Validate(m); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// writeTemp 写临时文件返回路径。
func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	p := dir + "/file.bin"
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	return p
}
