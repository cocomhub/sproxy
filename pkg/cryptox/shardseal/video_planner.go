// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
)

// VideoKeyframeBlockletPlanner 是 BlockletMode "video-keyframe" 的规划器：把单个块按
// 视频关键帧（I 帧）字节边界切分为 blocklet 序列。每个 blocklet 起点对齐关键帧 →
// seek 到任意时间点只需下载/解密「含目标关键帧的 blocklet 段」即可（边缓冲边播基础）。
//
// 降级语义（用户裁定，2026-10-04）：解析失败可能是视频异常截断，可用部分依旧可播放。
//   - 完全解析失败 → 每块退化为 fixed 定长 blocklet，记录失败，不中断整个写；
//   - 部分可用 → 已解析出的关键帧照用，未覆盖区间退化为 fixed；
//   - 任何失败都会在块末尾追加一个 Error 类型 blocklet（明文=失败 JSON，全密文入 blob）
//     并在 p.failures 记录，供 meta 落盘与告警预留。
type VideoKeyframeBlockletPlanner struct {
	// Min/Max 是 blocklet 大小区间（继承 BlockPolicy；仅作 GOP 异常告警阈值，不硬切 GOP
	// ——GOP 跨关键帧是一个解码单元，切开会破坏视频，宁可单 blocklet 超 Max 也不拆）。
	Min, Max int64
	// Indexer 是关键帧解析器（装配时由 ResolveBlockletMode 注入）。
	Indexer KeyframeIndexer
	// Fallback 是主解析失败时的备用解析器链（伪装扩展名/截断等：MP4 容器识别失败时
	// 依次尝试 ffprobe 兜底——方案 A 2026-10-04）。首个成功者用其结果。
	Fallback []KeyframeIndexer
	// SrcPath 是源文件真实路径（EncryptShards 文件变体注入；内存变体为空）。传给
	// KeyframeRequest.Path——ffprobe 有路径走文件模式（可 seek 最优），无则 stdin 流
	// （接受不可 seek 降级，**绝不落临时文件复制数据**——用户裁定禁隐藏高代价行为）。
	SrcPath string
	// fixed 是解析失败时的退化规划器。
	fixed *FixedBlockletPlanner

	once     sync.Once
	frames   []int64         // 绝对关键帧偏移（升序）；sync.Once 缓存跨块复用
	parseErr error           // 解析失败（部分可用时 frames 仍有效）
	failures []BlockErrorMsg // 本 planner 生命周期内的失败记录（写 meta 与错误段）
}

// NewVideoKeyframeBlockletPlanner 构造视频关键帧规划器：fixed 退化规划器**构造期初始化**
// （评审 I-2 修复：消除 fixedPlan 懒初始化的潜在数据竞争——当前写路径串行不触发，但显式
// 初始化彻底杜绝隐患，未来并发调用也安全）。fallback 链透传（主解析失败时按序尝试）。
func NewVideoKeyframeBlockletPlanner(min, max int64, indexer KeyframeIndexer, fallback ...KeyframeIndexer) *VideoKeyframeBlockletPlanner {
	if min <= 0 {
		min = 64 << 10
	}
	if max < min {
		max = min
	}
	return &VideoKeyframeBlockletPlanner{
		Min:      min,
		Max:      max,
		Indexer:  indexer,
		Fallback: fallback,
		fixed:    &FixedBlockletPlanner{Min: min, Max: max},
	}
}

// PlanBlocklets 实现 BlockletPlanner。校验 Min/Max>0、块区间不越界（fail-closed 保持）；
// 仅「关键帧解析失败」走兼容降级。
func (p *VideoKeyframeBlockletPlanner) PlanBlocklets(data io.ReaderAt, origSize, blockOffset, blockSize int64) ([]Blocklet, error) {
	if p.Min <= 0 || p.Max < p.Min {
		return nil, fmt.Errorf("shardseal: 非法 blocklet 区间 Min=%d Max=%d（需 0<Min≤Max）", p.Min, p.Max)
	}
	if blockSize <= 0 {
		return nil, fmt.Errorf("shardseal: 空块不可细分 blocklet（size=%d）", blockSize)
	}
	if blockOffset < 0 || origSize < blockOffset+blockSize {
		return nil, fmt.Errorf("shardseal: 块 [%d,%d) 越出文件 [0,%d)", blockOffset, blockOffset+blockSize, origSize)
	}
	p.parseOnce(data, origSize)

	out, err := p.splitAtKeyframes(blockOffset, blockOffset+blockSize)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("shardseal: 块 [%d,%d) 未产生任何 blocklet", blockOffset, blockOffset+blockSize)
	}
	// 末尾追加错误段（有失败记录时；Offset=块末越过已用区）。
	if errData := p.errorSegmentJSON(); errData != nil {
		out = append(out, Blocklet{
			Offset: blockOffset + blockSize,
			Size:   int64(len(errData)),
			Type:   BlockletTypeError,
			Data:   errData,
		})
	}
	return out, nil
}

