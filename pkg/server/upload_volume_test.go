// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// upload_volume_test.go 验证多卷 upload 路由（任务 4）：prefer-default 选卷 + 换卷、
// 显式 volume 的 ACL/唯一性、owner 全局配额跨卷封顶与 X-Volume 响应头。用假盘
// （t.TempDir）双卷经 actorUploadMux 直驱 h.upload 集成断言落卷位置。

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
)

// newVolumeUploadServer 装配多卷 Handlers（volSet 生效）并绑定 actor 的 upload mux。
// volumes 逐卷独立根（首卷即默认卷），ownerQuota 为 actor 的 owner_quotas（0 = 不限制）。
// 返回服务 URL、Handlers（供池/磁盘断言）与各卷根目录（与 volumes 一一对应）。
func newVolumeUploadServer(t *testing.T, actor string, volumes []VolumeConfig, ownerQuota int64) (string, *Handlers, []string) {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = volumes[0].Root
	cfg.Placement = "prefer-default"
	if ownerQuota > 0 {
		cfg.OwnerQuotas = map[string]int64{actor: ownerQuota}
	}
	cfg.Volumes = volumes
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	ts := httptest.NewServer(actorUploadMux(h, actor))
	t.Cleanup(ts.Close)
	dirs := make([]string, len(volumes))
	for i := range volumes {
		dirs[i] = volumes[i].Root
	}
	return ts.URL, h, dirs
}

// volumeUpload 执行带可选 volume 表单字段的 multipart 上传（恒带 X-File-Checksum），
// 返回状态码、响应头与 body。
func volumeUpload(t *testing.T, baseURL, filename string, body []byte, vol string) (int, http.Header, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if vol != "" {
		if err := mw.WriteField("volume", vol); err != nil {
			t.Fatalf("write volume field: %v", err)
		}
	}
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err = part.Write(body); err != nil {
		t.Fatalf("write part: %v", err)
	}
	_ = mw.Close()

	req, err := http.NewRequest("POST", baseURL+"/upload", &buf)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(headerFileChecksum, sha256hex(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do upload: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, respBody
}

// diskFileExists 断言卷根下 owner/user/<name> 是否存在。
func diskFileExists(t *testing.T, volRoot, owner, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(volRoot, owner, "user", name))
	return err == nil
}

func assertUploadVolume(t *testing.T, url string, filename string, body []byte, vol, wantVolume string, wantStatus int) {
	t.Helper()
	status, hdr, respBody := volumeUpload(t, url, filename, body, vol)
	if status != wantStatus {
		t.Fatalf("upload %s (volume=%q): status=%d want %d, body=%s", filename, vol, status, wantStatus, respBody)
	}
	if wantStatus == http.StatusOK {
		if got := hdr.Get("X-Volume"); got != wantVolume {
			t.Fatalf("upload %s: X-Volume=%q want %q", filename, got, wantVolume)
		}
	}
}

// TestUpload_RoutesToDefaultThenNext 双卷（main 容量小、disk2 大），prefer-default：
// main 未满落 main；main 满换 disk2。X-Volume 分别断言 main/disk2，磁盘布局 1+1。
func TestUpload_RoutesToDefaultThenNext(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 10},
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}
	url, h, dirs := newVolumeUploadServer(t, "alice", volumes, 0)

	bodyA := []byte("AAAAAAA") // 7B：main 容量 10 足够
	status, hdr, respBody := volumeUpload(t, url, "a.txt", bodyA, "")
	if status != http.StatusOK {
		t.Fatalf("首次上传应成功: %d %s", status, respBody)
	}
	if got := hdr.Get("X-Volume"); got != "main" {
		t.Fatalf("首次上传 X-Volume=%q want main（默认卷优先）", got)
	}

	bodyB := []byte("BBBBBBB") // 7B：main 已用 7，预留 7 > 10 → 换 disk2
	status, hdr, respBody = volumeUpload(t, url, "b.txt", bodyB, "")
	if status != http.StatusOK {
		t.Fatalf("main 满后上传应换卷成功: %d %s", status, respBody)
	}
	if got := hdr.Get("X-Volume"); got != "disk2" {
		t.Fatalf("第二次上传 X-Volume=%q want disk2（main 满换卷）", got)
	}

	if !diskFileExists(t, dirs[0], "alice", "a.txt") {
		t.Fatal("a.txt 应落在 main（默认卷）")
	}
	if diskFileExists(t, dirs[0], "alice", "b.txt") {
		t.Fatal("b.txt 不应落在 main")
	}
	if !diskFileExists(t, dirs[1], "alice", "b.txt") {
		t.Fatal("b.txt 应落在 disk2")
	}

	if got := h.volSet.Pool("main").Usage(); got != 7 {
		t.Fatalf("main 卷池 Usage=%d want 7", got)
	}
	if got := h.volSet.Pool("disk2").Usage(); got != 7 {
		t.Fatalf("disk2 卷池 Usage=%d want 7", got)
	}
}

