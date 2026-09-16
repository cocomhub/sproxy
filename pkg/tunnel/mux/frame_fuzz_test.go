// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"encoding/binary"
	"errors"
	"testing"
)

// FuzzDecodeFrame 检查 DecodeFrame 在任意字节输入下不 panic，且解析结果自洽。
//
// 背景（#301）：解码侧曾直接 `binary.BigEndian.Uint32(payload)` 而无长度校验，
// 对端只发 1 字节 WindowUpdate 负载即可让 readLoop panic（整个进程崩溃，远程 DoS）。
// 本 fuzz 是那类「短帧/畸形帧」问题的回归防线：断言
//   - 任意输入不 panic（fuzz 框架把 panic 当 crash）；
//   - 解析成功时 payload 长度必须等于头部声明的 Length 字段——一旦实现退化为
//     「声明大但实际不足时静默截断」（返回部分 payload + nil 错误），此处即红；
//   - 解析失败时错误必须是已定义的哨兵错误（ErrFrameTooShort / ErrFrameTruncated）。
func FuzzDecodeFrame(f *testing.F) {
	// seed corpus：合法帧 + 边界形状。
	sid := StreamID(1)
	valid, err := EncodeFrame(sid, FrameData, []byte("hello"))
	if err != nil {
		f.Fatalf("EncodeFrame(seed): %v", err)
	}
	f.Add(valid)
	f.Add([]byte{})              // 空
	f.Add([]byte{0, 0, 0, 0})    // 只有 streamID
	f.Add([]byte{0, 0, 0, 0, 1}) // streamID + 半个头
	f.Add(valid[:len(valid)-1])  // 截断一字节
	// 短 WindowUpdate：类型 7 + 声明 256 字节负载但只有 0 字节（#301 同类形状）。
	f.Add([]byte{0, 0, 0, 1, byte(FrameWindowUpdate), 0, 0x01, 0x00})

	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, payload, err := DecodeFrame(data)
		if err == nil {
			// 解析成功必须完整消费：payload 长度 == 头部声明的 Length。
			declared := int(binary.BigEndian.Uint16(data[headerLengthOff:]))
			if len(payload) != declared {
				t.Fatalf("DecodeFrame returned %d payload bytes, header declares %d (silent truncation)", len(payload), declared)
			}
			return
		}
		// 错误必须可预期（已定义哨兵错误），不 panic。
		if !errors.Is(err, ErrFrameTooShort) && !errors.Is(err, ErrFrameTruncated) {
			t.Fatalf("DecodeFrame returned unexpected error: %v", err)
		}
	})
}
