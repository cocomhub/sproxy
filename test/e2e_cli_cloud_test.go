// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// e2e_cli_cloud_test.go 覆盖 sclient `cloud-download` 命令族（真二进制 + 子进程）：
// submit / wait / list / cancel / delete，源站为 127.0.0.1 的 httptest 本地服务
// （startSPROXYImpl 基础配置已含 cloud_download_allow_private: true）。
package sproxy_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
)

// cloudTasks 运行 `cloud-download list --json` 并返回任务列表。
//
// 注意：CLI 在任务为空时走 fm.Println（cmd/sclient/cloud_list.go），JSONFormatter
// 对该调用是 no-op → **空列表在 JSON 模式下输出为空**，此时「空输出」即零任务，
// 不得当作解析失败。
func cloudTasks(t *testing.T, env *cliEnv) []client.CloudTask {
	t.Helper()
	out := env.sclient(t, env.TmpDir, "--json", "cloud-download", "list")
	if strings.TrimSpace(out) == "" {
		return nil
	}
	var l struct {
		Tasks []client.CloudTask `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		t.Fatalf("cloud-download list --json 解析失败: %v\nstdout:\n%s", err, out)
	}
	return l.Tasks
}

// findCloudTask 在 tasks 中按 id 查找，返回 (任务, 是否存在)。
func findCloudTask(tasks []client.CloudTask, id string) (client.CloudTask, bool) {
	for _, tk := range tasks {
		if tk.ID == id {
			return tk, true
		}
	}
	return client.CloudTask{}, false
}

// onlyCloudTaskID 断言当前恰有 1 个云任务并返回其 id。
func onlyCloudTaskID(t *testing.T, env *cliEnv) string {
	t.Helper()
	tasks := cloudTasks(t, env)
	if len(tasks) != 1 {
		t.Fatalf("应恰有 1 个云任务, got %d: %+v", len(tasks), tasks)
	}
	return tasks[0].ID
}

// TestE2E_CLI_CloudDownloadSubmitWaitDelete 覆盖云端下载主链路：
// submit → list 取 id → wait 至完成 → 磁盘落盘内容核对 → delete --yes → 列表与磁盘双清。
func TestE2E_CLI_CloudDownloadSubmitWaitDelete(t *testing.T) {
	env := startCLIEnv(t, "")

	payload := []byte("cloud download cli payload")
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer src.Close()

	// 1) submit（stdout 形如 "  <id>: <status> (<filename>)"）
	out := env.sclient(t, env.TmpDir, "cloud-download", "submit", src.URL+"/payload.bin")
	if !strings.Contains(out, "payload.bin") {
		t.Fatalf("submit 输出应含自动生成文件名 payload.bin, got:\n%s", out)
	}

	// 2) list --json 取 id 并核对文件名
	tid := onlyCloudTaskID(t, env)
	tasks := cloudTasks(t, env)
	if tasks[0].Filename != "payload.bin" {
		t.Fatalf("任务 filename 应为 payload.bin, got %q", tasks[0].Filename)
	}

	// 3) wait 至完成。显式 --timeout：CLI 默认 30m 与 go test 的 -timeout 同量级，
	// 一旦任务卡死会耗尽整轮预算；此处收窄到明显小于测试超时的值，让失败快速暴露。
	env.sclient(t, env.TmpDir, "cloud-download", "wait", tid, "--timeout", "2m")

	// 4) 状态为 completed
	task, ok := findCloudTask(cloudTasks(t, env), tid)
	if !ok {
		t.Fatalf("wait 后任务 %s 应仍在列表中", tid)
	}
	if task.Status != client.TaskStatusCompleted {
		t.Fatalf("wait 后任务状态应为 completed, got %q (error=%q)", task.Status, task.Error)
	}

	// 5) 磁盘副作用：payload.bin 落盘且内容/checksum 一致
	found := findFilesNamed(t, env.StorageRoot, "payload.bin")
	if len(found) != 1 {
		t.Fatalf("云下载完成后磁盘应恰有 1 个 payload.bin, got %d: %v", len(found), found)
	}
	onDisk, err := os.ReadFile(found[0])
	if err != nil {
		t.Fatalf("读取落盘云文件失败: %v", err)
	}
	if sha256hex(onDisk) != sha256hex(payload) {
		t.Fatalf("落盘云文件内容不一致: got %q, want %q", onDisk, payload)
	}

	// 6) delete --yes → 任务与磁盘文件双清
	env.sclient(t, env.TmpDir, "cloud-download", "delete", tid, "--yes")

	if tasks := cloudTasks(t, env); len(tasks) != 0 {
		t.Fatalf("delete 后任务列表应为空, got %+v", tasks)
	}
	if got := findFilesNamed(t, env.StorageRoot, "payload.bin"); len(got) != 0 {
		t.Fatalf("delete 后磁盘不应再有 payload.bin, got %v", got)
	}
}

// TestE2E_CLI_CloudDownloadCancel 覆盖取消语义：
// 源站先吐头部+部分字节（令下载器创建并写入 .partial）再阻塞 → 任务停留
// pending/downloading → 确认 .partial 已落盘 → cancel → 状态 cancelled 且
// **未完成产物（slow.bin.partial / .partial.etag）被清理干净**。
func TestE2E_CLI_CloudDownloadCancel(t *testing.T) {
	env := startCLIEnv(t, "")

	release := make(chan struct{})
	// 源站内容：先写前半段并 Flush，使下载器拿到响应头并 os.Create(.partial) 后阻塞在
	// 读 body；否则若 handler 在写任何字节前就阻塞，客户端拿不到响应头，下载器不会创建
	// .partial —— 后续「清理 .partial」断言就会因产物从未存在而空转。
	slowPayload := bytes.Repeat([]byte("s"), 64*1024)

	// defer 顺序（LIFO）关键：先注册 src.Close，后注册 close(release)，
	// 使退出时先释放阻塞中的 handler，src.Close() 才不会等待挂起请求而死锁。
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(slowPayload)))
		_, _ = w.Write(slowPayload[:len(slowPayload)/2])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write(slowPayload[len(slowPayload)/2:])
	}))
	defer src.Close()
	defer close(release)

	env.sclient(t, env.TmpDir, "cloud-download", "submit", src.URL+"/slow.bin")
	tid := onlyCloudTaskID(t, env)

	// 轮询至任务进入 pending/downloading（-race 下留 3 倍余量：30s 上限）
	deadline := time.Now().Add(30 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		task, ok := findCloudTask(cloudTasks(t, env), tid)
		if !ok {
			t.Fatalf("任务 %s 在轮询期间消失", tid)
		}
		status = task.Status
		if status == client.TaskStatusPending || status == client.TaskStatusDownloading {
			break
		}
		if status == client.TaskStatusCompleted || status == client.TaskStatusFailed {
			t.Fatalf("源站阻塞时任务不应进入终态 %q (error=%q)", status, task.Error)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if status != client.TaskStatusPending && status != client.TaskStatusDownloading {
		t.Fatalf("轮询超时：任务状态仍为 %q，期望 pending/downloading", status)
	}

	// 确认未完成产物已落盘：下载器把未完成内容写到 <dest>.partial
	// （pkg/server/downloader/http_downloader.go:167）。此步是断言非空转的前提——
	// 只有产物确实存在过，「cancel 清理了它」才有意义。
	partialDeadline := time.Now().Add(30 * time.Second)
	var partials []string
	for time.Now().Before(partialDeadline) {
		partials = findFilesPrefixed(t, env.StorageRoot, "slow.bin")
		if len(partials) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(partials) == 0 {
		t.Fatalf("取消前应已存在未完成产物 slow.bin.partial（否则清理断言空转）；"+
			"存储根 %s 下未找到 slow.bin* —— 请核对下载器 partial 落盘路径假设", env.StorageRoot)
	}

	// cancel → 状态 cancelled
	env.sclient(t, env.TmpDir, "cloud-download", "cancel", tid)
	task, ok := findCloudTask(cloudTasks(t, env), tid)
	if !ok {
		t.Fatalf("cancel 后任务 %s 应仍在列表中", tid)
	}
	if task.Status != client.TaskStatusCancelled {
		t.Fatalf("cancel 后状态应为 cancelled, got %q", task.Status)
	}

	// 磁盘副作用：取消丢弃未完成产物——断言 `slow.bin*` 前缀下**无任何残留**，
	// 覆盖 slow.bin.partial 与 slow.bin.partial.etag（只断精确名 "slow.bin" 会恒真空转：
	// 未完成时根本没有 slow.bin 这个文件）。
	// 清理是 best-effort 且可能异步：CancelTask 先尝试 removeTaskDir，若下载 goroutine
	// 仍持有文件（Windows 删除被占用文件会失败）则由 executeDownload 的取消路径兜底
	// （pkg/server/cloud_download.go CancelTask 注释）。故轮询至产物消失（-race 3 倍余量）。
	cleanDeadline := time.Now().Add(30 * time.Second)
	var leftover []string
	for time.Now().Before(cleanDeadline) {
		leftover = findFilesPrefixed(t, env.StorageRoot, "slow.bin")
		if len(leftover) == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(leftover) != 0 {
		t.Fatalf("取消后未完成产物（slow.bin.partial / .partial.etag）应被清理, got %v", leftover)
	}
}

// TestE2E_CLI_CloudDownloadDeleteRequiresYes 覆盖删除确认门禁：
// 无 --yes 时非零退出且任务不被删除（无副作用）。
func TestE2E_CLI_CloudDownloadDeleteRequiresYes(t *testing.T) {
	env := startCLIEnv(t, "")

	payload := []byte("requires yes payload")
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer src.Close()

	env.sclient(t, env.TmpDir, "cloud-download", "submit", src.URL+"/payload.bin")
	tid := onlyCloudTaskID(t, env)

	stdout, stderr, err := env.sclientRun(t, env.TmpDir, "cloud-download", "delete", tid)
	if err == nil {
		t.Fatalf("delete 无 --yes 应非零退出\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if _, ok := findCloudTask(cloudTasks(t, env), tid); !ok {
		t.Fatalf("未确认的 delete 不应删除任务 %s", tid)
	}
}
