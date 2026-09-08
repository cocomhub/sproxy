// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// locate_volume_test.go 验证读路径跨卷定位（任务 5）：download/stat/delete/rename 在 owner
// 卷视图内定位 rel 所在卷（默认卷快路径 + 视图其余卷遍历），list 聚合多卷带 volume 字段 +
// ?volume= 过滤；F1-B 承重（home 非默认卷的覆盖写 stay-home）；F2 reconcile 逐卷物理扫描校准。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// volumeRWMux 构造把固定 actor 注入请求 ctx 的读写 mux（upload/download/stat/delete/rename/list）。
func volumeRWMux(h *Handlers, actor string) *http.ServeMux {
	wrap := func(hf http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(withActor(r.Context(), actor))
			hf(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", wrap(h.upload))
	mux.HandleFunc("GET /download", wrap(h.download))
	mux.HandleFunc("HEAD /api/files/stat", wrap(h.stat))
	mux.HandleFunc("POST /delete", wrap(h.delete))
	mux.HandleFunc("POST /rename", wrap(h.rename))
	mux.HandleFunc("GET /api/files", wrap(h.listFiles))
	return mux
}

// volumeRWServer 构造带 volSet 的多卷读写测试服务（等价 RegisterRoutes 装配产物但直驱 handler）。
// volumes 首卷即默认卷；返回 URL、Handlers（池/磁盘断言用）与各卷根目录。
func volumeRWServer(t *testing.T, actor string, volumes []VolumeConfig, mod func(*Config)) (string, *Handlers, []string) {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = volumes[0].Root
	cfg.Placement = "prefer-default"
	cfg.Volumes = volumes
	if mod != nil {
		mod(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	ts := httptest.NewServer(volumeRWMux(h, actor))
	t.Cleanup(ts.Close)
	dirs := make([]string, len(volumes))
	for i := range volumes {
		dirs[i] = volumes[i].Root
	}
	return ts.URL, h, dirs
}

// listEntry 是 listFiles JSON 响应条目的解析形状（含 volume 新增字段）。
type listEntry struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Checksum string `json:"checksum"`
	ModTime  int64  `json:"mod_time"`
	IsDir    bool   `json:"is_dir"`
	Volume   string `json:"volume"`
}

type listRespShape struct {
	Files  []listEntry `json:"files"`
	Total  int         `json:"total"`
	Offset int         `json:"offset"`
	Limit  int         `json:"limit"`
}

// volumeDownload GET /download?filename=…；返回 status、响应头与 body。
func volumeDownload(t *testing.T, baseURL, filename, volume string) (int, http.Header, []byte) {
	t.Helper()
	reqURL := baseURL + "/download?filename=" + filename
	if volume != "" {
		reqURL += "&volume=" + volume
	}
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		t.Fatalf("new download req: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do download %s: %v", filename, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, body
}

// volumeStat HEAD /api/files/stat?filename=…；返回 status 与响应头。
func volumeStat(t *testing.T, baseURL, filename string) (int, http.Header) {
	t.Helper()
	req, err := http.NewRequest("HEAD", baseURL+"/api/files/stat?filename="+filename, nil)
	if err != nil {
		t.Fatalf("new stat req: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do stat %s: %v", filename, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header
}

// volumeDelete POST /delete?filename=… 带 X-File-Checksum；返回 status、响应头、body。
func volumeDelete(t *testing.T, baseURL, filename, checksum string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest("POST", baseURL+"/delete?filename="+filename, nil)
	if err != nil {
		t.Fatalf("new delete req: %v", err)
	}
	req.Header.Set(headerFileChecksum, checksum)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do delete %s: %v", filename, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, body
}

// volumeRename POST /rename?from=&to= 带 X-File-Checksum；返回 status、body。
func volumeRename(t *testing.T, baseURL, from, to, checksum string) (int, []byte) {
	t.Helper()
	return volumeRenameQuery(t, baseURL, from, to, checksum, "")
}

// volumeRenameQuery 与 volumeRename 同语义，支持附加 ?volume= 过滤参数。
func volumeRenameQuery(t *testing.T, baseURL, from, to, checksum, volume string) (int, []byte) {
	t.Helper()
	reqURL := baseURL + "/rename?from=" + from + "&to=" + to
	if volume != "" {
		reqURL += "&volume=" + volume
	}
	req, err := http.NewRequest("POST", reqURL, nil)
	if err != nil {
		t.Fatalf("new rename req: %v", err)
	}
	req.Header.Set(headerFileChecksum, checksum)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do rename %s→%s: %v", from, to, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// volumeDeleteQuery 与 volumeDelete 同语义，支持附加 ?volume= 过滤参数。
func volumeDeleteQuery(t *testing.T, baseURL, filename, checksum, volume string) (int, http.Header, []byte) {
	t.Helper()
	reqURL := baseURL + "/delete?filename=" + filename
	if volume != "" {
		reqURL += "&volume=" + volume
	}
	req, err := http.NewRequest("POST", reqURL, nil)
	if err != nil {
		t.Fatalf("new delete req: %v", err)
	}
	req.Header.Set(headerFileChecksum, checksum)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do delete %s: %v", filename, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, body
}

// volumeList GET /api/files[?subdir=&volume=]；返回解析后的列表响应结构。
func volumeList(t *testing.T, baseURL, subdir, volume string) (int, listRespShape) {
	t.Helper()
	reqURL := baseURL + "/api/files"
	qs := []string{}
	if subdir != "" {
		qs = append(qs, "subdir="+subdir)
	}
	if volume != "" {
		qs = append(qs, "volume="+volume)
	}
	if len(qs) > 0 {
		reqURL += "?" + strings.Join(qs, "&")
	}
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		t.Fatalf("new list req: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do list: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var shape listRespShape
	if err := json.Unmarshal(body, &shape); err != nil {
		t.Fatalf("parse list resp %q: %v", string(body), err)
	}
	return resp.StatusCode, shape
}

// twoTinyVolumes 返回 main(10B 容量) + disk2(大容量) 双卷配置（测试用假盘目录）。
func twoTinyVolumes(t *testing.T) ([]VolumeConfig, []string) {
	t.Helper()
	dirs := []string{t.TempDir(), t.TempDir()}
	return []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 10},
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}, dirs
}

// TestDownload_LocatesAcrossVolumes 文件在 disk2（非默认卷）也能定位下载：
// 默认卷先 miss → 视图遍历命中 disk2 → 200 且内容一致。
func TestDownload_LocatesAcrossVolumes(t *testing.T) {
	volumes, dirs := twoTinyVolumes(t)
	url, _, _ := volumeRWServer(t, "alice", volumes, nil)

	bodyA := []byte("AAAAAAAA")
	bodyB := []byte("BBBBBBBB")
	assertUploadVolume(t, url, "a.txt", bodyA, "", "main", http.StatusOK)
	assertUploadVolume(t, url, "b.txt", bodyB, "", "disk2", http.StatusOK)

	// b.txt 在 disk2，download 必须跨卷定位成功。
	status, _, data := volumeDownload(t, url, "b.txt", "")
	if status != http.StatusOK {
		t.Fatalf("download disk2 文件 status=%d want 200, body=%s", status, data)
	}
	if !bytes.Equal(data, bodyB) {
		t.Fatalf("download disk2 文件内容不符: got %q want %q", data, bodyB)
	}
	// 默认卷文件不受影响。
	status, _, data = volumeDownload(t, url, "a.txt", "")
	if status != http.StatusOK || !bytes.Equal(data, bodyA) {
		t.Fatalf("download main 文件异常: status=%d data=%q", status, data)
	}
	// 落盘位置：a 在 main、b 在 disk2。
	if !diskFileExists(t, dirs[0], "alice", "a.txt") || !diskFileExists(t, dirs[1], "alice", "b.txt") {
		t.Fatalf("卷布局异常: main=%v disk2=%v", dirs[0], dirs[1])
	}
}

// TestDownload_MissAllVolumes404 全视图未命中 → 404（与单卷现状一致）。
func TestDownload_MissAllVolumes404(t *testing.T) {
	volumes, _ := twoTinyVolumes(t)
	url, _, _ := volumeRWServer(t, "alice", volumes, nil)

	status, _, body := volumeDownload(t, url, "ghost.txt", "")
	if status != http.StatusNotFound {
		t.Fatalf("download 不存在文件 status=%d want 404, body=%s", status, body)
	}
}

// TestDownload_ExplicitVolumeFilter 显式 ?volume=：只在指定卷定位；指向不含该文件的卷 → 404。
func TestDownload_ExplicitVolumeFilter(t *testing.T) {
	volumes, _ := twoTinyVolumes(t)
	url, _, _ := volumeRWServer(t, "alice", volumes, nil)

	bodyA := []byte("AAAAAAAA")
	bodyB := []byte("BBBBBBBB")
	assertUploadVolume(t, url, "a.txt", bodyA, "", "main", http.StatusOK)
	assertUploadVolume(t, url, "b.txt", bodyB, "", "disk2", http.StatusOK)

	// b 在 disk2，?volume=disk2 → 200；?volume=main → 404（main 上无 b）。
	if status, _, _ := volumeDownload(t, url, "b.txt", "disk2"); status != http.StatusOK {
		t.Fatalf("download b ?volume=disk2 status=%d want 200", status)
	}
	if status, _, _ := volumeDownload(t, url, "b.txt", "main"); status != http.StatusNotFound {
		t.Fatalf("download b ?volume=main status=%d want 404（main 无 b）", status)
	}
	// ?volume=ghost（不在视图/未知卷）→ 404。
	if status, _, _ := volumeDownload(t, url, "b.txt", "ghost"); status != http.StatusNotFound {
		t.Fatalf("download b ?volume=ghost status=%d want 404", status)
	}
}

// TestStat_LocatesAcrossVolumes HEAD /api/files/stat 定位跨卷（disk2 文件元信息可读）。
func TestStat_LocatesAcrossVolumes(t *testing.T) {
	volumes, _ := twoTinyVolumes(t)
	url, _, _ := volumeRWServer(t, "alice", volumes, nil)

	bodyA := []byte("AAAAAAAA")
	bodyB := []byte("BBBBBBBB")
	assertUploadVolume(t, url, "a.txt", bodyA, "", "main", http.StatusOK)
	assertUploadVolume(t, url, "b.txt", bodyB, "", "disk2", http.StatusOK)

	status, hdr := volumeStat(t, url, "b.txt")
	if status != http.StatusOK {
		t.Fatalf("stat disk2 文件 status=%d want 200", status)
	}
	if got := hdr.Get("X-File-Size"); got != fmt.Sprintf("%d", len(bodyB)) {
		t.Fatalf("stat X-File-Size=%q want %d", got, len(bodyB))
	}
	// 不存在的文件 → 404。
	if status, _ := volumeStat(t, url, "ghost.txt"); status != http.StatusNotFound {
		t.Fatalf("stat 不存在文件 status=%d want 404", status)
	}
}

// TestList_AggregatesVolumesAndFilters list 聚合两卷：两卷各一文件 → /api/files 两条、各带
// volume 字段；?volume=main 只返回 main 那条；?volume=ghost → 404。
func TestList_AggregatesVolumesAndFilters(t *testing.T) {
	volumes, _ := twoTinyVolumes(t)
	url, _, _ := volumeRWServer(t, "alice", volumes, nil)

	bodyA := []byte("AAAAAAAA")
	bodyB := []byte("BBBBBBBB")
	assertUploadVolume(t, url, "a.txt", bodyA, "", "main", http.StatusOK)
	assertUploadVolume(t, url, "b.txt", bodyB, "", "disk2", http.StatusOK)

	status, shape := volumeList(t, url, "", "")
	if status != http.StatusOK {
		t.Fatalf("list status=%d want 200", status)
	}
	if len(shape.Files) != 2 {
		t.Fatalf("聚合列表应 2 条, got %d: %+v", len(shape.Files), shape.Files)
	}
	volOf := map[string]string{}
	for _, f := range shape.Files {
		volOf[f.Name] = f.Volume
	}
	if volOf["a.txt"] != "main" || volOf["b.txt"] != "disk2" {
		t.Fatalf("聚合条目 volume 字段不符: %+v", volOf)
	}

	// ?volume=main 只列 main。
	status, shape = volumeList(t, url, "", "main")
	if status != http.StatusOK {
		t.Fatalf("list ?volume=main status=%d want 200", status)
	}
	if len(shape.Files) != 1 || shape.Files[0].Name != "a.txt" || shape.Files[0].Volume != "main" {
		t.Fatalf("?volume=main 应只含 a.txt(main), got %+v", shape.Files)
	}

	// ?volume=ghost（不在视图）→ 404。
	status, _ = volumeList(t, url, "", "ghost")
	if status != http.StatusNotFound {
		t.Fatalf("list ?volume=ghost status=%d want 404", status)
	}
}

// TestDeleteRename_LocatesAcrossVolumes 删 disk2 文件 → 200 且 disk2 消失、main 不受影响；
// rename 同卷（不跨卷）→ 200。
func TestDeleteRename_LocatesAcrossVolumes(t *testing.T) {
	volumes, _ := twoTinyVolumes(t)
	url, h, dirs := volumeRWServer(t, "alice", volumes, nil)

	bodyA := []byte("AAAAAAAA")
	bodyB := []byte("BBBBBBBB")
	assertUploadVolume(t, url, "a.txt", bodyA, "", "main", http.StatusOK)
	assertUploadVolume(t, url, "b.txt", bodyB, "", "disk2", http.StatusOK)

	// rename a.txt（main 内）→ a2.txt → 200。
	if status, body := volumeRename(t, url, "a.txt", "a2.txt", sha256hex(bodyA)); status != http.StatusOK {
		t.Fatalf("rename main 文件 status=%d want 200, body=%s", status, body)
	}
	if !diskFileExists(t, dirs[0], "alice", "a2.txt") || diskFileExists(t, dirs[0], "alice", "a.txt") {
		t.Fatal("rename 后 a2.txt 应在 main、a.txt 应消失")
	}

	// rename b.txt（disk2 内）→ b2.txt → 200（同卷）。
	if status, body := volumeRename(t, url, "b.txt", "b2.txt", sha256hex(bodyB)); status != http.StatusOK {
		t.Fatalf("rename disk2 文件 status=%d want 200, body=%s", status, body)
	}
	if !diskFileExists(t, dirs[1], "alice", "b2.txt") || diskFileExists(t, dirs[1], "alice", "b.txt") {
		t.Fatal("rename 后 b2.txt 应在 disk2、b.txt 应消失")
	}

	// delete b2.txt（disk2 定位删除）→ 200。
	if status, _, body := volumeDelete(t, url, "b2.txt", sha256hex(bodyB)); status != http.StatusOK {
		t.Fatalf("delete disk2 文件 status=%d want 200, body=%s", status, body)
	}
	if diskFileExists(t, dirs[1], "alice", "b2.txt") {
		t.Fatal("b2.txt 应从 disk2 删除")
	}
	if !diskFileExists(t, dirs[0], "alice", "a2.txt") {
		t.Fatal("main 文件不应受 disk2 删除影响")
	}

	// 卷容量池对账：disk2 删除后池 Usage 归零（双 Release，防多卷账本泄漏）。
	if got := h.volSet.Pool("disk2").Usage(); got != 0 {
		t.Fatalf("delete 后 disk2 卷池 Usage=%d want 0", got)
	}
}

// TestDeleteRename_ExplicitVolumeFilter delete/rename 带 ?volume= 只在指定卷定位：
// b 在 disk2 → ?volume=disk2 删/改成功；指向 main → 404（不在该卷，不泄卷）。
func TestDeleteRename_ExplicitVolumeFilter(t *testing.T) {
	volumes, _ := twoTinyVolumes(t)
	url, _, dirs := volumeRWServer(t, "alice", volumes, nil)

	bodyA := []byte("AAAAAAAA")
	bodyB := []byte("BBBBBBBB")
	assertUploadVolume(t, url, "a.txt", bodyA, "", "main", http.StatusOK)
	assertUploadVolume(t, url, "b.txt", bodyB, "", "disk2", http.StatusOK)

	// rename b → b2 指定 ?volume=main → 404（b 在 disk2，不在 main）。
	if status, body := volumeRenameQuery(t, url, "b.txt", "b2.txt", sha256hex(bodyB), "main"); status != http.StatusNotFound {
		t.Fatalf("rename b ?volume=main status=%d want 404, body=%s", status, body)
	}
	// rename b → b2 指定 ?volume=disk2 → 200 同卷。
	if status, body := volumeRenameQuery(t, url, "b.txt", "b2.txt", sha256hex(bodyB), "disk2"); status != http.StatusOK {
		t.Fatalf("rename b ?volume=disk2 status=%d want 200, body=%s", status, body)
	}
	if !diskFileExists(t, dirs[1], "alice", "b2.txt") {
		t.Fatal("b2.txt 应落在 disk2")
	}

	// delete b2 指定 ?volume=main → 404；?volume=disk2 → 200。
	if status, _, _ := volumeDeleteQuery(t, url, "b2.txt", sha256hex(bodyB), "main"); status != http.StatusNotFound {
		t.Fatalf("delete b2 ?volume=main status=%d want 404", status)
	}
	if status, _, body := volumeDeleteQuery(t, url, "b2.txt", sha256hex(bodyB), "disk2"); status != http.StatusOK {
		t.Fatalf("delete b2 ?volume=disk2 status=%d want 200, body=%s", status, body)
	}
	if diskFileExists(t, dirs[1], "alice", "b2.txt") {
		t.Fatal("b2.txt 应从 disk2 删除")
	}
}

// TestUpload_OverwriteStayHome_NonDefaultVolume F1-B 承重：文件 home 在 disk2（默认卷满换卷
// 落 disk2），默认卷空间恢复（此处放大 main 容量）后 auto 覆盖写必须写回 disk2（home-aware），
// 不得因默认卷有空间而跨卷双份。disk2 仍 1 份新内容、main 无 x、无双计。
func TestUpload_OverwriteStayHome_NonDefaultVolume(t *testing.T) {
	volumes, _ := twoTinyVolumes(t)
	url, h, dirs := volumeRWServer(t, "alice", volumes, func(c *Config) {
		c.Versioning.Enabled = true
	})

	bodyA := []byte("AAAAAAAA") // 8B：占满 main（容量 10）
	bodyX := []byte("XXXXXXXX") // 8B：main 满 → 落 disk2（home=disk2）
	bodyX2 := []byte("YYYYYYYY")

	assertUploadVolume(t, url, "a.txt", bodyA, "", "main", http.StatusOK)
	assertUploadVolume(t, url, "x.txt", bodyX, "", "disk2", http.StatusOK)
	if !diskFileExists(t, dirs[1], "alice", "x.txt") {
		t.Fatal("前置：x.txt 应落在 disk2（home 非默认卷）")
	}

	// 默认卷空间恢复：放大 main 容量，使容量路由又会首选 main（若不 stay-home 会双份）。
	h.volSet.Pool("main").SetMaxBytes(30)

	// auto 覆盖写 x.txt（新内容，versioning 开启）→ 必须 200 落 disk2（home-aware）。
	status, hdr, respBody := volumeUpload(t, url, "x.txt", bodyX2, "")
	if status != http.StatusOK {
		t.Fatalf("auto 覆盖写 x.txt status=%d want 200, body=%s", status, respBody)
	}
	if got := hdr.Get("X-Volume"); got != "disk2" {
		t.Fatalf("auto 覆盖写 x.txt X-Volume=%q want disk2（stay-home 写回原卷）", got)
	}

	// disk2 仍 1 份且为最新内容；main 不得出现 x.txt（防跨卷双份）。
	if diskFileExists(t, dirs[0], "alice", "x.txt") {
		t.Fatal("x.txt 不得落在 main（同 rel 跨卷双份）")
	}
	if !diskFileExists(t, dirs[1], "alice", "x.txt") {
		t.Fatal("x.txt 应保留在 disk2（home 卷）")
	}
	data, _ := os.ReadFile(filepath.Join(dirs[1], "alice", "user", "x.txt"))
	if !bytes.Equal(data, bodyX2) {
		t.Fatalf("disk2 x.txt 内容=%q want %q（覆盖写应更新 home 卷内容）", data, bodyX2)
	}
	// main 的 a.txt 不受影响。
	if !diskFileExists(t, dirs[0], "alice", "a.txt") {
		t.Fatal("main a.txt 不应受影响")
	}
}

// TestUpload_OverwriteHomeNonDefault_VersioningOff_Conflict F1-B 安全：home 非默认卷 + versioning
// 关闭 + checksum 不匹配的 auto 重传 → 409 冲突（保护存量文件，不静默覆盖），且不得跨卷双份
// （定位到 disk2 home 后按单卷同 rel 冲突语义拒绝）。
func TestUpload_OverwriteHomeNonDefault_VersioningOff_Conflict(t *testing.T) {
	volumes, _ := twoTinyVolumes(t)
	url, h, dirs := volumeRWServer(t, "alice", volumes, nil) // versioning 关闭

	bodyA := []byte("AAAAAAAA")
	bodyX := []byte("XXXXXXXX")
	assertUploadVolume(t, url, "a.txt", bodyA, "", "main", http.StatusOK)
	assertUploadVolume(t, url, "x.txt", bodyX, "", "disk2", http.StatusOK)

	// 放大 main 容量使容量路由若被误用会首选 main（回归锁：冲突不得走容量路由制造双份）。
	h.volSet.Pool("main").SetMaxBytes(30)

	status, _, respBody := volumeUpload(t, url, "x.txt", []byte("YYYYYYYY"), "")
	if status != http.StatusConflict {
		t.Fatalf("home 非默认卷 + versioning off 冲突重传 status=%d want 409, body=%s", status, respBody)
	}
	// 双份不得出现：main 无 x、disk2 仍原内容一份。
	if diskFileExists(t, dirs[0], "alice", "x.txt") {
		t.Fatal("x.txt 不得落在 main（同 rel 跨卷双份）")
	}
	if !diskFileExists(t, dirs[1], "alice", "x.txt") {
		t.Fatal("x.txt 应保留在 disk2（home 卷原内容）")
	}
	data, _ := os.ReadFile(filepath.Join(dirs[1], "alice", "user", "x.txt"))
	if !bytes.Equal(data, bodyX) {
		t.Fatalf("disk2 x.txt 内容=%q want 原内容 %q（409 不应改动文件）", data, bodyX)
	}
}
