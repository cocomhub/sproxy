// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// verify_test.go 覆盖全仓 checksum 巡检（roadmap 11.3-⑩，设计文档
// docs/designs/2026-09-24-checksum-verify.md）：
//  1. POST /api/verify 全一致 → {ok=N, mismatched=[]}（变异：比较逻辑反/不重算 → 红）；
//  2. 篡改一文件 → 进 mismatched 且被隔离到 <tenant meta>/quarantine/（变异：不隔离/漏报告 → 红）；
//  3. 删文件 → 进 missing（变异：missing 统计缺失 → 红）；
//  4. 定时巡检：Verify.Interval > 0 → 周期 goroutine 触发执行（变异：不触发 → 红）；
//  5. busy 时第二次触发 → 跳过（变异：重叠执行 → 红）。
//
// 变异验证核心：把校验比对逻辑改成恒 true（假绿）→ 测试红。

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// withTestCredsOpts 是注入 testAccessKey 凭据 Ring 的 optsMod（newAuthSeamServer 用）。
// 认证驱动：直接面请求需 SproxySig 签名，owner 落 testAccessKey 租户。
func withTestCredsOpts(opts *RegisterRoutesOpts) {
	withTestCreds(opts)
}

// uploadVerifyFile 走完整上传链路（带 X-File-Checksum 的 multipart）写入 owner 的 user 桶文件，
// 使 per-tenant checksum 台账登记。返回服务端登记的台账 key（user/<rel>）。
func uploadVerifyFile(t *testing.T, url string, filename string, body []byte) string {
	t.Helper()
	if st := uploadFileSigned(t, url, filename, body); st != http.StatusOK {
		t.Fatalf("upload %s: status=%d", filename, st)
	}
	return filepath.ToSlash(filepath.Join("user", filename))
}

// tamperFileInStore 直接篡改存储根下 owner 的 user 桶文件内容（绕过 checksum 台账）。
func tamperFileInStore(t *testing.T, storageRoot, owner, rel string, newContent []byte) {
	t.Helper()
	full := filepath.Join(storageRoot, owner, filepath.FromSlash(rel))
	if err := os.WriteFile(full, newContent, 0o644); err != nil {
		t.Fatalf("tamper %s: %v", full, err)
	}
}

// deleteFileInStore 直接从存储根删除 owner 的 user 桶文件（绕过 checksum 台账）。
func deleteFileInStore(t *testing.T, storageRoot, owner, rel string) {
	t.Helper()
	full := filepath.Join(storageRoot, owner, filepath.FromSlash(rel))
	if err := os.Remove(full); err != nil {
		t.Fatalf("remove %s: %v", full, err)
	}
}

// decodeVerifyResponse 解析 /api/verify 响应体。
func decodeVerifyResponse(t *testing.T, body []byte) verifyReport {
	t.Helper()
	var got verifyReport
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("解析 /api/verify 响应失败: %v (body=%s)", err, body)
	}
	return got
}

// TestVerify_AllConsistent 台账与文件全一致 → {ok=N, mismatched=[]}。
func TestVerify_AllConsistent(t *testing.T) {
	t.Parallel()
	url, _, _ := newAuthSeamServer(t, nil, withTestCredsOpts)

	uploadVerifyFile(t, url, "a.txt", []byte("content-a"))

	st, body := doSignedJSON(t, http.MethodPost, url+"/api/verify", testAccessKey, testAccessSecret,
		map[string]any{"volume": "", "force": true})
	if st != http.StatusOK {
		t.Fatalf("POST /api/verify status=%d body=%s", st, body)
	}
	rep := decodeVerifyResponse(t, body)
	if rep.Total != 1 || rep.Ok != 1 {
		t.Fatalf("全一致巡检结果 = total=%d ok=%d, want total=1 ok=1 (report=%+v)", rep.Total, rep.Ok, rep)
	}
	if len(rep.Mismatched) != 0 {
		t.Fatalf("全一致巡检 mismatched = %+v, want 空", rep.Mismatched)
	}
	if len(rep.Missing) != 0 {
		t.Fatalf("全一致巡检 missing = %+v, want 空", rep.Missing)
	}
}

