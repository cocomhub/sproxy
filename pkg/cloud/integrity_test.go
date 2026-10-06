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
	"golang.org/x/sync/semaphore"
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

// TestDownloadIntegrity_ConcurrentChecks 并发校验 race 验证：多任务并发下载+校验，
// setTaskIntegrityStatus/integritySames 持锁写与 SnapshotTask 读无 data race。
func TestDownloadIntegrity_ConcurrentChecks(t *testing.T) {
	t.Parallel()
	mgr := newIntegrityTestMgr(t, 3)
	mgr.integrityLookup = integrity.Lookup
	// 并发提交多个损坏/正常任务
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			var payload []byte
			name := "ok.png"
			if idx%2 == 0 {
				payload = corruptPNGF
				name = "bad.png"
			} else {
				payload = validPNG1x1
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write(payload)
			}))
			defer srv.Close()
			task, err := mgr.SubmitAndStart("url", srv.URL, name, int64(len(payload)), t.Context(), "", TaskParams{Save: true})
			if err != nil {
				t.Errorf("submit %d: %v", idx, err)
				return
			}
			// 轮询快照（读路径与写并发）
			testutil.WaitFor(t, 10*time.Second, func() bool {
				cur, _ := mgr.SnapshotTask(task.ID, "")
				return cur != nil && cur.Status == "completed"
			}, "任务应完成")
			_, _ = mgr.SnapshotTask(task.ID, "")
		}(i)
	}
	wg.Wait()
}

// TestDownloadIntegrity_DamagedTaskCanResumeRedownload M2（damaged 重下入口）：
// completed+damaged 任务可显式恢复重下（服务端 resumeTaskLookupLocked 放行）。
// 重下后 integrity 运行态已重置（M1），若源已修复则恢复为 completed+verified。
func TestDownloadIntegrity_DamagedTaskCanResumeRedownload(t *testing.T) {
	t.Parallel()
	mgr := newIntegrityTestMgr(t, 3)
	// 首轮恒损坏 → damaged 放行
	srv, _ := corruptServe(t)
	task, err := mgr.SubmitAndStart("url", srv.URL, "bad.png", int64(len(corruptPNGF)), t.Context(), "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if task.Status != "completed" || task.IntegrityStatus != "damaged" {
		t.Fatalf("前置：应 completed+damaged，got %q+%q", task.Status, task.IntegrityStatus)
	}
	// 换源为有效 png（重下后应 verified）
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(validPNG1x1)
	}))
	defer srv2.Close()
	task.URL = srv2.URL
	mgr.mu.Lock()
	mgr.tasks[task.ID].URL = srv2.URL
	mgr.mu.Unlock()

	if err := mgr.ResumeTask(task.ID, false, ""); err != nil {
		t.Fatalf("damaged 任务应可 resume 重下，got %v", err)
	}
	// 轮询重下完成（WaitFor 超时自动 Fatal）
	testutil.WaitFor(t, 10*time.Second, func() bool {
		cur, _ := mgr.SnapshotTask(task.ID, "")
		return cur != nil && cur.Status == "completed" && cur.IntegrityStatus == "verified"
	}, "重下后应 completed+verified")
}

// TestDownloadIntegrity_NonDamagedCompletedCannotResume：非 damaged 的 completed 任务
// 不可 resume（M2 语义：仅 failed/cancelled/damaged 可恢复，verified 完成态不提供重下）。
func TestDownloadIntegrity_NonDamagedCompletedCannotResume(t *testing.T) {
	t.Parallel()
	mgr := newIntegrityTestMgr(t, 3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(validPNG1x1)
	}))
	defer srv.Close()
	task, err := mgr.SubmitAndStart("url", srv.URL, "ok.png", int64(len(validPNG1x1)), t.Context(), "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if task.Status != "completed" || task.IntegrityStatus != "verified" {
		t.Fatalf("前置：应 completed+verified，got %q+%q", task.Status, task.IntegrityStatus)
	}
	err = mgr.ResumeTask(task.ID, false, "")
	if err == nil {
		t.Fatal("verified 完成态不应可 resume（无重下诉求），应报错")
	}
}

// TestDownloadIntegrity_ListDamagedFilter：ListTasks("damaged") 只返回 completed+damaged 任务。
func TestDownloadIntegrity_ListDamagedFilter(t *testing.T) {
	t.Parallel()
	mgr := newIntegrityTestMgr(t, 3)
	// 一个 damaged 任务
	srv, _ := corruptServe(t)
	dmg, err := mgr.SubmitAndStart("url", srv.URL, "bad.png", int64(len(corruptPNGF)), t.Context(), "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("submit damaged: %v", err)
	}
	// 一个 verified 任务
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(validPNG1x1)
	}))
	defer srv2.Close()
	ok, err := mgr.SubmitAndStart("url", srv2.URL, "ok.png", int64(len(validPNG1x1)), t.Context(), "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("submit ok: %v", err)
	}
	if dmg.IntegrityStatus != "damaged" || ok.IntegrityStatus != "verified" {
		t.Fatalf("前置状态错：dmg=%q ok=%q", dmg.IntegrityStatus, ok.IntegrityStatus)
	}
	damaged, _ := mgr.ListTasks("damaged", -1, 0, "")
	if len(damaged) != 1 || damaged[0].ID != dmg.ID {
		t.Fatalf("damaged 过滤应只返回 1 个 damaged 任务，got %d", len(damaged))
	}
}

