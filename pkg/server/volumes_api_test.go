// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volumes_api_test.go 验证 T6c /api/volumes（per-owner 列表）+ POST /api/volumes/move（跨卷移动）
// + 版本桶入卷池账本：
//  1. GET /api/volumes 单卷红线段（1 卷）；多卷 per-owner 只列 ACL 允许卷（allow 收紧隐藏）。
//  2. move 跨卷成功（双账本迁移 + 落盘）；同卷 no-op；目标已存在 409；源不在 from 卷 404；
//     from/to 不在 owner 视图 403；目标卷容量不足 507；并发 move 恰 1 成功。
//  3. TestVersionBucket_VolumePoolLedger：版本字节写/删计入 home 卷容量池（T6a 发现-3）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// volumesAPIWrap 绑定卷 API + 配套文件操作 handler 到固定 actor（volSet 生效）。
func volumesAPIWrap(h *Handlers, actor string) *http.ServeMux {
	wrap := func(hf http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(withActor(r.Context(), actor))
			hf(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/volumes", wrap(h.listVolumesHandler))
	mux.HandleFunc("POST /api/volumes/move", wrap(h.moveVolumeHandler))
	mux.HandleFunc("POST /upload", wrap(h.upload))
	mux.HandleFunc("POST /delete", wrap(h.delete))
	return mux
}

// newVolumesAPIServer 装配多卷卷 API 测试服务，返回 URL、Handlers 与各卷根。
func newVolumesAPIServer(t *testing.T, actor string, volumes []VolumeConfig, mod func(*Config)) (string, *Handlers, []string) {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = volumes[0].Root
	cfg.Placement = "prefer-default"
	if mod != nil {
		mod(cfg)
	}
	cfg.Volumes = volumes
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	ts := httptest.NewServer(volumesAPIWrap(h, actor))
	t.Cleanup(ts.Close)
	dirs := make([]string, len(volumes))
	for i := range volumes {
		dirs[i] = volumes[i].Root
	}
	return ts.URL, h, dirs
}

// listVolumes 请求 GET /api/volumes，返回状态码与解码响应。
func listVolumes(t *testing.T, baseURL string) (int, volumesListResponse) {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/volumes")
	if err != nil {
		t.Fatalf("GET /api/volumes: %v", err)
	}
	defer resp.Body.Close()
	var out volumesListResponse
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode /api/volumes: %v (body=%s)", err, body)
		}
	}
	return resp.StatusCode, out
}

