// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"testing"
)

// TestParseBinaryListLong 钉住 `BaiduPCS-Go ls -l` 解析（**可选路径**，默认关闭；
// 开关 `AdapterConfig.ListLong`）。`ls -l` 一次调用即返回每条目的 MD5，理论上可替代
// 「ls + 逐条 meta」的 N 次 API 往返。
//
// 覆盖：文件行（带 MD5/大小）、目录行（MD5 列为空）、含空格文件名、表头与汇总行忽略、
// 以及「文件名本身是 32 位 hex」的歧义兜底。
func TestParseBinaryListLong(t *testing.T) {
	t.Parallel()
	out := []byte(`当前目录: /来自：本地电脑/sp/_t
----
  #       FS ID          APP ID    文件大小       创建日期             修改日期                MD5(截图请打码)           文件(目录)
  0  1117741158798010      250528  2.00KB    2026-11-05 00:23:46  2026-11-05 00:23:46  56F2040CA6EE8477CE56266809DAB955  x.bin
  1  956832244945519       250528  -         2026-10-24 11:12:51  2026-10-28 22:27:12                            OypEIkI8i9Y5/
  2  1117741158798011      250528  1.50MB    2026-11-04 10:00:00  2026-11-04 10:05:00  aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  a b c.bin
                       总: 1.50MB                                                      文件总数: 2, 目录总数: 1
`)
	entries, err := parseBinaryListLong(out)
	if err != nil {
		t.Fatalf("parseBinaryListLong: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("应解析 3 条，got %d", len(entries))
	}
	if entries[0].Key != "x.bin" || entries[0].ETag != "56f2040ca6ee8477ce56266809dab955" {
		t.Fatalf("文件行解析错误：key=%q etag=%q", entries[0].Key, entries[0].ETag)
	}
	if entries[0].Size != 2<<10 {
		t.Fatalf("2.00KB 应换算为 2048，got %d", entries[0].Size)
	}
	if entries[1].Key != "OypEIkI8i9Y5/" || !entries[1].IsDir || entries[1].ETag != "" || entries[1].Size != 0 {
		t.Fatalf("目录行解析错误：%+v", entries[1])
	}
	if entries[2].Key != "a b c.bin" || entries[2].ETag != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("含空格文件名解析错误：key=%q", entries[2].Key)
	}
	if entries[2].Size != 3<<19 {
		t.Fatalf("1.50MB 应换算为 1572864，got %d", entries[2].Size)
	}
}

// TestParseBinaryListLong_HexFileName 歧义兜底：文件名本身是 32 位 hex 时，
// 首个正则会把文件名当 MD5（name 为空）⇒ 必须以无 MD5 正则重解，不能丢条目。
func TestParseBinaryListLong_HexFileName(t *testing.T) {
	t.Parallel()
	out := []byte(`当前目录: /x
----
  #       FS ID     APP ID  文件大小  创建日期             修改日期              MD5(截图请打码)  文件(目录)
  0  111  250528  1.00KB  2026-11-05 00:00:00  2026-11-05 00:00:00  0123456789abcdef0123456789abcdef
`)
	entries, err := parseBinaryListLong(out)
	if err != nil {
		t.Fatalf("parseBinaryListLong: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("应解析 1 条，got %d", len(entries))
	}
	if entries[0].Key != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("hex 文件名应作为 name，got %q", entries[0].Key)
	}
}

// TestListLong_DisabledByDefault 锁定**默认关闭**：未开启 `ListLong` 时不得走 `ls -l`
// 路径（保持既有「ls + 逐条 meta」语义），确保生产行为零变化。
func TestListLong_DisabledByDefault(t *testing.T) {
	t.Parallel()
	s := newTestStorage(t, newFakeStorageAdapter())
	if s.listLong {
		t.Fatal("默认必须关闭 listLong（需干净环境 A/B 验证后再决定，见 docs §7.7）")
	}
}
