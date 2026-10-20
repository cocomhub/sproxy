// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_cold_seg_api_test.go 锁定评审 R13 的「归档后必须能按需/随机读取」：
//   - `GET /api/audit/cold/seg` 只返回**白名单**内的冷段区间（非白名单 ⇒ 404，防穿越/探测）；
//   - 管理员门（非管理员 403）；
//   - 区间语义（offset/length 夹紧 + Content-Range）与字节内容。
//
// 另含**读面方向**回归：newest-first + limit 时冷段必须返回**最新**条目
// （`QueryCold` 逐段从头读 + 收满即停 ⇒ 会返回最新段里最旧的 N 条）。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"sync/atomic"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// writeColdAuditSegment 在 dir 下写一个冷段（返回路径与内容）。
func writeColdAuditSegment(t *testing.T, dir, name string, base time.Time, count int) (string, []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var b strings.Builder
	for i := range count {
		line, err := json.Marshal(audit.Entry{
			Scope: "system", Kind: audit.KindEvent, Action: "cold_probe",
			Type: audit.TypeSensitive, Result: audit.ResultSuccess,
			Object: name + "-" + strconv.Itoa(i), TS: base.Add(time.Duration(i) * time.Second),
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(b.String()), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p, []byte(b.String())
}

// coldSegServer 装配一个启用归档目录、只需读面能力的测试服务。
func coldSegServer(t *testing.T) (*Handlers, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "audit-archive")
	h := &Handlers{}
	cfg := Default()
	cfg.Audit.ArchiveDir = dir
	var cfgp atomic.Pointer[Config]
	cfgp.Store(cfg)
	h.cfgPtr = &cfgp
	h.auditLogger = testLogger()
	return h, dir
}

// TestAuditColdSeg_RangeRead 白名单内区间读：字节正确 + 夹紧 + Content-Range。
func TestAuditColdSeg_RangeRead(t *testing.T) {
	t.Parallel()
	h, dir := coldSegServer(t)
	base := time.Unix(1_800_000_000, 0).UTC()
	_, content := writeColdAuditSegment(t, dir, "audit.log.1.aaaaaaaaaaaaaaaa", base, 3)

	// 先取白名单里的绝对键（ListCold 的 Path 即 FS 键）。
	fsys := h.auditColdFS()
	segs, err := audit.ListCold(context.Background(), fsys, "")
	if err != nil || len(segs) != 1 {
		t.Fatalf("ListCold: segs=%d err=%v", len(segs), err)
	}
	path := segs[0].Path

	req := httptest.NewRequest(http.MethodGet, "/api/audit/cold/seg?path="+path+"&offset=0&length=40", nil)
	req = req.WithContext(permCtxPrincipal(&Principal{AK: "", Role: "admin"}))
	rec := httptest.NewRecorder()
	h.auditColdSegHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("区间读应 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body, _ := io.ReadAll(rec.Result().Body)
	if len(body) != 40 {
		t.Fatalf("应返回请求长度 40, got %d", len(body))
	}
	if string(body) != string(content[:40]) {
		t.Fatalf("返回字节与源区间不一致")
	}
	if got := rec.Header().Get("Content-Range"); !strings.HasPrefix(got, "bytes 0-39/") {
		t.Fatalf("Content-Range 应描述实际区间, got %q", got)
	}

	// 越界夹紧：offset 超过大小 ⇒ 空区间（200 且长度 0）。
	big := httptest.NewRequest(http.MethodGet, "/api/audit/cold/seg?path="+path+"&offset=999999", nil)
	big = big.WithContext(permCtxPrincipal(&Principal{AK: "", Role: "admin"}))
	rec2 := httptest.NewRecorder()
	h.auditColdSegHandler(rec2, big)
	if rec2.Code != http.StatusOK {
		t.Fatalf("越界应夹紧为 200 空区间, got %d", rec2.Code)
	}
}

// TestAuditColdSeg_RejectsUnknownPath 非白名单路径 ⇒ 404（不区分不存在/无权）。
func TestAuditColdSeg_RejectsUnknownPath(t *testing.T) {
	t.Parallel()
	h, _ := coldSegServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/audit/cold/seg?path=../../etc/passwd", nil)
	req = req.WithContext(permCtxPrincipal(&Principal{AK: "", Role: "admin"}))
	rec := httptest.NewRecorder()
	h.auditColdSegHandler(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("非白名单路径应 404, got %d", rec.Code)
	}
}

// TestAuditColdSeg_NonAdminForbidden 非管理员 ⇒ 403（读面唯一裁决点）。
func TestAuditColdSeg_NonAdminForbidden(t *testing.T) {
	t.Parallel()
	h, _ := coldSegServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/audit/cold/seg?path=audit.log.1", nil)
	rec := httptest.NewRecorder()
	h.auditColdSegHandler(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非管理员应 403, got %d", rec.Code)
	}
}

// TestReadAuditEvents_NewestFirstUsesColdTail 读面方向回归（评审 R13）：
// newest-first + limit 必须返回**最新**条目（旧实现冷段从头读 + 收满即停 ⇒ 返回最新段里最旧的 N 条）。
func TestReadAuditEvents_NewestFirstUsesColdTail(t *testing.T) {
	t.Parallel()
	h, dir := coldSegServer(t)
	attachTestAudit(t, h) // 读面需要 journal（冷段合并路径在 readAuditFromJournal）
	base := time.Unix(1_800_000_000, 0).UTC()
	// 单段 5 条：最新一条是 ...-4。limit=1 必须拿到它。
	writeColdAuditSegment(t, dir, "audit.log.1.aaaaaaaaaaaaaaaa", base, 5)

	evs := h.readAuditEvents(context.Background(), audit.EntryFilter{Action: "cold_probe"}, 1, true)
	if len(evs) != 1 {
		t.Fatalf("应返回 1 条, got %d", len(evs))
	}
	if !strings.HasSuffix(evs[0].Object, "-4") {
		t.Fatalf("newest-first 应返回最新条目（...-4）, got %q", evs[0].Object)
	}
}