// moveVolumeCore 发起跨卷 move（不 t.Fatal，供并发 goroutine 使用）。
func moveVolumeCore(baseURL, fromVol, toVol, filename string) (int, []byte, error) {
	u := baseURL + "/api/volumes/move?from_volume=" + url.QueryEscape(fromVol) +
		"&to_volume=" + url.QueryEscape(toVol) + "&filename=" + url.QueryEscape(filename)
	req, err := http.NewRequest("POST", u, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, nil
}

// moveVolume 请求 POST /api/volumes/move（传输错误 t.Fatal）。
func moveVolume(t *testing.T, baseURL, fromVol, toVol, filename string) (int, []byte) {
	t.Helper()
	status, body, err := moveVolumeCore(baseURL, fromVol, toVol, filename)
	if err != nil {
		t.Fatalf("move %s: %v", filename, err)
	}
	return status, body
}

// moveSuccess 断言跨卷 move 成功（200）。
func moveSuccess(t *testing.T, baseURL, fromVol, toVol, filename string) {
	t.Helper()
	status, body := moveVolume(t, baseURL, fromVol, toVol, filename)
	if status != http.StatusOK {
		t.Fatalf("move %s %s→%s status=%d want 200, body=%s", filename, fromVol, toVol, status, body)
	}
}

// deleteFile 按内容 checksum 删除 user 文件。
func deleteFile(t *testing.T, baseURL, filename string, content []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest("POST", baseURL+"/delete?filename="+url.QueryEscape(filename), nil)
	if err != nil {
		t.Fatalf("new delete req: %v", err)
	}
	req.Header.Set(headerFileChecksum, sha256hex(content))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete %s: %v", filename, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// TestVolumesAPI_List_SingleVolumeReturnsOne 单卷红线：真实 HTTP 装配（RegisterRoutes）下单卷
// GET /api/volumes 返回恰 1 卷（default），usage=0、capacity=0（不限）。
func TestVolumesAPI_List_SingleVolumeReturnsOne(t *testing.T) {
	url, _ := newTestServerWithAllRoutes(t, nil)
	status, out := listVolumes(t, url)
	if status != http.StatusOK {
		t.Fatalf("GET /api/volumes status=%d want 200", status)
	}
	if len(out.Volumes) != 1 {
		t.Fatalf("单卷 /api/volumes 应返回 1 卷, got %d (%+v)", len(out.Volumes), out.Volumes)
	}
	if out.Volumes[0].Name != "default" {
		t.Fatalf("单卷名=%q want default", out.Volumes[0].Name)
	}
	if out.Volumes[0].Usage != 0 || out.Volumes[0].Allowed != true {
		t.Fatalf("单卷 info=%+v want usage=0 allowed=true", out.Volumes[0])
	}
}

// TestVolumesAPI_List_PerOwnerVisibility GET /api/volumes per-owner：alice 视图 main+disk2，
// bob 视图 main+disk2+priv（allow:[bob]）。绝不可全局列卷——ACL 收紧卷对 alice 不可见。
func TestVolumesAPI_List_PerOwnerVisibility(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 1 << 20},
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
		{Name: "priv", Root: dirs[2], VolCapacity: 1 << 20,
			ACL: &VolumeACLConfig{Mode: VolumeACLAllow, Owners: []string{"bob"}}},
	}
	cfg := Default()
	cfg.StorageRoot = dirs[0]
	cfg.Volumes = volumes
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	urlAlice := httptest.NewServer(volumesAPIWrap(h, "alice"))
	t.Cleanup(urlAlice.Close)
	urlBob := httptest.NewServer(volumesAPIWrap(h, "bob"))
	t.Cleanup(urlBob.Close)

	_, alice := listVolumes(t, urlAlice.URL)
	if len(alice.Volumes) != 2 {
		t.Fatalf("alice 应见 2 卷 [main disk2], got %+v", alice.Volumes)
	}
	if alice.Volumes[0].Name != "main" || alice.Volumes[1].Name != "disk2" {
		t.Fatalf("alice 卷序=%+v want [main disk2]（声明序）", alice.Volumes)
	}
	for _, v := range alice.Volumes {
		if v.Capacity != 1<<20 || v.Usage != 0 || !v.Allowed {
			t.Fatalf("alice 卷 %+v 字段异常（capacity/usage/allowed）", v)
		}
	}

	_, bob := listVolumes(t, urlBob.URL)
	if len(bob.Volumes) != 3 {
		t.Fatalf("bob 应见 3 卷, got %+v", bob.Volumes)
	}
	if bob.Volumes[2].Name != "priv" {
		t.Fatalf("bob 第 3 卷=%q want priv（allow:[bob]）", bob.Volumes[2].Name)
	}
	if bob.Volumes[2].Mode != string(VolumeACLAllow) {
		t.Fatalf("priv mode=%q want %q（allow 白名单）", bob.Volumes[2].Mode, VolumeACLAllow)
	}
}

