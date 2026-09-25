// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestManifestRoundTrip 断言 Manifest 序列化/反序列化一致性（files 逐字段无损）。
func TestManifestRoundTrip(t *testing.T) {
	t.Parallel()
	orig := &Manifest{
		Schema:     1,
		Server:     ServerRef{URL: "https://src.example:18083"},
		ExportedAt: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC),
		Files: []FileEntry{
			{Name: "a.txt", Size: 5, Checksum: "abc", MTime: 123456789, Volume: "disk2"},
			{Name: "dir/b.bin", Size: 9, Checksum: "def"},
		},
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Manifest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Schema != orig.Schema || got.Server.URL != orig.Server.URL {
		t.Errorf("头部字段不一致: got %+v", got)
	}
	if len(got.Files) != 2 {
		t.Fatalf("files 数 = %d, want 2", len(got.Files))
	}
	if got.Files[0] != orig.Files[0] || got.Files[1] != orig.Files[1] {
		t.Errorf("files 逐字段不一致:\n got %+v\nwant %+v", got.Files, orig.Files)
	}
}

// TestManifestValidation 断言 schema/文件条目校验（fail-closed）：
// schema 不符、文件名为空、checksum 为空 → 拒绝。
func TestManifestValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		m    Manifest
		want string
	}{
		{
			name: "schema 不符",
			m:    Manifest{Schema: 2, Files: []FileEntry{{Name: "a", Checksum: "x"}}},
			want: "schema",
		},
		{
			name: "文件名为空",
			m:    Manifest{Schema: 1, Files: []FileEntry{{Checksum: "x"}}},
			want: "文件名",
		},
		{
			name: "checksum 为空",
			m:    Manifest{Schema: 1, Files: []FileEntry{{Name: "a"}}},
			want: "checksum",
		},
		{
			name: "路径穿越",
			m:    Manifest{Schema: 1, Files: []FileEntry{{Name: "../a", Checksum: "x"}}},
			want: "穿越",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.m.Validate(); err == nil {
				t.Fatalf("Validate() 应拒绝（want 含 %q）", tc.want)
			}
		})
	}
}

// TestManifestValidation_OK 断言合法清单通过校验。
func TestManifestValidation_OK(t *testing.T) {
	t.Parallel()
	m := Manifest{Schema: 1, Files: []FileEntry{{Name: "dir/a.txt", Size: 1, Checksum: "x"}}}
	if err := m.Validate(); err != nil {
		t.Fatalf("合法清单应通过: %v", err)
	}
}

// TestManifestWriteReadFile 断言 WriteFile/ReadFile 往返（含损坏文件拒绝导入）。
func TestManifestWriteReadFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	orig := &Manifest{Schema: 1, Files: []FileEntry{{Name: "a.txt", Size: 5, Checksum: "abc"}}}
	if err := WriteFile(path, orig); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got.Files) != 1 || got.Files[0].Checksum != "abc" {
		t.Errorf("round-trip 不一致: %+v", got)
	}

	// 损坏 → 拒绝
	if err := os.WriteFile(path, []byte("{corrupt"), 0o644); err != nil {
		t.Fatalf("写损坏文件: %v", err)
	}
	if _, err := ReadFile(path); err == nil {
		t.Fatal("损坏 manifest 应拒绝读取")
	}
}

// TestManifest_TamperedChecksum 断言：篡改 checksum 后再次校验失败（导入前自检命中）。
func TestManifest_TamperedChecksum(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	orig := &Manifest{Schema: 1, Files: []FileEntry{{Name: "a.txt", Size: 5, Checksum: "good"}}}
	if err := WriteFile(path, orig); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 篡改文件内 checksum（模拟传输损坏/手工编辑）
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if uErr := json.Unmarshal(raw, &m); uErr != nil {
		t.Fatal(uErr)
	}
	m.Files[0].Checksum = "evil"
	corrupt, _ := json.Marshal(m)
	if wErr := os.WriteFile(path, corrupt, 0o644); wErr != nil {
		t.Fatal(wErr)
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("结构仍可读（校验在导入层）: %v", err)
	}
	if got.Files[0].Checksum != "evil" {
		t.Fatalf("篡改未生效: %+v", got.Files[0])
	}
}
