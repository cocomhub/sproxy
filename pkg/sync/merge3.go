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

	var result []string
	j, k := 0, 0

	for i, bl := range baseLines {
		oursHere := j < len(oursLines) && oursLines[j] == bl
		theirsHere := k < len(theirsLines) && theirsLines[k] == bl

		oursNext := indexOfMerge3(oursLines, bl, j)
		theirsNext := indexOfMerge3(theirsLines, bl, k)
		oursRemain := oursNext < len(oursLines)
		theirsRemain := theirsNext < len(theirsLines)
		// pre 段：remain 时 = 到该行重现（终点 oursNext/theirsNext）；
		// 否则 = 到下一 base 行止（修改段，终点收集推进后位置）。
		var oursPre, theirsPre []string
		oursPreEnd, theirsPreEnd := oursNext, theirsNext
		if oursRemain {
			oursPre = oursLines[j:oursNext]
		} else {
			oj := j
			oursPre = collectUntilAnyBaseMerge3(oursLines, &oj, baseLines, i+1)
			oursPreEnd = oj
		}
		if theirsRemain {
			theirsPre = theirsLines[k:theirsNext]
		} else {
			tk := k
			theirsPre = collectUntilAnyBaseMerge3(theirsLines, &tk, baseLines, i+1)
			theirsPreEnd = tk
		}

		switch {
		case oursHere && theirsHere:
			result = append(result, bl)
			j++
			k++

		case oursHere && theirsRemain:
			// ours 保留，theirs 插入（该行在 theirs 后续仍在）→ 输出插入段 + 行。
			result = append(result, theirsPre...)
			result = append(result, bl)
			j++
			k = theirsPreEnd + 1

		case theirsHere && oursRemain:
			result = append(result, oursPre...)
			result = append(result, bl)
			k++
			j = oursPreEnd + 1

		case oursHere && !theirsRemain:
			// ours 保留，theirs 修改/删除该行。
			if len(theirsPre) == 0 {
				// 删除：theirs 当前即下一 base 行（或结尾）→ 取 ours（输出 bl）。
				result = append(result, bl)
				j++
			} else {
				// 修改：theirs 用 pre 段替代了该行 → 取 theirs 修改段。
				result = append(result, theirsPre...)
				j++
				k = theirsPreEnd
			}

		case theirsHere && !oursRemain:
			if len(oursPre) == 0 {
				result = append(result, bl)
				k++
			} else {
				result = append(result, oursPre...)
				k++
				j = oursPreEnd
			}

		case !oursHere && !theirsHere:
			oursDel := !oursRemain
			theirsDel := !theirsRemain
			switch {
			case oursDel && theirsDel:
				// 双方都删除/修改该行。
				switch {
				case len(oursPre) == 0 && len(theirsPre) == 0:
					// 双方纯删除 → 不输出。
				case len(oursPre) == 0:
					// ours 删除，theirs 修改 → 取 theirs 修改段。
					result = append(result, theirsPre...)
					k = theirsPreEnd
				case len(theirsPre) == 0:
					result = append(result, oursPre...)
					j = oursPreEnd
				case merge3LinesEqual(oursPre, theirsPre):
					// 双方改成相同内容 → 输出一份。
					result = append(result, oursPre...)
					j = oursPreEnd
					k = theirsPreEnd
				default:
					// 双方不同修改 → 冲突。
					conflicted = true
					hunks = append(hunks, ConflictHunk{
						Base:   []string{bl},
						Ours:   append([]string{}, oursPre...),
						Theirs: append([]string{}, theirsPre...),
					})
					result = append(result, merge3MarkerOurs)
					result = append(result, oursPre...)
					result = append(result, merge3MarkerSplit)
					result = append(result, theirsPre...)
					result = append(result, merge3MarkerTheirs)
					j = oursPreEnd
					k = theirsPreEnd
				}
			case oursDel:
				// ours 删该行，theirs 保留/插入 → 取 theirs。
				result = append(result, theirsPre...)
				result = append(result, bl)
				k = theirsPreEnd + 1
			case theirsDel:
				result = append(result, oursPre...)
				result = append(result, bl)
				j = oursPreEnd + 1
			case len(oursPre) == 0:
				// ours 直接到该行（插入在 theirs）→ 输出 theirs 插入段 + 行。
				result = append(result, theirsPre...)
				result = append(result, bl)
				j++
				k = theirsPreEnd + 1
			case len(theirsPre) == 0:
				result = append(result, oursPre...)
				result = append(result, bl)
				k++
				j = oursPreEnd + 1
			default:
				// 双方都插入（pre 都非空且该行仍在两侧）→ 插入段拼接 + 行。
				result = append(result, oursPre...)
				result = append(result, theirsPre...)
				result = append(result, bl)
				j = oursPreEnd + 1
				k = theirsPreEnd + 1
			}
		}
	}

	for ; j < len(oursLines); j++ {
		result = append(result, oursLines[j])
	}
	for ; k < len(theirsLines); k++ {
		result = append(result, theirsLines[k])
	}
	return joinMerge3Lines(result), conflicted, hunks
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
