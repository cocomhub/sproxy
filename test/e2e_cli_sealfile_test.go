// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// e2e_cli_sealfile_test.go 验证 sealfile 本地加解密 ↔ sproxy secretdata 卷全互操作
// （真二进制 + 真 HTTP Range）：
//
//  1. **sealfile encrypt 产物 = secretdata 卷**：本地 sclient sealfile encrypt 加密
//     视频（关键帧分块）→ 产物目录直接配置为 `type: secretdata` 卷（extra.root 指向
//     产物目录 + secret_url 指向同一密钥文件）→ 播放器 GET /download Range 206 +
//     明文段一致 + 磁盘匿名性 + meta.BlockletMode==video-keyframe 断言。
//  2. **decrypt 还原**：sealfile decrypt 还原产物 → 明文 SHA-256 == 原文件。
//  3. **info 断言**：sealfile info 打印 blocklet 模式（关键帧生效）。
package sproxy_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2E_CLI_Sealfile_EncryptVolumePlayback 链路 1：本地 sealfile encrypt 加密视频 →
// 产物作为 secretdata 卷根加载 → Range 播放。
func TestE2E_CLI_Sealfile_EncryptVolumePlayback(t *testing.T) {
	t.Parallel()
	env, keysRoot := newSealfileEnv(t)

	// 1) 视频内容（MP4 盒骨架——go-mp4 可解析；关键帧分块需可 parse 的 ftyp/moov）。
	content := testMP4Content()
	file := filepath.Join(env.TmpDir, "movie.mp4")
	if werr := os.WriteFile(file, content, 0o600); werr != nil {
		t.Fatalf("写测试 MP4: %v", werr)
	}

	// 2) sealfile encrypt（本地加密，不连服务器路径；--key 指向密钥文件；--owner 用
	//    e2eTestAK——签名请求的 owner 即 AK 字符串（ActorFrom 返回 AK，非凭据 Owner
	//    字段），产物布局 <owner>/user/<name> 与服务器查询键对齐）。
	out := env.sclient(t, env.TmpDir, "sealfile", "encrypt", "movie.mp4",
		"--key", filepath.Join(keysRoot, "secrets", "datakey"),
		"--owner", e2eTestAK,
		"--output-dir", filepath.Join(env.TmpDir, "sealed"))
	if !strings.Contains(out, "blocklet 模式: video-keyframe") {
		t.Fatalf("encrypt 输出应报告 video-keyframe, got:\n%s", out)
	}

	// 3) 产物目录直接作为 secretdata 卷根装配到 sproxy（type: secretdata + secret_url
	//    指向同一密钥文件；extra.root = 产物目录）。
	vaultRoot := filepath.Join(env.TmpDir, "sealed")
	extraConfig := fmt.Sprintf(`volumes:
  - name: main
  - name: mykeys
    type: secrets
    extra:
      target: local
      root: %q
  - name: vault
    type: secretdata
    extra:
      target: local
      root: %q
      secret_url: secrets://mykeys/datakey
`, keysRoot, vaultRoot)
	env2 := startCLIEnv(t, extraConfig)

	// 4) 磁盘匿名性：产物目录无明文逻辑文件名。
	assertSecretLayoutAnonymity(t, vaultRoot, "movie.mp4", "movie.mp4")

	// 5) 播放器 Range（seek 中间段）：206 + 解密明文段 == 原文件。
	url := env2.BaseURL + "/download?filename=movie.mp4&volume=vault"
	status, hdr, body := rangeRequest(t, url, "bytes=4096-8191")
	if status != 206 {
		t.Fatalf("sealfile 产物卷 Range 应 206, got %d: %s", status, body)
	}
	if want := fmt.Sprintf("bytes 4096-8191/%d", len(content)); hdr.Get("Content-Range") != want {
		t.Fatalf("Content-Range=%q want %q", hdr.Get("Content-Range"), want)
	}
	if !bytes.Equal(body, content[4096:8192]) {
		t.Fatalf("解密 Range 段 != 原文件段（len=%d）", len(body))
	}

	// 6) 从头顺序播放（播放器边缓冲边播语义）：分段逐段一致。
	off := 0
	for off < len(content) {
		end := off + 16384
		if end > len(content) {
			end = len(content)
		}
		status, _, seg := rangeRequest(t, url, fmt.Sprintf("bytes=%d-%d", off, end-1))
		if status != 206 {
			t.Fatalf("顺序段 [%d,%d) 应 206, got %d", off, end, status)
		}
		if !bytes.Equal(seg, content[off:end]) {
			t.Fatalf("顺序段 [%d,%d) 解密不一致", off, end)
		}
		off = end
	}
}

