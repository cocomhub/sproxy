// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"bytes"
	"errors"
	"testing"
)

// fixedIndexer 返回固定关键帧偏移（测试确定性）。
type fixedIndexer struct{ frames []int64 }

func (f *fixedIndexer) KeyframeOffsets(_ KeyframeRequest) ([]int64, error) {
	return f.frames, nil
}

// errIndexer 总是解析失败（视频截断等）。
type errIndexer struct{}

func (e *errIndexer) KeyframeOffsets(_ KeyframeRequest) ([]int64, error) {
	return nil, errors.New("keyframe: 解析失败")
}

// TestVideoKeyframePlanner_FallbackChain（评审 C1 回归）：主 Indexer 解析失败（伪装
// 扩展名/截断，如 TS 流改名 .mp4）→ Fallback 链依次尝试 → 首个成功者用其结果，
// **不记 parseErr、不退 fixed**。
func TestVideoKeyframePlanner_FallbackChain(t *testing.T) {
	t.Parallel()
	p := &VideoKeyframeBlockletPlanner{
		Min: 64, Max: 4096,
		Indexer:  &errIndexer{}, // 主解析必失败（伪装容器）
		Fallback: []KeyframeIndexer{&fixedIndexer{frames: []int64{0, 1000}}},
	}
	bls, err := p.PlanBlocklets(bytes.NewReader(nil), 2000, 0, 2000)
	if err != nil {
		t.Fatalf("PlanBlocklets: %v", err)
	}
	// 关键帧 {0,1000} 生效 → 两段切分，非 fixed 退化、非降级错误段。
	if len(bls) != 2 {
		t.Fatalf("fallback 成功后 blocklet 数=%d，应为 2（关键帧切分生效）", len(bls))
	}
	if bls[0].Offset != 0 || bls[0].Size != 1000 || bls[1].Offset != 1000 || bls[1].Size != 1000 {
		t.Errorf("fallback 关键帧切分错误：%+v", bls)
	}
	// 主解析失败但 fallback 成功 → 无 parseErr（不降级）。
	if p.parseErr != nil {
		t.Errorf("fallback 成功不应记录 parseErr：%v", p.parseErr)
	}
	if len(p.failures) != 0 {
		t.Errorf("fallback 成功不应记录 failures：%v", p.failures)
	}
}

// TestVideoKeyframePlanner_FallbackAllFail（评审 C1）：主失败 + 全部 fallback 失败 →
// 按原降级语义退 fixed + 记录失败。
func TestVideoKeyframePlanner_FallbackAllFail(t *testing.T) {
	t.Parallel()
	p := &VideoKeyframeBlockletPlanner{
		Min: 64, Max: 256,
		Indexer:  &errIndexer{},
		Fallback: []KeyframeIndexer{&errIndexer{}, &errIndexer{}},
	}
	bls, err := p.PlanBlocklets(bytes.NewReader(nil), 1000, 0, 1000)
	if err != nil {
		t.Fatalf("PlanBlocklets: %v", err)
	}
	// 全部失败 → fixed 退化（blocklet ≤256）。
	got := int64(0)
	for _, bl := range bls {
		if bl.Type == BlockletTypeError {
			continue
		}
		if bl.Size > 256 {
			t.Errorf("全部失败 fixed 退化 blocklet=%d，应 ≤256", bl.Size)
		}
		got += bl.Size
	}
	if got != 1000 {
		t.Errorf("全部失败 fixed 覆盖=%d，应为 1000", got)
	}
	if p.parseErr == nil {
		t.Error("全部失败应记录 parseErr")
	}
}