// parseOnce 首次调用解析整文件关键帧表（跨块复用缓存）。部分失败 → frames 有效 +
// parseErr 记录；完全失败 → frames 为空，退 fixed。
//
// **并发安全（评审 I-2 修复）**：fixed 惰性构造移到构造期一次性完成（NewVideoKeyframePlanner），
// fixedPlan 不再懒初始化——消除「先判 nil 再赋值」的潜在数据竞争（当前写路径串行不触发，
// 但显式初始化彻底杜绝隐患）。failures 的写只在 once.Do 内（勿移出）。
//
// **Fallback 链（方案 A 2026-10-04）**：主 Indexer 解析失败（伪装扩展名/截断/异常容器，
// 如 TS 流改名 .mp4）→ 依次尝试 Fallback（装配时注入 ffprobe）。首个成功者用其结果
// （frames + 无 parseErr）；全部失败 → 按原降级语义退 fixed。req 模式：Path 与 Reader
// 至少一个可用（secretdata 写路径持内存明文传 Reader；EncryptShards 文件变体传 Path
// 供 ffprobe 走文件路径避免 stdin 大文件降级）。
func (p *VideoKeyframeBlockletPlanner) parseOnce(data io.ReaderAt, origSize int64) {
	p.once.Do(func() {
		if p.Indexer == nil {
			p.parseErr = errors.New("shardseal: video-keyframe 模式缺少 Indexer（未注册 keyframe 提供者）")
			return
		}
		req := KeyframeRequest{Path: p.SrcPath, Reader: data, Size: origSize}
		frames, err := keyframeOffsetsDispatch(p.Indexer, req)
		if err != nil {
			// 主解析失败 → fallback 链（按序尝试，首个成功者用其结果）。
			for _, fb := range p.Fallback {
				fframes, ferr := keyframeOffsetsDispatch(fb, req)
				if ferr == nil {
					p.frames = fframes
					return // fallback 成功：不记失败、不退 fixed
				}
				err = ferr // 保留最后一个错误（全部失败时记录）
			}
		}
		p.frames = frames
		p.parseErr = err
		if err != nil {
			p.failures = append(p.failures, BlockErrorMsg{
				Code: "keyframe_parse",
				Msg:  fmt.Sprintf("关键帧解析失败: %v", err),
			})
			slog.Warn("shardseal: 视频关键帧解析失败，降级处理",
				"err", err, "usable_keyframes", len(frames))
		}
	})
}

// splitAtKeyframes 按关键帧边界切分 [lo, hi)：首块 [lo, kf1)（P 帧延续），之后
// [kf_i, kf_{i+1})，末块 [kf_m, hi)。块内无关键帧 → 单块整体。关键帧不在块内时退化 fixed。
func (p *VideoKeyframeBlockletPlanner) splitAtKeyframes(lo, hi int64) ([]Blocklet, error) {
	// 完全解析失败 / 无 Indexer → 退化为 fixed。
	if p.Indexer == nil || (p.parseErr != nil && len(p.frames) == 0) {
		return p.fixedPlan(lo, hi)
	}
	// 收集落在 [lo, hi) 内的关键帧。
	var kf []int64
	for _, f := range p.frames {
		if f >= lo && f < hi {
			kf = append(kf, f)
		}
	}
	if len(kf) == 0 {
		// 块内无关键帧 → 单块整体（与 fixed 同语义）。
		return []Blocklet{{Offset: lo, Size: hi - lo}}, nil
	}
	var out []Blocklet
	cur := lo
	for i, f := range kf {
		if f > cur {
			out = append(out, p.warnIfHugeGOP(Blocklet{Offset: cur, Size: f - cur}))
		}
		cur = f
		if i < len(kf)-1 {
			// 中间关键帧：段到下一关键帧（GOP 边界）。
			continue
		}
		// 末关键帧：段到块末。
		if hi > cur {
			out = append(out, p.warnIfHugeGOP(Blocklet{Offset: cur, Size: hi - cur}))
		}
	}
	return out, nil
}

// warnIfHugeGOP 对超过 Max 的单 blocklet（长 GOP：两关键帧间隔超 blocklet 上限）打一条
// warn 告警。不拆 GOP（跨关键帧是解码单元，切开会破坏视频）——单 blocklet 允许超 Max，
// 随机读退化为整段下载属已知取舍（M6 评审修复）。返回原 blocklet 不改动。
func (p *VideoKeyframeBlockletPlanner) warnIfHugeGOP(bl Blocklet) Blocklet {
	if p.Max > 0 && bl.Size > p.Max {
		slog.Warn("shardseal: 视频关键帧间隔（GOP）超 blocklet 上限，单段容纳",
			"size", bl.Size, "max", p.Max, "offset", bl.Offset)
	}
	return bl
}

// fixedPlan 退化为 fixed 定长 blocklet（解析失败时整文件降级）。fixed 已在构造期初始化
// （NewVideoKeyframeBlockletPlanner），正式路径无懒初始化竞态；下方 nil 兜底仅防御
// 测试/直接字面量构造（懒路径永不执行 → 无「先判 nil 再赋值」竞态窗口）。
func (p *VideoKeyframeBlockletPlanner) fixedPlan(lo, hi int64) ([]Blocklet, error) {
	if p.fixed == nil {
		p.fixed = &FixedBlockletPlanner{Min: p.Min, Max: p.Max}
	}
	// FixedBlockletPlanner.PlanBlocklets 需要整文件尺寸与块内相对偏移；这里构造一个
	// 覆盖 [lo,hi) 的块描述调用。
	return p.fixed.PlanBlocklets(nil, hi, lo, hi-lo)
}

// errorSegmentJSON 返回错误段的 JSON 载荷（failures 记录；无失败返回 nil）。
// 全密文：载荷作为段明文经 sealBlockletSegment GCM 加密后入 blob，磁盘无明文。
func (p *VideoKeyframeBlockletPlanner) errorSegmentJSON() []byte {
	if len(p.failures) == 0 {
		return nil
	}
	b, err := json.Marshal(p.failures)
	if err != nil {
		slog.Warn("shardseal: 错误段 JSON 序列化失败", "err", err)
		return nil
	}
	return b
}

// Failures 返回本 planner 生命周期内的失败记录（供 meta 落盘 ChunkInfo.Failures）。
func (p *VideoKeyframeBlockletPlanner) Failures() []BlockErrorMsg {
	return p.failures
}
