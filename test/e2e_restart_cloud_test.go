// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// e2e_restart_cloud_test.go 验证**真实进程被 kill -9 后，服务重启能自动继续云下载**。
//
// 覆盖两条 R14 修复的**服务级**行为（此前只有单测）：
//   - R1 下载中断 ⇒ 重启后按 `.partial` + Range **续传**并完成（源站收到非零 Range 请求）；
//   - R2 **排队中**的任务（`cloud_max_concurrent` 限制）重启后继续，而不是被当成孤儿转 failed。
package sproxy_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// slowRangeSource 是支持 Range 的慢速源站：每 chunk 睡眠 delay，并记录请求的 Range 起点。
type slowRangeSource struct {
	payload []byte
	chunk   int
	delay   time.Duration

	mu       sync.Mutex
	starts   []int64 // 每次请求的 Range 起点（无 Range ⇒ -1）
	requests atomic.Int64
}

func newSlowRangeSource(payload []byte, chunk int, delay time.Duration) *slowRangeSource {
	return &slowRangeSource{payload: payload, chunk: chunk, delay: delay}
}

func (s *slowRangeSource) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	start := int64(-1)
	if rh := r.Header.Get("Range"); strings.HasPrefix(rh, "bytes=") {
		v := strings.TrimSuffix(strings.TrimPrefix(rh, "bytes="), "-")
		if i := strings.IndexByte(v, '-'); i >= 0 {
			v = v[:i]
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			start = n
		}
	}
	s.mu.Lock()
	s.starts = append(s.starts, start)
	s.mu.Unlock()

	off := int64(0)
	if start > 0 {
		off = start
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, int64(len(s.payload))-1, len(s.payload)))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(len(s.payload)))
		w.WriteHeader(http.StatusOK)
	}
	w.Header().Set("Accept-Ranges", "bytes")

	for p := off; p < int64(len(s.payload)); p += int64(s.chunk) {
		end := min(p+int64(s.chunk), int64(len(s.payload)))
		if _, err := w.Write(s.payload[p:end]); err != nil {
			return
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(s.delay)
	}
}

// rangeStarts 返回记录到的 Range 起点（副本）。
func (s *slowRangeSource) rangeStarts() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.starts...)
}

