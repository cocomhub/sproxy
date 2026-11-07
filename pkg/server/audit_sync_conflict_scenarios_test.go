// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_insertion 说明：sync 冲突解决（`sync_conflict_resolve`）是 §9.1 里最后一个未覆盖动作。
// 产生冲突需要真实双向同步（push/pull 各改同一文件），代价高且易 flaky；本用例改为**直接向
// 冲突索引注入一条真实冲突记录**（`ConflictIndex.Record`，与同步执行写入的是同一结构），
// 再走真实 `syncResolveConflict` handler ⇒ 覆盖「解决动作的审计」这一被测语义。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
)

// TestAuditScenario_SyncConflictResolve 冲突解决 ⇒ 一条 sync_conflict_resolve（object=冲突文件路径）。
func TestAuditScenario_SyncConflictResolve(t *testing.T) {
	t.Parallel()
	h, _ := newUserVolumeAPIHandlers(t, true)
	attachTestAudit(t, h)

	idx, err := syncmgr.NewConflictIndex(t.TempDir())
	if err != nil {
		t.Fatalf("NewConflictIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	h.conflictIndex = idx

	id := idx.Record(syncpkg.ConflictRecord{
		Path:   "sync-conflict.txt",
		Ours:   []string{"local line"},
		Theirs: []string{"remote line"},
	})
	if id == "" {
		t.Fatal("冲突记录应返回 ID")
	}

	body, _ := json.Marshal(map[string]string{"choice": "ours"})
	req := httptest.NewRequest(http.MethodPost, "/api/sync/conflicts/"+id+"/resolve", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentTypeJSON)
	req.SetPathValue("id", id)
	req = req.WithContext(withActor(req.Context(), "alice"))
	rec := httptest.NewRecorder()
	h.syncResolveConflict(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	entries := h.readAuditEvents(context.Background(), audit.EntryFilter{Action: "sync_conflict_resolve"}, 0, false)
	if len(entries) == 0 {
		t.Fatal("缺少 sync_conflict_resolve 审计行（冲突解决会写数据，必须可追溯 ✗）")
	}
	ev := entries[len(entries)-1]
	if ev.Object != "sync-conflict.txt" {
		t.Errorf("object = %q, want sync-conflict.txt", ev.Object)
	}
	if ev.Actor != "alice" {
		t.Errorf("actor = %q, want alice", ev.Actor)
	}
}
