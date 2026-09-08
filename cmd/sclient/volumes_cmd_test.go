// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
)

// ---- volumes 子命令 ----

func TestVolumesCommand_TextOutput(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/volumes" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"volumes":[{"name":"main","mode":"deny","capacity":0,"usage":0,"allowed":true},{"name":"disk2","mode":"allow","capacity":2048,"usage":1024,"allowed":true}]}`)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var out strings.Builder
	cmd := NewCmdVolumes(factory, cli.IOStreams{Out: &out, ErrOut: io.Discard})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("volumes command failed: %v", err)
	}
	got := out.String()
	for _, want := range []string{"main", "disk2", "不限", "2.0 KB", "1.0 KB (50.0%)", "是"} {
		if !strings.Contains(got, want) {
			t.Errorf("输出缺少 %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "main") || !strings.Contains(got, "disk2") {
		t.Errorf("输出应包含两卷名:\n%s", got)
	}
}

func TestVolumesCommand_Empty(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"volumes":[]}`)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var out strings.Builder
	cmd := NewCmdVolumes(factory, cli.IOStreams{Out: &out, ErrOut: io.Discard})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("volumes command failed: %v", err)
	}
	if !strings.Contains(out.String(), "无可见卷") {
		t.Errorf("空卷列表应提示无可见卷，got %q", out.String())
	}
}

// ---- mv --to-volume：跨卷 move + 改名（两步）----

func TestMvCommand_ToVolume_CrossVolumeMovesThenRenames(t *testing.T) {
	var movedQuery, renamedQuery url.Values
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/files/stat": // 源 Stat（HEAD）
			if r.Method != http.MethodHead {
				t.Errorf("stat method = %s, want HEAD", r.Method)
			}
			w.Header().Set("X-File-Checksum", "abc123")
			w.Header().Set("X-File-Size", "5")
			w.WriteHeader(http.StatusOK)
		case "/api/files": // VolumeOf 定位源卷
			if got := r.URL.Query().Get("volume"); got != "" {
				t.Errorf("VolumeOf 定位不应带 volume，got %q", got)
			}
			_, _ = io.WriteString(w, `{"files":[{"name":"a.txt","size":5,"volume":"main"}]}`)
		case "/api/volumes/move":
			movedQuery = r.URL.Query()
			_, _ = io.WriteString(w, `{"success":true,"message":"文件已移动"}`)
		case "/rename":
			renamedQuery = r.URL.Query()
			if got := renamedQuery.Get("volume"); got != "disk2" {
				t.Errorf("rename volume query = %q, want disk2", got)
			}
			if renamedQuery.Get("from") != "a.txt" || renamedQuery.Get("to") != "b.txt" {
				t.Errorf("rename query = %v", renamedQuery)
			}
			_, _ = io.WriteString(w, `{"success":true,"message":"文件已重命名"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}
	var out strings.Builder
	cmd := NewCmdMv(factory, cli.IOStreams{Out: &out, ErrOut: io.Discard}, st)
	_ = cmd.Flags().Set("to-volume", "disk2")
	cmd.SetArgs([]string{"a.txt", "b.txt"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("mv --to-volume failed: %v", err)
	}
	if movedQuery.Get("from_volume") != "main" || movedQuery.Get("to_volume") != "disk2" || movedQuery.Get("filename") != "a.txt" {
		t.Errorf("move query = %v, want from=main/to=disk2/file=a.txt", movedQuery)
	}
	if !strings.Contains(out.String(), "disk2") {
		t.Errorf("输出应提到目标卷 disk2:\n%s", out.String())
	}
}

// ---- mv 缺省（无 --to-volume）：同卷 rename（回归）----

func TestMvCommand_NoToVolume_RenameAsBefore(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/files/stat":
			w.Header().Set("X-File-Checksum", "abc123")
			w.Header().Set("X-File-Size", "5")
			w.WriteHeader(http.StatusOK)
		case "/rename":
			if got := r.URL.Query().Get("volume"); got != "" {
				t.Errorf("缺省 mv 不应带 volume query，got %q", got)
			}
			_, _ = io.WriteString(w, `{"success":true,"message":"文件已重命名"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdMv(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{"a.txt", "b.txt"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("mv failed: %v", err)
	}
}

// ---- 编译期引用：确保 JSON 序列化体积字段可用（防字段拼写漂移）----

func TestVolumeInfoJSONTags(t *testing.T) {
	v := client.VolumeInfo{Name: "n", Mode: "deny", Capacity: 1, Usage: 2, Allowed: true}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, key := range []string{`"name"`, `"mode"`, `"capacity"`, `"usage"`, `"allowed"`} {
		if !strings.Contains(s, key) {
			t.Errorf("VolumeInfo JSON 缺少 %s: %s", key, s)
		}
	}
}
