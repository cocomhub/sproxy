// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import "strings"

// reconcileBlockList 比较远端 `block_list` 与本地分块 md5 列表（依据 docs/external-dependencies.md §7.8.5 实测）。
//
// 语义（实测：百度 `block_list` = **按 4 MiB 分块的真实内容 md5 列表**，与本地 `uploadBlockSize` 同构、
// 且**上传后 T+0 即可用**；1 MiB / 8 MiB / 100 MiB / 512 MiB 四档逐块相等已验证）：
//   - 返回 (true, true)   ⇒ **内容已在云端**（可对客户端返回成功；`block_list` 一致即客户端成功）；
//   - 返回 (false, true)  ⇒ **真不一致**（逐块比对后有差异）⇒ 重传；
//   - 返回 (false, false) ⇒ **不可判定**（任一侧为空 / 长度不等 / 含非法值）⇒ 调用方**必须**
//     保留本地副本 + 重试，**绝不可**视为一致（否则会误删唯一本地源）。
//
// 为什么长度不等必须判"不可判定"而非"不一致"：远端可能只返回部分块（尚未可见/分页/异常），
// 直接判"不一致"会触发无谓重传；判"不可判定"则走"保留 + 复查"（更安全、且由重试收敛）。
func reconcileBlockList(remote, local []string) (equal, decidable bool) {
	if len(remote) == 0 || len(local) == 0 || len(remote) != len(local) {
		return false, false
	}
	for i := range remote {
		if !isHex32Str(remote[i]) || !isHex32Str(local[i]) {
			return false, false
		}
	}
	for i := range remote {
		if !strings.EqualFold(remote[i], local[i]) {
			return false, true
		}
	}
	return true, true
}

// md5Authoritative 判断远端 `md5` 字段是否权威：**形态合法（32 位 hex）且与期望值相等**（大小写不敏感）。
//
// ⚠️ 实测反例（§7.8.5）：远端曾返回 `a6804cf3…`（**形态合法但不是整文件 md5**，是分片组合值），
// 也出现过 `23c82fb98j92d22b2632be89eeb6cee8`（**33 位且含非法字符 `j`**）
// ⇒ **只看形态会误判权威**，必须与实际值逐字比对。
func md5Authoritative(remoteMD5, wantMD5 string) bool {
	if !isHex32Str(remoteMD5) || !isHex32Str(wantMD5) {
		return false
	}
	return strings.EqualFold(remoteMD5, wantMD5)
}

// isHex32Str 判断是否为合法 32 位 hex（百度"可信 md5"的形态）。
func isHex32Str(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !ok {
			return false
		}
	}
	return true
}
