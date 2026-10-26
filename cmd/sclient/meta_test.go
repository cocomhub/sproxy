// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// newMetaTestRoot 构造带上 meta 子命令的测试根命令，并注册 server/auth-token 两个 persistent flag。
func newMetaTestRoot(svc *client.FileClient, ios cli.IOStreams) *cobra.Command {
	root := &cobra.Command{Use: "sclient"}
	root.PersistentFlags().String("server", "", "")
	root.PersistentFlags().String("access-key", "", "")
	root.PersistentFlags().String("access-key-secret", "", "")
	// 注册其余 factory 依赖的 persistent flags（NewClient 会读取 json/insecure 等）
	root.PersistentFlags().String("chunk-size", "", "")
	root.PersistentFlags().Bool("json", false, "")
	root.PersistentFlags().String("client-cert", "", "")
	root.PersistentFlags().String("client-key", "", "")
	root.PersistentFlags().Bool("client-cert-allow-missing", false, "")
	root.PersistentFlags().Bool("allow-transport-fallback", false, "")
	root.PersistentFlags().Bool("insecure", false, "")
	root.PersistentFlags().String("ca-file", "", "")
	root.AddCommand(NewCmdMeta(clientfactory.NewMock(svc, nil), ios, &state.State{}))
	return root
}

func TestMetaCmd_Use(t *testing.T) {
	factory := clientfactory.NewMock(nil, nil)
	cmd := NewCmdMeta(factory, cli.IOStreams{Out: io.Discard}, &state.State{})
	if !strings.HasPrefix(cmd.Use, "meta") {
		t.Errorf("metaCmd.Use = %q, want prefix 'meta'", cmd.Use)
	}
	found := false
	for _, c := range cmd.Commands() {
		if c.Name() == "version" {
			found = true
		}
	}
	if !found {
		t.Error("expected meta to have 'version' subcommand")
	}
}

func TestMetaVersionListCmd_Integration(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/versions" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"versions": []client.VersionInfo{
				{Filename: "file.txt", VersionID: 1, Size: 100, CreatedAt: "2026-01-01T00:00:00Z"},
			},
		})
	}))
	defer mock.Close()

	var buf strings.Builder
	svc := client.NewFileClient(mock.URL)
	root := newMetaTestRoot(svc, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	root.SetArgs([]string{"meta", "version", "list", "file.txt", "--server", mock.URL})
	if err := root.Execute(); err != nil {
		t.Fatalf("meta version list failed: %v", err)
	}
	if !strings.Contains(buf.String(), "file.txt") {
		t.Errorf("expected output to contain 'file.txt', got: %s", buf.String())
	}
}

func TestMetaVersionRestoreCmd_Integration(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/versions/restore" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"message":"restored"}`))
	}))
	defer mock.Close()

	var buf strings.Builder
	svc := client.NewFileClient(mock.URL)
	root := newMetaTestRoot(svc, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	root.SetArgs([]string{"meta", "version", "restore", "file.txt", "2", "--server", mock.URL})
	if err := root.Execute(); err != nil {
		t.Fatalf("meta version restore failed: %v", err)
	}
	if !strings.Contains(buf.String(), "已恢复") {
		t.Errorf("expected output to contain '已恢复', got: %s", buf.String())
	}
}

func TestMetaVersionDeleteCmd_Integration(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" || r.URL.Path != "/api/versions" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"message":"deleted"}`))
	}))
	defer mock.Close()

	var buf strings.Builder
	svc := client.NewFileClient(mock.URL)
	root := newMetaTestRoot(svc, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	root.SetArgs([]string{"meta", "version", "delete", "file.txt", "3", "--server", mock.URL})
	if err := root.Execute(); err != nil {
		t.Fatalf("meta version delete failed: %v", err)
	}
	if !strings.Contains(buf.String(), "已删除") {
		t.Errorf("expected output to contain '已删除', got: %s", buf.String())
	}
}

func TestMetaCmd_FileInfo(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/files/stat" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("X-File-Checksum", "abc123")
		w.Header().Set("X-File-Size", strconv.FormatInt(1024, 10))
		w.Header().Set("X-File-IsDir", "false")
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	var buf strings.Builder
	svc := client.NewFileClient(mock.URL)
	root := newMetaTestRoot(svc, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	root.SetArgs([]string{"meta", "file.txt", "--server", mock.URL})
	if err := root.Execute(); err != nil {
		t.Fatalf("meta command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "file.txt") {
		t.Errorf("expected output to contain 'file.txt', got: %s", buf.String())
	}
}

// TestParseMetaSetArgs_ThreeModes 钉住 meta set 的三模式归一（默认合并非覆盖）。
// 变异：把默认 mode 改成 replace → 本用例红。
func TestParseMetaSetArgs_ThreeModes(t *testing.T) {
	t.Parallel()
	newCmd := func(extra, del string, replace bool) *cobra.Command {
		c := &cobra.Command{}
		c.Flags().String("extra", "", "")
		c.Flags().String("delete", "", "")
		c.Flags().Bool("replace", false, "")
		_ = c.Flags().Set("extra", extra)
		_ = c.Flags().Set("delete", del)
		_ = c.Flags().Set("replace", "false")
		if replace {
			_ = c.Flags().Set("replace", "true")
		}
		return c
	}
	// 默认 = merge（新增非覆盖）
	if _, mode, err := parseMetaSetArgs(newCmd(`{"code":"X"}`, "", false)); err != nil || mode != client.MetaUpdateMerge {
		t.Fatalf("默认应 merge, got %s err=%v", mode, err)
	}
	// --replace = 整表替换
	if _, mode, err := parseMetaSetArgs(newCmd(`{"code":"X"}`, "", true)); err != nil || mode != client.MetaUpdateReplace {
		t.Fatalf("--replace 应 replace, got %s err=%v", mode, err)
	}
	// --delete = 删键
	extra, mode, err := parseMetaSetArgs(newCmd("", "code,maker", false))
	if err != nil || mode != client.MetaUpdateDelete {
		t.Fatalf("--delete 应 delete, got %s err=%v", mode, err)
	}
	if _, ok := extra["code"]; !ok {
		t.Fatalf("--delete 应含 code 键: %v", extra)
	}
	// --delete 与 --extra 互斥
	if _, _, err := parseMetaSetArgs(newCmd(`{"code":"X"}`, "code", false)); err == nil {
		t.Fatal("--delete 与 --extra 应互斥")
	}
	// 空 --extra 无 --delete → 报错
	if _, _, err := parseMetaSetArgs(newCmd("", "", false)); err == nil {
		t.Fatal("空 --extra 应报错")
	}
}
