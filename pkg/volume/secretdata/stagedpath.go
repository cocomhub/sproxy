// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"context"
	"fmt"
	"os"
	"strings"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// stagedpath.go 接入仓库既有的**本地 staging 路径复用**机制（`syncpkg.StagedPathReuse`，
// 2026-10-18）：调用方若已把上传内容暂存到本地文件，本卷**直接以该路径为源**加密，不再要求
// 调用方把内容再经 Reader 喂一遍（对不可 seek 的源，写路径会回退「整读进内存」⇒ 峰值 1× 内存 ✗）。
//
// 与 `StagingQuotaCapable` 的分工（两个关注点显式分离）：
//   - `StagingQuotaCapable`：本卷**自身**的加密分块临时目录（≈ 明文）纳入本地 staging 配额；
//   - `StagedPathReuse`：**调用方**已暂存的源文件被本卷直接复用（只读），省掉「再拷一份源」。
//     该源文件的空间已由调用方（经 gate 按 1×）预留 ⇒ 本卷**不**为它重复预留（避免双计）。
var _ syncpkg.StagedPathReuse = (*SecretdataFS)(nil)

// WriteFromStagedPath 把调用方已落盘的本地 staging 文件 stagedPath 加密写入 relPath。
//
// 生命周期契约（见 pkg/sync/stagedpath.go）：stagedPath **只读**——本实现不移动/删除/改写它，
// 清理由调用方负责（本实现返回前可能仍在读它）。size<=0 = 未知，按实际文件大小结算。
func (s *SecretdataFS) WriteFromStagedPath(ctx context.Context, relPath, stagedPath string, size, mtime int64) error {
	if stagedPath == "" {
		return fmt.Errorf("secretdata: WriteFromStagedPath 需要已暂存的本地路径")
	}
	f, err := os.Open(stagedPath) // 只读打开（不碰调用方的文件）
	if err != nil {
		return fmt.Errorf("secretdata: 打开已暂存文件 %s 失败: %w", stagedPath, err)
	}
	defer f.Close()
	if size <= 0 {
		if fi, serr := f.Stat(); serr == nil {
			size = fi.Size()
		}
	}
	// 以可 seek 的 *os.File 为源 ⇒ 写路径走**流式加密**（内存峰值 O(单块上限)），
	// 而不是把整份内容读进内存。
	return s.writeFileOpt(ctx, strings.TrimPrefix(relPath, "/"), f, size, mtime, -1, false)
}
