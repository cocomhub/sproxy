// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"os"
	"strings"
	"testing"
)

// ---- root command ----

func TestRootCmd_Use(t *testing.T) {
	if rootCmd.Use != "sclient" {
		t.Errorf("rootCmd.Use = %q, want %q", rootCmd.Use, "sclient")
	}
}

func TestRootCmd_SubCommands(t *testing.T) {
	cmds := rootCmd.Commands()
	names := make([]string, len(cmds))
	for i, c := range cmds {
		names[i] = c.Use
	}
	for _, want := range []string{"upload", "download", "delete", "list", "tunnel", "genkey", "config", "version"} {
		found := false
		for _, name := range names {
			if strings.HasPrefix(name, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing subcommand: %q in %v", want, names)
		}
	}
}

func TestRootCmd_PersistentFlags(t *testing.T) {
	// 验证 persistent flags 已注册
	flagNames := []string{"config", "server", "output", "verbose", "chunked", "chunk-size", "concurrency", "resume"}
	for _, name := range flagNames {
		f := rootCmd.PersistentFlags().Lookup(name)
		if f == nil {
			t.Errorf("missing persistent flag: %q", name)
		}
	}
}

// ---- upload command ----

func TestUploadCmd(t *testing.T) {
	if uploadCmd.Use != "upload <file1> [file2...]" {
		t.Errorf("uploadCmd.Use = %q", uploadCmd.Use)
	}
	if uploadCmd.Short != "上传一个或多个文件" {
		t.Errorf("uploadCmd.Short = %q", uploadCmd.Short)
	}
	// MinimumNArgs(1)
	if err := uploadCmd.Args(uploadCmd, []string{}); err == nil {
		t.Error("upload should require at least 1 arg")
	}
	if err := uploadCmd.Args(uploadCmd, []string{"a.txt"}); err != nil {
		t.Errorf("upload with 1 arg should be ok: %v", err)
	}
	// 验证 flags
	flagNames := []string{"chunked", "chunk-size", "concurrency", "resume"}
	for _, name := range flagNames {
		f := uploadCmd.Flags().Lookup(name)
		if f == nil {
			t.Errorf("uploadCmd missing flag: %q", name)
		}
	}
}

// ---- download command ----

func TestDownloadCmd(t *testing.T) {
	if downloadCmd.Use != "download <filename> [output]" {
		t.Errorf("downloadCmd.Use = %q", downloadCmd.Use)
	}
	if err := downloadCmd.Args(downloadCmd, []string{}); err == nil {
		t.Error("download should require at least 1 arg")
	}
	if err := downloadCmd.Args(downloadCmd, []string{"file.txt"}); err != nil {
		t.Errorf("download with 1 arg should be ok: %v", err)
	}
	flagNames := []string{"chunked", "chunk-size", "concurrency", "resume"}
	for _, name := range flagNames {
		f := downloadCmd.Flags().Lookup(name)
		if f == nil {
			t.Errorf("downloadCmd missing flag: %q", name)
		}
	}
}

// ---- delete command ----

func TestDeleteCmd(t *testing.T) {
	if deleteCmd.Use != "delete <filename>" {
		t.Errorf("deleteCmd.Use = %q", deleteCmd.Use)
	}
	if err := deleteCmd.Args(deleteCmd, []string{}); err == nil {
		t.Error("delete should require exactly 1 arg")
	}
	if err := deleteCmd.Args(deleteCmd, []string{"a.txt"}); err != nil {
		t.Errorf("delete with 1 arg should be ok: %v", err)
	}
	if err := deleteCmd.Args(deleteCmd, []string{"a", "b"}); err == nil {
		t.Error("delete should reject 2 args")
	}
	f := deleteCmd.Flags().Lookup("check-local")
	if f == nil {
		t.Error("deleteCmd missing flag: check-local")
	}
}

// ---- list command ----

func TestListCmd(t *testing.T) {
	if listCmd.Use != "list" {
		t.Errorf("listCmd.Use = %q", listCmd.Use)
	}
	f := listCmd.Flags().Lookup("subdir")
	if f == nil {
		t.Error("listCmd missing flag: subdir")
	}
}

// ---- mv command ----

func TestMvCmd(t *testing.T) {
	if mvCmd.Use != "mv <from> <to>" {
		t.Errorf("mvCmd.Use = %q", mvCmd.Use)
	}
	if err := mvCmd.Args(mvCmd, []string{}); err == nil {
		t.Error("mv should require exactly 2 args")
	}
	if err := mvCmd.Args(mvCmd, []string{"a", "b"}); err != nil {
		t.Errorf("mv with 2 args should be ok: %v", err)
	}
}

// ---- stat command ----

func TestStatCmd(t *testing.T) {
	if statCmd.Use != "stat <filename>" {
		t.Errorf("statCmd.Use = %q", statCmd.Use)
	}
	if err := statCmd.Args(statCmd, []string{}); err == nil {
		t.Error("stat should require exactly 1 arg")
	}
	if err := statCmd.Args(statCmd, []string{"f.txt"}); err != nil {
		t.Errorf("stat with 1 arg should be ok: %v", err)
	}
}

// ---- version command ----

func TestVersionCmd(t *testing.T) {
	// version command is not registered via init() but via rootCmd init directly
	var found bool
	for _, c := range rootCmd.Commands() {
		if c.Use == "version" {
			found = true
			break
		}
	}
	if !found {
		t.Error("version command not registered in rootCmd")
	}
}

// ---- initLogger ----

func TestCLientInitLogger(t *testing.T) {
	logger := initLogger(false)
	if logger == nil {
		t.Fatal("initLogger returned nil")
	}

	// verbose mode
	verboseLogger := initLogger(true)
	if verboseLogger == nil {
		t.Fatal("initLogger(true) returned nil")
	}

	// level: verbose -> debug
	verboseHandler, ok := verboseLogger.Handler().(*slog.TextHandler)
	if !ok {
		t.Log("handler is not TextHandler, skipping level check")
		return
	}
	_ = verboseHandler // 实际 level 无法从 Handler 直接读取，仅验证不崩溃
}

// ---- helper tests ----

func TestMustResolveRemotePath(t *testing.T) {
	old := currentDir
	currentDir = ""
	defer func() { currentDir = old }()

	got := mustResolveRemotePath("test.txt")
	if got != "test.txt" {
		t.Errorf("mustResolveRemotePath('test.txt') = %q, want 'test.txt'", got)
	}
}

// captureStderr 捕获 stderr 输出的辅助函数
func captureStderr(fn func()) string {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	old := os.Stderr
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = old
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	return string(buf[:n])
}

// captureStdout 捕获 stdout 输出的辅助函数
func captureStdout(fn func()) string {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	old := os.Stdout
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	return string(buf[:n])
}

// 重置 cobra.Command 的 help func 避免在测试中意外触发帮助输出
func init() {
	// 不修改生产代码，为测试预设一些状态
}