// TestVideoKeyframePlanner_SplitsAtKeyframes：关键帧 {0, 1000, 2400}，块 [0,3000) →
// blocklet 边界 [0,1000),[1000,2400),[2400,3000)。
func TestVideoKeyframePlanner_SplitsAtKeyframes(t *testing.T) {
	t.Parallel()
	p := &VideoKeyframeBlockletPlanner{
		Min: 64, Max: 4096,
		Indexer: &fixedIndexer{frames: []int64{0, 1000, 2400}},
	}
	bls, err := p.PlanBlocklets(bytes.NewReader(nil), 3000, 0, 3000)
	if err != nil {
		t.Fatalf("PlanBlocklets: %v", err)
	}
	want := [][2]int64{{0, 1000}, {1000, 1400}, {2400, 600}}
	if len(bls) != 3 {
		t.Fatalf("blocklet 数=%d，应为 3", len(bls))
	}
	for i, w := range want {
		if bls[i].Offset != w[0] || bls[i].Size != w[1] {
			t.Errorf("blocklet[%d]=[%d,%d)，应为 [%d,%d)", i, bls[i].Offset, bls[i].Size, w[0], w[0]+w[1])
		}
		// 普通数据段 Type 为 0（加密时 blockletType 推断为 Data），与 fixed 契约一致。
		if bls[i].Type != 0 {
			t.Errorf("blocklet[%d] type=%d，应为 0（推断 Data）", i, bls[i].Type)
		}
	}
}

// TestVideoKeyframePlanner_BlockStraddlesKeyframe：块 [1500,2500) 起点落在 GOP 中间 →
// 首 blocklet 从块起点延续到下一关键帧（P 帧延续段），后续从关键帧切分。
func TestVideoKeyframePlanner_BlockStraddlesKeyframe(t *testing.T) {
	t.Parallel()
	p := &VideoKeyframeBlockletPlanner{
		Min: 64, Max: 4096,
		Indexer: &fixedIndexer{frames: []int64{1000, 2400}},
	}
	// 块 [1500,3000)，关键帧 2400 在块内：首段 [1500,2400)（P 延续），末段 [2400,3000)。
	bls, err := p.PlanBlocklets(bytes.NewReader(nil), 3000, 1500, 1500)
	if err != nil {
		t.Fatalf("PlanBlocklets: %v", err)
	}
	if len(bls) != 2 {
		t.Fatalf("blocklet 数=%d，应为 2（首段 P 延续 + 关键帧段）", len(bls))
	}
	if bls[0].Offset != 1500 || bls[0].Size != 900 {
		t.Errorf("首段=[%d,%d)，应为 [1500,2400)", bls[0].Offset, bls[0].Offset+bls[0].Size)
	}
	if bls[1].Offset != 2400 || bls[1].Size != 600 {
		t.Errorf("次段=[%d,%d)，应为 [2400,3000)", bls[1].Offset, bls[1].Offset+bls[1].Size)
	}
}

// TestVideoKeyframePlanner_ParseFailureFallsBackToFixed：解析完全失败 → 每块退化为
// fixed 定长 blocklet，并记录失败（不中断写）。
func TestVideoKeyframePlanner_ParseFailureFallsBackToFixed(t *testing.T) {
	t.Parallel()
	p := &VideoKeyframeBlockletPlanner{
		Min: 64, Max: 256,
		Indexer: &errIndexer{},
	}
	bls, err := p.PlanBlocklets(bytes.NewReader(nil), 1000, 0, 1000)
	if err != nil {
		t.Fatalf("PlanBlocklets: %v", err)
	}
	// fixed 退化：Min=64/Max=256 → 每 blocklet ≤256，末块收尾。
	if len(bls) == 0 {
		t.Fatal("fixed 退化不应为空")
	}
	got := int64(0)
	for _, bl := range bls {
		if bl.Type == BlockletTypeError {
			continue // 错误段不计入数据覆盖
		}
		if bl.Size <= 0 || bl.Size > 256 {
			t.Errorf("fixed 退化 blocklet 大小=%d，应在 (0,256]", bl.Size)
		}
		got += bl.Size
	}
	if got != 1000 {
		t.Errorf("fixed 退化覆盖总长=%d，应为 1000", got)
	}
	if len(p.failures) == 0 {
		t.Error("解析失败应记录 failures")
	}
}

