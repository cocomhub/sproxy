// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestChunkDecryptor_SourceInjectedAndFailClosed 钉住「分块来源可注入」的解密路径与
// `DecryptFile` **逐条等价**（唯一实现）。
//
// 背景（2026-10-18 架构级修复 Step 1）：`DecryptFile` 是「本地目录 → 本地文件」的路径型 API，
// 调用方被迫先把**全部分块密文**落盘（secretdata 读路径 ⇒ 峰值 ≈2× 文件 ✗）。新 API
// `ChunkDecryptor` 把「分块来源」变成回调，调用方可直接从底层 FS/内存/远端取密文，
// **不落密文临时文件**。本用例保证新路径不削弱任何既有保证。
func TestChunkDecryptor_SourceInjectedAndFailClosed(t *testing.T) {
	t.Parallel()
	src, want := writeTestFile(t)
	outDir := t.TempDir()
	secret := []byte("chunk-decryptor-secret")
	res, err := EncryptShards(src, outDir, secret, testPolicy(), 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	if len(res.Meta.Chunks) < 2 {
		t.Fatalf("夹具需要 ≥2 块，got %d", len(res.Meta.Chunks))
	}
	fromDir := func(ci ChunkInfo) ([]byte, error) {
		return os.ReadFile(filepath.Join(outDir, ci.FileName))
	}

	dec, derr := NewChunkDecryptor(secret, res.Meta)
	if derr != nil {
		t.Fatalf("NewChunkDecryptor: %v", derr)
	}
	if dec.Meta() != res.Meta {
		t.Error("Meta() 应返回绑定的同一 meta")
	}

	// ① 全量解密（来源 = 本地目录）必须与原文逐字节一致（等价 DecryptFile）。
	var buf bytes.Buffer
	if err := dec.DecryptAllTo(&buf, fromDir); err != nil {
		t.Fatalf("DecryptAllTo: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("解密内容与原文不一致（%d vs %d 字节）", buf.Len(), len(want))
	}

	// ② 单块解密：明文必须与原文对应区间一致（随机访问路径的基础）。
	for _, ci := range res.Meta.Chunks {
		plain, cerr := dec.DecryptChunk(ci, fromDir)
		if cerr != nil {
			t.Fatalf("DecryptChunk(%s): %v", ci.FileName, cerr)
		}
		if !bytes.Equal(plain, want[ci.Offset:ci.Offset+ci.OrigSize]) {
			t.Fatalf("分块 %s 明文与原文区间不一致", ci.FileName)
		}
	}

	// ③ 来源错误必须透传（不吞错、不返回半份内容当成功）。
	sentinel := errors.New("来源故障")
	srcErr := dec.DecryptAllTo(&bytes.Buffer{}, func(ChunkInfo) ([]byte, error) { return nil, sentinel })
	if !errors.Is(srcErr, sentinel) {
		t.Fatalf("来源错误应透传，got %v", srcErr)
	}

	// ④ 篡改任一密文块 ⇒ GCM 认证失败（fail-closed）。
	badDir := t.TempDir()
	for _, cn := range res.ChunkNames {
		b, rerr := os.ReadFile(filepath.Join(outDir, cn))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if werr := os.WriteFile(filepath.Join(badDir, cn), b, 0o600); werr != nil {
			t.Fatal(werr)
		}
	}
	target := res.Meta.Chunks[1].FileName
	tb, _ := os.ReadFile(filepath.Join(badDir, target))
	tb[len(tb)-1] ^= 0xff
	if werr := os.WriteFile(filepath.Join(badDir, target), tb, 0o600); werr != nil {
		t.Fatal(werr)
	}
	if err := dec.DecryptAllTo(&bytes.Buffer{}, func(ci ChunkInfo) ([]byte, error) {
		return os.ReadFile(filepath.Join(badDir, ci.FileName))
	}); err == nil {
		t.Fatal("篡改密文块后解密不应成功 ✗")
	}

	// ⑤ **等长交换**两块顺序 ⇒ 逐块长度/认证都过，但整文件 SHA-256 必须拦住
	// （这正是「只按块比对不够」的判据）⇒ 必须返回 ErrIntegrityMismatch 哨兵。
	// 找一对**等长**块（定长分块下相邻块通常等长；随机分块则全表查找）——等长是前提，
	// 否则块长校验就会先拦下，无法验证「整文件 SHA-256 才是拦住等长交换的那道闸」。
	idxA, idxB := -1, -1
	for i := range res.Meta.Chunks {
		for j := i + 1; j < len(res.Meta.Chunks); j++ {
			if res.Meta.Chunks[i].OrigSize == res.Meta.Chunks[j].OrigSize {
				idxA, idxB = i, j
				break
			}
		}
		if idxA >= 0 {
			break
		}
	}
	if idxA < 0 {
		t.Fatal("夹具未找到等长块对（等长交换判据无法执行，判据失效）")
	}
	nameA := res.Meta.Chunks[idxA].FileName
	nameB := res.Meta.Chunks[idxB].FileName
	swappedSrc := func(ci ChunkInfo) ([]byte, error) {
		name := ci.FileName
		switch ci.FileName {
		case nameA:
			name = nameB
		case nameB:
			name = nameA
		}
		return os.ReadFile(filepath.Join(outDir, name))
	}
	swapErr := dec.DecryptAllTo(&bytes.Buffer{}, swappedSrc)
	if !errors.Is(swapErr, ErrIntegrityMismatch) {
		t.Fatalf("等长交换分块应被整文件校验拦住（ErrIntegrityMismatch），got %v", swapErr)
	}
	if !bytes.Contains([]byte(swapErr.Error()), []byte("完整性校验失败")) {
		t.Fatalf("错误文案应保持既有口径，got %v", swapErr)
	}

	// ⑥ DecryptFile（委托同一实现）行为不变：整文件校验失败时**删除残件**。
	res.Meta.Original.SHA256 = "ffffffffffffffff" + res.Meta.Original.SHA256[16:]
	dst := filepath.Join(t.TempDir(), "restored.bin")
	ferr := DecryptFile(res.Meta, outDir, dst, secret)
	if ferr == nil {
		t.Fatal("meta 声明 sha256 不符时应失败 ✗")
	}
	if !errors.Is(ferr, ErrIntegrityMismatch) {
		t.Errorf("DecryptFile 应返回 ErrIntegrityMismatch 哨兵，got %v", ferr)
	}
	if _, serr := os.Stat(dst); serr == nil {
		t.Error("完整性校验失败后不应残留还原文件（既有语义）")
	}
}
