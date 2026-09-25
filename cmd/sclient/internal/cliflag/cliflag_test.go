// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cliflag

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// newTestCmd 构造带若干 flag 的测试命令（模拟 meshconn/cloud 的 flag 集）。
func newTestCmd() (*cobra.Command, *string, *bool, *time.Duration, *[]string) {
	var s string
	var b bool
	var d time.Duration
	var sl []string
	cmd := &cobra.Command{Use: "test"}
	f := cmd.Flags()
	f.StringVar(&s, "name", "default", "name flag")
	f.BoolVar(&b, "verbose", false, "verbose flag")
	f.DurationVar(&d, "timeout", 5*time.Second, "timeout flag")
	f.StringSliceVar(&sl, "tags", nil, "tags flag")
	return cmd, &s, &b, &d, &sl
}

// TestString_Registered：已注册 flag 读取值。
func TestString_Registered(t *testing.T) {
	t.Parallel()
	cmd, _, _, _, _ := newTestCmd()
	cmd.SetArgs([]string{"--name", "hello"})
	if err := cmd.ParseFlags([]string{"--name", "hello"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	var got string
	if err := String(cmd, "name", &got); err != nil {
		t.Fatalf("String: %v", err)
	}
	if got != "hello" {
		t.Errorf("String 应读到 hello, got %q", got)
	}
}

// TestString_Unregistered：flag 未注册时跳过（target 保持零值，不报错）。
func TestString_Unregistered(t *testing.T) {
	t.Parallel()
	cmd, _, _, _, _ := newTestCmd()
	got := "preset"
	if err := String(cmd, "not-registered", &got); err != nil {
		t.Fatalf("String 未注册应跳过不报错: %v", err)
	}
	if got != "preset" {
		t.Errorf("未注册 flag 应保持 target 原值, got %q", got)
	}
}

// TestBool_TypeMismatch：类型不匹配时错误传播（不静默忽略）。
func TestBool_TypeMismatch(t *testing.T) {
	t.Parallel()
	cmd, _, _, _, _ := newTestCmd()
	var got bool
	err := Bool(cmd, "name", &got) // name 是 string flag，用 Bool 读 → 类型错误
	if err == nil {
		t.Fatal("Bool 读 string flag 应报类型错误")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("错误应含 flag 名: %v", err)
	}
}

// TestDuration_Default：未显式设置时读默认值。
func TestDuration_Default(t *testing.T) {
	t.Parallel()
	cmd, _, _, _, _ := newTestCmd()
	var got time.Duration
	if err := Duration(cmd, "timeout", &got); err != nil {
		t.Fatalf("Duration: %v", err)
	}
	if got != 5*time.Second {
		t.Errorf("未设置应读默认 5s, got %v", got)
	}
}

// TestStringSlice_Explicit：显式设置 StringSlice。
func TestStringSlice_Explicit(t *testing.T) {
	t.Parallel()
	cmd, _, _, _, _ := newTestCmd()
	if err := cmd.ParseFlags([]string{"--tags", "a,b"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	var got []string
	if err := StringSlice(cmd, "tags", &got); err != nil {
		t.Fatalf("StringSlice: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("StringSlice 应读 [a b], got %v", got)
	}
}

// TestStringArray_NoCommaSplit：StringArray **不在逗号处拆分**——每次显式设置
// 追加一个条目（值内含逗号的 flag 用，如 --route .example.com=node-a,node-b）。
func TestStringArray_NoCommaSplit(t *testing.T) {
	t.Parallel()
	var routes []string
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().StringArrayVar(&routes, "route", nil, "route flag")
	if err := cmd.ParseFlags([]string{"--route", ".example.com=node-a,node-b", "--route", "10.0.0.0/8=node-c"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	var got []string
	if err := StringArray(cmd, "route", &got); err != nil {
		t.Fatalf("StringArray: %v", err)
	}
	want := []string{".example.com=node-a,node-b", "10.0.0.0/8=node-c"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("StringArray 应读 %v（逗号不拆分、逐条追加）, got %v", want, got)
	}
}

// TestStringArray_Unregistered：flag 未注册时跳过（target 保持原值，不报错）。
func TestStringArray_Unregistered(t *testing.T) {
	t.Parallel()
	cmd, _, _, _, _ := newTestCmd()
	got := []string{"preset"}
	if err := StringArray(cmd, "not-registered", &got); err != nil {
		t.Fatalf("StringArray 未注册应跳过不报错: %v", err)
	}
	if len(got) != 1 || got[0] != "preset" {
		t.Errorf("未注册 flag 应保持 target 原值, got %v", got)
	}
}

// TestChanged：Changed 区分「未指定」与「显式设置」。
func TestChanged(t *testing.T) {
	t.Parallel()
	cmd, _, _, _, _ := newTestCmd()
	if err := cmd.ParseFlags(nil); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if Changed(cmd, "name") {
		t.Error("未显式设置 name，Changed 应为 false")
	}
	if err := cmd.ParseFlags([]string{"--name", "x"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if !Changed(cmd, "name") {
		t.Error("显式设置 name 后，Changed 应为 true")
	}
}
