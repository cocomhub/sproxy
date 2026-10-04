// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestEncryptShardsBytes_VideoKeyframeFullChain 验证 video-keyframe 模式全链路：
// 关键帧切分加密 → meta 记录 BlockletMode=video-keyframe 与 ChunkInfo.Failures →
// 解密还原 == 原文件；错误段留在 blob 密文内但**不写入** meta.Blocklets
// （validateChunkBlocklets 连续覆盖不变量保持）。
func TestEncryptShardsBytes_VideoKeyframeFullChain(t *testing.T) {
	t.Parallel()
	secret := []byte("video-keyframe-integration-secret")
	// 构造确定性关键帧：{0, 500}，文件 1000B。
	idx := &fixedIndexer{frames: []int64{0, 500}}
	policy := DefaultBlockPolicy()
	policy.BlockletMode = "video-keyframe"
	policy.BlockletMin = 64
	policy.BlockletMax = 256
	policy.Indexer = idx

	data := make([]byte, 1000)
	for i := range data {
		data[i] = byte(i % 251)
	}
	outDir := t.TempDir()
	res, err := EncryptShardsBytes(data, "clip.mp4", outDir, secret, policy, 0, AlgoV1GCMLow)
	if err != nil {
		t.Fatalf("EncryptShardsBytes: %v", err)
	}
	// meta.BlockletMode 记录 video-keyframe。
	if res.Meta.Block.BlockletMode != "video-keyframe" {
		t.Errorf("meta.BlockletMode=%q，应为 video-keyframe", res.Meta.Block.BlockletMode)
	}
	// 无失败场景：所有块 Failures 为空。
	for _, c := range res.Meta.Chunks {
		if len(c.Failures) != 0 {
			t.Errorf("无失败场景 ChunkInfo.Failures=%+v，应为空", c.Failures)
		}
	}
	// 解密还原 == 原文件。
	dst := filepath.Join(t.TempDir(), "restored.mp4")
	if derr := DecryptFile(res.Meta, outDir, dst, secret); derr != nil {
		t.Fatalf("DecryptFile: %v", derr)
	}
	got, rerr := os.ReadFile(dst)
	if rerr != nil {
		t.Fatalf("读还原文件: %v", rerr)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("还原内容 != 原文件：len(got)=%d len(want)=%d", len(got), len(data))
	}
}

// TestEncryptShardsBytes_VideoKeyframeFailureRecorded 验证解析失败时：
// 错误段入 blob 密文（不写 meta.Blocklets）、ChunkInfo.Failures 记录失败、解密还原仍成功
// （降级不中断写）。
func TestEncryptShardsBytes_VideoKeyframeFailureRecorded(t *testing.T) {
	t.Parallel()
	secret := []byte("video-keyframe-failure-secret")
	idx := &errIndexer{}
	policy := DefaultBlockPolicy()
	policy.BlockletMode = "video-keyframe"
	policy.BlockletMin = 64
	policy.BlockletMax = 256
	policy.Indexer = idx

	data := bytes.Repeat([]byte{0xAB}, 800)
	outDir := t.TempDir()
	res, err := EncryptShardsBytes(data, "broken.mp4", outDir, secret, policy, 0, AlgoV1GCMLow)
	if err != nil {
		t.Fatalf("EncryptShardsBytes: %v", err)
	}
	// ChunkInfo.Failures 记录失败（全密文 meta 内）。
	hasFail := false
	for _, c := range res.Meta.Chunks {
		if len(c.Failures) > 0 {
			hasFail = true
		}
	}
	if !hasFail {
		t.Fatal("解析失败场景应有 ChunkInfo.Failures 记录")
	}
	// 错误段不写入 meta.Blocklets（连续覆盖不变量）。
	for _, c := range res.Meta.Chunks {
		for _, bl := range c.Blocklets {
			if bl.Type == byte(BlockletTypeError) {
				t.Errorf("meta.Blocklets 不应含错误段（连续覆盖不变量），got type=0x%02x", bl.Type)
			}
		}
	}
	// 解密还原仍成功（降级不中断）。
	dst := filepath.Join(t.TempDir(), "restored.mp4")
	if derr := DecryptFile(res.Meta, outDir, dst, secret); derr != nil {
		t.Fatalf("DecryptFile: %v", derr)
	}
	got, rerr := os.ReadFile(dst)
	if rerr != nil {
		t.Fatalf("读还原文件: %v", rerr)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("还原内容 != 原文件：len(got)=%d len(want)=%d", len(got), len(data))
	}
}

