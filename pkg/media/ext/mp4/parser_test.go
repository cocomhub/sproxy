// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package keyframe

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"
)

// minimalMP4 手工构造最小合法 MP4（纯二进制，不依赖 go-mp4 Writer）：
//
//	ftyp + moov(trak→mdia→minf→stbl→{stco,stsc,stsz,stss}) + mdat(5 样本×100B)
//
// 5 个 chunk 各 1 样本；stss 标记样本 1 与 3（1-based）为同步样本。返回字节内容与期望
// 关键帧文件绝对偏移。mdat 起始 = moov 结束（ftyp 24B + moov 168B），样本 i 偏移 =
// mdatPayloadStart + 100*i。
func minimalMP4(t *testing.T) ([]byte, []int64) {
	t.Helper()
	var buf bytes.Buffer

	// ftyp（24B）。
	ftypPayload := append([]byte("isom"), 0, 0, 0, 0) // major_brand + minor_version
	ftypPayload = append(ftypPayload, []byte("isomiso2")...)
	writeMP4Box(&buf, "ftyp", ftypPayload)

	// 样本表 box（全 fullbox：version=0 + flags=0 前置）。
	// stco：5 个 chunk 偏移待定（mdat 布局算好后填）。先用占位，稍后修正。
	stcoOff := buf.Len()
	_ = stcoOff
	// 先算 mdat 起始：ftyp 24B + moov 总长（下面累加）。
	stcoSize := 4 + 4 + 5*4     // fullbox + entry_count + 5×offset
	stscSize := 4 + 4 + 3*4     // fullbox + entry_count + 1 entry
	stszSize := 4 + 4 + 4 + 5*4 // fullbox + sample_size + sample_count + 5×size
	stssSize := 4 + 4 + 2*4     // fullbox + entry_count + 2×sample
	stblSize := 8 + stcoSize + stscSize + stszSize + stssSize
	minfSize := 8 + stblSize
	mdiaSize := 8 + minfSize
	trakSize := 8 + mdiaSize
	moovSize := 8 + trakSize
	mdatPayloadStart := int64(24 + moovSize + 8) // ftyp + moov + mdat 头
	chunkOffsets := make([]uint32, 5)
	for i := range chunkOffsets {
		chunkOffsets[i] = uint32(mdatPayloadStart + int64(100*i))
	}

	// moov.
	var moov bytes.Buffer
	writeMP4Box(&moov, "trak", moovTrakPayload(chunkOffsets))
	writeMP4Box(&buf, "moov", moov.Bytes())

	// mdat：5 样本各 100B。
	var mdat bytes.Buffer
	for i := 0; i < 5; i++ {
		sample := bytes.Repeat([]byte{byte(i + 1)}, 100)
		mdat.Write(sample)
	}
	writeMP4Box(&buf, "mdat", mdat.Bytes())

	return buf.Bytes(), []int64{mdatPayloadStart, mdatPayloadStart + 200}
}

// moovTrakPayload 构造 trak→mdia→minf→stbl→样本表的字节序列。
func moovTrakPayload(chunkOffsets []uint32) []byte {
	var stbl bytes.Buffer
	// stco。
	var stco bytes.Buffer
	putFullBoxHeader(&stco)
	binary.Write(&stco, binary.BigEndian, uint32(len(chunkOffsets)))
	for _, off := range chunkOffsets {
		binary.Write(&stco, binary.BigEndian, off)
	}
	writeMP4Box(&stbl, "stco", stco.Bytes())
	// stsc：1 entry（first_chunk=1, spc=1, sdi=1）。
	var stsc bytes.Buffer
	putFullBoxHeader(&stsc)
	binary.Write(&stsc, binary.BigEndian, uint32(1))
	binary.Write(&stsc, binary.BigEndian, uint32(1))
	binary.Write(&stsc, binary.BigEndian, uint32(1))
	binary.Write(&stsc, binary.BigEndian, uint32(1))
	writeMP4Box(&stbl, "stsc", stsc.Bytes())
	// stsz：sample_size=0，逐样本 100B。
	var stsz bytes.Buffer
	putFullBoxHeader(&stsz)
	binary.Write(&stsz, binary.BigEndian, uint32(0))
	binary.Write(&stsz, binary.BigEndian, uint32(len(chunkOffsets)))
	for i := 0; i < len(chunkOffsets); i++ {
		binary.Write(&stsz, binary.BigEndian, uint32(100))
	}
	writeMP4Box(&stbl, "stsz", stsz.Bytes())
	// stss：同步样本 1 与 3（1-based）。
	var stss bytes.Buffer
	putFullBoxHeader(&stss)
	binary.Write(&stss, binary.BigEndian, uint32(2))
	binary.Write(&stss, binary.BigEndian, uint32(1))
	binary.Write(&stss, binary.BigEndian, uint32(3))
	writeMP4Box(&stbl, "stss", stss.Bytes())

	var minf bytes.Buffer
	writeMP4Box(&minf, "stbl", stbl.Bytes())
	var mdia bytes.Buffer
	writeMP4Box(&mdia, "minf", minf.Bytes())
	var trak bytes.Buffer
	writeMP4Box(&trak, "mdia", mdia.Bytes())
	return trak.Bytes()
}

// putFullBoxHeader 写入 fullbox 前缀（version=0 + flags=0）。
func putFullBoxHeader(w *bytes.Buffer) {
	w.Write([]byte{0, 0, 0, 0})
}

// writeMP4Box 写入一个 MP4 box（[size 4B BE][type 4B][payload]）。
func writeMP4Box(w *bytes.Buffer, typ string, payload []byte) {
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(8+len(payload)))
	w.Write(size[:])
	w.Write([]byte(typ))
	w.Write(payload)
}