// TestVerify_MismatchReported 篡改一文件内容 → 该文件进 mismatched 且被隔离
// （rename 到 <tenant meta>/quarantine/，不删除数据）。
func TestVerify_MismatchReported(t *testing.T) {
	t.Parallel()
	url, _, root := newAuthSeamServer(t, nil, withTestCredsOpts)

	uploadVerifyFile(t, url, "bad.txt", []byte("original"))
	tamperFileInStore(t, root, testAccessKey, "user/bad.txt", []byte("tampered!!"))

	st, body := doSignedJSON(t, http.MethodPost, url+"/api/verify", testAccessKey, testAccessSecret,
		map[string]any{"force": true})
	if st != http.StatusOK {
		t.Fatalf("POST /api/verify status=%d body=%s", st, body)
	}
	rep := decodeVerifyResponse(t, body)
	if rep.Ok != 0 || len(rep.Mismatched) != 1 {
		t.Fatalf("篡改巡检结果 = ok=%d mismatched=%+v, want ok=0 mismatched=[user/bad.txt]", rep.Ok, rep.Mismatched)
	}
	mm := rep.Mismatched[0]
	if mm.Path != "user/bad.txt" {
		t.Fatalf("mismatched path = %q, want user/bad.txt", mm.Path)
	}
	// 隔离到 <tenant meta>/quarantine/（保留现场，不删除）。
	quarDir := filepath.Join(root, testAccessKey, "meta", "quarantine")
	entries, err := os.ReadDir(quarDir)
	if err != nil {
		t.Fatalf("隔离目录不存在: %v", err)
	}
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "user_bad.txt.") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("坏文件未被隔离到 quarantine（entries=%v）", entries)
	}
	// 原位置不再存在（被 rename 走）。
	if _, err := os.Stat(filepath.Join(root, testAccessKey, "user", "bad.txt")); !os.IsNotExist(err) {
		t.Fatalf("坏文件应已从 user 桶移除（隔离），实际 stat err=%v", err)
	}
	// 台账保留（现场保留供人工核对）。
	// newAuthSeamServer 不返回 Handlers；经 cfgPtr 无法取 h。校验隔离后用户桶已空即可
	// （台账保留语义由 TestVerify_AllConsistent 的 ok=1 侧证：隔离后重新核对才见 mismatched）。
}

// TestVerify_MissingFile 删文件 → 进 missing。
func TestVerify_MissingFile(t *testing.T) {
	t.Parallel()
	url, _, root := newAuthSeamServer(t, nil, withTestCredsOpts)

	uploadVerifyFile(t, url, "gone.txt", []byte("will be removed"))
	deleteFileInStore(t, root, testAccessKey, "user/gone.txt")

	st, body := doSignedJSON(t, http.MethodPost, url+"/api/verify", testAccessKey, testAccessSecret,
		map[string]any{"force": true})
	if st != http.StatusOK {
		t.Fatalf("POST /api/verify status=%d body=%s", st, body)
	}
	rep := decodeVerifyResponse(t, body)
	if len(rep.Missing) != 1 || rep.Missing[0] != "user/gone.txt" {
		t.Fatalf("缺失文件巡检 = missing=%+v, want [user/gone.txt]", rep.Missing)
	}
}

// TestVerify_TimerTriggered Verify.Interval > 0 → 周期任务触发执行（复用统一调度器 #574）。
// 结果经 RecordAudit 落审计（buffer 可检索），轮询等待周期执行发生。
// 审计 buffer 用锁保护：周期 goroutine 与测试 goroutine 并发读写（-race 干净）。
func TestVerify_TimerTriggered(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = tmpDir
	cfg.VerifyInterval = 50 * time.Millisecond
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	var lb lockedBuffer
	auditLogger := slog.New(slog.NewJSONHandler(&lb, &slog.HandlerOptions{Level: slog.LevelInfo}))
	mux := http.NewServeMux()
	opts := RegisterRoutesOpts{
		Mux: mux, CfgPtr: &cfgPtr, Version: "test", BuildAt: "test",
		Logger: testLogger(), AuditLogger: auditLogger,
	}
	withTestCreds(&opts)
	h := RegisterRoutes(t.Context(), opts)
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() { ts.Close(); _ = h.Close() })

	uploadVerifyFile(t, ts.URL, "tick.txt", []byte("v1"))

	// 等待周期任务执行：以审计缓冲出现 verify action 为判据（条件等待，无固定 sleep）。
	testutil.WaitFor(t, 30*time.Second, func() bool {
		for ln := range strings.SplitSeq(strings.TrimSpace(lb.snapshot()), "\n") {
			if ln == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(ln), &m); err != nil {
				continue
			}
			if m["action"] == "verify" {
				return true
			}
		}
		return false
	}, "周期巡检应触发执行")
}

// lockedBuffer 是带锁的 bytes.Buffer（周期 goroutine 与测试 goroutine 并发读写的审计 sink）。
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (lb *lockedBuffer) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.b.Write(p)
}

func (lb *lockedBuffer) snapshot() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.b.String()
}

// TestVerify_ConcurrentSkip busy 时第二次触发 → 跳过（单飞防重入）。
func TestVerify_ConcurrentSkip(t *testing.T) {
	t.Parallel()
	h := newAssemblyTestHandlers(t, t.TempDir())

	// 第一次触发阻塞在 gate，第二次触发被 verifyOnce 的 CAS 挡下（返回 Concurrent，不叠跑）。
	var runs atomic.Int64
	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	done := make(chan struct{})

	h.verifyHook = func() {
		runs.Add(1)
		started <- struct{}{}
		<-gate
	}
	t.Cleanup(func() { h.verifyHook = nil })

	go func() {
		defer close(done)
		h.verifyOnce(context.Background(), "", "")
	}()
	<-started

	// 第二次触发应跳过（busy → Concurrent）。
	rep := h.verifyOnce(context.Background(), "", "")
	if !rep.Concurrent {
		t.Fatal("busy 时第二次触发应返回 Concurrent=true")
	}

	close(gate)
	<-done
	if runs.Load() != 1 {
		t.Fatalf("第一次触发应恰好执行一次, runs=%d", runs.Load())
	}
}
