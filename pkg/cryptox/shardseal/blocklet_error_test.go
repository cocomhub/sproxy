// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"bytes"
	"testing"
)

// TestEncryptBlocklets_ErrorSegmentCarriesData 验证错误信息块（BlockletTypeError）：
// 放在块末尾（Offset 越过块尾）、Data 明文入密文并 GCM 认证、索引记录该段、全量解密
// 跳过错误段（不属于文件逻辑内容）。
func TestEncryptBlocklets_ErrorSegmentCarriesData(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x11}, KeyLen)
	salt := bytes.Repeat([]byte{0x22}, SaltLen)
	// 两个数据段 [0,16) + 末尾错误段（Offset=16=块尾，Size=Data 明文长）。
	errData := []byte(`["boom"]`)
	blocklets := []Blocklet{
		{Offset: 0, Size: 8},
		{Offset: 8, Size: 8},
		{Offset: 16, Size: int64(len(errData)), Type: BlockletTypeError, Data: errData},
	}
	data := bytes.Repeat([]byte{0xA5}, 16)
	blob, entries, err := encryptBlocklets(key, salt, blocklets, data, AlgoV1GCM)
	if err != nil {
		t.Fatalf("encryptBlocklets: %v", err)
	}
	// 索引含 3 个条目，末条目为 Error 段（type=0x15）。
	if len(entries) != 3 {
		t.Fatalf("索引条目数=%d，应为 3", len(entries))
	}
	last := entries[2]
	if last.Type != BlockletTypeError {
		t.Errorf("末条目 type=0x%02x，应为 Error(0x15)", byte(last.Type))
	}
	if last.Off != 16 || last.Len != int64(len(errData)) {
		t.Errorf("错误段描述=%+v，应为 {type=Error off=16 len=%d}", last, len(errData))
	}
	// 错误段密文可独立解开（明文 == Data，GCM 认证）。
	plain, err := decryptBlockletSegment(key, blob, last)
	if err != nil {
		t.Fatalf("解密错误段: %v", err)
	}
	if !bytes.Equal(plain, []byte(`["boom"]`)) {
		t.Errorf("错误段明文=%q，应为 %q", plain, `["boom"]`)
	}
	// 全量解密 == 数据段明文（跳过错误段，不进入文件内容）。
	full, err := decryptBlock(key, salt, blob)
	if err != nil {
		t.Fatalf("decryptBlock: %v", err)
	}
	if !bytes.Equal(full, data) {
		t.Errorf("全量解密=%q，应为数据段 %q（错误段应被跳过）", full, data)
	}
}

// TestEncryptBlocklets_ErrorSegmentTamperFailClosed 篡改错误段密文必须 fail-closed
// （错误段同样 GCM 认证）。
func TestEncryptBlocklets_ErrorSegmentTamperFailClosed(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x33}, KeyLen)
	salt := bytes.Repeat([]byte{0x44}, SaltLen)
	errData := []byte(`{"f":1}`)
	blocklets := []Blocklet{
		{Offset: 0, Size: 8},
		{Offset: 8, Size: int64(len(errData)), Type: BlockletTypeError, Data: errData},
	}
	blob, entries, err := encryptBlocklets(key, salt, blocklets, bytes.Repeat([]byte{0x5A}, 8), AlgoV1GCM)
	if err != nil {
		t.Fatalf("encryptBlocklets: %v", err)
	}
	bad := append([]byte(nil), blob...)
	e := entries[1]
	// 篡改错误段密文内一个字节（数据区中部）。
	bad[e.EncOffset+int(e.EncSize)/2] ^= 0xFF
	if _, err := decryptBlockletSegment(key, bad, e); err == nil {
		t.Error("篡改错误段密文应 fail-closed（GCM 认证）")
	}
}