// TestUpload_ExplicitVolume 显式 volume：指定 disk2 生效（main 未满也落 disk2）；
// 不在视图（ghost）→ 403；目标 rel 已在 main → 409（唯一性，AD-4）。
func TestUpload_ExplicitVolume(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 10},
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}
	url, _, dirs := newVolumeUploadServer(t, "alice", volumes, 0)

	body := []byte("12345678") // 8B
	assertUploadVolume(t, url, "a.txt", body, "", "main", http.StatusOK)

	// main 未满（8 ≤ 10）也须显式指定 disk2 生效。
	assertUploadVolume(t, url, "b.txt", body, "disk2", "disk2", http.StatusOK)
	if !diskFileExists(t, dirs[1], "alice", "b.txt") {
		t.Fatal("b.txt 应落在 disk2（显式 volume 指定）")
	}
	if diskFileExists(t, dirs[0], "alice", "b.txt") {
		t.Fatal("b.txt 不应落在 main（显式指定 disk2）")
	}

	// ghost 不在卷集合 / 视图 → 403。
	status, _, _ := volumeUpload(t, url, "c.txt", body, "ghost")
	if status != http.StatusForbidden {
		t.Fatalf("volume=ghost status=%d want 403", status)
	}

	// 唯一性：a.txt 已在 main，显式指定 disk2 → 409 + 所在卷提示。
	status, _, respBody := volumeUpload(t, url, "a.txt", body, "disk2")
	if status != http.StatusConflict {
		t.Fatalf("跨卷同名显式上传 status=%d want 409, body=%s", status, respBody)
	}
	if len(respBody) == 0 {
		t.Fatal("409 响应应有提示 body")
	}
}

// TestUpload_OwnerGlobalQuotaCrossVolume owner 全局配额（15B）跨卷封顶：
// 两次 7B 分别落 main/disk2，第三次 7B → 507（owner 全局满，不因有第二卷放行）。
func TestUpload_OwnerGlobalQuotaCrossVolume(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 10},
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}
	url, _, dirs := newVolumeUploadServer(t, "alice", volumes, 15)

	body := []byte("1234567") // 7B
	// 第一次 7B → main（7 ≤ 10 且 7 ≤ 15）。
	assertUploadVolume(t, url, "a.txt", body, "", "main", http.StatusOK)
	// 第二次 7B：main 卷池 7+7>10 → 换 disk2；owner 全局 14 ≤ 15。
	assertUploadVolume(t, url, "b.txt", body, "", "disk2", http.StatusOK)
	// 第三次 7B：owner 全局 14+7>15 → 507（即使两卷都有余量）。
	status, _, respBody := volumeUpload(t, url, "c.txt", body, "")
	if status != http.StatusInsufficientStorage {
		t.Fatalf("owner 全局满后第三次上传 status=%d want 507, body=%s", status, respBody)
	}

	if !diskFileExists(t, dirs[0], "alice", "a.txt") {
		t.Fatal("a.txt 应落在 main")
	}
	if !diskFileExists(t, dirs[1], "alice", "b.txt") {
		t.Fatal("b.txt 应落在 disk2")
	}
	if diskFileExists(t, dirs[0], "alice", "c.txt") || diskFileExists(t, dirs[1], "alice", "c.txt") {
		t.Fatal("c.txt 不应落盘（owner 全局配额拒绝）")
	}
}

