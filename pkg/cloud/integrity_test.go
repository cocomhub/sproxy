// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/integrity"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// corruptPNGF 是"名为 .png 但内容非图片"的损坏载荷：ImageChecker 语义解码必然失败。
var corruptPNGF = []byte("corrupt-integrity-test-payload")

// newIntegrityTestMgr 构造注入完整性校验器 lookup 的真实下载器管理器。
// maxRetries 控制重下上限（供收敛用例走满 sames 累计节奏）。
// 测试装配：mgr.integrityLookup = integrity.Lookup（默认注册表代理，Image/Tar 校验器已由
// init 装配），使 corrupt png 能命中 ImageChecker。
func newIntegrityTestMgr(t *testing.T, maxRetries int) *CloudDownloadManager {
	t.Helper()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold:   4 * 1024 * 1024,
		MaxConcurrent:   3,
		TaskTTL:         24 * time.Hour,
		FailedTaskTTL:   time.Hour,
		AllowPrivate:    true,
		MaxRetries:      maxRetries,
		RetryDelay:      time.Millisecond,
		DownloadTimeout: 10 * time.Second,
	}
	mgr, _ := newCloudTestManager(t, dir, sm, cfg)
	mgr.integrityLookup = integrity.Lookup
	return mgr
}

// corruptServe 返回一个恒返回损坏 png 载荷的 HTTP 服务器。hits 记录请求次数（验证不无限重下）。
func corruptServe(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(corruptPNGF)))
		_, _ = w.Write(corruptPNGF)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestDownloadIntegrity_DamagedAllowsContinue：语义校验失败（损坏文件）→ 默认放行 +
// IntegrityStatus="damaged" + 任务 completed。两次 sames 一致仍异常即收敛（hits==2，不无限重下）。
func TestDownloadIntegrity_DamagedAllowsContinue(t *testing.T) {
	mgr := newIntegrityTestMgr(t, 3)
	srv, hits := corruptServe(t)

	task, err := mgr.SubmitAndStart("url", srv.URL, "bad.png", int64(len(corruptPNGF)), t.Context(), "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if task.Status != "completed" {
		t.Fatalf("damaged 语义异常应默认放行 completed，got %q (error=%q)", task.Status, task.Error)
	}
	if task.IntegrityStatus != "damaged" {
		t.Fatalf("IntegrityStatus 应为 damaged，got %q", task.IntegrityStatus)
	}
	if task.Error != "" {
		t.Fatalf("放行路径 task.Error 应为空，got %q", task.Error)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("损坏载荷应只下载 2 次即 permanent（wants hits=2），got %d", got)
	}
}

// TestDownloadIntegrity_ForceBlocks：ForceIntegrity=true + 语义校验失败 → 任务 failed（原因含 integrity）。
func TestDownloadIntegrity_ForceBlocks(t *testing.T) {
	mgr := newIntegrityTestMgr(t, 3)
	srv, _ := corruptServe(t)

	task, err := mgr.SubmitAndStart("url", srv.URL, "bad.png", int64(len(corruptPNGF)), t.Context(), "", TaskParams{Save: true, ForceIntegrity: true})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if task.Status != "failed" {
		t.Fatalf("ForceIntegrity 语义：损坏文件应失败，got %q", task.Status)
	}
	if !strings.Contains(task.Error, "integrity") {
		t.Fatalf("failed 错误应含 integrity 关键字，got %q", task.Error)
	}
	// Force parity：阻断路径不放行（IntegrityStatus 保持未设/未放行）。
	if task.IntegrityStatus == "damaged" {
		t.Fatalf("ForceIntegrity 失败路径不应置 damaged（放行标记），got %q", task.IntegrityStatus)
	}
}

// TestDownloadIntegrity_AuthoritySkipsSemantic：下载器 ModeAuthority + 权威匹配 → 跳过语义校验 → verified。
// 载荷仍为损坏 png（若走语义校验必 damaged），但权威下载器已确认 → 必须 verified。
func TestDownloadIntegrity_AuthoritySkipsSemantic(t *testing.T) {
	mgr := newIntegrityTestMgr(t, 2)
	srv, _ := corruptServe(t)

	fd := &fakeAuthorityDownloader{url: srv.URL, authHash: "authority-hash"}
	reg := downloader.NewRegistry()
	reg.Register(downloader.Plugin[downloader.Downloader]{Name: "fakeauthority", Instance: fd, Priority: 10})
	mgr.registry = reg

	task, err := mgr.SubmitAndStart("url", srv.URL, "bad.png", int64(len(corruptPNGF)), t.Context(), "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if task.Status != "completed" {
		t.Fatalf("权威匹配应完成任务，got %q", task.Status)
	}
	if task.IntegrityStatus != "verified" {
		t.Fatalf("ModeAuthority 应跳过语义校验并置 verified，got %q", task.IntegrityStatus)
	}
}