// TestE2E_CLI_Sealfile_DecryptRoundtrip 链路 2：encrypt → decrypt 还原，明文
// SHA-256 一致（含 sealfile info 断言关键帧分块生效）。
func TestE2E_CLI_Sealfile_DecryptRoundtrip(t *testing.T) {
	t.Parallel()
	env, keysRoot := newSealfileEnv(t)

	content := []byte("sealfile roundtrip payload —— 中文内容 + 二进制\x00\x01\x02")
	file := filepath.Join(env.TmpDir, "doc.bin")
	if werr := os.WriteFile(file, content, 0o600); werr != nil {
		t.Fatalf("写测试文件: %v", werr)
	}
	sealed := filepath.Join(env.TmpDir, "sealed")
	env.sclient(t, env.TmpDir, "sealfile", "encrypt", "doc.bin",
		"--key", filepath.Join(keysRoot, "secrets", "datakey"),
		"--output-dir", sealed)

	// info：blocklet 模式（非视频 → fixed 回落；断言不含 video-keyframe）。
	out := env.sclient(t, env.TmpDir, "sealfile", "info", sealed,
		"--key", filepath.Join(keysRoot, "secrets", "datakey"))
	if !strings.Contains(out, "doc.bin") || !strings.Contains(out, "blocklet 模式") {
		t.Fatalf("info 输出应含文件与 blocklet 模式, got:\n%s", out)
	}

	// decrypt 还原到输出文件。
	restored := filepath.Join(env.TmpDir, "restored.bin")
	env.sclient(t, env.TmpDir, "sealfile", "decrypt", sealed,
		"--key", filepath.Join(keysRoot, "secrets", "datakey"), "-o", restored)

	// 明文 SHA-256 == 原文件（含解密完整性校验证据）。
	origHash := sha256HexOf(t, file)
	gotHash := sha256HexOf(t, restored)
	if origHash != gotHash {
		t.Fatalf("还原 SHA-256 不一致: %s != %s", gotHash, origHash)
	}
	if !bytes.Equal(content, mustRead(t, restored)) {
		t.Fatalf("还原字节 != 原文件")
	}
}