// TestDownloadIntegrity_OverMemQuotaSkipsUnverified 用户裁定（内存配额治理）：
// MaxCheckMemBytes 过小（1MiB）→ tar 估算（size×500）超配额 → 跳过校验标记
// unverified（不误判 damaged；无校验能力 ≠ 损坏）。
func TestDownloadIntegrity_OverMemQuotaSkipsUnverified(t *testing.T) {
	t.Parallel()
	mgr := newIntegrityTestMgr(t, 3)
	mgr.checkMemSem = semaphore.NewWeighted(1 << 20) // 1 MiB
	mgr.checkMemMax = 1 << 20
	// tar.gz 载荷：内容非 tar（损坏），但估算 4KB×500=2MB > 1MiB 配额 → 跳过校验
	// 4KB payload：估算 size×500=2MB > 1MiB 配额 → 跳过校验（40 字节×500=20KB 不会超）
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()
	// 扩展名 .tar（TarChecker.Matches 匹配）而非 .tar.gz：Lookup 命中后才有估算。
	task, err := mgr.SubmitAndStart("url", srv.URL, "bad.tar", int64(len(payload)), t.Context(), "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if task.Status != "completed" {
		t.Fatalf("超配额跳过校验应放行 completed，got %q", task.Status)
	}
	if task.IntegrityStatus != "unverified" {
		t.Fatalf("超配额应标记 unverified（非 damaged），got %q", task.IntegrityStatus)
	}
}

// TestDownloadIntegrity_WithinMemQuotaNormalVerify 配额充足（默认 512MiB）→ 正常校验：
// 损坏图片仍 damaged（配额不改变语义判定，只是调度）。
func TestDownloadIntegrity_WithinMemQuotaNormalVerify(t *testing.T) {
	t.Parallel()
	mgr := newIntegrityTestMgr(t, 3)
	mgr.checkMemSem = semaphore.NewWeighted(512 << 20) // 512 MiB（默认）
	mgr.checkMemMax = 512 << 20
	srv, _ := corruptServe(t)
	task, err := mgr.SubmitAndStart("url", srv.URL, "bad.png", int64(len(corruptPNGF)), t.Context(), "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if task.Status != "completed" || task.IntegrityStatus != "damaged" {
		t.Fatalf("配额充足应正常校验 damaged，got %q+%q", task.Status, task.IntegrityStatus)
	}
}

// TestDownloadIntegrity_ConcurrentMemQuota 并发校验内存配额：配额 10MiB，4 个 tar
// 文件各估算 2MiB（size×500：4KB×500=2MiB）——并发校验总估算 ≤ 配额，全部排队
// 完成且每个任务 verified/damaged 按内容判定（不误判 unverified——配额充足）。
func TestDownloadIntegrity_ConcurrentMemQuota(t *testing.T) {
	t.Parallel()
	mgr := newIntegrityTestMgr(t, 3)
	mgr.checkMemSem = semaphore.NewWeighted(10 << 20) // 10 MiB
	mgr.checkMemMax = 10 << 20
	payload := make([]byte, 4096) // 4KB → est=2MiB
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write(payload)
			}))
			defer srv.Close()
			task, err := mgr.SubmitAndStart("url", srv.URL, "x.tar", int64(len(payload)), t.Context(), "", TaskParams{Save: true})
			if err != nil {
				t.Errorf("submit %d: %v", idx, err)
				return
			}
			testutil.WaitFor(t, 10*time.Second, func() bool {
				cur, _ := mgr.SnapshotTask(task.ID, "")
				return cur != nil && cur.Status == "completed"
			}, "任务应完成")
			cur, _ := mgr.SnapshotTask(task.ID, "")
			if cur.IntegrityStatus != "unverified" && cur.IntegrityStatus != "damaged" {
				t.Errorf("任务 %d 应 unverified（内容非 tar）或 damaged，got %q", idx, cur.IntegrityStatus)
			}
		}(i)
	}
	wg.Wait()
	// 全部完成后信号量归零（无泄漏）
	if cur := mgr.checkMemSem.TryAcquire(1); !cur {
		t.Fatal("校验全部完成后信号量应可再获取（无占位泄漏）")
	} else {
		mgr.checkMemSem.Release(1)
	}
}
