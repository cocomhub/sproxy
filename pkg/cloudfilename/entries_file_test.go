// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudfilename

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadEntriesFromFile 覆盖文件级行为（自 cmd/sclient 原 TestCloudDownloadCmd_ReadEntriesFromFile 迁入）。
func TestReadEntriesFromFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("写 %s: %v", name, err)
		}
		return p
	}

	t.Run("普通两行", func(t *testing.T) {
		entries, err := ReadEntriesFromFile(write("urls.txt", "https://example.com/a.zip\nhttps://example.com/b.zip\n"))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 {
			t.Fatalf("want 2 entries, got %d", len(entries))
		}
		if entries[0].URL != "https://example.com/a.zip" {
			t.Fatalf("entry[0].URL = %q", entries[0].URL)
		}
	})

	t.Run("注释与空行被忽略", func(t *testing.T) {
		entries, err := ReadEntriesFromFile(write("with-comments.txt", "# comment\n\nhttps://example.com/valid.zip\n  # another comment\n"))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].URL != "https://example.com/valid.zip" {
			t.Fatalf("want 1 有效条目, got %+v", entries)
		}
	})

	t.Run("空文件零条目", func(t *testing.T) {
		entries, err := ReadEntriesFromFile(write("empty.txt", ""))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("want 0 entries, got %d", len(entries))
		}
	})

	t.Run("文件不存在报错", func(t *testing.T) {
		if _, err := ReadEntriesFromFile(filepath.Join(dir, "nonexistent.txt")); err == nil {
			t.Fatal("缺文件应报错")
		}
	})
}

// TestParseEntries 覆盖格式契约边界（自 cmd/sclient 原 TestReadEntriesFromFile 迁入并扩充：
// 多 Tab 容错与 CRLF 是原先未钉住的契约）。
func TestParseEntries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		content string
		want    []Entry
	}{
		{
			name:    "Tab 分隔文件名 + 空行 + 注释",
			content: "# comment line\nhttps://example.com/a.zip\tcustom-a.zip\n\nhttps://example.com/b.zip\nhttps://example.com/my%20file.txt\t我的文件.txt\n",
			want: []Entry{
				{URL: "https://example.com/a.zip", Filename: "custom-a.zip"},
				{URL: "https://example.com/b.zip"},
				{URL: "https://example.com/my%20file.txt", Filename: "我的文件.txt"},
			},
		},
		{
			name:    "URL 含空格不受影响（只按 Tab 切分）",
			content: "https://example.com/a b.zip\t保留空格.txt\n",
			want:    []Entry{{URL: "https://example.com/a b.zip", Filename: "保留空格.txt"}},
		},
		{
			name:    "多 Tab 只取前两列",
			content: "https://example.com/a.zip\tfile\twith\ttabs.zip\n",
			want:    []Entry{{URL: "https://example.com/a.zip", Filename: "file"}},
		},
		{
			name:    "CRLF 兼容",
			content: "https://example.com/a.zip\r\nhttps://example.com/b.zip\tb.zip\r\n",
			want: []Entry{
				{URL: "https://example.com/a.zip"},
				{URL: "https://example.com/b.zip", Filename: "b.zip"},
			},
		},
		{
			name:    "末尾无换行也计入",
			content: "https://example.com/a.zip",
			want:    []Entry{{URL: "https://example.com/a.zip"}},
		},
		{
			name:    "值两侧空白被 trim",
			content: "  https://example.com/a.zip  \t  a.zip  \n",
			want:    []Entry{{URL: "https://example.com/a.zip", Filename: "a.zip"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ParseEntries(tc.content)
			if len(got) != len(tc.want) {
				t.Fatalf("条目数 = %d, want %d (%+v)", len(got), len(tc.want), got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("entry[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