// TestVolumesAPI_Move_Success 跨卷 move 成功：main 文件 → disk2 同 rel；磁盘迁移、内容完整、
// 双账本迁移（main 池 0、disk2 池 = size、owner user 桶 Scope 不变）。
func TestVolumesAPI_Move_Success(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 1 << 20},
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}
	url, h, dirs := newVolumesAPIServer(t, "alice", volumes, nil)

	body := []byte("move me across volumes")
	status, _, respBody := volumeUpload(t, url, "a.txt", body, "")
	if status != http.StatusOK {
		t.Fatalf("上传应 200, got %d %s", status, respBody)
	}
	if !diskFileExists(t, dirs[0], "alice", "a.txt") {
		t.Fatal("a.txt 应初始落 main")
	}
	if got := h.volSet.Pool("main").Usage(); got != int64(len(body)) {
		t.Fatalf("main 池=%d want %d", got, len(body))
	}

	moveSuccess(t, url, "main", "disk2", "a.txt")

	if diskFileExists(t, dirs[0], "alice", "a.txt") {
		t.Fatal("move 后 main 不应再有 a.txt（源已删）")
	}
	if !diskFileExists(t, dirs[1], "alice", "a.txt") {
		t.Fatal("a.txt 应落 disk2")
	}
	got, err := os.ReadFile(filepath.Join(dirs[1], "alice", "user", "a.txt"))
	if err != nil {
		t.Fatalf("read disk2 a.txt: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("move 后内容 = %q want %q", got, body)
	}
	// 双账本迁移：main 池归 0、disk2 池 = size、owner 全局 user 桶 Scope 不变。
	if got := h.volSet.Pool("main").Usage(); got != 0 {
		t.Fatalf("move 后 main 池=%d want 0", got)
	}
	if got := h.volSet.Pool("disk2").Usage(); got != int64(len(body)) {
		t.Fatalf("move 后 disk2 池=%d want %d", got, len(body))
	}
	if got := h.quotaBucketFor("alice", "user").Usage(); got != int64(len(body)) {
		t.Fatalf("move 后 owner user 桶 Scope=%d want %d（同 owner 移动字节不净增）", got, len(body))
	}
}

// TestCleanupUploadingFilesPass_SkipsMoveLock 回归 T6c 修复轮建议 1：uploadingFiles 过期清理
// 必须把 value=="move" 与 value=="upload" 并列跳过——若把 "move" 当 upload_id 查
// GetSession("move")==nil 会误删 move 锁条目（超 10 分钟长 move 持锁被清理 → 同 rel 并发 move
// 越过锁）。value=裸 upload_id 且无对应 session 的条目仍应被清理。
func TestCleanupUploadingFilesPass_SkipsMoveLock(t *testing.T) {
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	h := buildVolSetHandlers(t, cfg)

	h.uploadingFiles.Store("alice\x00upload.txt", "upload")         // 普通 upload：无 session，保留
	h.uploadingFiles.Store("alice\x00move.txt", "move")             // move 锁：无 session，保留（回归）
	h.uploadingFiles.Store("alice\x00ghost.txt", "no-such-session") // 分块上传裸 id：session 不存在 → 清理
	h.cleanupUploadingFilesPass()

	for _, k := range []string{"alice\x00upload.txt", "alice\x00move.txt"} {
		if _, ok := h.uploadingFiles.Load(k); !ok {
			t.Fatalf("条目 %q 应保留（value=upload/move 无 session 直接跳过，修复前 move 被误删）", k)
		}
	}
	if _, ok := h.uploadingFiles.Load("alice\x00ghost.txt"); ok {
		t.Fatal("无对应 session 的分块上传条目应被清理")
	}
}

// TestVolumesAPI_List_VolSetNilReturnsEmpty GET /api/volumes volSet nil（旧装配/手工构造路径）
// 返回空列表 200（生产 RegisterRoutes 恒装配 volSet，此分支属防御覆盖）。
func TestVolumesAPI_List_VolSetNilReturnsEmpty(t *testing.T) {
	h := &Handlers{}
	req := httptest.NewRequest(http.MethodGet, "/api/volumes", nil)
	rr := httptest.NewRecorder()
	h.listVolumesHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("volSet nil GET /api/volumes status=%d want 200", rr.Code)
	}
	var out volumesListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rr.Body.String())
	}
	if len(out.Volumes) != 0 {
		t.Fatalf("volSet nil 应返回空列表, got %+v", out.Volumes)
	}
}

