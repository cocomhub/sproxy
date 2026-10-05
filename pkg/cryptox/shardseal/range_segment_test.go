// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"bytes"
	"testing"
)

// TestDecryptBlockletSegmentStandalone：加密块 → 用 meta 段描述「只拉目标段的密文」独立
// 解密 == 原段明文（Range 读取：不下载整块）。salt 一致性经 VerifyBlockSalt 单独校验。
func TestDecryptBlockletSegmentStandalone(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x11}, KeyLen)
	salt := bytes.Repeat([]byte{0x22}, SaltLen)
	blocklets := []Blocklet{
		{Offset: 0, Size: 64}, {Offset: 64, Size: 64}, {Offset: 128, Size: 64}, {Offset: 192, Size: 64},
	}
	data := make([]byte, 256)
	for i := range data {
		data[i] = byte(i)
	}
	blob, entries, err := encryptBlocklets(key, salt, blocklets, data, AlgoV1GCM)
	if err != nil {
		t.Fatalf("encryptBlocklets: %v", err)
	}
	// 构造 meta 段描述（与 encryptWriteChunks 产出同构）。
	infos := make([]BlockletInfo, len(blocklets))
	for i, bl := range blocklets {
		e := entries[i]
		seg := data[bl.Offset : bl.Offset+bl.Size]
		segHex, _ := hash16(seg)
		infos[i] = BlockletInfo{
			Offset: bl.Offset, Size: bl.Size,
			EncOffset: int64(e.EncOffset), EncSize: e.EncSize,
			OrigSHA256: segHex, Used: true, Type: byte(BlockletTypeData),
		}
	}
	// 对每个段：只切该段密文 [EncOffset, EncOffset+EncSize) → 独立解 == 原明文。
	for i, bl := range infos {
		seg := blob[bl.EncOffset : bl.EncOffset+bl.EncSize]
		// salt 一致性单独校验（Range 读取先拉块头部 salt 段）。
		headSalt := blob[blSaltOff:blListOff]
		if verr := VerifyBlockSalt(headSalt, salt); verr != nil {
			t.Fatalf("VerifyBlockSalt: %v", verr)
		}
		plain, derr := DecryptBlockletSegmentStandalone(key, seg, bl, 0)
		if derr != nil {
			t.Fatalf("段 %d 解密: %v", i, derr)
		}
		want := data[bl.Offset : bl.Offset+bl.Size]
		if !bytes.Equal(plain, want) {
			t.Errorf("段 %d 明文 != 原段", i)
		}
	}
}

// TestDecryptBlockletSegmentStandalone_Tamper：篡改段密文 fail-closed（GCM 认证）。
func TestDecryptBlockletSegmentStandalone_Tamper(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x33}, KeyLen)
	salt := bytes.Repeat([]byte{0x44}, SaltLen)
	blocklets := []Blocklet{{Offset: 0, Size: 100}}
	blob, entries, err := encryptBlocklets(key, salt, blocklets, bytes.Repeat([]byte{7}, 100), AlgoV1GCM)
	if err != nil {
		t.Fatalf("encryptBlocklets: %v", err)
	}
	e := entries[0]
	bl := BlockletInfo{Offset: 0, Size: 100, EncOffset: int64(e.EncOffset), EncSize: e.EncSize, Used: true, Type: byte(BlockletTypeData)}
	seg := blob[bl.EncOffset : bl.EncOffset+bl.EncSize]
	bad := append([]byte(nil), seg...)
	bad[len(bad)-1] ^= 0xFF
	if _, derr := DecryptBlockletSegmentStandalone(key, bad, bl, 0); derr == nil {
		t.Error("篡改段密文应 fail-closed")
	}
	// 段长与 meta EncSize 不符 fail-closed。
	if _, derr := DecryptBlockletSegmentStandalone(key, seg[:len(seg)-1], bl, 0); derr == nil {
		t.Error("段长不符应 fail-closed")
	}
	// salt 不一致 fail-closed（VerifyBlockSalt 独立校验）。
	if VerifyBlockSalt(bytes.Repeat([]byte{0x99}, SaltLen), salt) == nil {
		t.Error("salt 不一致应 fail-closed")
	}
}
