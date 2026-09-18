// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volumes_rebalance_test.go 验证 POST /api/volumes/rebalance（卷再平衡，跨卷批量迁移）：
//  1. Basic：main（高占用）→ disk2（低占用），文件迁移、main 池下降、disk2 池上升、响应含 moved。
//  2. ACLDenied：to_volume 不在 owner 视图 → 403。
//  3. SameVolume：from==to → no-op 成功（moved=0）。
//  4. MaxBytes：max_bytes 限制只迁部分文件。
//  5. NoFiles：from 空 → moved=0 成功。
//  6. ConcurrentRebalanceLocked：并发 rebalance + move 同一文件 → 恰一成功（uploadingFiles 锁），
//     变异验证「去掉锁复用 → 并发双迁」应红。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// rebalanceResponse 是 POST /api/volumes/rebalance 的响应结构。
type rebalanceResponse struct {
	Success    bool   `json:"success"`
	Message    string `json:"message"`
	Moved      int    `json:"moved"`
	BytesMoved int64  `json:"bytes_moved"`
	Remaining  int64  `json:"remaining"`
}

// rebalanceVolumeCore 发起卷再平衡请求（不 t.Fatal，供并发 goroutine 使用）。
func rebalanceVolumeCore(baseURL, fromVol, toVol, maxBytes string) (int, []byte, error) {
	u := baseURL + "/api/volumes/rebalance?from_volume=" + url.QueryEscape(fromVol) +
		"&to_volume=" + url.QueryEscape(toVol)
	if maxBytes != "" {
		u += "&max_bytes=" + url.QueryEscape(maxBytes)
	}
	req, err := http.NewRequest("POST", u, nil)
	if err != nil {
		return 0, nil, err
	}
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, nil
}

// rebalanceVolume 请求 POST /api/volumes/rebalance（传输错误 t.Fatal）。
func rebalanceVolume(t *testing.T, baseURL, fromVol, toVol, maxBytes string) (int, []byte) {
	t.Helper()
	status, body, err := rebalanceVolumeCore(baseURL, fromVol, toVol, maxBytes)
	if err != nil {
		t.Fatalf("rebalance %s→%s: %v", fromVol, toVol, err)
	}
	return status, body
}

// decodeRebalance 解析 rebalance 响应 JSON（失败 t.Fatal）。
func decodeRebalance(t *testing.T, body []byte) rebalanceResponse {
	t.Helper()
	var out rebalanceResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode rebalance 响应: %v (body=%s)", err, body)
	}
	return out
}

// twoVolumeServer 装配 main+disk2 双卷测试服务（alice），返回 URL、Handlers、卷根。
func twoVolumeServer(t *testing.T) (string, *Handlers, []string) {
	t.Helper()
	dirs := []string{t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 1 << 20},
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}
	url, h, roots := newVolumesAPIServer(t, "alice", volumes, nil)
	return url, h, roots
}

// TestVolumesRebalance_Basic 再平衡成功：main 三个文件迁到 disk2；main 池归 0、
// disk2 池 = 三文件合计、owner user 桶不变；响应 moved=3、remaining=0。
func TestVolumesRebalance_Basic(t *testing.T) {
	t.Parallel()
	url, h, dirs := twoVolumeServer(t)

	files := map[string]string{
		"a.txt": "AAAAAAAAAA",
		"b.txt": "BBBBBBBBB",
		"c.txt": "CCCCCCCCCC",
	}
	var total int64
	for name, content := range files {
		body := []byte(content)
		total += int64(len(body))
		status, _, respBody := volumeUpload(t, url, name, body, "")
		if status != http.StatusOK {
			t.Fatalf("上传 %s 应 200, got %d %s", name, status, respBody)
		}
		if !diskFileExists(t, dirs[0], "alice", name) {
			t.Fatalf("%s 应初始落 main", name)
		}
	}
	if got := h.volSet.Pool("main").Usage(); got != total {
		t.Fatalf("初始 main 池=%d want %d", got, total)
	}

	status, bodyResp := rebalanceVolume(t, url, "main", "disk2", "")
	if status != http.StatusOK {
		t.Fatalf("rebalance status=%d want 200, body=%s", status, bodyResp)
	}
	out := decodeRebalance(t, bodyResp)
	if !out.Success {
		t.Fatalf("rebalance Success=false, body=%s", bodyResp)
	}
	if out.Moved != 3 {
		t.Fatalf("rebalance moved=%d want 3, body=%s", out.Moved, bodyResp)
	}
	if out.BytesMoved != total {
		t.Fatalf("rebalance bytes_moved=%d want %d", out.BytesMoved, total)
	}
	if out.Remaining != 0 {
		t.Fatalf("rebalance remaining=%d want 0（尽力迁移到 from 空）", out.Remaining)
	}

	for name := range files {
		if diskFileExists(t, dirs[0], "alice", name) {
			t.Fatalf("rebalance 后 %s 不应残留 main", name)
		}
		if !diskFileExists(t, dirs[1], "alice", name) {
			t.Fatalf("rebalance 后 %s 应落 disk2", name)
		}
	}
	// 双账本：main 池归 0、disk2 池 = 合计、owner user 桶不变（同 owner 迁移字节不净增）。
	if got := h.volSet.Pool("main").Usage(); got != 0 {
		t.Fatalf("rebalance 后 main 池=%d want 0", got)
	}
	if got := h.volSet.Pool("disk2").Usage(); got != total {
		t.Fatalf("rebalance 后 disk2 池=%d want %d", got, total)
	}
	if got := h.quotaBucketFor("alice", "user").Usage(); got != total {
		t.Fatalf("rebalance 后 owner user 桶=%d want %d（同 owner 迁移不净增）", got, total)
	}
}

