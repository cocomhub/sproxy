// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"bytes"
	"strings"
)

// ConflictHunk 描述一个冲突段的三方内容（索引展示/手动解决用）。
type ConflictHunk struct {
	// Base 是冲突段在 base 中的行（可能为空 = base 无此行）。
	Base []string `json:"base,omitempty"`
	// Ours 是冲突段在 ours 中的行。
	Ours []string `json:"ours"`
	// Theirs 是冲突段在 theirs 中的行。
	Theirs []string `json:"theirs"`
}

// merge3Marker 是冲突段的标记行（与 diff3/传统合并工具一致，纯文本可人工编辑）。
const (
	merge3MarkerOurs   = "<<<<<<<"
	merge3MarkerSplit  = "======="
	merge3MarkerTheirs = ">>>>>>>"
)

// Merge3 纯 Go 三方合并（类 diff3 语义）。
//
// 以 base 行为锚逐行推进，对每个 base 行判定两侧状态：
//   - here：目标当前行即该 base 行（保留待消费）；
//   - remain：该行在目标当前游标之后仍存在（未删除/未替代）；
//   - pre：目标当前游标到下一个该行之间插入/替代的行段。
//
// 判定：
//   - 单方保留原行 + 另一方 pre 为空但 remain=false（该行被替代）→ 单方修改取修改方；
//   - 单方保留 + 另一方 pre 非空且 remain=false → 修改取修改方 pre 段；
//   - 单方保留 + 另一方 pre 非空且 remain=true → 插入：输出 pre 段 + 该行；
//   - 双方都非 here：
//     · 双方 pre 都空且 remain 都 true（不可能，here 已排除）→ 防御；
//     · 一方 remain=true（插入/保留）另一方 pre 非空 remain=true → 输出插入方；
//     · 双方 remain=false 且双方 pre 都非空（都修改）→ 冲突；
//     · 一方 remain=false pre 空（删除）另一方保留 → 取保留方；
//     · 双方都 remain=false 且 pre 都空（双方删除）→ 不输出。
//
// 空 base：ours 与 theirs 逐行拼接（不判冲突）。
// 二进制/非文本由调用方判定（isMerge3Textable），本函数只做逐行文本合并。
func Merge3(base, ours, theirs []byte) (out []byte, conflicted bool, hunks []ConflictHunk) {
	baseLines := splitMerge3Lines(base)
	oursLines := splitMerge3Lines(ours)
	theirsLines := splitMerge3Lines(theirs)

	if len(baseLines) == 0 {
		merged := append([]string{}, oursLines...)
		merged = append(merged, theirsLines...)
		return joinMerge3Lines(merged), false, nil
	}

	s := &merge3State{}

	for i, bl := range baseLines {
		oursHere := s.j < len(oursLines) && oursLines[s.j] == bl
		theirsHere := s.k < len(theirsLines) && theirsLines[s.k] == bl

		oursNext := indexOfMerge3(oursLines, bl, s.j)
		theirsNext := indexOfMerge3(theirsLines, bl, s.k)
		oursRemain := oursNext < len(oursLines)
		theirsRemain := theirsNext < len(theirsLines)
		// pre 段：remain 时 = 到该行重现（终点 oursNext/theirsNext）；
		// 否则 = 到下一 base 行止（修改段，终点收集推进后位置）。
		oursPre, oursPreEnd := merge3PreSegment(oursLines, s.j, oursNext, oursRemain, baseLines, i+1)
		theirsPre, theirsPreEnd := merge3PreSegment(theirsLines, s.k, theirsNext, theirsRemain, baseLines, i+1)

		l := mergeLine{bl: bl, oursHere: oursHere, theirsHere: theirsHere,
			oursRemain: oursRemain, theirsRemain: theirsRemain,
			oursPre: oursPre, theirsPre: theirsPre, oursPreEnd: oursPreEnd, theirsPreEnd: theirsPreEnd}
		if hunk := s.decide(l); hunk != nil {
			s.conflicted = true
			hunks = append(hunks, *hunk)
		}
	}

	for ; s.j < len(oursLines); s.j++ {
		s.result = append(s.result, oursLines[s.j])
	}
	for ; s.k < len(theirsLines); s.k++ {
		s.result = append(s.result, theirsLines[s.k])
	}
	return joinMerge3Lines(s.result), s.conflicted, hunks
}

