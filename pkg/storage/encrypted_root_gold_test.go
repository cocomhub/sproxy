// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

// encrypted_root_gold_test.go 用**冻结的密文字节夹具**（testdata/encroot_gold_v1.bin）锁定
// at-rest 加密卷的**落盘格式**：该夹具由收敛前的旧实现（encRootWriter）真实产出，
// 保存其 sha256 与关键尺寸；此后任何「帧布局/魔数/块大小/每块开销」的改动都必须让本用例红。
//
// 为什么需要它：`TestEncryptedRoot_RoundTrip` 一类往返用例**两侧同时改**也照样绿（自洽但已漂移）；
// 只有外部冻结的字节产物能证明「线上既有密文仍可解」。
//
// 全部断言走**公开 API**（OpenRoot / SetEncryption / OpenDecrypted / EncryptedPhysicalSize），
// 以便在内部实现收敛（迁移到 pkg/cipherstream）前后保持同一份 oracle 不变。

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// goldKey/goldPlainLen 与夹具生成时一致（生成脚本已删除，值在此固化）。
var goldKey = bytes.Repeat([]byte{0x11}, 32)

const goldPlainLen = 3*(64<<10) + 7

// goldPlain 重现夹具的明文（byte(i % 251)），字节级可比。
func goldPlain(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func TestEncRootGoldFixture_FormatFrozen(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "encroot_gold_v1.bin"))
	if err != nil {
		t.Fatalf("读金值夹具: %v", err)
	}
	// 夹具尺寸必须与物理尺寸公式一致（公式是「写路径落盘字节数」的账本口径）。
	if got := EncryptedPhysicalSize(goldPlainLen); got != int64(len(raw)) {
		t.Fatalf("EncryptedPhysicalSize(%d)=%d, 夹具实测 %d（公式与格式已漂移）", goldPlainLen, got, len(raw))
	}
	// 魔数是**跨域契约**（与 pkg/sync.EncryptedFS 同值 ⇒ 同一密钥域，密文互通）。
	if string(encRootMagic) != "SPROXY-FS-ENC-v1" {
		t.Fatalf("魔数变更 %q —— 会让既有密文不可解，需迁移方案而非直接改", encRootMagic)
	}

	dir := t.TempDir()
	if werr := os.WriteFile(filepath.Join(dir, "gold.bin"), raw, 0o644); werr != nil {
		t.Fatalf("落地夹具: %v", werr)
	}
	rt, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer rt.Close()
	if serr := rt.SetEncryption(goldKey); serr != nil {
		t.Fatalf("SetEncryption: %v", serr)
	}

	want := goldPlain(goldPlainLen)
	// 顺序读全文 + 明文长度视图。
	rc, err := rt.OpenDecrypted("gold.bin")
	if err != nil {
		t.Fatalf("OpenDecrypted: %v", err)
	}
	// 明文长度视图（OpenDecrypted 结果额外提供 Stat，Size = 明文长）。
	st, ok := rc.(interface {
		Stat() (os.FileInfo, error)
	})
	if !ok {
		t.Fatalf("OpenDecrypted 结果应提供 Stat（明文长度视图）")
	}
	fi, err := st.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Size() != goldPlainLen {
		t.Fatalf("明文长度视图 = %d, 期望 %d", fi.Size(), goldPlainLen)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("读夹具明文: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("夹具解密结果不符（长度 %d vs %d）—— 帧格式已漂移", len(got), len(want))
	}
	_ = rc.Close()

	// 定位读（含块内、跨块边界、末尾、EOF）必须与顺序读切片逐字节一致。
	for _, off := range []int64{0, 1, (64 << 10) - 1, 64 << 10, 2*(64<<10) + 3, goldPlainLen - 1, goldPlainLen} {
		rc, err := rt.OpenDecrypted("gold.bin")
		if err != nil {
			t.Fatalf("OpenDecrypted(off=%d): %v", off, err)
		}
		sk, ok := rc.(io.Seeker)
		if !ok {
			t.Fatalf("OpenDecrypted 结果应支持 Seek（Range/断点续传依赖）")
		}
		if _, err := sk.Seek(off, io.SeekStart); err != nil {
			t.Fatalf("Seek(%d, Start): %v", off, err)
		}
		end := min(off+64, goldPlainLen)
		buf := make([]byte, end-off)
		if _, err := io.ReadFull(rc, buf); err != nil {
			t.Fatalf("Seek(%d) 后读取: %v", off, err)
		}
		if !bytes.Equal(buf, want[off:end]) {
			t.Fatalf("Seek(%d) 内容不符", off)
		}
		_ = rc.Close()
	}
}
