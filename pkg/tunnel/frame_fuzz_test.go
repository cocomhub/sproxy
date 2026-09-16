// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// FuzzTunnelMetaFrame 检查隧道统一帧的元数据头（[4B big-endian metaLen][encrypted
// metadata]）解析在任意字节输入下不 panic、metaLen 上界受控。
//
// 威胁模型：metaLen 声明超过 MaxMetadataBytes（1 MiB）时必须快速返回
// ErrMetadataTooLarge（readEncMeta 的 fail-closed），不能尝试 make([]byte, 巨量)
// 分配或 panic（tunnel.go:255 readEncMeta 是 resolveKey / decodeMetadataFrame 的
// 共享解析点；tunnel_mux.go readAndDecryptMeta 有独立同款校验）。
func FuzzTunnelMetaFrame(f *testing.F) {
	// seed corpus：合法元数据帧（encodeMetadataFrame 编码侧生成）+ 边界形状。
	valid, err := encodeMetadataFrame(testKey, []byte(`{"method":"GET","url":"/"}`))
	if err != nil {
		f.Fatalf("encodeMetadataFrame(seed): %v", err)
	}
	f.Add(valid)
	f.Add([]byte{0, 0, 0, 0})             // metaLen=0
	f.Add([]byte{0, 0, 0, 10})            // 声明 10 但无 metadata 字节
	f.Add([]byte{0xff, 0xff, 0xff, 0xff}) // 超大 metaLen（> 1 MiB，应快速拒收）
	f.Add([]byte{0, 0, 0, 5, 1, 2, 3})    // 声明 5 只有 3（截断）
	f.Add([]byte{0, 0, 0, 0, 0, 0})       // 只有半个头

	f.Fuzz(func(t *testing.T, data []byte) {
		// 直接调用共享解析函数 readEncMeta（不解密；解密/JSON 解析不属于本 fuzz 面）。
		encMeta, err := readEncMeta(bytes.NewReader(data))
		if err == nil {
			// 解析成功：密文长度必须 == 头部声明的 metaLen，且不超协议上限。
			if uint32(len(encMeta)) != declaredMetaLen(data) {
				t.Fatalf("readEncMeta returned %d bytes, header declares %d", len(encMeta), declaredMetaLen(data))
			}
			return
		}
		// 超限声明必须得到 ErrMetadataTooLarge（fail-closed），而非尝试分配巨内存。
		if len(data) >= 4 && declaredMetaLen(data) > MaxMetadataBytes {
			if !errors.Is(err, ErrMetadataTooLarge) {
				t.Fatalf("oversized metaLen %d: expected ErrMetadataTooLarge, got %v", declaredMetaLen(data), err)
			}
			return
		}
		// 其它错误（头不足 / body 截断）可预期；关键是解析不 panic。
	})
}

// declaredMetaLen 读取 [4B big-endian] 声明的元数据长度。
func declaredMetaLen(data []byte) uint32 {
	if len(data) < 4 {
		return 0
	}
	return binary.BigEndian.Uint32(data[:4])
}
