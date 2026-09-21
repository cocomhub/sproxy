// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package checksum

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sync"
)

// hashBufSize 是流式哈希的拷贝缓冲大小。只影响拷贝次数，不影响摘要值；两侧历史实现
// 同为 256 KiB，本包作为单一事实源沿用该值（改动不会改变任何文件的 SHA-256）。
const hashBufSize = 256 * 1024

// copyBufPool 复用以 Reader 为主的 256 KiB 拷贝缓冲。io.CopyBuffer 会在返回前释放对
// 缓冲的引用，归还前无残留引用；缓冲内容为读取的明文数据，不清零（池内缓冲被再次
// 取出时按容量切片使用，读入的数据会被覆盖）。
var copyBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, hashBufSize)
		return b
	},
}

// Reader 计算 src 的 SHA-256 十六进制摘要（小写）。会完全消耗 src，调用方负责关闭
// 实现了 io.Closer 的入参。
//
// **本函数是「文件/流校验和计算」的单一事实源**：pkg/files 的 checksumReader 与
// pkg/server 的 Checksum 都委托到这里。二者原先各持一份逐字相同的实现（sha256 + hex
// + 256 KiB CopyBuffer），属"同一语义两处定义"——抽取期无法收敛（pkg/files 不能反向
// import pkg/server），故下沉到两侧共同依赖的下层包（本包为 L0，两侧均在其上层）。
// 委托后算法只有一处，任何一侧重新内联实现都会被源码级守卫
// `pkg/server/helper_impl_drift_test.go` 判红。
func Reader(src io.Reader) (string, error) {
	dst := sha256.New()
	bufp, _ := copyBufPool.Get().(*[]byte) //nolint:errcheck // pool 无错误返回，断言防御
	buf := make([]byte, hashBufSize)
	if bufp != nil {
		buf = *bufp
	}
	if _, err := io.CopyBuffer(dst, src, buf); err != nil {
		return "", err
	}
	if bufp != nil {
		copyBufPool.Put(bufp)
	}
	return hex.EncodeToString(dst.Sum(nil)), nil
}
