// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestReportError_SinglePrintWhenNotReported 验证：命令未打印错误时，main 补一行
// `sclient: <err>`（stdout 空，非 --json）。
func TestReportError_SinglePrintWhenNotReported(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	reportError(errors.New("boom"), false, false, &out, &errOut)
	if out.Len() != 0 {
		t.Fatalf("非 --json 时 stdout 应为空，得到 %q", out.String())
	}
	if got := errOut.String(); got != "sclient: boom\n" {
		t.Fatalf("stderr=%q want %q", got, "sclient: boom\n")
	}
}

// TestReportError_NoDuplicateWhenCommandPrinted 验证：命令已 WriteErrLine（reported=true）
// 时 main 不再补打（保留命令的稳定文案）。
func TestReportError_NoDuplicateWhenCommandPrinted(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	reportError(errors.New("boom"), false, true, &out, &errOut)
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Fatalf("已报告时不应再打印：stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

// TestReportError_JSONEmitsStructuredError 验证：--json 时 stdout 输出结构化错误对象
// （脚本无需解析 stderr 文案），且命令已打印时 stderr 不重复。
func TestReportError_JSONEmitsStructuredError(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	reportError(errors.New("下载失败: x"), true, true, &out, &errOut)
	if errOut.Len() != 0 {
		t.Fatalf("已报告时 stderr 应为空，得到 %q", errOut.String())
	}
	var obj map[string]any
	if err := json.Unmarshal(out.Bytes(), &obj); err != nil {
		t.Fatalf("stdout 应为合法 JSON: %v (%q)", err, out.String())
	}
	if obj["success"] != false || obj["error"] != "下载失败: x" || obj["exit_code"] != float64(1) {
		t.Fatalf("JSON 错误对象不符: %v", obj)
	}
}

// TestReportError_InspectDiff 验证：审查发现差异（errInspectDiff）不额外输出 JSON 错误对象
// （命令已输出结构化结果），退出码 3，且未报告时补一行 stderr。
func TestReportError_InspectDiff(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	reportError(errInspectDiff, true, false, &out, &errOut)
	if out.Len() != 0 {
		t.Fatalf("errInspectDiff 不应输出 JSON 错误对象，得到 %q", out.String())
	}
	if !strings.Contains(errOut.String(), "sclient:") {
		t.Fatalf("未报告时应补一行 stderr，得到 %q", errOut.String())
	}
	if exitCodeFor(errInspectDiff) != 3 {
		t.Fatalf("errInspectDiff 退出码应为 3")
	}
}
