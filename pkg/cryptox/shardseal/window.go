// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

// window.go —— **写侧确定性缓冲窗口规划**（2026-10-25 用户设计）。
//
// 设计要点（三合一）：
//  1. **写侧确定**：窗口边界在加密时就规划好并写入 meta（ci.Windows）——读侧不再动态
//     计算（跨进程/跨版本一致，缓存可复用）；
//  2. **首位相连**：窗口连续覆盖块密文（含 blocklet 之间的随机间隙），无空洞 ⇒ 读任意
//     区间只需首尾两窗，并集 = 连续区间；
//  3. **salt 入首窗**：首窗起点 = 块头 salt 起点（blSaltOff），读时 salt 已在窗内 ⇒
//     免单独 32B Range 请求 + 可直接 VerifyBlockSalt（防块替换）；
//  4. **每窗 checksum**：sha256(窗口密文) 写入 meta ⇒ 读窗后本地校验，完整性**独立于
//     网盘**（不依赖远端 ETag/md5）——加密层即可发现网盘改/坏/丢字节。
//
// 窗口长度目标 = 调用方传入（如 4MiB = CDN 单次 Range 上限）；单个 blocklet 超目标时
// 独占一窗（GCM 段不可分割，绝不切分 blocklet）。

import "fmt"

// WindowInfo 描述块内一个**缓冲窗口**（写侧确定、读侧消费）。
type WindowInfo struct {
	// Index 是窗口在块内序号（0-based）。
	Index int `json:"index"`
	// EncOffset 是窗口密文起点（首窗 = 块头 salt 起点）。
	EncOffset int64 `json:"enc_offset"`
	// EncSize 是窗口密文长度。
	EncSize int64 `json:"enc_size"`
	// Checksum 是窗口密文的 sha256（hex，64 字符；旧 meta 无此字段——omitempty）。
	// 读侧命中缓存或远端拉取后校验，fail-closed（不因缓存/网盘掩盖损坏）。
	Checksum string `json:"checksum,omitempty"`
}

// PlanChunkWindows 规划块密文的缓冲窗口（纯函数；写侧与读侧兜底共用，唯一实现）。
//
// 输入：
//   - blobLen：块密文全长（ci.EncSize）；
//   - blocklets：块内 blocklet 索引（EncOffset/EncSize；Used=false 的 padding 跳过）；
//   - targetBytes：单窗长度目标（≤0 时取 4MiB）。
//
// 规则：
//  1. 首窗起点 = blSaltOff（含块头 salt）；末窗终点 = blobLen（含末尾 index 段）；
//  2. 窗口终点落在 blocklet 边界（EncOffset+EncSize）上；单个 blocklet 超目标 ⇒ 独占；
//  3. **首位相连**：每窗起点 = 上一窗终点（无空洞）；
//  4. 覆盖 [blSaltOff, blobLen)；blobLen ≤ 起点（空块）⇒ 返回 nil。
//
// windowCutPoints 收集有效 blocklet 的「密文末尾」作为候选切点（升序、去重、> blSaltOff）。
func windowCutPoints(blobLen int64, blocklets []BlockletInfo) []int64 {
	var cuts []int64
	seen := map[int64]struct{}{}
	for _, bl := range blocklets {
		if !bl.Used || bl.EncSize <= 0 {
			continue
		}
		end := bl.EncOffset + bl.EncSize
		if end <= int64(blSaltOff) || end >= blobLen {
			continue
		}
		if _, dup := seen[end]; dup {
			continue
		}
		seen[end] = struct{}{}
		cuts = append(cuts, end)
	}
	// 插入排序（blocklet 数少，且输入多为有序）。
	for i := 1; i < len(cuts); i++ {
		for j := i; j > 0 && cuts[j-1] > cuts[j]; j-- {
			cuts[j-1], cuts[j] = cuts[j], cuts[j-1]
		}
	}
	return cuts
}

func PlanChunkWindows(blobLen int64, blocklets []BlockletInfo, targetBytes int64) []WindowInfo {
	if targetBytes <= 0 {
		targetBytes = 4 << 20
	}
	if blobLen <= int64(blSaltOff) {
		return nil
	}
	cuts := windowCutPoints(blobLen, blocklets)
	var out []WindowInfo
	start := int64(blSaltOff)
	for start < blobLen {
		end := pickWindowEnd(start, blobLen, targetBytes, cuts)
		out = append(out, WindowInfo{Index: len(out), EncOffset: start, EncSize: end - start})
		if end <= start {
			// 防御：不可能（pickWindowEnd 恒 > start）；防死循环。
			break
		}
		start = end
	}
	return out
}

// pickWindowEnd 选窗口终点：不超过 start+targetBytes 的最大切点；无合适切点则取
// blobLen（末窗）或「越过目标的 blocklet 末尾」（单 blocklet 超目标时独占）。
func pickWindowEnd(start, blobLen, targetBytes int64, cuts []int64) int64 {
	limit := start + targetBytes
	end := int64(0)
	for _, c := range cuts {
		if c <= start {
			continue
		}
		if c <= limit {
			end = c // 仍在目标内：尽量吃满
			continue
		}
		if end == 0 {
			end = c // 首个切点已超目标 ⇒ 该 blocklet 独占
		}
		break
	}
	if end <= start || end > blobLen {
		end = blobLen
	}
	return end
}

// FillWindowChecksums 用块密文 enc 填充各窗口的 checksum（写侧调用；就地写）。
// 越界窗口（不应发生）跳过并返回错误——写路径 fail-closed，不产不可校验的 meta。
func FillWindowChecksums(windows []WindowInfo, enc []byte) error {
	for i := range windows {
		w := &windows[i]
		if w.EncOffset < 0 || w.EncSize <= 0 || w.EncOffset+w.EncSize > int64(len(enc)) {
			return fmt.Errorf("shardseal: 窗口 %d 区间 [%d,%d) 越出块密文长 %d", w.Index, w.EncOffset, w.EncOffset+w.EncSize, len(enc))
		}
		w.Checksum = Hash256(enc[w.EncOffset : w.EncOffset+w.EncSize])
	}
	return nil
}

// VerifyWindowChecksum 校验窗口密文与 meta 记录的 checksum 一致（读侧调用）。
// meta 无 checksum（旧格式）⇒ 跳过校验（零回归）。不一致 ⇒ 错误（fail-closed）。
func VerifyWindowChecksum(w WindowInfo, data []byte) error {
	if w.Checksum == "" {
		return nil
	}
	if int64(len(data)) != w.EncSize {
		return fmt.Errorf("shardseal: 窗口 %d 密文长度 %d 与 meta %d 不符", w.Index, len(data), w.EncSize)
	}
	if got := Hash256(data); got != w.Checksum {
		return fmt.Errorf("shardseal: 窗口 %d 密文校验失败（checksum 不符：%s != %s）", w.Index, got, w.Checksum)
	}
	return nil
}