// cloudTaskByID 通过 CLI 读取任务（list --json）。
func (e *restartEnv) cloudTaskByID(t *testing.T, id string) (client.CloudTask, bool) {
	t.Helper()
	out := e.cli(t, "--json", "cloud-download", "list")
	if strings.TrimSpace(out) == "" {
		return client.CloudTask{}, false
	}
	var l struct {
		Tasks []client.CloudTask `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		t.Fatalf("解析任务列表失败: %v\n%s", err, out)
	}
	for _, tk := range l.Tasks {
		if tk.ID == id {
			return tk, true
		}
	}
	return client.CloudTask{}, false
}

// submitCloudTask 提交下载并返回任务 ID。
//
// **不解析 stdout**（CLI 的人类可读输出含进度/日志行，格式不稳定）⇒ 提交后用
// `list --json` 按 **URL 精确匹配**找任务（每个用例的源 URL 唯一）。
func (e *restartEnv) submitCloudTask(t *testing.T, url string) string {
	t.Helper()
	_ = e.cli(t, "cloud-download", "submit", url)
	var id string
	testutil.WaitFor(t, 20*time.Second, func() bool {
		out := e.cli(t, "--json", "cloud-download", "list")
		if strings.TrimSpace(out) == "" {
			return false
		}
		var l struct {
			Tasks []client.CloudTask `json:"tasks"`
		}
		if err := json.Unmarshal([]byte(out), &l); err != nil {
			return false
		}
		for _, tk := range l.Tasks {
			if tk.URL == url {
				id = tk.ID
				return true
			}
		}
		return false
	}, "提交后未在任务列表中看到 URL="+url)
	return id
}

// waitTask 轮询任务直到满足条件（失败时打印最近状态，便于诊断）。
// 用 testutil.WaitFor（避免在测试文件里新增固定 time.Sleep —— 仓库棘轮只减不增）。
func (e *restartEnv) waitTask(t *testing.T, id string, timeout time.Duration, cond func(client.CloudTask) bool) client.CloudTask {
	t.Helper()
	var last client.CloudTask
	defer func() {
		if t.Failed() {
			t.Logf("任务 %s 最近状态: status=%q downloaded=%d/%d err=%q", id, last.Status, last.Downloaded, last.TotalSize, last.Error)
		}
	}()
	testutil.WaitFor(t, timeout, func() bool {
		tk, ok := e.cloudTaskByID(t, id)
		if !ok {
			return false
		}
		last = tk
		return cond(tk)
	}, "任务 "+id+" 未在超时内满足条件")
	return last
}

// TestE2E_Restart_CloudDownloadResumesAfterKill9 下载中断 ⇒ 重启续传（Range 非零起点）并完成。
func TestE2E_Restart_CloudDownloadResumesAfterKill9(t *testing.T) {
	env := newRestartEnv(t, "")
	t.Cleanup(env.Close)

	payload := make([]byte, 2<<20) // 2 MiB
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	src := newSlowRangeSource(payload, 128<<10, 120*time.Millisecond) // ~2s 才完成
	srv := httptest.NewServer(src)
	defer srv.Close()

	env.boot(t)
	id := env.submitCloudTask(t, srv.URL+"/big.bin")

	// 等到确实开始下载（有字节落盘）再 kill —— 确保中断点在下载中段。
	env.waitTask(t, id, 30*time.Second, func(tk client.CloudTask) bool { return tk.Downloaded > 0 })

	env.Kill9(t)
	env.boot(t) // 同一 storage root 重启

	tk := env.waitTask(t, id, 120*time.Second, func(tk client.CloudTask) bool {
		return tk.Status == "completed" || tk.Status == "failed"
	})
	if tk.Status != "completed" {
		env.dumpLogs(t)
		t.Fatalf("重启后应继续并完成，实际 status=%q err=%q downloaded=%d/%d",
			tk.Status, tk.Error, tk.Downloaded, tk.TotalSize)
	}
	if tk.Downloaded != int64(len(payload)) {
		t.Fatalf("完成时 downloaded=%d, want %d", tk.Downloaded, len(payload))
	}

	// 续传证据：源站至少收到一次**非零起点**的 Range 请求。
	starts := src.rangeStarts()
	resumed := false
	for _, s := range starts {
		if s > 0 {
			resumed = true
			break
		}
	}
	if !resumed {
		t.Fatalf("未观察到 Range 续传（源站请求起点=%v，请求数=%d）——重启后退化成整文件重下",
			starts, src.requests.Load())
	}

	// 产物内容正确（经 CLI 下载到本地后比 checksum）。
	outDir := t.TempDir()
	if _, err := os.Stat(outDir); err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	env.cli(t, "cloud-download", "download", id, "--output-dir", outDir)
	local := filepath.Join(outDir, "big.bin")
	got, err := os.ReadFile(local)
	if err != nil {
		t.Fatalf("读取产物 %s: %v", local, err)
	}
	if sha256hex(got) != sha256hex(payload) {
		t.Fatalf("产物 checksum 不匹配（len=%d want=%d）", len(got), len(payload))
	}
}

// TestE2E_Restart_QueuedTasksContinue 排队任务重启后必须继续（R14 C1 的服务级验证）。
func TestE2E_Restart_QueuedTasksContinue(t *testing.T) {
	// 并发 1 ⇒ 其余任务在磁盘上是 pending（正是 C1 的触发条件）。
	env := newRestartEnv(t, "cloud_max_concurrent: 1\n")
	t.Cleanup(env.Close)

	payload := make([]byte, 512<<10)
	for i := range payload {
		payload[i] = byte(i % 97)
	}
	src := newSlowRangeSource(payload, 128<<10, 200*time.Millisecond) // 每个约 0.8s
	srv := httptest.NewServer(src)
	defer srv.Close()

	env.boot(t)
	ids := []string{
		env.submitCloudTask(t, srv.URL+"/a.bin"),
		env.submitCloudTask(t, srv.URL+"/b.bin"),
		env.submitCloudTask(t, srv.URL+"/c.bin"),
	}

	// 第一个开始下载后再 kill ⇒ 另外两个仍是 pending（排队中）。
	env.waitTask(t, ids[0], 30*time.Second, func(tk client.CloudTask) bool { return tk.Downloaded > 0 })

	env.Kill9(t)
	env.boot(t)

	// 三个都必须最终 completed（修复前：排队任务被转 failed，且 1h 后删除）。
	testutil.WaitFor(t, 180*time.Second, func() bool {
		done := 0
		for _, id := range ids {
			tk, ok := env.cloudTaskByID(t, id)
			if !ok {
				continue
			}
			switch tk.Status {
			case "completed":
				done++
			case "failed":
				t.Fatalf("任务 %s 重启后被判 failed（排队任务未继续）: %s", id, tk.Error)
			}
		}
		return done == len(ids)
	}, "排队任务未在超时内全部完成")
}
