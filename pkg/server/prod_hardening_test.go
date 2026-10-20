// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// prod_hardening_test.go 钉住 2026-10-21 对抗审查 P1：云端归档（落桶路径）返回的 checksum
// 必须是**落盘产物**的 SHA-256，且 tar/gzip 收尾必须真正完成（可解压）。
//
// 此前 `createTarGz`/`createMultiFileTarGz` 用 `defer gw.Close()/tw.Close()`，而 checksum 在
// defer 之前计算 ⇒ 哈希漏掉 tar 的 EOF 两块 + gzip 尾（CRC32/ISIZE）⇒ 响应里的 checksum
// 100% 与文件不符（客户端按它校验必然判「归档损坏」）；且 Close 的错误被丢弃、succeeded 已置
// true ⇒ 收尾失败的截断归档被当成功返回且不清理。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// TestCreateTarGz_ChecksumMatchesArtifact 归档 checksum 必须等于落盘文件的 SHA-256（且可解压）。
// 直接打 `createTarGz`（落桶路径的唯一实现），避免依赖下载端点/桶布局。
func TestCreateTarGz_ChecksumMatchesArtifact(t *testing.T) {
	srcDir := t.TempDir()
	srcRoot, err := storage.OpenRoot(srcDir)
	if err != nil {
		t.Fatalf("open src root: %v", err)
	}
	t.Cleanup(func() { _ = srcRoot.Close() })
	payload := []byte(strings.Repeat("payload-", 500))
	sf, serr2 := srcRoot.OpenFile("a.bin", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if serr2 != nil {
		t.Fatalf("open src: %v", serr2)
	}
	if _, werr := sf.Write(payload); werr != nil {
		t.Fatalf("write src: %v", werr)
	}
	if cerr := sf.Close(); cerr != nil {
		t.Fatalf("close src: %v", cerr)
	}

	outDir := t.TempDir()
	outRoot, err := storage.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("open out root: %v", err)
	}
	t.Cleanup(func() { _ = outRoot.Close() })

	sum, aerr := createTarGz(srcRoot, "a.bin", "a.bin", outRoot, "check.tar.gz", testLogger())
	if aerr != nil {
		t.Fatalf("createTarGz: %v", aerr)
	}
	raw, rerr := os.ReadFile(filepath.Join(outDir, "check.tar.gz"))
	if rerr != nil {
		t.Fatalf("读归档: %v", rerr)
	}

	// ① checksum 必须等于落盘产物的 SHA-256（此前漏算 tar EOF + gzip 尾 ⇒ 恒不等）。
	want := sha256.Sum256(raw)
	if sum != hex.EncodeToString(want[:]) {
		t.Fatalf("checksum 与产物不符: 返回 %s, 实际 %s（收尾顺序修复前必然不等）", sum, hex.EncodeToString(want[:]))
	}

	// ② 归档必须完整可解压（gzip 尾/flush 缺失会在此报错）。
	zr, zerr := gzip.NewReader(bytes.NewReader(raw))
	if zerr != nil {
		t.Fatalf("gzip 头解析失败（归档不完整）: %v", zerr)
	}
	tr := tar.NewReader(zr)
	entries := 0
	for {
		_, terr := tr.Next()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			t.Fatalf("tar 解析失败（归档截断）: %v", terr)
		}
		entries++
	}
	if entries == 0 {
		t.Fatal("归档内无任何条目")
	}
	if cerr := zr.Close(); cerr != nil {
		t.Fatalf("gzip 尾校验失败（归档截断）: %v", cerr)
	}
}