// TestVolumesAPI_LocalMuxRegistered 直接断言两条新卷路由在隧道内层 localMux 裸注册可达
// （TestLocalMuxCoversAllTunnelRoutes 只探主 mux srvMux，对 localMux 侧是间接保证；此处
// 直打 LocalHandler()：GET /api/volumes → 200、POST /api/volumes/move 缺参 → 400，均非 404）。
func TestVolumesAPI_LocalMuxRegistered(t *testing.T) {
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	noAuth := defaultNoAuthRegOpts()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:                   mux,
		CfgPtr:                &cfgPtr,
		Version:               "v",
		BuildAt:               "b",
		Logger:                testLogger(),
		CredentialRing:        noAuth.CredentialRing,
		CredentialStore:       noAuth.CredentialStore,
		AllowInsecureLoopback: noAuth.AllowInsecureLoopback,
	})
	t.Cleanup(func() { _ = h.Close() })
	lh := h.LocalHandler()

	// GET /api/volumes 隧道内层可达 → handler 处理（单卷默认开放返回 200），非 404。
	w := httptest.NewRecorder()
	lh.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/volumes", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("localMux GET /api/volumes status=%d want 200（body=%s）", w.Code, w.Body.String())
	}
	// POST /api/volumes/move 隧道内层可达 → 缺参 handler 返回 400，非 404 page not found。
	w2 := httptest.NewRecorder()
	lh.ServeHTTP(w2, httptest.NewRequest(http.MethodPost, "/api/volumes/move", nil))
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("localMux POST /api/volumes/move status=%d want 400（body=%s）", w2.Code, w2.Body.String())
	}
}

// TestVolumesAPI_Move_ConcurrentDeleteIsNotExistReleasesOnce 回归 PR-D F1：并发 delete（不持
// uploadingFiles 锁）在 move stat 与 Remove 之间已删源 → move Remove 返 IsNotExist——该分支
// **只 commit to 侧**、绝不再 from 侧释放（并发 delete 已释放过 owner 全局 + from 卷池，再释放
// 即欠计）。用 removeMovedSource seam 确定性模拟 IsNotExist（真实并发时序跨平台不可确定）；
// 预置「并发 delete 已完成 from 侧释放」的账本态（owner 全局 + main 卷池各 -S），断言 move 后
// 不因第二次 IsNotExist 再减（owner 全局 = S+T、main 池 = T、disk2 池 = S）。
func TestVolumesAPI_Move_ConcurrentDeleteIsNotExistReleasesOnce(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 1 << 20},
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}
	url, h, _ := newVolumesAPIServer(t, "alice", volumes, nil)

	bodyA := []byte("AAAAAAAAAA") // 10B（将移动）
	bodyB := []byte("BBBBBBB")    // 7B（留在 main，作池非零基线以暴露双释放）
	volumeUpload(t, url, "a.txt", bodyA, "")
	volumeUpload(t, url, "b.txt", bodyB, "")
	sA, sB := int64(len(bodyA)), int64(len(bodyB))
	if got := h.quotaBucketFor("alice", "user").Usage(); got != sA+sB {
		t.Fatalf("初始 owner user Scope=%d want %d", got, sA+sB)
	}
	if got := h.volSet.Pool("main").Usage(); got != sA+sB {
		t.Fatalf("初始 main 池=%d want %d", got, sA+sB)
	}

	// 模拟并发 delete 已完成 a.txt 的 from 侧释放（owner 全局 + main 卷池各 -S）；源文件仍物理
	// 在盘使 move 的 stat 通过、复制成功。Remove seam 返回 IsNotExist 模拟「delete 在 stat 与
	// Remove 间删源」。
	h.quotaBucketFor("alice", "user").ReleaseUsage(sA)
	h.volSet.Pool("main").ReleaseCommitted(sA)
	orig := removeMovedSource
	removeMovedSource = func(*storage.Root, string) error { return os.ErrNotExist }
	t.Cleanup(func() { removeMovedSource = orig })

	status, respBody := moveVolume(t, url, "main", "disk2", "a.txt")
	if status != http.StatusOK {
		t.Fatalf("IsNotExist 分支应回成功（数据已落目标卷）, got %d %s", status, respBody)
	}

	// 关键断言：owner 全局与 main 卷池只释放一次——a.txt 的 from 释放已由并发 delete 完成，
	// IsNotExist 分支不得再 scope.ReleaseUsage / fromPool.ReleaseCommitted（预修复双释放 → owner
	// 欠计 B、main 池把 B 也扣掉/钳 0）。
	if got := h.quotaBucketFor("alice", "user").Usage(); got != sA+sB {
		t.Fatalf("owner user Scope=%d want %d（a.txt 落 disk2 + b.txt 留 main；IsNotExist 双释放会欠计）", got, sA+sB)
	}
	if got := h.volSet.Pool("main").Usage(); got != sB {
		t.Fatalf("main 池=%d want %d（只剩 b.txt；IsNotExist 再释放会连 b.txt 也扣掉）", got, sB)
	}
	if got := h.volSet.Pool("disk2").Usage(); got != sA {
		t.Fatalf("disk2 池=%d want %d（a.txt 已落 disk2）", got, sA)
	}
}