// TestEncryptShardsBytes_VideoKeyframeBlobErrorSegment 验证错误段确实入 blob 密文：
// 从块 blob 索引中能定位到 Error 类型段且其明文 == Failures JSON（全密文往返）。
func TestEncryptShardsBytes_VideoKeyframeBlobErrorSegment(t *testing.T) {
	t.Parallel()
	secret := []byte("video-keyframe-blob-secret")
	idx := &errIndexer{}
	policy := DefaultBlockPolicy()
	policy.BlockletMode = "video-keyframe"
	policy.BlockletMin = 64
	policy.BlockletMax = 512
	policy.Indexer = idx

	data := bytes.Repeat([]byte{0xCD}, 1200)
	outDir := t.TempDir()
	res, err := EncryptShardsBytes(data, "blob.mp4", outDir, secret, policy, 0, AlgoV1GCMLow)
	if err != nil {
		t.Fatalf("EncryptShardsBytes: %v", err)
	}
	salt, err := decodeSalt(res.Meta)
	if err != nil {
		t.Fatalf("decodeSalt: %v", err)
	}
	key, err := deriveKey(secret, salt, res.Meta.AlgoVersion)
	if err != nil {
		t.Fatalf("deriveKey: %v", err)
	}
	// 逐块读取 blob，索引中应含 Error 段且明文可解开。
	found := 0
	for _, c := range res.Meta.Chunks {
		blob, rerr := os.ReadFile(filepath.Join(outDir, c.FileName))
		if rerr != nil {
			t.Fatalf("读分块 %s: %v", c.FileName, rerr)
		}
		entries, _, ierr := decryptBlobIndex(key, salt, blob)
		if ierr != nil {
			t.Fatalf("decryptBlobIndex: %v", ierr)
		}
		for _, e := range entries {
			if e.Type == BlockletTypeError {
				plain, derr := decryptBlockletSegment(key, blob, e)
				if derr != nil {
					t.Fatalf("解密错误段: %v", derr)
				}
				var msgs []BlockErrorMsg
				if uerr := json.Unmarshal(plain, &msgs); uerr != nil {
					t.Fatalf("错误段 JSON 解析失败: %v (%q)", uerr, plain)
				}
				if len(msgs) == 0 {
					t.Error("错误段应含失败记录")
				}
				found++
			}
		}
	}
	if found == 0 {
		t.Fatal("块 blob 内未找到 Error 段")
	}
}

// TestMediaKindOf：文件名扩展名 → 媒体容器族判据（自动选型用；I2+库选型评估定稿口径：
// Kind 按容器族拆分，未注册的容器族由注册表回落 fixed）。
func TestMediaKindOf(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"a.mp4":  "video/mp4", // ISO-BMFF：go-mp4 可解析（当前唯一注册）
		"d.mov":  "video/mp4",
		"b.mkv":  "video/mkv", // EBML：返回 Kind，注册表未命中 → 回落 fixed
		"c.webm": "video/mkv",
		"e.avi":  "video/avi", // RIFF：同上
		"f.ts":   "video/ts",  // MPEG-TS：同上
		"g.txt":  "",
		"h":      "",
		"i.png":  "",
	}
	for name, want := range cases {
		if got := MediaKindOf(name); got != want {
			t.Errorf("MediaKindOf(%q)=%q，应为 %q", name, got, want)
		}
	}
}

// TestResolveBlockletMode_UnregisteredKindFallsBack（库选型评估补）：返回了具体容器族
// Kind（video/mkv/video/ts/video/avi）但注册表无对应解析器 → 回落默认 fixed（不报错，
// 不「宣称支持实为必败降级」）。
func TestResolveBlockletMode_UnregisteredKindFallsBack(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"video/mkv", "video/ts", "video/avi"} {
		p, err := ResolveBlockletMode(kind)
		if err != nil {
			t.Fatalf("ResolveBlockletMode(%s) 未注册应回落 fixed 而非报错: %v", kind, err)
		}
		if p.Mode != "fixed" {
			t.Errorf("ResolveBlockletMode(%s) 模式=%q，应为 fixed（未注册回落）", kind, p.Mode)
		}
	}
}