// TestVideoKeyframePlanner_PartialParseUsesAvailableKeyframes：解析部分失败（截断）→
// 已解析出的关键帧照用，错误段追加在块末。
func TestVideoKeyframePlanner_PartialParseUsesAvailableKeyframes(t *testing.T) {
	t.Parallel()
	p := &VideoKeyframeBlockletPlanner{
		Min: 64, Max: 4096,
		Indexer: &partialIndexer{frames: []int64{0, 1000}},
	}
	bls, err := p.PlanBlocklets(bytes.NewReader(nil), 3000, 0, 3000)
	if err != nil {
		t.Fatalf("PlanBlocklets: %v", err)
	}
	// 已解析关键帧 {0,1000} 照用：[0,1000) + [1000,3000)，块内无更多关键帧；
	// 末尾追加错误段（部分解析失败记录）。
	if len(bls) != 3 {
		t.Fatalf("blocklet 数=%d，应为 3（部分关键帧照用 + 错误段）", len(bls))
	}
	if bls[0].Offset != 0 || bls[0].Size != 1000 {
		t.Errorf("段0=[%d,%d)，应为 [0,1000)", bls[0].Offset, bls[0].Offset+bls[0].Size)
	}
	if bls[1].Offset != 1000 || bls[1].Size != 2000 {
		t.Errorf("段1=[%d,%d)，应为 [1000,3000)", bls[1].Offset, bls[1].Offset+bls[1].Size)
	}
	if bls[2].Type != BlockletTypeError {
		t.Errorf("段2 type=%d，应为错误段", bls[2].Type)
	}
}

// partialIndexer 解析部分失败：返回可用关键帧 + 错误。
type partialIndexer struct{ frames []int64 }

func (p *partialIndexer) KeyframeOffsets(_ KeyframeRequest) ([]int64, error) {
	return p.frames, errors.New("keyframe: 视频截断，尾部关键帧不可用")
}

// TestVideoKeyframePlanner_NoKeyframesInBlock：块内无任何关键帧 → 单块整体
// （与 fixed 同语义，不产生空 blocklet）。
func TestVideoKeyframePlanner_NoKeyframesInBlock(t *testing.T) {
	t.Parallel()
	p := &VideoKeyframeBlockletPlanner{
		Min: 64, Max: 4096,
		Indexer: &fixedIndexer{frames: []int64{0, 5000}},
	}
	// 块 [1000,2000) 内无关键帧 → 单块 [1000,2000)。
	bls, err := p.PlanBlocklets(bytes.NewReader(nil), 6000, 1000, 1000)
	if err != nil {
		t.Fatalf("PlanBlocklets: %v", err)
	}
	if len(bls) != 1 || bls[0].Offset != 1000 || bls[0].Size != 1000 {
		t.Errorf("块内无关键帧应单块整体，got %+v", bls)
	}
}

// TestVideoKeyframePlanner_ErrorSegmentAppended：有失败记录时，返回序列末尾追加一个
// Error 类型 blocklet（Offset=块末、Data=失败 JSON）。
func TestVideoKeyframePlanner_ErrorSegmentAppended(t *testing.T) {
	t.Parallel()
	p := &VideoKeyframeBlockletPlanner{
		Min: 64, Max: 4096,
		Indexer: &errIndexer{},
	}
	bls, err := p.PlanBlocklets(bytes.NewReader(nil), 1000, 0, 1000)
	if err != nil {
		t.Fatalf("PlanBlocklets: %v", err)
	}
	last := bls[len(bls)-1]
	if last.Type != BlockletTypeError {
		t.Fatalf("末段 type=%d，应为 Error(0x15)", last.Type)
	}
	if last.Offset != 1000 {
		t.Errorf("错误段 Offset=%d，应为块末 1000", last.Offset)
	}
	if len(last.Data) == 0 || !bytes.Contains(last.Data, []byte("解析失败")) {
		t.Errorf("错误段 Data 应含失败 JSON，got %q", last.Data)
	}
}

// TestVideoKeyframePlanner_BadRangeFailClosed：块区间越界仍返回错误（保持 API
// fail-closed，仅解析失败走兼容降级）。
func TestVideoKeyframePlanner_BadRangeFailClosed(t *testing.T) {
	t.Parallel()
	p := &VideoKeyframeBlockletPlanner{
		Min: 64, Max: 4096,
		Indexer: &fixedIndexer{frames: []int64{0, 1000}},
	}
	if _, err := p.PlanBlocklets(bytes.NewReader(nil), 100, 0, 2000); err == nil {
		t.Error("块越出文件应 fail-closed")
	}
	if _, err := p.PlanBlocklets(bytes.NewReader(nil), 1000, 0, 0); err == nil {
		t.Error("空块应 fail-closed")
	}
}