// TestUpload_IdempotentReuploadVolumeFull 幂等重传不被卷容量误拒（零回归）：单卷容量打满后，
// 同 checksum 重传已有文件 → 幂等 200（不重新预留）；不同 checksum → 409 冲突（versioning 关闭），
// 均不因卷满返回 507。
func TestUpload_IdempotentReuploadVolumeFull(t *testing.T) {
	dirs := []string{t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 10},
	}
	url, _, _ := newVolumeUploadServer(t, "alice", volumes, 0)

	body := []byte("12345678") // 8B：占满 main 卷容量 10（committed 8）
	assertUploadVolume(t, url, "a.txt", body, "", "main", http.StatusOK)

	// 同 checksum 重传 → 幂等 200（无 507），X-Volume 仍为 main。
	status, hdr, respBody := volumeUpload(t, url, "a.txt", body, "")
	if status != http.StatusOK {
		t.Fatalf("卷满后同 checksum 幂等重传 status=%d want 200, body=%s", status, respBody)
	}
	if got := hdr.Get("X-Volume"); got != "main" {
		t.Fatalf("幂等重传 X-Volume=%q want main", got)
	}

	// 不同 checksum 重传（versioning 关闭）→ 409 冲突（保留现有文件，不覆盖）。
	status, _, _ = volumeUpload(t, url, "a.txt", []byte("DIFFERENT"), "")
	if status != http.StatusConflict {
		t.Fatalf("卷满后不同 checksum 重传 status=%d want 409", status)
	}
}

// TestUpload_SingleVolumeXVolumeHeader 单卷零回归：真实 HTTP 装配（RegisterRoutes，
// volSet 生效）下普通上传落默认卷，响应头带 X-Volume: default（向后兼容，多卷客户端可见）。
func TestUpload_SingleVolumeXVolumeHeader(t *testing.T) {
	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("single volume content")
	status, hdr, respBody := volumeUpload(t, url, "sv.txt", body, "")
	if status != http.StatusOK {
		t.Fatalf("单卷上传应成功: %d %s", status, respBody)
	}
	if got := hdr.Get("X-Volume"); got != "default" {
		t.Fatalf("单卷上传 X-Volume=%q want default", got)
	}
}

// TestVolumeOrderPreferDefault 直接验证 pkg/volume 排序与装配卷集合的输入形状打通
// （main 恒前；used 按池 Usage 生效）。
func TestVolumeOrderPreferDefault(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	cfg := Default()
	cfg.StorageRoot = dirs[0]
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 10},
		{Name: "disk2", Root: dirs[1], VolCapacity: 100},
	}
	vs, err := assembleVolumes(cfg, testLogger())
	if err != nil {
		t.Fatalf("assembleVolumes: %v", err)
	}
	t.Cleanup(func() { _ = vs.Close() })

	allowed := volume.AllowedVolumes(vs.All(), "bob")
	ordered := volume.OrderCandidates(allowed, volume.ModePreferDefault, func(name string) int64 {
		p := vs.Pool(name)
		if p == nil {
			return 0
		}
		return p.Usage()
	})
	if len(ordered) != 2 || ordered[0].Name != "main" || ordered[1].Name != "disk2" {
		t.Fatalf("prefer-default 候选序应为 [main disk2], got %+v", ordered)
	}
}
