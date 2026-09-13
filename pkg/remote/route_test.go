// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package remote

import "testing"

func TestParseRef(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    Ref
		wantErr bool
	}{
		{name: "文件", in: "remote://nodeB/main/docs/a.txt", want: Ref{Node: "nodeB", Volume: "main", Path: "docs/a.txt"}},
		{name: "卷根", in: "remote://nodeB/main", want: Ref{Node: "nodeB", Volume: "main"}},
		{name: "卷根带尾斜杠", in: "remote://nodeB/main/", want: Ref{Node: "nodeB", Volume: "main"}},
		{name: "重复斜杠归一", in: "remote://nodeB/main//docs//a.txt", want: Ref{Node: "nodeB", Volume: "main", Path: "docs/a.txt"}},
		{name: "前导斜杠归一", in: "remote://nodeB/main//docs/a.txt", want: Ref{Node: "nodeB", Volume: "main", Path: "docs/a.txt"}},
		{name: "单点段归一", in: "remote://nodeB/main/./docs/./a.txt", want: Ref{Node: "nodeB", Volume: "main", Path: "docs/a.txt"}},
		{name: "反斜杠视为分隔符", in: `remote://nodeB/main/docs\a.txt`, want: Ref{Node: "nodeB", Volume: "main", Path: "docs/a.txt"}},
		{name: "首尾空白容忍", in: "  remote://nodeB/main/x  ", want: Ref{Node: "nodeB", Volume: "main", Path: "x"}},

		{name: "缺卷", in: "remote://nodeB", wantErr: true},
		{name: "缺卷只有斜杠", in: "remote://nodeB/", wantErr: true},
		{name: "缺节点", in: "remote:///main/x", wantErr: true},
		{name: "前缀不符", in: "http://nodeB/main/x", wantErr: true},
		{name: "无 scheme", in: "nodeB/main/x", wantErr: true},
		{name: "空串", in: "", wantErr: true},
		{name: "路径含父引用", in: "remote://nodeB/main/docs/../meta/x", wantErr: true},
		{name: "路径为父引用", in: "remote://nodeB/main/..", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRef(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseRef(%q) 应报错，got %+v", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRef(%q) 报错: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseRef(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

// TestRef_StringRoundTrip 钉住「句柄往返一致」：String() 的结果再解析必须等价。
func TestRef_StringRoundTrip(t *testing.T) {
	for _, in := range []string{
		"remote://nodeB/main",
		"remote://nodeB/main/docs/a.txt",
		"remote://node-b/vol-1/a/b/c.bin",
	} {
		ref, err := ParseRef(in)
		if err != nil {
			t.Fatalf("ParseRef(%q): %v", in, err)
		}
		s := ref.String()
		if s != in {
			t.Fatalf("String() = %q, want %q（往返必须一致）", s, in)
		}
		again, err := ParseRef(s)
		if err != nil {
			t.Fatalf("再次 ParseRef(%q): %v", s, err)
		}
		if again != ref {
			t.Fatalf("往返后 Ref 不等价: %+v vs %+v", again, ref)
		}
	}
}

func TestRef_RootAndIsRoot(t *testing.T) {
	file, err := ParseRef("remote://nodeB/main/docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if file.IsRoot() {
		t.Fatal("含路径的句柄不应为卷根")
	}
	root := file.Root()
	if root.Node != "nodeB" || root.Volume != "main" || root.Path != "" {
		t.Fatalf("Root() = %+v, want {nodeB main }", root)
	}
	if !root.IsRoot() {
		t.Fatal("Root() 应为卷根")
	}
}

// TestNormalizeRelPath 钉住路径归一契约（与 sync.FS 的路径契约一致：根相对、正斜杠）。
func TestNormalizeRelPath(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: ""},
		{in: "/", want: ""},
		{in: "a/b", want: "a/b"},
		{in: "//a//b//", want: "a/b"},
		{in: "./a/./b", want: "a/b"},
		{in: `a\b`, want: "a/b"},
		{in: "..", wantErr: true},
		{in: "a/../b", wantErr: true},
	}
	for _, tc := range cases {
		got, err := normalizeRelPath(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("normalizeRelPath(%q) 应报错", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("normalizeRelPath(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("normalizeRelPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
