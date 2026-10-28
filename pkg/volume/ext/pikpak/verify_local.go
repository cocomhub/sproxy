// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// verify_local.go 是「本地文件是否等于分享/网盘里的那份内容」的**唯一判据**
// （用户裁定 2026-10-28：pikget 只做交互，判定逻辑属领域能力）。
//
// 用途：下载前的「已完成/冲突」判定（pikget 现存文件检查），以及重建后的定稿校验。
package pikpak

import (
	"fmt"
	"os"

	"github.com/cocomhub/sproxy/pkg/integrity"
)

// LocalFileMatch 是本地文件与期望 GCID 的比对结果。
type LocalFileMatch struct {
	Matched   bool   // 是否与期望 GCID 一致（任一候选分块命中）
	LocalGCID string // 命中的本地 GCID（未命中时给首个候选，供诊断；空 = 计算失败）
	Size      int64  // 本地文件大小（0 = 不存在/空）
}

// VerifyLocalFile 用官方 GCID 校验本地文件（候选分块全对比）。
// 文件不存在/为空 → (Size=0)，err 为 nil（调用方按「未完成」处理）。
// expectedGCID 为空 → 返回 Size 但 Matched=false（无权威可比）。
func VerifyLocalFile(path, expectedGCID string) (LocalFileMatch, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return LocalFileMatch{}, nil
		}
		return LocalFileMatch{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if st.IsDir() || st.Size() <= 0 {
		return LocalFileMatch{}, nil
	}
	out := LocalFileMatch{Size: st.Size()}
	if expectedGCID == "" {
		return out, nil
	}
	results, gerr := integrity.RecomputeGCIDBlocked(path)
	if gerr != nil {
		return out, fmt.Errorf("verify local %s: %w", path, gerr)
	}
	for _, r := range results {
		out.LocalGCID = r.GCID
		if equalFoldASCII(r.GCID, expectedGCID) {
			out.Matched = true
			return out, nil
		}
	}
	return out, nil
}

// equalFoldASCII 是仅 ASCII 的大小写无关比较（GCID 为十六进制，无需 unicode 折叠）。
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
