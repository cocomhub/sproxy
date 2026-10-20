// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// e2e_restart_matrix_test.go 是**状态 × 恢复**的确定性矩阵（真实进程，但崩溃点由**落盘状态**构造）：
//
// 与 `e2e_restart_cloud_test.go`（靠 kill -9 时机覆盖「阶段」）互补：
//   - 那边验证「在某个阶段被打断」的**时序**（下载中、排队中）；
//   - 这里把每种**持久化状态**直接写到磁盘，再启动真实服务 ⇒ 断言恢复决策（不依赖时机，稳定）。
//
// 覆盖的状态：
//
//	pending(未提交) / pending(已提交=排队) / downloading(有 partial) / downloading(空 partial)
//	/ completed(+DownloadDone) / completed(带 Transfer 未完成) / completed+damaged
//	/ failed / cancelled / 损坏 JSON / 半写 .tmp 残留
package sproxy_test

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
)

// seededTask 是**最小可恢复**的任务 JSON（与 CloudTask 的 json 字段对齐）。
type seededTask struct {
	ID             string    `json:"id"`
	Owner          string    `json:"owner"`
	URL            string    `json:"url"`
	Method         string    `json:"method"`
	Filename       string    `json:"filename"`
	Status         string    `json:"status"`
	TotalSize      int64     `json:"total_size"`
	Downloaded     int64     `json:"downloaded"`
	Checksum       string    `json:"checksum"`
	Error          string    `json:"error"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	Submitted      bool      `json:"submitted,omitempty"`
	DownloadDone   bool      `json:"download_done,omitempty"`
	DownloadedSize int64     `json:"downloaded_size,omitempty"`
	IntegrityStat  string    `json:"integrity_status,omitempty"`
	Transfer       *struct {
		Volume string `json:"volume"`
		Path   string `json:"path,omitempty"`
	} `json:"transfer,omitempty"`
	Save          *bool `json:"save,omitempty"`
	DownloadLocal bool  `json:"download_local,omitempty"`
}

// ownerDir 返回 owner 在存储根下的目录名（owner = AK）。
func ownerDir(env *restartEnv) string { return e2eTestAK }

// cloudStatePath 返回任务状态 JSON 的绝对路径。
func cloudStatePath(env *restartEnv, id string) string {
	return filepath.Join(env.storageRoot, ownerDir(env), "meta", "cloud", id+".json")
}

// taskDirPath 返回任务产物目录。
func taskDirPath(env *restartEnv, id string) string {
	return filepath.Join(env.storageRoot, ownerDir(env), "cloud", id)
}

// seedTask 在**服务停止时**把任务状态写到磁盘（模拟「上一次进程遗留的持久化状态」）。
func seedTask(t *testing.T, env *restartEnv, task seededTask) {
	t.Helper()
	now := time.Now()
	if task.Method == "" {
		task.Method = "url"
	}
	if task.Owner == "" {
		task.Owner = e2eTestAK
	}
	if task.Save == nil {
		// 与新建任务一致（服务端 `save=true` 保留 cloud 桶副本）；否则任务完成时
		// cleanupCloudIfNotNeeded 会按 save=false 语义把产物删掉（日志矩阵会误判「产物丢失」）。
		save := true
		task.Save = &save
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = now
	}
	if task.UpdatedAt.IsZero() {
		task.UpdatedAt = now
	}
	if task.ExpiresAt.IsZero() {
		task.ExpiresAt = now.Add(24 * time.Hour)
	}
	p := cloudStatePath(env, task.ID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	b, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("marshal seeded task: %v", err)
	}
	if werr := os.WriteFile(p, b, 0o644); werr != nil {
		t.Fatalf("write seeded task: %v", werr)
	}
}

// seedArtifact 写任务产物（complete=true 写正式产物，否则写 `.partial`）。
func seedArtifact(t *testing.T, env *restartEnv, id, filename string, content []byte, complete bool) {
	t.Helper()
	dir := taskDirPath(env, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir task dir: %v", err)
	}
	name := filename
	if !complete {
		name += ".partial"
	}
	if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
}

// newMatrixSource 生成 payload 与慢速源站。
func newMatrixSource(t *testing.T, size int, delay time.Duration) ([]byte, *slowRangeSource, string, func()) {
	t.Helper()
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	src := newSlowRangeSource(payload, 128<<10, delay)
	srv := httptest.NewServer(src)
	return payload, src, srv.URL, srv.Close
}

// TestE2E_RecoveryMatrix_States 遍历「持久化状态 × 恢复决策」矩阵。
//
// 每个子用例都独立存储根（真实进程），因为恢复决策依赖整份磁盘状态。
func TestE2E_RecoveryMatrix_States(t *testing.T) {
	newEnv := func(t *testing.T) *restartEnv {
		t.Helper()
		env := newRestartEnv(t, "")
		t.Cleanup(env.Close)
		return env
	}

	t.Run("pending_unsubmitted_becomes_failed", func(t *testing.T) {
		env := newEnv(t)
		payload, src, srcBase, closeSrv := newMatrixSource(t, 64<<10, 0)
		defer closeSrv()
		seedTask(t, env, seededTask{
			ID: "t-unsubmitted", URL: srcBase + "/f.bin", Filename: "u.bin",
			Status: "pending", TotalSize: int64(len(payload)),
		})
		env.boot(t)

		tk := env.waitTask(t, "t-unsubmitted", 20*time.Second, func(tk client.CloudTask) bool { return tk.Status != "pending" })
		if tk.Status != "failed" {
			t.Fatalf("未提交的 pending 应转为 failed（孤儿），got %q", tk.Status)
		}
		if !strings.Contains(tk.Error, "interrupted before start") {
			t.Fatalf("错误文案应标明孤儿语义, got %q", tk.Error)
		}
		if n := src.requests.Load(); n != 0 {
			t.Fatalf("孤儿任务不得触发下载，got %d 次请求", n)
		}
	})

	t.Run("pending_submitted_restarts_and_completes", func(t *testing.T) {
		env := newEnv(t)
		payload, _, srcBase, closeSrv := newMatrixSource(t, 256<<10, 30*time.Millisecond)
		defer closeSrv()
		seedTask(t, env, seededTask{
			ID: "t-queued", URL: srcBase + "/f.bin", Filename: "q.bin",
			Status: "pending", TotalSize: int64(len(payload)), Submitted: true,
		})
		env.boot(t)

		tk := env.waitTask(t, "t-queued", 60*time.Second, func(tk client.CloudTask) bool {
			return tk.Status == "completed" || tk.Status == "failed"
		})
		if tk.Status != "completed" {
			env.dumpLogs(t)
			t.Fatalf("已提交的排队任务应继续并完成，got %q err=%q", tk.Status, tk.Error)
		}
	})

	t.Run("downloading_with_partial_resumes", func(t *testing.T) {
		env := newEnv(t)
		payload, src, srcBase, closeSrv := newMatrixSource(t, 512<<10, 20*time.Millisecond)
		defer closeSrv()
		half := len(payload) / 2
		seedTask(t, env, seededTask{
			ID: "t-partial", URL: srcBase + "/f.bin", Filename: "p.bin",
			Status: "downloading", TotalSize: int64(len(payload)), Downloaded: int64(half), Submitted: true,
		})
		seedArtifact(t, env, "t-partial", "p.bin", payload[:half], false)
		env.boot(t)

		tk := env.waitTask(t, "t-partial", 60*time.Second, func(tk client.CloudTask) bool {
			return tk.Status == "completed" || tk.Status == "failed"
		})
		if tk.Status != "completed" {
			env.dumpLogs(t)
			t.Fatalf("有 .partial 的 downloading 应续传完成，got %q err=%q", tk.Status, tk.Error)
		}
		resumed := false
		for _, s := range src.rangeStarts() {
			if s > 0 {
				resumed = true
			}
		}
		if !resumed {
			t.Fatalf("未观察到非零 Range（应续传），起点=%v", src.rangeStarts())
		}
		got, err := os.ReadFile(filepath.Join(taskDirPath(env, "t-partial"), "p.bin"))
		if err != nil {
			t.Fatalf("读产物: %v", err)
		}
		if sha256hex(got) != sha256hex(payload) {
			t.Fatal("续传产物内容不正确")
		}
	})

	t.Run("downloading_empty_partial_full_redownload", func(t *testing.T) {
		env := newEnv(t)
		payload, _, srcBase, closeSrv := newMatrixSource(t, 128<<10, 0)
		defer closeSrv()
		seedTask(t, env, seededTask{
			ID: "t-empty-partial", URL: srcBase + "/f.bin", Filename: "e.bin",
			Status: "downloading", TotalSize: int64(len(payload)), Submitted: true,
		})
		seedArtifact(t, env, "t-empty-partial", "e.bin", nil, false) // 空 .partial
		env.boot(t)

		tk := env.waitTask(t, "t-empty-partial", 60*time.Second, func(tk client.CloudTask) bool {
			return tk.Status == "completed" || tk.Status == "failed"
		})
		if tk.Status != "completed" {
			t.Fatalf("空 .partial 应整文件下载并完成，got %q err=%q", tk.Status, tk.Error)
		}
	})

	t.Run("completed_keeps_artifact_no_redownload", func(t *testing.T) {
		env := newEnv(t)
		payload, src, srcBase, closeSrv := newMatrixSource(t, 64<<10, 0)
		defer closeSrv()
		seedTask(t, env, seededTask{
			ID: "t-done", URL: srcBase + "/f.bin", Filename: "d.bin",
			Status: "completed", TotalSize: int64(len(payload)), Downloaded: int64(len(payload)),
			Checksum: sha256hex(payload), Submitted: true, DownloadDone: true, DownloadedSize: int64(len(payload)),
		})
		seedArtifact(t, env, "t-done", "d.bin", payload, true)
		env.boot(t)

		// 等一轮恢复（RecoverAll 在构造期执行）后断言：仍 completed 且**没有**任何下载请求。
		<-time.After(1500 * time.Millisecond)
		tk, ok := env.cloudTaskByID(t, "t-done")
		if !ok || tk.Status != "completed" {
			t.Fatalf("completed 应保持 completed, ok=%v status=%q", ok, tk.Status)
		}
		if n := src.requests.Load(); n != 0 {
			t.Fatalf("completed 任务不得重新下载，got %d 次请求", n)
		}
	})

	t.Run("failed_stays_failed", func(t *testing.T) {
		env := newEnv(t)
		_, src, srcBase, closeSrv := newMatrixSource(t, 1024, 0)
		defer closeSrv()
		seedTask(t, env, seededTask{
			ID: "t-failed", URL: srcBase + "/f.bin", Filename: "f.bin",
			Status: "failed", TotalSize: 1024, Error: "boom", Submitted: true,
		})
		env.boot(t)
		tk, ok := env.cloudTaskByID(t, "t-failed")
		if !ok || tk.Status != "failed" {
			t.Fatalf("failed 不得被重启复活, ok=%v status=%q", ok, tk.Status)
		}
		if n := src.requests.Load(); n != 0 {
			t.Fatalf("failed 任务不得触发下载，got %d", n)
		}
	})

	t.Run("cancelled_stays_cancelled", func(t *testing.T) {
		env := newEnv(t)
		_, src, srcBase, closeSrv := newMatrixSource(t, 1024, 0)
		defer closeSrv()
		seedTask(t, env, seededTask{
			ID: "t-cancelled", URL: srcBase + "/f.bin", Filename: "c.bin",
			Status: "cancelled", TotalSize: 1024, Submitted: true,
		})
		env.boot(t)
		tk, ok := env.cloudTaskByID(t, "t-cancelled")
		if !ok || tk.Status != "cancelled" {
			t.Fatalf("cancelled 不得被重启复活, ok=%v status=%q", ok, tk.Status)
		}
		if n := src.requests.Load(); n != 0 {
			t.Fatalf("cancelled 任务不得触发下载，got %d", n)
		}
	})

	t.Run("completed_damaged_not_auto_redownloaded", func(t *testing.T) {
		env := newEnv(t)
		payload, src, srcBase, closeSrv := newMatrixSource(t, 64<<10, 0)
		defer closeSrv()
		seedTask(t, env, seededTask{
			ID: "t-damaged", URL: srcBase + "/f.bin", Filename: "dm.bin",
			Status: "completed", TotalSize: int64(len(payload)), Downloaded: int64(len(payload)),
			Submitted: true, DownloadDone: true, DownloadedSize: int64(len(payload)),
			IntegrityStat: "damaged",
		})
		seedArtifact(t, env, "t-damaged", "dm.bin", payload, true)
		env.boot(t)

		<-time.After(1500 * time.Millisecond)
		tk, ok := env.cloudTaskByID(t, "t-damaged")
		if !ok || tk.Status != "completed" || tk.IntegrityStatus != "damaged" {
			t.Fatalf("completed+damaged 应保持原状态（需显式 resume 才修复）, ok=%v status=%q integrity=%q",
				ok, tk.Status, tk.IntegrityStatus)
		}
		if n := src.requests.Load(); n != 0 {
			t.Fatalf("damaged 不得被自动重下（必须显式 resume），got %d 次请求", n)
		}
	})

	t.Run("corrupt_json_skipped_healthy_task_recovers", func(t *testing.T) {
		env := newEnv(t)
		payload, _, srcBase, closeSrv := newMatrixSource(t, 64<<10, 0)
		defer closeSrv()
		// 健康任务（downloading，无 partial ⇒ 整文件下载）
		seedTask(t, env, seededTask{
			ID: "t-healthy", URL: srcBase + "/f.bin", Filename: "h.bin",
			Status: "downloading", TotalSize: int64(len(payload)), Submitted: true,
		})
		// 损坏 JSON
		p := cloudStatePath(env, "t-corrupt")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte("{not-json"), 0o644); err != nil {
			t.Fatalf("write corrupt: %v", err)
		}
		env.boot(t)

		tk := env.waitTask(t, "t-healthy", 60*time.Second, func(tk client.CloudTask) bool {
			return tk.Status == "completed" || tk.Status == "failed"
		})
		if tk.Status != "completed" {
			env.dumpLogs(t)
			t.Fatalf("坏 JSON 不应影响其它任务恢复，got %q err=%q", tk.Status, tk.Error)
		}
		if !strings.Contains(env.Logs(), "unreadable on recovery") {
			t.Fatalf("不可读任务必须可观测（日志含 unreadable on recovery），实际日志:\n%s", env.Logs())
		}
		if _, ok := env.cloudTaskByID(t, "t-corrupt"); ok {
			t.Fatal("损坏 JSON 不应被当成任务恢复出来")
		}
	})

	t.Run("half_written_tmp_is_ignored", func(t *testing.T) {
		env := newEnv(t)
		payload, _, srcBase, closeSrv := newMatrixSource(t, 64<<10, 0)
		defer closeSrv()
		seedTask(t, env, seededTask{
			ID: "t-tmp", URL: srcBase + "/f.bin", Filename: "tm.bin",
			Status: "downloading", TotalSize: int64(len(payload)), Submitted: true,
		})
		// 半写的原子写临时文件（`.tmp.<hex>`）——恢复必须忽略（按 .json 后缀过滤）。
		dir := filepath.Dir(cloudStatePath(env, "t-tmp"))
		if err := os.WriteFile(filepath.Join(dir, "t-tmp.json.tmp.deadbeef"), []byte("{half"), 0o644); err != nil {
			t.Fatalf("write tmp: %v", err)
		}
		env.boot(t)

		tk := env.waitTask(t, "t-tmp", 60*time.Second, func(tk client.CloudTask) bool {
			return tk.Status == "completed" || tk.Status == "failed"
		})
		if tk.Status != "completed" {
			env.dumpLogs(t)
			t.Fatalf("半写 .tmp 残留不应影响恢复，got %q err=%q", tk.Status, tk.Error)
		}
	})
}

// TestE2E_RecoveryMatrix_DownloadDoneBeforeFinalizeReprisesWithoutRedownload
// 覆盖「**下载已完成、终态提交前**」这一阶段：磁盘仍是 `downloading` 但 `download_done=true`
// 且产物完整 ⇒ 重启必须**复用产物**（源站零请求）并直接收口为 completed。
//
// 为什么磁盘状态是 downloading：`finalizeCompleted` 在转存**之后**，所以这个窗口里 status
// 从未变成 completed（这也解释了为什么 C2 必须靠 `download_done` 而不是 status 判定）。
func TestE2E_RecoveryMatrix_DownloadDoneBeforeFinalizeReprisesWithoutRedownload(t *testing.T) {
	env := newRestartEnv(t, "")
	t.Cleanup(env.Close)

	payload := make([]byte, 256<<10)
	for i := range payload {
		payload[i] = byte(i % 197)
	}
	src := newSlowRangeSource(payload, 128<<10, 0)
	srv := httptest.NewServer(src)
	defer srv.Close()

	seedTask(t, env, seededTask{
		ID: "t-done-window", URL: srv.URL + "/x.bin", Filename: "x.bin",
		Status: "downloading", TotalSize: int64(len(payload)), Downloaded: int64(len(payload)),
		Checksum: sha256hex(payload), Submitted: true, DownloadDone: true, DownloadedSize: int64(len(payload)),
	})
	seedArtifact(t, env, "t-done-window", "x.bin", payload, true)

	env.boot(t)

	tk := env.waitTask(t, "t-done-window", 60*time.Second, func(tk client.CloudTask) bool {
		return tk.Status == "completed" || tk.Status == "failed"
	})
	if tk.Status != "completed" {
		env.dumpLogs(t)
		t.Fatalf("产物完整的任务应直接收口为 completed，got %q err=%q", tk.Status, tk.Error)
	}
	if n := src.requests.Load(); n != 0 {
		t.Fatalf("产物已完整时不得重下，got %d 次请求", n)
	}
	got, err := os.ReadFile(filepath.Join(taskDirPath(env, "t-done-window"), "x.bin"))
	if err != nil {
		t.Fatalf("读产物: %v", err)
	}
	if sha256hex(got) != sha256hex(payload) {
		t.Fatal("复用的产物内容被改动")
	}
}

// ---- 小工具 ----