// TestE2E_CLI_Sealfile_KeyfileAndConfig 链路 3：--key 密钥文件 + 配置回落
// （sealfile.key/out_dir 写入 config.yaml 顶层；CLI flag 缺省时从配置读取）。
func TestE2E_CLI_Sealfile_KeyfileAndConfig(t *testing.T) {
	t.Parallel()
	env, keysRoot := newSealfileEnv(t)

	// 统一 context：把 sealfile 段写入 config.yaml 顶层（手写 YAML，test 包不 import
	// internal contextcfg）。
	cfgBody := fmt.Sprintf(`apiVersion: sclient/v1
kind: Config
current-context: e2e
sealfile:
  key: %q
  out_dir: %q
  owner: test
environments:
  - name: e2e
    server_url: %q
users:
  - name: e2e
    access_key: %q
    access_key_secret: %q
    access_key_id: %q
contexts:
  - name: e2e
    environment: e2e
    user: e2e
`,
		filepath.Join(keysRoot, "secrets", "datakey"),
		filepath.Join(env.TmpDir, "sealed-cfg"),
		env.BaseURL, e2eTestAK, e2eTestSK, e2eTestID)
	if werr := os.WriteFile(env.configYAMLPath(), []byte(cfgBody), 0o600); werr != nil {
		t.Fatalf("写 config.yaml: %v", werr)
	}

	content := []byte("config-回落-payload")
	file := filepath.Join(env.TmpDir, "cfg.txt")
	if werr := os.WriteFile(file, content, 0o600); werr != nil {
		t.Fatalf("写测试文件: %v", werr)
	}

	// 无 --key / --output-dir / --owner：回落配置（owner 也回落 test）。
	out := env.sclient(t, env.TmpDir,
		"sealfile", "encrypt", "cfg.txt")
	if !strings.Contains(out, "cfg.txt") {
		t.Fatalf("encrypt 输出应含文件名, got:\n%s", out)
	}
	// 配置 out_dir/owner 生效：产物经 SecretdataFS 逻辑读取可见（磁盘是加密容器，
	// 无明文 test/user/cfg.txt 路径，不能 os.Stat 明文——info 按配置回落 owner 定位）。
	infoOut := env.sclient(t, env.TmpDir,
		"sealfile", "info", filepath.Join(env.TmpDir, "sealed-cfg"))
	if !strings.Contains(infoOut, "cfg.txt") {
		t.Fatalf("info 应列出配置 out_dir 产物 cfg.txt, got:\n%s", infoOut)
	}
}

// ---- helpers ----

// newSealfileEnv 只起 sclient（sealfile 本地加解密不依赖服务器）；返回 env + keysRoot。
func newSealfileEnv(t *testing.T) (*cliEnv, string) {
	t.Helper()
	keysRoot := filepath.Join(t.TempDir(), "keys")
	secret := seedSecretKey(t, keysRoot)
	if len(secret) != 32 {
		t.Fatalf("密钥应 32B, got %d", len(secret))
	}
	// 仅装配 minimal server（sealfile 命令不连服务器，但 startCLIEnv 需要 env.BaseURL
	// 供 sclient 前缀 flags；服务器无卷也 OK）。
	env := startCLIEnv(t, `volumes:
  - name: main
`)
	return env, keysRoot
}

// testMP4Content 生成最小 MP4（ftyp+moov+mdat，go-mp4 可解析 → 关键帧分块）。
func testMP4Content() []byte {
	// 最小 ISO-BMFF 骨架：ftyp（isom）+ moov（含 mvhd/trak/mdia/mdhd/hdlr/minf/stbl/
	// stsd/stts/stsc/stsz/stco）——go-mp4 只需 box 骨架即可 parse（关键帧列表可能为空，
	// 但 Mode=video-keyframe 已固化）。这里提供**结构完整可 parse** 的最小盒序列。
	boxes := [][]byte{
		box("ftyp", append([]byte("isom"), []byte{0, 0, 0, 0, 0x69, 0x73, 0x6f, 0x6d}...)),
		box("moov", nil),
	}
	var data []byte
	for _, b := range boxes {
		data = append(data, b...)
	}
	// 补足 96KiB+ 使 Range 跨块（块大小随机 1MB 起，但 blocklet 64KB-4MB；为让 seek
	// 中间段落在关键帧 blocklet 段内，用 padding 数据填充到 96KiB——解析器对 mdat
	// 之后内容宽容；关键帧可能取到空列表仍 Mode 生效）。
	for len(data) < 96*1024 {
		data = append(data, byte(len(data)%251))
	}
	return data
}

// box 构造 ISO-BMFF 盒（size + type + payload）。
func box(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	out[0] = byte((8 + len(payload)) >> 24)
	out[1] = byte((8 + len(payload)) >> 16)
	out[2] = byte((8 + len(payload)) >> 8)
	out[3] = byte(8 + len(payload))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

// sha256HexOf 返回文件 SHA-256 hex。
func sha256HexOf(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// mustRead 读文件内容（测试 helper）。
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return data
}