// TestDownloadIntegrity_TwiceConsistentStillDamaged：重下两次本地 checksum 一致仍异常 → damaged 放行
// （不因 MaxRetries 大而无限重下——Review Focus 3：2 次即收敛）。
func TestDownloadIntegrity_TwiceConsistentStillDamaged(t *testing.T) {
	mgr := newIntegrityTestMgr(t, 5)
	srv, hits := corruptServe(t)

	task, err := mgr.SubmitAndStart("url", srv.URL, "bad.png", int64(len(corruptPNGF)), t.Context(), "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if task.Status != "completed" {
		t.Fatalf("两次一致性仍异常应放行 completed，got %q (error=%q)", task.Status, task.Error)
	}
	if task.IntegrityStatus != "damaged" {
		t.Fatalf("IntegrityStatus 应为 damaged，got %q", task.IntegrityStatus)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("两次校验收敛：应 download 2 次，got %d（不因 MaxRetries=5 无限重下）", got)
	}
}

// fakeAuthorityDownloader 声明完整性归属为 ModeType=ModeAuthority（权威匹配），内部完成真实下载。
type fakeAuthorityDownloader struct {
	url      string
	authHash string
}

func (d *fakeAuthorityDownloader) Name() string                { return "fakeauthority" }
func (d *fakeAuthorityDownloader) Supports(source string) bool { return source == d.url }
func (d *fakeAuthorityDownloader) IntegrityMode() downloader.IntegrityMode {
	return downloader.ModeAuthority
}
func (d *fakeAuthorityDownloader) Download(ctx context.Context, source string, destPath string, prog downloader.ProgressFunc) (*downloader.Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(destPath, data, 0644); err != nil {
		return nil, err
	}
	if prog != nil {
		prog(int64(len(data)), int64(len(data)))
	}
	sum := sha256.Sum256(data)
	return &downloader.Result{
		Size:          int64(len(data)),
		Checksum:      hex.EncodeToString(sum[:]),
		Integrity:     downloader.ModeAuthority,
		AuthorityHash: d.authHash,
	}, nil
}

// TestDownloadIntegrity_TransientCorruptionRetries R1-C2 回归：瞬态损坏（两次下载
// checksum 不同）→ 不累计 permanent（不误判 damaged）——首次失败重试，内容变化后
// 校验通过 → completed + verified。
func TestDownloadIntegrity_TransientCorruptionRetries(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	serveCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		serveCount++
		n := serveCount
		mu.Unlock()
		if n == 1 {
			w.Write([]byte("corrupt-not-image")) // 首次：损坏内容
			return
		}
		// 后续：有效 PNG（1x1）
		w.Write(validPNG1x1)
	}))
	defer srv.Close()

	mgr := newIntegrityTestMgr(t, 3)       // MaxRetries=3：首次失败 + 重试成功
	mgr.integrityLookup = integrity.Lookup // 真实注册表（image checker）
	task, err := mgr.SubmitAndStart("url", srv.URL, "t.png", 0, t.Context(), "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	testutil.WaitFor(t, 10*time.Second, func() bool {
		cur, _ := mgr.SnapshotTask(task.ID, "")
		return cur != nil && cur.Status == "completed"
	}, "任务应完成")
	cur, _ := mgr.SnapshotTask(task.ID, "")
	if cur.IntegrityStatus != "verified" {
		t.Fatalf("瞬态损坏重试后应 verified，got %q", cur.IntegrityStatus)
	}
}

// validPNG1x1 是 1x1 有效 PNG（image/png 可解码，Bounds 1x1 非空）。
var validPNG1x1 = func() []byte {
	buf := &bytes.Buffer{}
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	if err := png.Encode(buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}()

// TestDownloadIntegrity_ResumeResetsState M1 回归：resume 重置 integrity 运行时累计
// （integritySames/LastChecksum/IntegrityStatus）——新下载会话从零判定。
func TestDownloadIntegrity_ResumeResetsState(t *testing.T) {
	t.Parallel()
	mgr := newIntegrityTestMgr(t, 3)
	mgr.integrityLookup = integrity.Lookup
	task := &CloudTask{ID: "t-resume", Filename: "r.bin", Status: "failed", Save: true}
	mgr.mu.Lock()
	mgr.tasks[task.ID] = task
	mgr.mu.Unlock()
	// 预置 stale 累计（模拟上次会话残留）
	mgr.mu.Lock()
	task.integritySames = 2
	task.integrityLastChecksum = "stale-checksum"
	task.IntegrityStatus = "damaged"
	mgr.mu.Unlock()
	if err := mgr.ResumeTask(task.ID, false, ""); err != nil {
		t.Fatalf("resume: %v", err)
	}
	mgr.mu.RLock()
	got := mgr.tasks[task.ID]
	mgr.mu.RUnlock()
	if got.integritySames != 0 || got.integrityLastChecksum != "" || got.IntegrityStatus != "" {
		t.Fatalf("M1: resume 应重置 integrity 状态，got sames=%d last=%q status=%q",
			got.integritySames, got.integrityLastChecksum, got.IntegrityStatus)
	}
}
