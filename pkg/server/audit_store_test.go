// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// audit_store_test.go 验证审计落盘（audit.persist_dir，roadmap §2 P1）：
//  1. AuditStore 载入：启动时扫描 <dir>/audit.log 恢复历史（重启可查）。
//  2. AuditStore Append：RecordAudit 落盘 JSON line + 内存全量。
//  3. 过滤：Recent 支持 action/actor/时间范围（复用 AuditFilter）。
//  4. 重启语义：新 AuditStore 载入旧日志后可查历史（验收核心）。
//  5. 落盘格式：每行一个 AuditEvent JSON（可机器回放）。

// newTestAuditStore 创建指向 t.TempDir 的 AuditStore（helper）。
func newTestAuditStore(t *testing.T) *AuditStore {
	t.Helper()
	dir := t.TempDir()
	st, err := NewAuditStore(filepath.Join(dir, "audit.log"), testLogger())
	if err != nil {
		t.Fatalf("NewAuditStore: %v", err)
	}
	return st
}

// TestAuditStore_AppendAndRecent 验证 append 进内存 + 过滤查询。
func TestAuditStore_AppendAndRecent(t *testing.T) {
	t.Parallel()
	st := newTestAuditStore(t)

	evt1 := AuditEvent{Action: "delete", Actor: "ak-a", Object: "f1.txt", Result: "success", TS: time.Now()}
	evt2 := AuditEvent{Action: "rename", Actor: "ak-b", Object: "f2.txt", Result: "success", TS: time.Now().Add(time.Second)}
	if err := st.Append(evt1); err != nil {
		t.Fatalf("Append(evt1): %v", err)
	}
	if err := st.Append(evt2); err != nil {
		t.Fatalf("Append(evt2): %v", err)
	}

	// 全量：2 条（按 TS 倒序，最新在前）。
	all := st.Recent(10, AuditFilter{})
	if len(all) != 2 {
		t.Fatalf("Recent(全量) = %d 条, want 2", len(all))
	}
	if all[0].Action != "rename" {
		t.Fatalf("Recent[0].Action = %q, want rename（最新在前）", all[0].Action)
	}

	// action 过滤：只 delete。
	del := st.Recent(10, AuditFilter{Action: "delete"})
	if len(del) != 1 || del[0].Action != "delete" {
		t.Fatalf("action=delete 过滤 = %+v, want 1 条 delete", del)
	}

	// actor 过滤：只 ak-b。
	akb := st.Recent(10, AuditFilter{Actor: "ak-b"})
	if len(akb) != 1 || akb[0].Actor != "ak-b" {
		t.Fatalf("actor=ak-b 过滤 = %+v, want 1 条", akb)
	}
}

// TestAuditStore_PersistReload 验证重启可查（验收核心）：append 后新建 AuditStore
// 载入同一日志文件，历史可查。
func TestAuditStore_PersistReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "audit.log")

	st1, err := NewAuditStore(logPath, testLogger())
	if err != nil {
		t.Fatalf("NewAuditStore(st1): %v", err)
	}
	base := time.Now()
	events := []AuditEvent{
		{Action: "delete", Actor: "ak-x", Object: "a.txt", Result: "success", TS: base},
		{Action: "delete", Actor: "ak-x", Object: "b.txt", Result: "success", TS: base.Add(time.Second)},
	}
	for _, ev := range events {
		if appendErr := st1.Append(ev); appendErr != nil {
			t.Fatalf("st1.Append: %v", appendErr)
		}
	}

	// 重启：新实例载入同一日志。
	st2, err := NewAuditStore(logPath, testLogger())
	if err != nil {
		t.Fatalf("NewAuditStore(st2): %v", err)
	}
	got := st2.Recent(10, AuditFilter{})
	if len(got) != 2 {
		t.Fatalf("重启后 Recent = %d 条, want 2（落盘可查）", len(got))
	}
	if got[0].Object != "b.txt" {
		t.Fatalf("重启后 Recent[0].Object = %q, want b.txt（最新在前）", got[0].Object)
	}
}

// TestAuditStore_LogFormatJSONLines 验证落盘格式：每行一个 AuditEvent JSON。
func TestAuditStore_LogFormatJSONLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "audit.log")
	st, err := NewAuditStore(logPath, testLogger())
	if err != nil {
		t.Fatalf("NewAuditStore: %v", err)
	}
	evt := AuditEvent{Action: "delete", Actor: "ak-z", Object: "c.txt", Result: "success", TS: time.Now()}
	if appendErr := st.Append(evt); appendErr != nil {
		t.Fatalf("Append: %v", appendErr)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := splitLines(string(raw))
	if len(lines) != 1 {
		t.Fatalf("audit.log 应 1 行, got %d 行: %q", len(lines), string(raw))
	}
	var decoded AuditEvent
	if err := json.Unmarshal([]byte(lines[0]), &decoded); err != nil {
		t.Fatalf("JSON line 解析失败: %v", err)
	}
	if decoded.Action != "delete" || decoded.Object != "c.txt" {
		t.Fatalf("解码事件 = %+v, want delete c.txt", decoded)
	}
	if !reflect.DeepEqual(decoded.TS.UTC(), evt.TS.UTC()) {
		t.Fatalf("TS 不一致: %v vs %v", decoded.TS, evt.TS)
	}
}

// TestAuditStore_RecentSince 验证时间范围过滤（Since 保留 TS.After(since)）。
func TestAuditStore_RecentSince(t *testing.T) {
	t.Parallel()
	st := newTestAuditStore(t)
	base := time.Now()
	if err := st.Append(AuditEvent{Action: "delete", Actor: "a", Object: "old.txt", Result: "success", TS: base.Add(-time.Hour)}); err != nil {
		t.Fatalf("Append(old): %v", err)
	}
	if err := st.Append(AuditEvent{Action: "delete", Actor: "a", Object: "new.txt", Result: "success", TS: base}); err != nil {
		t.Fatalf("Append(new): %v", err)
	}

	got := st.Recent(10, AuditFilter{Since: base.Add(-time.Minute)})
	if len(got) != 1 || got[0].Object != "new.txt" {
		t.Fatalf("Since 过滤 = %+v, want 仅 new.txt", got)
	}
}

// splitLines 按换行拆分（去掉末尾空行）。
func splitLines(s string) []string {
	var out []string
	for _, line := range splitLinesRaw(s) {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