// merge3PreSegment 计算 base 行 bl 在单侧行集 target 中的 pre 段及推进终点：
// remain 时 = 到该行重现（终点 next）；否则 = 到下一 base 行止（修改段，终点收集推进后位置）。
func merge3PreSegment(target []string, j, next int, remain bool, baseLines []string, nextIdx int) ([]string, int) {
	if remain {
		return target[j:next], next
	}
	oj := j
	pre := collectUntilAnyBaseMerge3(target, &oj, baseLines, nextIdx)
	return pre, oj
}

// mergeLine 是单个 base 行的两侧判定上下文（S107：收敛 merge3Decide/merge3DecideNeither
// 的 12 参数为结构体；here/remain/pre/preEnd 判定结果在单行决策内固定）。
type mergeLine struct {
	bl           string
	oursHere     bool
	theirsHere   bool
	oursRemain   bool
	theirsRemain bool
	oursPre      []string
	theirsPre    []string
	oursPreEnd   int
	theirsPreEnd int
}

// merge3State 承载 Merge3 逐行三方合并的运行游标与输出状态（S107：result/j/k/conflicted
// 在各 base 行间传递），供 merge3Decide 家族方法读写。冲突 hunk 由调用方（Merge3 主循环）
// 收集，不存于本结构体。
type merge3State struct {
	result     []string
	j, k       int
	conflicted bool
}

// decide 对单个 base 行做三方合并决策，返回冲突 hunk（conflicted=true 时非 nil）。
// 合并判定与 Merge3 主循环逐字一致。
func (s *merge3State) decide(l mergeLine) *ConflictHunk {
	switch {
	case l.oursHere && l.theirsHere:
		s.result = append(s.result, l.bl)
		s.j++
		s.k++

	case l.oursHere && l.theirsRemain:
		// ours 保留，theirs 插入（该行在 theirs 后续仍在）→ 输出插入段 + 行。
		s.result = append(s.result, l.theirsPre...)
		s.result = append(s.result, l.bl)
		s.j++
		s.k = l.theirsPreEnd + 1

	case l.theirsHere && l.oursRemain:
		s.result = append(s.result, l.oursPre...)
		s.result = append(s.result, l.bl)
		s.k++
		s.j = l.oursPreEnd + 1

	case l.oursHere && !l.theirsRemain:
		// ours 保留，theirs 修改/删除该行。
		if len(l.theirsPre) == 0 {
			// 删除：theirs 当前即下一 base 行（或结尾）→ 取 ours（输出 bl）。
			s.result = append(s.result, l.bl)
			s.j++
		} else {
			// 修改：theirs 用 pre 段替代了该行 → 取 theirs 修改段。
			s.result = append(s.result, l.theirsPre...)
			s.j++
			s.k = l.theirsPreEnd
		}

	case l.theirsHere && !l.oursRemain:
		if len(l.oursPre) == 0 {
			s.result = append(s.result, l.bl)
			s.k++
		} else {
			s.result = append(s.result, l.oursPre...)
			s.k++
			s.j = l.oursPreEnd
		}

	case !l.oursHere && !l.theirsHere:
		return s.decideNeither(l)
	}
	return nil
}

