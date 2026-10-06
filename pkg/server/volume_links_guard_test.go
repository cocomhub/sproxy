// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_links_guard_test.go 第二轮对抗性评审（嵌套封装接线）修复的回归测试：
//   - Fix 3 建卷 TOCTOU：register 内互斥重叠判定（并发建卷重叠子目录仅一成功）；
//   - Fix 4 links() 重建时序：store 未装配先建空表不缓存，Set 后恢复；扫描失败重试；
//   - Fix 2 写保护旁路闭环：move / S3 PUT-DELETE / WebDAV / 备份恢复 / sync 冲突写回 /
//     cloud 转存预检 —— 对占用子目录全部 403/拒绝，正例放行。

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// ---- Fix 3：register 互斥重叠判定（建卷 TOCTOU 收口） ----

// TestVolumeLinks_Register_OverlapRejected register 层互斥判定：与既有占用路径重叠（含
// 子/父/同目录）→ false；前缀相似不重叠 / 不同分支 / 跨卷 → true。
func TestVolumeLinks_Register_OverlapRejected(t *testing.T) {
	t.Parallel()
	r := newVolumeLinksRegistry()
	if !r.register(volumeLink{Base: "main", Subdir: "videos", Wrapper: "wrap1", Owner: "a"}) {
		t.Fatal("首个占用 main/videos 应登记成功")
	}
	if r.register(volumeLink{Base: "main", Subdir: "videos", Wrapper: "wrap2", Owner: "a"}) {
		t.Fatal("同目录（另一 wrapper）应拒绝")
	}
	if r.register(volumeLink{Base: "main", Subdir: "videos/sub", Wrapper: "wrap3", Owner: "a"}) {
		t.Fatal("占用子路径（wrap1 的 videos 是父）应拒绝")
	}
	if !r.register(volumeLink{Base: "main", Subdir: "vide", Wrapper: "wrap4", Owner: "a"}) {
		t.Fatal("main/vide 与 main/videos 前缀相似但不重叠，应登记成功")
	}
	if !r.register(volumeLink{Base: "main", Subdir: "photos", Wrapper: "wrap5", Owner: "a"}) {
		t.Fatal("同 base 不同分支应登记成功")
	}
	if r.register(volumeLink{Base: "main", Subdir: "photos/sub", Wrapper: "wrap7", Owner: "a"}) {
		t.Fatal("占用子路径（wrap5 的 photos 是父）应拒绝")
	}
	if !r.register(volumeLink{Base: "other", Subdir: "videos", Wrapper: "wrap6", Owner: "a"}) {
		t.Fatal("跨卷（other/videos）应登记成功")
	}
}

// TestVolumeLinks_ConcurrentCreate_Overlap_OnlyOneWins 并发建卷（同 base 重叠子目录）恰一
// 成功（register 内互斥判定 fail-closed，其余回滚 409）。
func TestVolumeLinks_ConcurrentCreate_Overlap_OnlyOneWins(t *testing.T) {
	t.Parallel()
	h, _ := newNestedWrapperAPIHandlers(t)
	mux := userVolWrap(h, "alice")
	const n = 8
	results := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := postUserVol(t, mux, map[string]any{
				"name": "cwrap" + string(rune('a'+i)), "type": userVolWrapTestType,
				"extra": map[string]any{"target": "main/videos"},
			})
			results[i] = rec.Code
		}(i)
	}
	wg.Wait()
	var ok int
	for _, c := range results {
		if c == http.StatusOK {
			ok++
		} else if c != http.StatusConflict {
			t.Fatalf("并发建卷应 200 或 409，got %d", c)
		}
	}
	if ok != 1 {
		t.Fatalf("并发重叠子目录应恰 1 成功，got %d（results=%v）", ok, results)
	}
	if refs := h.links().refsOfBase("main"); len(refs) != 1 {
		t.Fatalf("main 应恰 1 条占用关联，got %+v", refs)
	}
}

// ---- Fix 4：links() 重建时序（store 后置装配 / 扫描失败重试） ----

// newRestoreHandlers 构造无 userVolumes store 的嵌套封装 Handlers（buildVolSetHandlers
// 的 userVolumes 为 nil——复现「启动竞序：Once 首次触达时 store 未装配」）。
func newRestoreHandlers(t *testing.T) *Handlers {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	return buildVolSetHandlers(t, cfg)
}

