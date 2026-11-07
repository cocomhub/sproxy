// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// e2e_restart_quota_test.go 验证「配额不足时**自动等待重试**」与「崩溃后配额回到磁盘真值」。
//
// 这两条是 R14 修复的核心生产保证（C3/C4/NEW-3 的语义），此前只有单测：
//   - 配额打满后发起上传 ⇒ 服务端**排队等待**（不是立刻 507）；期间删除旧文件释放额度 ⇒
//     上传**自动成功**（不需要客户端重试）；
//   - kill -9 后重启 ⇒ `/api/quota` 的 used 必须等于磁盘真实占用（不残留幽灵占用、不重复计）。
package sproxy_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestE2E_QuotaFull_WaitsThenSucceeds 配额打满时上传**排队等待**，删除旧文件后自动成功。
func TestE2E_QuotaFull_WaitsThenSucceeds(t *testing.T) {
	// per-owner 配额 1 MiB：A=700KiB 占满大半，B=700KiB 必然触发等待。
	env := newRestartEnv(t, "owner_quotas:\n  \"*\": 1048576\n")
	t.Cleanup(env.Close)
	env.boot(t)

	a := make([]byte, 700<<10)
	b := make([]byte, 700<<10)
	for i := range a {
		a[i] = byte(i % 251)
	}
	for i := range b {
		b[i] = byte(i % 241)
	}

	if status, body := uploadFile(t, env.baseURL, "a.bin", a, map[string]string{"X-File-Checksum": sha256hex(a)}); status != http.StatusOK {
		t.Fatalf("首个上传应 200, got %d: %s", status, body)
	}

	// 并发发起第二个上传：它应进入**等待**（而不是立刻 507）。
	type result struct {
		status int
		body   []byte
		at     time.Time
	}
	done := make(chan result, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		st, body := uploadFile(t, env.baseURL, "b.bin", b, map[string]string{"X-File-Checksum": sha256hex(b)})
		done <- result{status: st, body: body, at: time.Now()}
	}()
	t.Cleanup(wg.Wait)

	start := time.Now()
	// 等 1.2s：足够确认「没有立刻失败」，同时给等待者入队时间（用 time.After 而非 Sleep，
	// 避免新增固定等待 —— 仓库棘轮只减不增）。
	select {
	case r := <-done:
		t.Fatalf("配额不足时应排队等待而非立刻返回：status=%d body=%s（耗时 %v）",
			r.status, r.body, r.at.Sub(start))
	case <-time.After(1200 * time.Millisecond):
	}

	// 释放额度（删除 A）⇒ 排队中的 B 应自动成功（客户端没有任何重试）。
	if st, body := deleteFile(t, env.baseURL, "a.bin", sha256hex(a)); st != http.StatusOK {
		t.Fatalf("删除文件应 200, got %d: %s", st, body)
	}

	var got result
	select {
	case got = <-done:
	case <-time.After(40 * time.Second):
		t.Fatal("释放额度后上传未在 40s 内完成（等待未唤醒 ⇒ 丢唤醒/未排队）")
	}
	if got.status != http.StatusOK {
		t.Fatalf("释放额度后上传应成功, got %d body=%s", got.status, got.body)
	}

	// 落盘内容正确（真副作用）。
	status, _, content := downloadFile(t, env.baseURL, "b.bin")
	if status != http.StatusOK {
		t.Fatalf("下载 b.bin 失败 status=%d", status)
	}
	if sha256hex(content) != sha256hex(b) {
		t.Fatal("b.bin 内容与上传不一致")
	}
}

// TestE2E_Restart_QuotaMatchesDiskTruth 崩溃重启后配额账本回到磁盘真值（**行为断言**）。
//
// 直接读 /api/quota 的 used 依赖「联邦视图对该卷有权威」，在默认本地卷上可能 `partial`
// ⇒ 断言不稳。改用**行为**验证账本真假：
//   - 配额 2 MiB，写 1.2 MiB 后重启；
//   - 再写 1.2 MiB **必须失败**（若账本归零=泄漏，会误放行 ⇒ 超配额）；
//   - 删除第一个文件后再写 1.2 MiB **必须成功**（若账本重复累计=幽灵占用，会假 507）。
func TestE2E_Restart_QuotaMatchesDiskTruth(t *testing.T) {
	env := newRestartEnv(t, "owner_quotas:\n  \"*\": 2097152\n")
	t.Cleanup(env.Close)
	env.boot(t)

	size := 1200 << 10 // 1.2 MiB
	a := make([]byte, size)
	for i := range a {
		a[i] = byte(i % 233)
	}
	if status, body := uploadFile(t, env.baseURL, "keep.bin", a, map[string]string{"X-File-Checksum": sha256hex(a)}); status != http.StatusOK {
		t.Fatalf("上传应 200, got %d: %s", status, body)
	}

	env.Kill9(t)
	env.boot(t)

	// ① 账本未丢失 ⇒ 第二个 1.2 MiB 必须被拒（2 MiB 配额已用 1.2 MiB）。
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i % 211)
	}
	st, body := uploadFile(t, env.baseURL, "b.bin", b, map[string]string{"X-File-Checksum": sha256hex(b)})
	if st == http.StatusOK {
		t.Fatalf("重启后配额账本疑似归零：第二个超配额上传被放行（body=%s）", body)
	}
	if st != http.StatusInsufficientStorage {
		t.Logf("超配额返回 %d（期望 507；不同配额层可能映射不同，记录以观察）: %s", st, body)
	}

	// ② 释放后应可写（账本未重复累计）。
	if dst, dbody := deleteFile(t, env.baseURL, "keep.bin", sha256hex(a)); dst != http.StatusOK {
		t.Fatalf("删除 keep.bin 失败 %d: %s", dst, dbody)
	}
	if st2, body2 := uploadFile(t, env.baseURL, "c.bin", b, map[string]string{"X-File-Checksum": sha256hex(b)}); st2 != http.StatusOK {
		t.Fatalf("删除后上传应成功（账本不应对同一字节重复计）: %d %s", st2, body2)
	}
}

// TestE2E_GracefulShutdown_ResumesDownload 优雅停机（SIGINT）后重启仍能继续未完成的下载。
func TestE2E_GracefulShutdown_ResumesDownload(t *testing.T) {
	env := newRestartEnv(t, "")
	t.Cleanup(env.Close)

	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i % 199)
	}
	src := newSlowRangeSource(payload, 128<<10, 120*time.Millisecond)
	srv := httptest.NewServer(src)
	defer srv.Close()

	env.boot(t)
	id := env.submitCloudTask(t, srv.URL+"/g.bin")
	env.waitTask(t, id, 30*time.Second, func(tk client.CloudTask) bool { return tk.Downloaded > 0 })

	env.Stop(t) // 优雅停机（非 kill -9）
	env.boot(t)

	testutil.WaitFor(t, 120*time.Second, func() bool {
		tk, ok := env.cloudTaskByID(t, id)
		if !ok {
			return false
		}
		if tk.Status == "failed" {
			t.Fatalf("优雅停机后重启，任务被判失败: %s", tk.Error)
		}
		return tk.Status == "completed"
	}, "优雅停机重启后任务未完成")
}
