// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// helper_impl_drift_test.go 守卫**同一纯函数的两份实现**：`pkg/files`（领域侧）与
// `pkg/server`（装配侧）各有一份 `atomicRenameRoot`。
//
// 为什么需要它：领域侧那份是"无法 import 装配层"的产物（见 pkg/files/service.go 的
// 「跨族共享的纯函数」小节与包文档）。它的语义里有一段 **Windows 句柄释放延迟的
// 退避重试**——行为测试在 Linux/CI 上走不到慢速路径，两份实现分叉**不会被任何现有测试
// 发现**（没有测试同时驱动两份）。故此处做**源码级等价断言**：抽取两处实现的语义骨架
// （重试次数、退避基数、关键调用次序）逐项比对，改坏任一侧即红。
//
// 与 `chunked_wire_drift_test.go` 同属"跨侧漂移守卫"，但对象不是 JSON 契约而是实现语义，
// 故独立成文件（前者只读字段形状与序列化字节）。

// renameSemantics 是 atomicRenameRoot 的语义骨架。
type renameSemantics struct {
	MaxAttempts string   // `const maxAttempts = <X>` 的字面值
	BaseDelay   string   // `const baseDelay = <Y>` 的字面值
	Ops         []string // 关键调用序列（Rename / Remove / Sleep / 退避位移），保序
}

var (
	renameConstRe = regexp.MustCompile(`const\s+maxAttempts\s*=\s*([0-9]+)`)
	baseDelayRe   = regexp.MustCompile(`const\s+baseDelay\s*=\s*([^/\n]+)`)
	renameOpRe    = regexp.MustCompile(`root\.(Rename|Remove)\(|time\.Sleep\(|baseDelay\s*<<\s*i`)
)

// funcBody 截取源码中 `func ... <name>(` 起始到下一个顶层 `}` 之间的函数体。
// 找不到该函数（被改名/删除）即 fail——这本身也是漂移的一种。
func funcBody(t *testing.T, src, name string) string {
	t.Helper()
	decl := regexp.MustCompile(`(?m)^func [^\n]*\b` + regexp.QuoteMeta(name) + `\(`)
	loc := decl.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("源码中未找到函数 %s（被改名/删除？）", name)
	}
	rest := src[loc[1]:]
	before, _, ok := strings.Cut(rest, "\n}\n")
	if !ok {
		t.Fatalf("函数 %s 的函数体未找到结束花括号", name)
	}
	return before
}

func extractRenameSemantics(t *testing.T, src, name string) renameSemantics {
	t.Helper()
	body := funcBody(t, src, name)
	m := renameConstRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("%s 中未找到 `const maxAttempts = <N>`", name)
	}
	d := baseDelayRe.FindStringSubmatch(body)
	if d == nil {
		t.Fatalf("%s 中未找到 `const baseDelay = <Y>`", name)
	}
	var ops []string
	for _, op := range renameOpRe.FindAllString(body, -1) {
		ops = append(ops, strings.Join(strings.Fields(op), " "))
	}
	if len(ops) == 0 {
		t.Fatalf("%s 中未抽取到任何关键调用（Rename/Remove/Sleep）", name)
	}
	return renameSemantics{MaxAttempts: m[1], BaseDelay: strings.TrimSpace(d[1]), Ops: ops}
}

// TestAtomicRenameRoot_ImplParity 断言 pkg/files 与 pkg/server 两份 atomicRenameRoot 的
// 语义骨架逐项一致：重试次数、退避基数、以及"快速路径 Rename → 慢速路径 Remove 后 Rename
// → 失败时 Sleep(退避位移)"的调用次序。
func TestAtomicRenameRoot_ImplParity(t *testing.T) {
	domain := extractRenameSemantics(t, readRepoFile(t, "pkg/files/service.go"), "atomicRenameRoot")
	assembly := extractRenameSemantics(t, readRepoFile(t, "pkg/server/upload_handler.go"), "atomicRenameRoot")

	if domain.MaxAttempts != assembly.MaxAttempts {
		t.Fatalf("atomicRenameRoot 重试次数漂移：pkg/files=%s pkg/server=%s", domain.MaxAttempts, assembly.MaxAttempts)
	}
	if domain.BaseDelay != assembly.BaseDelay {
		t.Fatalf("atomicRenameRoot 退避基数漂移：pkg/files=%q pkg/server=%q", domain.BaseDelay, assembly.BaseDelay)
	}
	if !reflect.DeepEqual(domain.Ops, assembly.Ops) {
		t.Fatalf("atomicRenameRoot 调用次序漂移：\n pkg/files =%v\n pkg/server=%v", domain.Ops, assembly.Ops)
	}
	// 正探针：确认两份实现都真的被抽到了内容（否则上面的相等是"两个空值相等"的假绿）。
	if domain.MaxAttempts == "" || len(domain.Ops) < 4 {
		t.Fatalf("抽取结果过弱，守卫可能失效：%+v", domain)
	}
}
