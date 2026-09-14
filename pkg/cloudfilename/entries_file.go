// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudfilename

import (
	"os"
	"strings"
)

// ReadEntriesFromFile 从文件中读取云端下载条目（每行一个）。
//
// 自 cmd/sclient 原样下沉（原 readEntriesFromFile）：**行格式是域契约**（客户端与服务端共享
// 同一套 Entry 语义），不属于「参数解析」——故与 Entry 同包，供 CLI 与将来其它入口复用。
//
// 格式契约：
//   - 每行 `URL` 或 `URL<TAB>FILENAME`：URL 本身可能含空格，故文件名与 URL 之间**必须**用
//     Tab 分隔（不用空格）；
//   - 一行含多个 Tab 时只取前两列——FILENAME 本身允许含 Tab，不因多余 Tab 拒绝整份文件；
//   - 先 trim 行首尾空白，再忽略空行与 `#` 开头的注释行；
//   - 兼容 CRLF（Windows 导出）；
//   - 文件不存在 / 不可读 → 返回底层错误（CLI 文案由调用方补）。
func ReadEntriesFromFile(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseEntries(string(data)), nil
}

// ParseEntries 按 ReadEntriesFromFile 的格式契约解析条目文本。
// 单独导出以便调用方（含测试）直接覆盖格式边界而无需落盘。
func ParseEntries(content string) []Entry {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	var entries []Entry
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		e := Entry{URL: strings.TrimSpace(parts[0])}
		if len(parts) > 1 {
			e.Filename = strings.TrimSpace(parts[1])
		}
		entries = append(entries, e)
	}
	return entries
}
