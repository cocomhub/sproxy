// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"errors"
	"fmt"
	"io"
)

// ExactRangeReader 把区间流限制为**恰好 size 字节**，并让**提前 EOF 变成错误**。
//
// 动机（2026-10-14 跨层扫雷）：RangeReader 的三处实现长度语义不一致——本地卷/加密卷对
// 越出区间 fail-closed（`offset+size > 文件大小` ⇒ 错误），而 baidupcs/remote 把请求
// 区间交给下游 HTTP（`Content-Range` 被服务端**钳制**时只回更少字节、无任何错误）⇒
// 同一请求随卷类型表现为「报错」或「静默短读」。静默短读是截断型缺陷的温床（播放器/
// 校验/断点续传都建立在「区间字节数精确」之上），故此处统一兜底：
// 服务端钳制/对象短于声明 ⇒ 报错，而不是安静地少给。
//
// 语义：
//   - 读满 size 字节后 ⇒ EOF（后续 Read 恒 (0, io.EOF)）；
//   - 底层在未读满前 EOF ⇒ 返回**错误**（附带还差多少字节）；
//   - `size <= 0` 或 `rc == nil` ⇒ 原样返回（空区间由调用方直接返回空流）。
type RangeExactReader struct {
	rc     io.ReadCloser
	remain int64
}

// ExactRangeReader 见类型注释。
func ExactRangeReader(rc io.ReadCloser, size int64) io.ReadCloser {
	if rc == nil || size <= 0 {
		return rc
	}
	return &RangeExactReader{rc: rc, remain: size}
}

// Read 实现 io.Reader：限流到 remain 字节，提前 EOF ⇒ 错误。
func (e *RangeExactReader) Read(p []byte) (int, error) {
	if e.remain <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > e.remain {
		p = p[:e.remain]
	}
	n, err := e.rc.Read(p)
	e.remain -= int64(n)
	if e.remain > 0 && errors.Is(err, io.EOF) {
		return n, fmt.Errorf("sync: 区间流提前结束（还差 %d 字节；服务端钳制或对象短于声明）", e.remain)
	}
	return n, err
}

// Close 委托底层。
func (e *RangeExactReader) Close() error { return e.rc.Close() }

// 空区间读流：`size == 0` 时各实现统一返回它（不读底层、不报错）。
func emptyRangeReader() io.ReadCloser { return io.NopCloser(&emptyReader{}) }

type emptyReader struct{}

func (*emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

// EmptyRangeReader 返回契约规定的「空区间」读流（供各 RangeReader 实现统一复用）。
func EmptyRangeReader() io.ReadCloser { return emptyRangeReader() }