// TestVolumeLinks_Restore_AfterStoreSet 修复 I-3 时序：Once 首次触发时 store 未装配 → 建空表
// 不缓存；store Set 后下次 links() 恢复已持久化占用（首个动态卷立即 403）。
func TestVolumeLinks_Restore_AfterStoreSet(t *testing.T) {
	t.Parallel()
	h := newRestoreHandlers(t)
	// 首次触达 links()：store 未装配 → 空表（修复前 Once 缓存空重建，旧占用永久不恢复）。
	_ = h.links()
	if err := h.checkWrapperOccupiedWrite("main", "videos/x"); err != nil {
		t.Fatalf("store 未装配时不应有占用: %v", err)
	}
	// 构造已持久化 wrapper 的 store（模拟重启后装配）并注入。
	store := NewUserVolumeStore(h.volSet.Default().RootDir)
	owner := "alice"
	if err := store.Create(owner, UserVolume{
		Name: "vault", Type: userVolWrapTestType, Owner: owner,
		Extra: map[string]any{"target": "main/videos"},
	}); err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	h.SetUserVolumeStore(store)
	// 下次 links() → tryRestoreLinks 恢复占用 → 写保护立即生效（重启后首个动态卷 403）。
	_ = h.links()
	if err := h.checkWrapperOccupiedWrite("main", "videos/x"); err == nil {
		t.Fatal("store Set 后应恢复 main/videos 占用（写保护应命中 403）")
	}
	refs := h.links().refsOfBase("main")
	if len(refs) != 1 || refs[0].Wrapper != "vault" || refs[0].Subdir != "videos" {
		t.Fatalf("恢复后 main 应恰 1 条 {videos,vault}, got %+v", refs)
	}
}