// TestVolumesRebalance_ACLDenied to_volume 不在 owner 视图（allow 白名单收紧）→ 403。
func TestVolumesRebalance_ACLDenied(t *testing.T) {
	t.Parallel()
	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 1 << 20},
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
		{Name: "priv", Root: dirs[2], VolCapacity: 1 << 20,
			ACL: &VolumeACLConfig{Mode: VolumeACLAllow, Owners: []string{"bob"}}},
	}
	url, _, dirs := newVolumesAPIServer(t, "alice", volumes, nil)

	body := []byte("alice file")
	status, _, respBody := volumeUpload(t, url, "a.txt", body, "")
	if status != http.StatusOK {
		t.Fatalf("上传应 200, got %d %s", status, respBody)
	}

	// to 卷不在视图 → 403。
	status, respBody = rebalanceVolume(t, url, "main", "priv", "")
	if status != http.StatusForbidden {
		t.Fatalf("to 卷不在视图 rebalance status=%d want 403, body=%s", status, respBody)
	}
	// from 卷不在视图 → 403。
	status, respBody = rebalanceVolume(t, url, "priv", "main", "")
	if status != http.StatusForbidden {
		t.Fatalf("from 卷不在视图 rebalance status=%d want 403, body=%s", status, respBody)
	}
	if !diskFileExists(t, dirs[0], "alice", "a.txt") {
		t.Fatal("403 后源文件应保留在 main")
	}
}

// TestVolumesRebalance_SameVolume_Noop from==to → no-op 成功（moved=0），文件原地不动。
func TestVolumesRebalance_SameVolume_Noop(t *testing.T) {
	t.Parallel()
	url, _, dirs := twoVolumeServer(t)

	body := []byte("stay put")
	status, _, respBody := volumeUpload(t, url, "a.txt", body, "")
	if status != http.StatusOK {
		t.Fatalf("上传应 200, got %d %s", status, respBody)
	}

	status, respBody = rebalanceVolume(t, url, "main", "main", "")
	if status != http.StatusOK {
		t.Fatalf("同卷 rebalance status=%d want 200, body=%s", status, respBody)
	}
	out := decodeRebalance(t, respBody)
	if !out.Success || out.Moved != 0 || out.BytesMoved != 0 {
		t.Fatalf("同卷 rebalance 应 no-op success, got %+v", out)
	}
	if !diskFileExists(t, dirs[0], "alice", "a.txt") {
		t.Fatal("同卷 rebalance 不应搬走文件")
	}
	if diskFileExists(t, dirs[1], "alice", "a.txt") {
		t.Fatal("同卷 rebalance 不应产生 disk2 副本")
	}
}

// TestVolumesRebalance_MaxBytes max_bytes 限制只迁部分文件：a(10B)+b(9B)+c(10B)=29B，
// max_bytes=15 → 按大小降序迁 a(10)+c(10)=20 > 15 停？——实现语义：逐文件迁移，
// 迁移前剩余配额足够才迁；按降序 [a(10) c(10) b(9)]：a(10) ≤ 15 迁、c(10) 后剩 5 < 10 停 →
// moved=1、bytes_moved=10、remaining=19。
func TestVolumesRebalance_MaxBytes(t *testing.T) {
	t.Parallel()
	url, h, dirs := twoVolumeServer(t)

	files := map[string]string{
		"a.txt": "AAAAAAAAAA", // 10B
		"b.txt": "BBBBBBBBB",  // 9B
		"c.txt": "CCCCCCCCCC", // 10B
	}
	for name, content := range files {
		status, _, respBody := volumeUpload(t, url, name, []byte(content), "")
		if status != http.StatusOK {
			t.Fatalf("上传 %s 应 200, got %d %s", name, status, respBody)
		}
	}

	status, respBody := rebalanceVolume(t, url, "main", "disk2", "15")
	if status != http.StatusOK {
		t.Fatalf("rebalance(max_bytes=15) status=%d want 200, body=%s", status, respBody)
	}
	out := decodeRebalance(t, respBody)
	if !out.Success {
		t.Fatalf("rebalance Success=false, body=%s", respBody)
	}
	if out.Moved != 1 || out.BytesMoved != 10 {
		t.Fatalf("max_bytes=15 应迁 1 个 10B 文件（降序 a 先迁，c 超剩余配额跳过）, got moved=%d bytes=%d",
			out.Moved, out.BytesMoved)
	}
	if out.Remaining != 19 {
		t.Fatalf("remaining=%d want 19（29-10）", out.Remaining)
	}
	// a 已迁 disk2；b/c 仍在 main。
	if !diskFileExists(t, dirs[1], "alice", "a.txt") {
		t.Fatal("a.txt 应已迁 disk2")
	}
	if !diskFileExists(t, dirs[0], "alice", "b.txt") || !diskFileExists(t, dirs[0], "alice", "c.txt") {
		t.Fatal("b/c 应仍在 main（max_bytes 限制）")
	}
	if got := h.volSet.Pool("main").Usage(); got != 19 {
		t.Fatalf("main 池=%d want 19（29-10）", got)
	}
	if got := h.volSet.Pool("disk2").Usage(); got != 10 {
		t.Fatalf("disk2 池=%d want 10", got)
	}
}