// TestVolumesAPI_Move_SameVolumeNoop from==to：文件已在目标卷 → 200 无操作（幂等）。
func TestVolumesAPI_Move_SameVolumeNoop(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 1 << 20},
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}
	url, _, dirs := newVolumesAPIServer(t, "alice", volumes, nil)

	body := []byte("stay put")
	volumeUpload(t, url, "a.txt", body, "")

	moveSuccess(t, url, "main", "main", "a.txt")
	if !diskFileExists(t, dirs[0], "alice", "a.txt") {
		t.Fatal("同卷 move 不应搬走文件")
	}
	if diskFileExists(t, dirs[1], "alice", "a.txt") {
		t.Fatal("同卷 move 不应产生 disk2 副本")
	}
}

// TestVolumesAPI_Move_TargetExists409 AD-4 唯一性：目标卷已有同 rel（异常双份状态）→ 409，
// 源文件不动（不产生第三份/不静默覆盖）。
func TestVolumesAPI_Move_TargetExists409(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 1 << 20},
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}
	url, _, dirs := newVolumesAPIServer(t, "alice", volumes, nil)

	body := []byte("source on main")
	volumeUpload(t, url, "a.txt", body, "")
	// 异常双份状态：直接在 disk2 落同 rel 遗留（AD-4 约束外的防御场景）。
	seedVolumeFile(t, dirs[1], "alice", "user/a.txt", []byte("target existing"))

	status, bodyResp := moveVolume(t, url, "main", "disk2", "a.txt")
	if status != http.StatusConflict {
		t.Fatalf("move 到已有同 rel 卷 status=%d want 409, body=%s", status, bodyResp)
	}
	if !diskFileExists(t, dirs[0], "alice", "a.txt") {
		t.Fatal("409 后源文件应保留在 main")
	}
	got, _ := os.ReadFile(filepath.Join(dirs[0], "alice", "user", "a.txt"))
	if !bytes.Equal(got, body) {
		t.Fatalf("源文件内容被改动: %q want %q", got, body)
	}
}

// TestVolumesAPI_Move_ACLDenied to/from 卷不在 owner 视图（allow 白名单收紧）→ 403；
// 源在 from 卷但 to 不在视图 → 403；from 不在视图 → 403（不泄存在性）。
func TestVolumesAPI_Move_ACLDenied(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 1 << 20},
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
		{Name: "priv", Root: dirs[2], VolCapacity: 1 << 20,
			ACL: &VolumeACLConfig{Mode: VolumeACLAllow, Owners: []string{"bob"}}},
	}
	url, _, dirs := newVolumesAPIServer(t, "alice", volumes, nil)

	body := []byte("alice file")
	volumeUpload(t, url, "a.txt", body, "")
	// priv 不在 alice 视图 → 403（即使源在 main 可读）。
	status, bodyResp := moveVolume(t, url, "main", "priv", "a.txt")
	if status != http.StatusForbidden {
		t.Fatalf("to 卷不在视图 status=%d want 403, body=%s", status, bodyResp)
	}
	// from 卷不在视图（priv 只属 bob）→ 403。
	status, _ = moveVolume(t, url, "priv", "main", "a.txt")
	if status != http.StatusForbidden {
		t.Fatalf("from 卷不在视图 status=%d want 403", status)
	}
	if !diskFileExists(t, dirs[0], "alice", "a.txt") {
		t.Fatal("403 后源文件应保留")
	}
}

// TestVolumesAPI_Move_SourceNotFound404 from 卷上源不存在 → 404。
func TestVolumesAPI_Move_SourceNotFound404(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 1 << 20},
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}
	url, _, _ := newVolumesAPIServer(t, "alice", volumes, nil)

	status, body := moveVolume(t, url, "main", "disk2", "ghost.txt")
	if status != http.StatusNotFound {
		t.Fatalf("move 不存在源 status=%d want 404, body=%s", status, body)
	}
}