// TestVolumeLinks_Restore_ScanFailureRetries 修复 I-3 重试：ScanRestore 失败（损坏 store 文件
// → Get 解析失败，读盘错误）→ 不标记；修复后下次 links() 恢复（Once 不再吞失败）。
func TestVolumeLinks_Restore_ScanFailureRetries(t *testing.T) {
	t.Parallel()
	h := newRestoreHandlers(t)
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := NewUserVolumeStore(storeRoot)
	// 损坏的卷 JSON → ScanRestore 的 Get 解析失败 → 扫描整体失败（不标记，须可重试）。
	badVolDir := filepath.Join(storeRoot, "alice", "meta", "volume")
	if err := os.MkdirAll(badVolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badVolDir, "bad.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.SetUserVolumeStore(store)
	_ = h.links()
	if h.linksRestored.Load() {
		t.Fatal("ScanRestore 失败不应标记 restored（须可重试）")
	}
	// 修复 store：清掉坏文件 + 落真实 wrapper → 下次 links() 恢复成功并标记。
	if err := os.Remove(filepath.Join(badVolDir, "bad.json")); err != nil {
		t.Fatal(err)
	}
	if err := store.Create("alice", UserVolume{
		Name: "vault", Type: userVolWrapTestType, Owner: "alice",
		Extra: map[string]any{"target": "main/videos"},
	}); err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	_ = h.links()
	if !h.linksRestored.Load() {
		t.Fatal("ScanRestore 成功后应标记 restored")
	}
	if err := h.checkWrapperOccupiedWrite("main", "videos/x"); err == nil {
		t.Fatal("扫描重试后应恢复 main/videos 占用（写保护应命中）")
	}
}

// ---- Fix 2：写保护旁路闭环 ----

// occupiedMainHandlers 构造带「main/videos 被 vault 占用」的 Handlers（共享辅助）。
func occupiedMainHandlers(t *testing.T) *Handlers {
	t.Helper()
	h := newRestoreHandlers(t)
	if !h.links().register(volumeLink{Base: "main", Subdir: "videos", Wrapper: "vault", Owner: "alice"}) {
		t.Fatal("register main/videos 应成功")
	}
	return h
}

// TestMove_OccupiedTarget_Rejected 跨卷 move 目标命中占用子目录 → 403；非占用 → 200。
func TestMove_OccupiedTarget_Rejected(t *testing.T) {
	t.Parallel()
	dirs := []string{t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 1 << 20},
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}
	url, h, _ := newVolumesAPIServer(t, "alice", volumes, nil)
	if !h.links().register(volumeLink{Base: "main", Subdir: "videos", Wrapper: "vault", Owner: "alice"}) {
		t.Fatal("register main/videos 应成功")
	}
	body := []byte("move me")
	// 源文件落 disk2 的 user/videos/x.txt（占用目标是 main/videos，源侧 disk2 未占用）。
	// 直接落盘（上传会剥离子目录路径；账本：from 侧释放防下溢归零，不影响断言）。
	srcRel := filepath.Join(dirs[1], "alice", "user", "videos")
	if err := os.MkdirAll(srcRel, 0o755); err != nil {
		t.Fatalf("mkdir disk2 user/videos: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcRel, "x.txt"), body, 0o600); err != nil {
		t.Fatalf("write disk2 videos/x.txt: %v", err)
	}
	status, respBody := moveVolume(t, url, "disk2", "main", "videos/x.txt")
	if status != http.StatusForbidden {
		t.Fatalf("move 到占用子目录 = %d, want 403 (body: %s)", status, respBody)
	}
	// 正例：free.txt 从 disk2 → main（未占用）→ 200。
	if status, _, respBody := volumeUpload(t, url, "free.txt", body, "disk2"); status != http.StatusOK {
		t.Fatalf("上传 free.txt 到 disk2 应 200, got %d %s", status, respBody)
	}
	moveSuccess(t, url, "disk2", "main", "free.txt")
}

// TestS3_OccupiedWrite_Rejected S3 PUT/DELETE 目标命中占用子目录 → 403；非占用 → 201/204。
func TestS3_OccupiedWrite_Rejected(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	h.credentialRing = ringForTestCreds()
	if !h.links().register(volumeLink{Base: "main", Subdir: "videos", Wrapper: "vault", Owner: testAccessKey}) {
		t.Fatal("register main/videos 应成功")
	}
	host := "127.0.0.1"
	now := time.Now()
	body := []byte("s3 data")

	// PUT 占用子目录 → 403（写前 guard，不落盘）。
	req, _ := http.NewRequest(http.MethodPut, "/s3/videos/x.txt", bytes.NewReader(body))
	req.Host = host
	req.Header.Set("Authorization", sigV4SignHost(testAccessKey, testAccessSecret, http.MethodPut, "/s3/videos/x.txt", host, body, now))
	req.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", hex.EncodeToString(sha256sum(body)))
	rec := httptest.NewRecorder()
	h.s3Handler(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("S3 PUT 占用子目录 = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(cfg.StorageRoot, testAccessKey, "user", "videos", "x.txt")); !os.IsNotExist(err) {
		t.Fatal("guard 拒绝的 PUT 不应落盘")
	}

	// DELETE 占用子目录对象 → 403（先放一个文件到占用目录外再尝试删占用目录文件——占用目录
	// 下不应有文件；此处直接 DELETE videos/x.txt 由 guard 拦截，与 PUT 同语义）。
	delReq, _ := http.NewRequest(http.MethodDelete, "/s3/videos/x.txt", nil)
	delReq.Host = host
	delReq.Header.Set("Authorization", sigV4SignHost(testAccessKey, testAccessSecret, http.MethodDelete, "/s3/videos/x.txt", host, nil, now))
	delReq.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	delReq.Header.Set("x-amz-content-sha256", hex.EncodeToString(sha256sum(nil)))
	delRec := httptest.NewRecorder()
	h.s3Handler(delRec, delReq)
	if delRec.Code != http.StatusForbidden {
		t.Fatalf("S3 DELETE 占用子目录 = %d, want 403 (body: %s)", delRec.Code, delRec.Body.String())
	}

	// 正例：PUT /s3/free.txt → 201；DELETE /s3/free.txt → 204。
	okReq, _ := http.NewRequest(http.MethodPut, "/s3/free.txt", bytes.NewReader(body))
	okReq.Host = host
	okReq.Header.Set("Authorization", sigV4SignHost(testAccessKey, testAccessSecret, http.MethodPut, "/s3/free.txt", host, body, now))
	okReq.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	okReq.Header.Set("x-amz-content-sha256", hex.EncodeToString(sha256sum(body)))
	okRec := httptest.NewRecorder()
	h.s3Handler(okRec, okReq)
	if okRec.Code != http.StatusCreated {
		t.Fatalf("S3 PUT free.txt = %d, want 201 (body: %s)", okRec.Code, okRec.Body.String())
	}
	delOk, _ := http.NewRequest(http.MethodDelete, "/s3/free.txt", nil)
	delOk.Host = host
	delOk.Header.Set("Authorization", sigV4SignHost(testAccessKey, testAccessSecret, http.MethodDelete, "/s3/free.txt", host, nil, now))
	delOk.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	delOk.Header.Set("x-amz-content-sha256", hex.EncodeToString(sha256sum(nil)))
	delOkRec := httptest.NewRecorder()
	h.s3Handler(delOkRec, delOk)
	if delOkRec.Code != http.StatusNoContent {
		t.Fatalf("S3 DELETE free.txt = %d, want 204 (body: %s)", delOkRec.Code, delOkRec.Body.String())
	}
}

// TestDAV_OccupiedWrite_Rejected WebDAV 写方法目标命中占用子目录 → 403（预检）；非占用 → 201。
func TestDAV_OccupiedWrite_Rejected(t *testing.T) {
	t.Parallel()
	h := occupiedMainHandlers(t)

	// 反例：PUT /dav/videos/x.txt → 403。
	req := httptest.NewRequest(http.MethodPut, "/dav/videos/x.txt", strings.NewReader("data"))
	req = req.WithContext(withActor(req.Context(), "alice"))
	rec := httptest.NewRecorder()
	h.davHandler(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("WebDAV PUT 占用子目录 = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}

	// 反例：MKCOL 占用子目录 → 403。
	mkReq := httptest.NewRequest("MKCOL", "/dav/videos", nil)
	mkReq = mkReq.WithContext(withActor(mkReq.Context(), "alice"))
	mkRec := httptest.NewRecorder()
	h.davHandler(mkRec, mkReq)
	if mkRec.Code != http.StatusForbidden {
		t.Fatalf("WebDAV MKCOL 占用子目录 = %d, want 403 (body: %s)", mkRec.Code, mkRec.Body.String())
	}

	// 正例：PUT /dav/free.txt → 201/204（写保护不拦非占用）。
	okReq := httptest.NewRequest(http.MethodPut, "/dav/free.txt", strings.NewReader("ok"))
	okReq = okReq.WithContext(withActor(okReq.Context(), "alice"))
	okRec := httptest.NewRecorder()
	h.davHandler(okRec, okReq)
	if okRec.Code != http.StatusCreated && okRec.Code != http.StatusNoContent {
		t.Fatalf("WebDAV PUT free.txt = %d, want 201/204 (body: %s)", okRec.Code, okRec.Body.String())
	}
}

// TestBackup_OccupiedTargetFS_RejectsWrite 备份恢复目标 FS（backupTargetFSBase 本地卷分支）
// 包上占用写保护：写占用子目录拒绝；写非占用放行。
func TestBackup_OccupiedTargetFS_RejectsWrite(t *testing.T) {
	t.Parallel()
	h := occupiedMainHandlers(t)
	fs, err := h.backupTargetFSBase(context.Background(), "alice", "main")
	if err != nil {
		t.Fatalf("backupTargetFSBase: %v", err)
	}
	// 反例：写 videos/x.txt → 拒绝。
	if werr := fs.WriteFile(context.Background(), "videos/x.txt", strings.NewReader("d"), 1, 0); werr == nil {
		t.Fatal("备份恢复写占用子目录应被拒绝")
	}
	// 正例：写 free.txt → 放行。
	if werr := fs.WriteFile(context.Background(), "free.txt", strings.NewReader("ok"), 2, 0); werr != nil {
		t.Fatalf("备份恢复写非占用目录应放行: %v", werr)
	}
}

// TestSyncConflictWrite_Occupied_Rejected sync 冲突写回（writeConflictFile 直写默认卷 user
// 桶）占用子目录 → 拒绝；非占用 → 放行。
func TestSyncConflictWrite_Occupied_Rejected(t *testing.T) {
	t.Parallel()
	h := occupiedMainHandlers(t)
	// 反例：写回 videos/x.txt → 拒绝（writeConflictFile 的 rel 形如 user/videos/x.txt）。
	if werr := h.writeConflictFile("user/videos/x.txt", []byte("content")); werr == nil {
		t.Fatal("sync 冲突写回占用子目录应被拒绝")
	}
	// 正例：写回 free.txt → 放行。
	if werr := h.writeConflictFile("user/free.txt", []byte("content")); werr != nil {
		t.Fatalf("sync 冲突写回非占用目录应放行: %v", werr)
	}
}

// TestCheckTransferACL_Occupied_Rejected cloud 转存创建期预检：显式路径命中占用子目录 →
// 返回错误文案（handler 映射 403）；非占用/自动派生路径 → ""。
func TestCheckTransferACL_Occupied_Rejected(t *testing.T) {
	t.Parallel()
	h := occupiedMainHandlers(t)
	// 反例：显式路径 videos/x.mp4 → 占用拒绝。
	msg := h.checkTransferACL("alice", &cloud.TransferSpec{Volume: "main", Path: "videos/x.mp4"})
	if msg == "" || !strings.Contains(msg, "占用") {
		t.Fatalf("转存占用路径应返回占用文案, got %q", msg)
	}
	// 正例：非占用路径 → 放行。
	if msg2 := h.checkTransferACL("alice", &cloud.TransferSpec{Volume: "main", Path: "free/x.mp4"}); msg2 != "" {
		t.Fatalf("转存非占用路径应放行, got %q", msg2)
	}
	// 自动派生路径（Path 空，首段任务 ID 不可预判）→ 不预检（转存写前 guard 兜底）。
	if msg3 := h.checkTransferACL("alice", &cloud.TransferSpec{Volume: "main"}); msg3 != "" {
		t.Fatalf("转存自动派生路径不应预检, got %q", msg3)
	}
}

// TestOccupiedGuardFS_ReadPassThrough 占用写保护装饰器读方法透传不拦（读路径零回归）。
func TestOccupiedGuardFS_ReadPassThrough(t *testing.T) {
	t.Parallel()
	h := occupiedMainHandlers(t)
	fs, err := h.backupTargetFSBase(context.Background(), "alice", "main")
	if err != nil {
		t.Fatalf("backupTargetFSBase: %v", err)
	}
	// 写占用子目录拒绝后，读（ListDir/Stat/OpenRead）不拦（目录不存在 → Stat nil, nil）。
	if e, serr := fs.Stat(context.Background(), "videos/x.txt"); serr != nil {
		t.Fatalf("读 Stat 不应被写保护拦截: %v", serr)
	} else if e != nil {
		t.Fatalf("占用子目录文件不应存在: %+v", e)
	}
}

// TestTransferUserVisibleRel 坐标归一：转存键（[owner/]user/<rel>）→ 用户可见 rel。
func TestTransferUserVisibleRel(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"user/videos/x.mp4":       "videos/x.mp4",
		"alice/user/videos/x.mp4": "videos/x.mp4", // 共享卷 owner 前缀跳过
		"alice/user/videos":       "videos",
		"user/free/b.mp4":         "free/b.mp4",
	}
	for key, want := range cases {
		if got := volume.UserVisibleRel(key); got != want {
			t.Fatalf("UserVisibleRel(%q) = %q, want %q", key, got, want)
		}
	}
}

// TestCheckWrapperOccupiedWrite_ErrSentinel 写保护命中错误可 errors.Is 到 errVolumeOccupiedReadOnly。
func TestCheckWrapperOccupiedWrite_ErrSentinel(t *testing.T) {
	t.Parallel()
	h := occupiedMainHandlers(t)
	err := h.checkWrapperOccupiedWrite("main", "videos/x")
	if err == nil {
		t.Fatal("占用子目录写应命中")
	}
	if !errors.Is(err, errVolumeOccupiedReadOnly) {
		t.Fatalf("应包装哨兵 errVolumeOccupiedReadOnly, got %v", err)
	}
}