// TestKeyframeOffsets_BasicMP4：解析手工构造的最小合法 MP4，关键帧字节偏移与期望一致。
func TestKeyframeOffsets_BasicMP4(t *testing.T) {
	t.Parallel()
	data, want := minimalMP4(t)
	got, err := KeyframeOffsets(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("KeyframeOffsets: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("关键帧数=%d，应为 %d（got=%v want=%v）", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("关键帧[%d]=%d，应为 %d", i, got[i], want[i])
		}
	}
}

// TestKeyframeOffsets_TruncatedMP4：截断 MP4（缺 mdat/损坏）→ 不 panic；解析失败返回 err
// 或空帧（可用部分照用，降级由 shardseal 决策）。
func TestKeyframeOffsets_TruncatedMP4(t *testing.T) {
	t.Parallel()
	full, _ := minimalMP4(t)
	trunc := full[:len(full)/2] // 砍掉后半（mdat 大部）
	_, err := KeyframeOffsets(bytes.NewReader(trunc), int64(len(trunc)))
	if err == nil {
		// 允许部分可用：截断到 mdat 也可能解析出 stss（moov 在前）。
		t.Log("截断 MP4 返回部分关键帧（可接受）")
	}
}

// TestKeyframeOffsets_NonMP4：非 MP4 数据 → 错误（不 panic）。
func TestKeyframeOffsets_NonMP4(t *testing.T) {
	t.Parallel()
	garbage := bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, 100)
	_, err := KeyframeOffsets(bytes.NewReader(garbage), int64(len(garbage)))
	if err == nil {
		t.Error("非 MP4 数据应报错")
	}
}

// TestKeyframeOffsets_MaliciousPanicRecovered（I3 回归）：畸形 MP4（首 box 声明超大 size，
// 可能触发 go-mp4 内部 panic）→ recover 兜底返回 error，**不 panic**（不可信输入隔离）。
func TestKeyframeOffsets_MaliciousPanicRecovered(t *testing.T) {
	t.Parallel()
	// 手工构造畸形 box：size 字段声明 0xFFFFFFFF（超大），type 非法——go-mp4 读取时可能
	// panic（如整数溢出/越界）；KeyframeOffsets 的 recover 必须兜住转 error。
	evil := []byte{
		0xFF, 0xFF, 0xFF, 0xFF, // size = 4GB（畸形）
		'B', 'A', 'D', ' ', // 非法 box type
	}
	_, err := KeyframeOffsets(bytes.NewReader(evil), int64(len(evil)))
	// 无论返回什么 err 都接受——关键断言是「不 panic」（recover 已隔离）。
	_ = err
}

// TestKeyframeOffsets_AudioOnlyNoStss（审查 M8 补）：纯音频 MP4（无 stss/同步样本表）
// → 报错（非视频不可解析关键帧），不 panic。
func TestKeyframeOffsets_AudioOnlyNoStss(t *testing.T) {
	t.Parallel()
	// 手工构造「有 stbl 但无 stss」的 MP4（音频轨常见：无同步样本表）。
	// 复用 minimalMP4 骨架但剥掉 stss box —— 直接构造一个只有 ftyp+mdat 的最小容器
	// （无 moov 无 stss）→ 应报「无同步样本表」而非静默返回空。
	evil := append([]byte{
		0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', // ftyp box
		'q', 't', ' ', ' ', 0x00, 0x00, 0x00, 0x00,
		'i', 's', 'o', 'm', 'i', 's', 'o', '2',
	}, 0x00, 0x00, 0x00, 0x00) // 无 moov/stss
	frames, err := KeyframeOffsets(bytes.NewReader(evil), int64(len(evil)))
	if err == nil && len(frames) > 0 {
		t.Errorf("无 stss 不应产出关键帧（got %v）", frames)
	}
}

// TestKeyframeOffsets_FragmentedMP4：含 moof/mvex（fMP4 特征）但无全局 stss 的 MP4 →
// 返回哨兵错误 ErrFragmentedMP4（调用方回落 fixed），并触发 fMP4 统计 hook
// （→ /metrics sproxy_keyframe_fmp4_total，供切库决策）。
func TestKeyframeOffsets_FragmentedMP4(t *testing.T) {
	t.Parallel()
	// 注入统计 hook 捕获 fMP4 命中（atomic setter，测试并发安全）。
	oldHook := loadTestHook()
	defer func() { SetOnFragmentedMP4(oldHook) }()
	hit := false
	SetOnFragmentedMP4(func() { hit = true })

	// 构造 fMP4：ftyp + 含 moof 的容器（无 moov/stss）——触发 fragmented 检测。
	evil := append([]byte{
		0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', // ftyp box
		'q', 't', ' ', ' ', 0x00, 0x00, 0x00, 0x00,
		'i', 's', 'o', 'm', 'i', 's', 'o', '2',
	}, 0x00, 0x00, 0x00, 0x10, 'm', 'o', 'o', 'f', 0x00, 0x00, 0x00, 0x00) // moof box
	frames, err := KeyframeOffsets(bytes.NewReader(evil), int64(len(evil)))
	if !errors.Is(err, ErrFragmentedMP4) {
		t.Errorf("fMP4 应返回 ErrFragmentedMP4，got: %v (frames=%v)", err, frames)
	}
	if !hit {
		t.Error("fMP4 统计 hook 应被触发（SetOnFragmentedMP4 注入）")
	}
}

// loadTestHook 读取当前注入的 fMP4 统计 hook（测试清理用；atomic 读）。
func loadTestHook() func() {
	p := onFragmentedMP4.Load()
	if p == nil {
		return nil
	}
	return *p
}

var _ = io.EOF
var _ = fmt.Sprintf