// TestVolumesAPI_Move_RejectsDirectory move 目标是目录（非文件）→ 400（跨卷目录移动不在
// 本 API 范围——流式复制语义针对文件）。
func TestVolumesAPI_Move_RejectsDirectory(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 1 << 20},
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}
	url, _, dirs := newVolumesAPIServer(t, "alice", volumes, nil)

	// 直接落盘建 user/sub 目录（multipart 文件名会被 Go 清洗为 basename，不能经上传建目录）。
	seedVolumeFile(t, dirs[0], "alice", "user/sub/.keep", []byte("x"))

	status, respBody := moveVolume(t, url, "main", "disk2", "sub")
	if status != http.StatusBadRequest {
		t.Fatalf("move 目录 status=%d want 400, body=%s", status, respBody)
	}
}

// TestVolumesAPI_Move_CapacityInsufficient507 目标卷容量不足（reserve 失败）→ 507；源不动。
func TestVolumesAPI_Move_CapacityInsufficient507(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 1 << 20},
		{Name: "disk2", Root: dirs[1], VolCapacity: 5}, // 放不下 8B 文件
	}
	url, h, dirs := newVolumesAPIServer(t, "alice", volumes, nil)

	body := []byte("12345678") // 8B
	status, _, respBody := volumeUpload(t, url, "a.txt", body, "")
	if status != http.StatusOK {
		t.Fatalf("上传应 200（落 main）, got %d %s", status, respBody)
	}
	status, bodyResp := moveVolume(t, url, "main", "disk2", "a.txt")
	if status != http.StatusInsufficientStorage {
		t.Fatalf("目标卷容量不足 move status=%d want 507, body=%s", status, bodyResp)
	}
	if !diskFileExists(t, dirs[0], "alice", "a.txt") {
		t.Fatal("507 后源应保留在 main")
	}
	if diskFileExists(t, dirs[1], "alice", "a.txt") {
		t.Fatal("507 后目标卷不得落盘")
	}
	if got := h.volSet.Pool("disk2").Usage(); got != 0 {
		t.Fatalf("507 后 disk2 池=%d want 0（预留已回滚）", got)
	}
	if got := h.volSet.Pool("main").Usage(); got != int64(len(body)) {
		t.Fatalf("507 后 main 池=%d want %d", got, len(body))
	}
}

// TestVolumesAPI_Move_Concurrent 并发同文件 move：恰 1 成功，其余 409（锁）或 404（已迁走），
// 绝无 500 / 数据损坏；最终文件完整落 disk2，双账本一致（main 0 / disk2 size）。
func TestVolumesAPI_Move_Concurrent(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 1 << 20},
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}
	url, h, dirs := newVolumesAPIServer(t, "alice", volumes, nil)

	body := []byte("concurrent move payload")
	status, _, respBody := volumeUpload(t, url, "c.txt", body, "")
	if status != http.StatusOK {
		t.Fatalf("上传应 200, got %d %s", status, respBody)
	}

	const n = 6
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := make([]int, n)
	transportErrs := 0
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _, err := moveVolumeCore(url, "main", "disk2", "c.txt")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				transportErrs++
				codes[i] = -1
				return
			}
			codes[i] = code
		}(i)
	}
	wg.Wait()
	if transportErrs != 0 {
		t.Fatalf("%d 个并发 move 传输失败", transportErrs)
	}
	ok, notFound, conflict, other := 0, 0, 0, 0
	for i := range n {
		switch codes[i] {
		case http.StatusOK:
			ok++
		case http.StatusNotFound:
			notFound++
		case http.StatusConflict:
			conflict++
		default:
			other++
			t.Logf("并发 move #%d status=%d", i, codes[i])
		}
	}
	if ok != 1 {
		t.Fatalf("并发 move 应恰 1 成功, got ok=%d notFound=%d conflict=%d other=%d", ok, notFound, conflict, other)
	}
	if other != 0 {
		t.Fatalf("并发 move 不应有其它状态（500 等）, other=%d", other)
	}
	// 最终一致性：文件完整在 disk2，main 无。
	if diskFileExists(t, dirs[0], "alice", "c.txt") {
		t.Fatal("并发 move 后 main 不应残留 c.txt")
	}
	if !diskFileExists(t, dirs[1], "alice", "c.txt") {
		t.Fatal("并发 move 后 c.txt 应在 disk2")
	}
	got, err := os.ReadFile(filepath.Join(dirs[1], "alice", "user", "c.txt"))
	if err != nil {
		t.Fatalf("read disk2 c.txt: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("并发 move 后内容损坏: %q want %q", got, body)
	}
	if got := h.volSet.Pool("main").Usage(); got != 0 {
		t.Fatalf("并发 move 后 main 池=%d want 0", got)
	}
	if got := h.volSet.Pool("disk2").Usage(); got != int64(len(body)) {
		t.Fatalf("并发 move 后 disk2 池=%d want %d", got, len(body))
	}
}

