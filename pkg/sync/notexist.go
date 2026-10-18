// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"errors"
	"io"
	"io/fs"
	"os"
)

// 本文件是**项目 not-exist 约定的单一实现**（此前散落在 pkg/volume/enclosure/region.go
// 的 regionAbsent 与各卷后端）。「不存在」有两种合法表达：
//
//	① 返回满足 `errors.Is(err, os/fs.ErrNotExist)` 的错误；
//	② 按 sync.FS 契约返回 `(nil, nil)`（由调用方判空）。
//
// 为何必须收敛：卷内账本（pkg/volume/capacity）据「不存在 vs 真错误」区分
// 「空账本（首次运行）」与「不可确定（⇒ 判损坏 ⇒ fail-closed 拒写）」。
// 任一后端把「不存在」表达成裸错误 ⇒ 该卷账本恒拒写（整卷不可用 ✗）；反之把真错误
// 当「不存在」⇒ 账本读空 ⇒ 限额 fail-open（本包最危险形态 ✗）。两侧都是生产级事故。

// IsNotExist 报告 err 是否表示「不存在」（契约①，含任意层 %w 包装）。
func IsNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, fs.ErrNotExist)
}

// Absent 报告一次「按名读取」是否表示不存在（覆盖契约①与②）：
//   - err != nil ⇒ 仅当 IsNotExist(err)；
//   - err == nil && rc == nil ⇒ 契约②（不存在）；
//   - err == nil && rc != nil ⇒ 存在。
func Absent(err error, rc io.ReadCloser) bool {
	if err != nil {
		return IsNotExist(err)
	}
	return rc == nil
}