// decideNeither 处理两侧目标行都不在当前位置的情况（删除/插入/冲突判定），
// 返回冲突 hunk（非冲突返回 nil）。合并判定与 Merge3 主循环逐字一致。
func (s *merge3State) decideNeither(l mergeLine) *ConflictHunk {
	oursDel := !l.oursRemain
	theirsDel := !l.theirsRemain
	switch {
	case oursDel && theirsDel:
		// 双方都删除/修改该行。
		switch {
		case len(l.oursPre) == 0 && len(l.theirsPre) == 0:
			// 双方纯删除 → 不输出。
		case len(l.oursPre) == 0:
			// ours 删除，theirs 修改 → 取 theirs 修改段。
			s.result = append(s.result, l.theirsPre...)
			s.k = l.theirsPreEnd
		case len(l.theirsPre) == 0:
			s.result = append(s.result, l.oursPre...)
			s.j = l.oursPreEnd
		case merge3LinesEqual(l.oursPre, l.theirsPre):
			// 双方改成相同内容 → 输出一份。
			s.result = append(s.result, l.oursPre...)
			s.j = l.oursPreEnd
			s.k = l.theirsPreEnd
		default:
			// 双方不同修改 → 冲突。
			hunk := &ConflictHunk{
				Base:   []string{l.bl},
				Ours:   append([]string{}, l.oursPre...),
				Theirs: append([]string{}, l.theirsPre...),
			}
			s.result = append(s.result, merge3MarkerOurs)
			s.result = append(s.result, l.oursPre...)
			s.result = append(s.result, merge3MarkerSplit)
			s.result = append(s.result, l.theirsPre...)
			s.result = append(s.result, merge3MarkerTheirs)
			s.j = l.oursPreEnd
			s.k = l.theirsPreEnd
			return hunk
		}
	case oursDel:
		// ours 删该行，theirs 保留/插入 → 取 theirs。
		s.result = append(s.result, l.theirsPre...)
		s.result = append(s.result, l.bl)
		s.k = l.theirsPreEnd + 1
	case theirsDel:
		s.result = append(s.result, l.oursPre...)
		s.result = append(s.result, l.bl)
		s.j = l.oursPreEnd + 1
	case len(l.oursPre) == 0:
		// ours 直接到该行（插入在 theirs）→ 输出 theirs 插入段 + 行。
		s.result = append(s.result, l.theirsPre...)
		s.result = append(s.result, l.bl)
		s.j++
		s.k = l.theirsPreEnd + 1
	case len(l.theirsPre) == 0:
		s.result = append(s.result, l.oursPre...)
		s.result = append(s.result, l.bl)
		s.k++
		s.j = l.oursPreEnd + 1
	default:
		// 双方都插入（pre 都非空且该行仍在两侧）→ 插入段拼接 + 行。
		s.result = append(s.result, l.oursPre...)
		s.result = append(s.result, l.theirsPre...)
		s.result = append(s.result, l.bl)
		s.j = l.oursPreEnd + 1
		s.k = l.theirsPreEnd + 1
	}
	return nil
}

// merge3LinesEqual 比较两个行切片是否逐行相等。
func merge3LinesEqual(a, b []string) bool {
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

// splitMerge3Lines 按行拆分（保留空行；统一 \r\n → \n）。
func splitMerge3Lines(b []byte) []string {
	s := string(bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")))
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	// 尾部空行（文本以 \n 结尾）不视为内容行。
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// joinMerge3Lines 逐行拼接（恢复 \n 结尾）。
func joinMerge3Lines(lines []string) []byte {
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// indexOfMerge3 返回 target[from:] 中首个 == want 的绝对下标（无则 len(target)）。
func indexOfMerge3(target []string, want string, from int) int {
	for i := from; i < len(target); i++ {
		if target[i] == want {
			return i
		}
	}
	return len(target)
}

// collectUntilAnyBaseMerge3 从 target[from:] 收集直到（不含）任一后续 base 行
// （baseLines[nextIdx:] 中的行）为止的行，推进游标（修改段收集：到下一 base 行止）。
func collectUntilAnyBaseMerge3(target []string, from *int, baseLines []string, nextIdx int) []string {
	nextBases := map[string]bool{}
	for _, bl := range baseLines[nextIdx:] {
		nextBases[bl] = true
	}
	var collected []string
	for *from < len(target) && !nextBases[target[*from]] {
		collected = append(collected, target[*from])
		*from++
	}
	return collected
}