// TestVersionBucket_VolumePoolLedger T6a 发现-3：版本字节写/删必须计入 home 卷容量池
// （磁盘2 文件的版本写 disk2/version，不再只靠 reconcile 自愈）。
func TestVersionBucket_VolumePoolLedger(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 5}, // 容量小 → 文件落 disk2
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}
	url, h, dirs := versionFeatureServer(t, "alice", volumes, func(c *Config) {
		c.Versioning.Enabled = true
		c.Versioning.MaxVersions = 10
	})

	body1 := []byte("AAAAAAAAAA") // 10B > main 5 → disk2
	status, _, respBody := volumeUpload(t, url, "f.txt", body1, "")
	if status != http.StatusOK {
		t.Fatalf("首次上传应 200, got %d %s", status, respBody)
	}
	if !diskFileExists(t, dirs[1], "alice", "f.txt") {
		t.Fatal("f.txt 应落 disk2")
	}
	if got := h.volSet.Pool("disk2").Usage(); got != 10 {
		t.Fatalf("首次上传后 disk2 池=%d want 10", got)
	}

	body2 := []byte("BBBBBBBBBBBBBB") // 14B
	status, _, respBody = volumeUpload(t, url, "f.txt", body2, "")
	if status != http.StatusOK {
		t.Fatalf("覆盖写应 200, got %d %s", status, respBody)
	}
	// disk2 池 = user 14 + version 10 = 24（版本桶字节已入卷池）。
	if got := h.volSet.Pool("disk2").Usage(); got != 24 {
		t.Fatalf("覆盖写后 disk2 池=%d want 24（user 14 + version 10，T6c 版本桶入账）", got)
	}
	if got := h.volSet.Pool("main").Usage(); got != 0 {
		t.Fatalf("main 池=%d want 0（版本不落 main）", got)
	}
	// owner 全局 version 桶 Scope 也入账（单卷既有语义不回归）。
	if got := h.quotaBucketFor("alice", "version").Usage(); got != 10 {
		t.Fatalf("owner version 桶 Scope=%d want 10", got)
	}

	// 删除版本 → 释放 10 → disk2 池 14。
	verDirDisk2 := filepath.Join(dirs[1], "alice", "version", "f.txt")
	entries, err := os.ReadDir(verDirDisk2)
	if err != nil {
		t.Fatalf("disk2 version 目录应存在: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("应恰 1 个版本, got %d", len(entries))
	}
	delReq, _ := http.NewRequest("DELETE",
		fmt.Sprintf("%s/api/versions?filename=f.txt&version_id=%s", url, entries[0].Name()), nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("delete-version: %v", err)
	}
	delBody, _ := io.ReadAll(delResp.Body)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete-version 应 200, got %d %s", delResp.StatusCode, delBody)
	}
	if got := h.volSet.Pool("disk2").Usage(); got != 14 {
		t.Fatalf("删版本后 disk2 池=%d want 14（版本字节已释放）", got)
	}
	if got := h.quotaBucketFor("alice", "version").Usage(); got != 0 {
		t.Fatalf("删版本后 owner version 桶 Scope=%d want 0", got)
	}

	// 删除 user 文件 → 释放 14 → disk2 池 0。
	delStatus, delBody := deleteFile(t, url, "f.txt", body2)
	if delStatus != http.StatusOK {
		t.Fatalf("delete user 应 200, got %d %s", delStatus, delBody)
	}
	if got := h.volSet.Pool("disk2").Usage(); got != 0 {
		t.Fatalf("删 user 文件后 disk2 池=%d want 0", got)
	}
}
