// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// manager_audit_sanitize_test.go 钉住审计 Err 脱敏（对抗性评审 S2）：*url.Error（HTTP 下载
// 失败即此形态）文本含完整 URL + query（token/签名），写入审计行（task.Audit + 磁盘审计
// 文件双持久化）前必须剥离 query——host+path 保留、token 不落盘。

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestSanitizeAuditErr_StripsURLQuery（S2 纯函数）：*url.Error（含可重试包装）文本脱敏为
// host+path，query（token）剥离；无 URL 错误保持原样；sanitizeAuditError(nil) 返回 nil。
func TestSanitizeAuditErr_StripsURLQuery(t *testing.T) {
	t.Parallel()
	uerr := &url.Error{Op: "Get", URL: "https://host.example/share/file?token=SECRET_QUERY_TOKEN&x=1", Err: errors.New("dial tcp: refused")}
	got := sanitizeAuditErr(uerr)
	if strings.Contains(got, "SECRET_QUERY_TOKEN") || strings.Contains(got, "?") {
		t.Fatalf("原始 url.Error 脱敏后仍含 query: %q", got)
	}
	if !strings.Contains(got, "https://host.example/share/file") {
		t.Fatalf("脱敏应保留 host+path: %q", got)
	}
	// http_downloader 可重试错误形态：RetryableError{Err: fmt.Errorf("http get: %w", urlErr)}。
	wrapped := &downloader.RetryableError{Err: fmt.Errorf("http get: %w", uerr)}
	if got2 := sanitizeAuditErr(wrapped); strings.Contains(got2, "SECRET_QUERY_TOKEN") {
		t.Fatalf("包装后脱敏仍含 query: %q", got2)
	}
	// 无 URL 的错误保持原样（不含 URL 结构即无此类凭据暴露面）。
	plain := errors.New("http status 500")
	if got3 := sanitizeAuditErr(plain); got3 != plain.Error() {
		t.Fatalf("非 URL 错误应原样: got %q, want %q", got3, plain.Error())
	}
	// sanitizeAuditError 对 nil 返回 nil（与 Fail(nil) 语义一致）。
	if sanitizeAuditError(nil) != nil {
		t.Fatal("sanitizeAuditError(nil) 应返回 nil")
	}
}

// TestCloudTask_Audit_ErrSanitized（S2 端到端）：下载连接失败（*url.Error，文本含完整 URL
// + query token）→ 任务 failed 后 task.Audit 的 Row.Err 不得含 token/query，且至少一条带
// 脱敏后的 Err 行。
func TestCloudTask_Audit_ErrSanitized(t *testing.T) {
	t.Parallel()
	// 服务器关闭即释放端口 → 后续请求必然连接失败（*url.Error）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	badURL := srv.URL + "/share/file?token=SECRET_QUERY_TOKEN"
	srv.Close()

	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 4*1024*1024, nil, testLogger())
	mgr, _ := newCloudTestManager(t, dir, sm, &CloudDownloadConfig{
		MaxConcurrent: 3,
		SyncThreshold: 20 * 1024 * 1024,
		AllowPrivate:  true,
		TaskTTL:       time.Hour,
		MaxRetries:    1,
	})

	task, err := mgr.SubmitAndStart("url", badURL, "audit.bin", 1024, t.Context(), "", TaskParams{Save: true})
	if err != nil {
		t.Fatal(err)
	}
	// 同步路径（小文件 + syncCtx）在当前 goroutine 完成：failed + 审计行已收口。轮询防时序抖动。
	testutil.WaitFor(t, 10*time.Second, func() bool {
		snap, ok := mgr.SnapshotTask(task.ID, "")
		return ok && (snap.Status == "failed" || len(snap.Audit) > 0)
	}, func() string { return "等待任务 failed + 审计落行" })
	snap, ok := mgr.SnapshotTask(task.ID, "")
	if !ok {
		t.Fatal("SnapshotTask not found")
	}
	if snap.Status != "failed" {
		t.Fatalf("status = %q, want failed（连接失败应 failTask）", snap.Status)
	}
	hasErrRow := false
	for i := range snap.Audit {
		r := &snap.Audit[i]
		if r.Err != "" {
			hasErrRow = true
		}
		if strings.Contains(r.Err, "SECRET_QUERY_TOKEN") || strings.Contains(r.Err, "?token=") {
			t.Fatalf("审计 Err 应剥离 URL query token（S2），got %q", r.Err)
		}
	}
	if !hasErrRow {
		t.Fatalf("failed 任务应至少含一条带 Err 的审计行（脱敏后），实际 rows: %+v", snap.Audit)
	}
}