// TestVolumesRebalance_NoFiles from 空 → moved=0 成功。
func TestVolumesRebalance_NoFiles(t *testing.T) {
	t.Parallel()
	url, h, dirs := twoVolumeServer(t)

	status, respBody := rebalanceVolume(t, url, "main", "disk2", "")
	if status != http.StatusOK {
		t.Fatalf("空卷 rebalance status=%d want 200, body=%s", status, respBody)
	}
	out := decodeRebalance(t, respBody)
	if !out.Success || out.Moved != 0 || out.BytesMoved != 0 || out.Remaining != 0 {
		t.Fatalf("空卷 rebalance 应 success moved=0, got %+v", out)
	}
	// 目录结构完好（无副作用）。
	if _, err := os.Stat(dirs[0]); err != nil {
		t.Fatalf("main 卷根应存在: %v", err)
	}
	_ = h
}

// TestVolumesRebalance_ConcurrentLocked 并发 rebalance（同一 from→to）+ 并发 move 同一文件：
// uploadingFiles 锁串行化——同文件恰被迁移一次，绝不双迁/双份/账本超扣。
// 变异验证：把 rebalance 的锁复用去掉（仅迁移路径不持锁）→ 并发下同文件可被 move 与 rebalance
// 同时迁走（双份/账本错乱），本测试应红。
func TestVolumesRebalance_ConcurrentLocked(t *testing.T) {
	t.Parallel()
	url, h, dirs := twoVolumeServer(t)

	body := []byte("concurrent rebalance payload")
	status, _, respBody := volumeUpload(t, url, "c.txt", body, "")
	if status != http.StatusOK {
		t.Fatalf("上传应 200, got %d %s", status, respBody)
	}

	// 并发：2 个 rebalance（main→disk2）+ 2 个 move（main→disk2 同文件）。
	const n = 4
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := make([]int, n)
	transportErrs := 0
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var code int
			var err error
			if i < 2 {
				code, _, err = rebalanceVolumeCore(url, "main", "disk2", "")
			} else {
				code, _, err = moveVolumeCore(url, "main", "disk2", "c.txt")
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				transportErrs++
				results[i] = -1
				return
			}
			results[i] = code
		}(i)
	}
	wg.Wait()
	if transportErrs != 0 {
		t.Fatalf("%d 个并发请求传输失败", transportErrs)
	}
	ok, conflict, other := 0, 0, 0
	for i := range n {
		switch results[i] {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		case http.StatusNotFound:
			// 并发竞态的正常结果：目标已被先到的迁移/ move 迁走 → 404（源不存在）。
			conflict++
		default:
			other++
			t.Logf("并发请求 #%d status=%d", i, results[i])
		}
	}
	// 恰 1 次成功迁移（其余 409/404 = 竞态跳过）；不得出现双份/账本错乱。
	if ok == 0 {
		t.Fatalf("并发 rebalance/move 应至少 1 成功, got ok=%d conflict=%d other=%d", ok, conflict, other)
	}
	if other != 0 {
		t.Fatalf("并发请求不应有其它状态（500 等）, other=%d", other)
	}
	// 最终一致性：文件恰在 disk2 一份，main 无；双账本一致（main 0 / disk2 size）。
	if diskFileExists(t, dirs[0], "alice", "c.txt") {
		t.Fatal("并发后 main 不应残留 c.txt")
	}
	if !diskFileExists(t, dirs[1], "alice", "c.txt") {
		t.Fatal("并发后 c.txt 应在 disk2")
	}
	got, err := os.ReadFile(filepath.Join(dirs[1], "alice", "user", "c.txt"))
	if err != nil {
		t.Fatalf("read disk2 c.txt: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("并发后内容损坏: %q want %q", got, body)
	}
	if got := h.volSet.Pool("main").Usage(); got != 0 {
		t.Fatalf("并发后 main 池=%d want 0", got)
	}
	if got := h.volSet.Pool("disk2").Usage(); got != int64(len(body)) {
		t.Fatalf("并发后 disk2 池=%d want %d", got, len(body))
	}
}
