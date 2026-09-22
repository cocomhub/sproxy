// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"io"
)

// BlockAccessor 是可选接口：FS 实现它时，sync 引擎对 ActionUpdated 走块级增量
// （BlockDiff 找相同块 → 只复制差异块，省目标端写放大）。
//
// 复用分块基建说明：OpenReaderAt 返回的块读取器（*os.File 等）天然是
// io.ReaderAt + io.Seeker——BlockDiff 的块校验和（SHA-256）与分块上传
// ChunkChecksums 同算法，后续跨 FS 可衔接服务端块表实现「只传差异块」。
//
// 未实现该接口的 FS 回退原整文件复制（零回归）。
type BlockAccessor interface {
	// OpenReaderAt 打开路径的随机读（按块读取旧文件/源文件）。
	// 返回 (io.ReaderAt 实现, 关闭器, 错误)；关闭器可能为 nil。
	OpenReaderAt(ctx context.Context, path string) (io.ReaderAt, io.Closer, error)
	// OpenWriterAt 打开路径的随机写（差异块按 offset 写入）。
	// size 为最终文件大小（用于预分配）；mtime 为写入完成后应保留的 mtime
	// （0 = 不设置，保持当前）。返回 (io.WriterAt 实现, 关闭器, 错误)。
	OpenWriterAt(ctx context.Context, path string, size, mtime int64) (io.WriterAt, io.Closer, error)
}
