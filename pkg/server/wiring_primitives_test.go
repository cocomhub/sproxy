// Copyright (C) 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// wiring_primitives_test.go 覆盖装配层**纯装配原语**（此前零覆盖）：周期任务构造、
// 外部认证路由入表、卷级保留期/镜像判定、TOTP pending 表、备份请求解析与源卷解析、
// 外部卷 du。这些原语没有 HTTP 语义，但它们的判定（谁被拒绝/谁被跳过）直接决定运维行为，
// 必须由测试锁定。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/authn"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// TestTOTPPendingTable_Lifecycle 钉住两段式注册 pending 语义：同 owner 冲突、
// 首 admin 单槽、过期回收、失败计数回收、HashMap 上限淘汰最旧。
func TestTOTPPendingTable_Lifecycle(t *testing.T) {
	t.Parallel()
	tbl := newTotpPendingTable()
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	now := base
	tbl.SetClock(func() time.Time { return now })

	if ok, conflict := tbl.Add("ak1", "alice", []byte("s1"), false); !ok || conflict {
		t.Fatalf("首次插入应成功: ok=%v conflict=%v", ok, conflict)
	}
	if ok, conflict := tbl.Add("ak2", "alice", []byte("s2"), false); ok || !conflict {
		t.Fatalf("同 owner 第二个 pending 应冲突: ok=%v conflict=%v", ok, conflict)
	}
	if !tbl.HasActivePending() {
		t.Fatal("活跃 pending 应可观测")
	}
	if e, ok := tbl.Get("ak1"); !ok || e.Owner != "alice" || string(e.TOTPSecret) != "s1" {
		t.Fatalf("Get 应返回 pending 内容: %+v ok=%v", e, ok)
	}
	if tbl.Len() != 1 {
		t.Fatalf("Len=%d want 1", tbl.Len())
	}

	// 失败计数：未达阈值不回收，达到阈值回收。
	if should := tbl.RecordFail("ak1", 2); should {
		t.Fatal("第一次失败未达阈值不应回收")
	}
	if _, ok := tbl.Get("ak1"); !ok {
		t.Fatal("未回收时条目应存在")
	}
	if should := tbl.RecordFail("ak1", 2); !should {
		t.Fatal("达到阈值应回收")
	}
	if should := tbl.RecordFail("missing", 2); !should {
		t.Fatal("不存在的 AK 应视为应移除")
	}

	// 过期回收：超过 TTL 后 Get 未命中、HasActivePending=false。
	if ok, _ := tbl.Add("ak3", "bob", []byte("s3"), false); !ok {
		t.Fatal("重新插入应成功")
	}
	now = base.Add(totpPendingTTL + time.Second)
	if _, ok := tbl.Get("ak3"); ok {
		t.Fatal("过期 pending 不应命中")
	}
	if tbl.HasActivePending() {
		t.Fatal("过期后不应有活跃 pending")
	}

	// 首 admin 单槽：已有任一活跃 pending ⇒ 拒绝。
	now = base
	if ok, _ := tbl.Add("ak4", "carol", nil, false); !ok {
		t.Fatal("插入应成功")
	}
	if ok, conflict := tbl.Add("ak5", "dave", nil, true); ok || !conflict {
		t.Fatalf("首 admin 单槽应拒绝第二个 pending: ok=%v conflict=%v", ok, conflict)
	}

	// 池满：插满后淘汰最旧（ExpiresAt 最小）。
	tbl.Remove("ak4")
	for i := range totpPendingMax {
		ak := "fill-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		now = base.Add(time.Duration(i) * time.Millisecond)
		if ok, conflict := tbl.Add(ak, "owner-"+ak, nil, false); !ok || conflict {
			t.Fatalf("填充插入失败 i=%d: ok=%v conflict=%v", i, ok, conflict)
		}
	}
	if tbl.Len() != totpPendingMax {
		t.Fatalf("池满后 Len=%d want %d", tbl.Len(), totpPendingMax)
	}
	now = base.Add(time.Hour)
	if ok, conflict := tbl.Add("newest", "owner-newest", nil, false); !ok || conflict {
		t.Fatalf("池满时新插入应成功（淘汰最旧）: ok=%v conflict=%v", ok, conflict)
	}
	if _, ok := tbl.Get("fill-aa"); ok {
		t.Fatal("最旧条目应被淘汰")
	}
	tbl.Remove("newest")
	if _, ok := tbl.Get("newest"); ok {
		t.Fatal("Remove 后不应命中")
	}
}

// fakeExternalAuthHandler 是 authn.ExternalAuthHandler 的测试实现（可带认证器）。
type fakeExternalAuthHandler struct {
	routes []authn.ExternalAuthRoute
	auths  []authn.Authenticator
}

func (f *fakeExternalAuthHandler) Name() string                      { return "fake-ext" }
func (f *fakeExternalAuthHandler) Routes() []authn.ExternalAuthRoute { return f.routes }
func (f *fakeExternalAuthHandler) Close() error                      { return nil }
func (f *fakeExternalAuthHandler) Authenticators() []authn.Authenticator {
	return f.auths
}

// TestAppendExternalAuthRoutes_FiltersIncomplete 钉住外部认证路由入表：缺 method/pattern/handler
// 的声明被跳过（否则挂载期 panic），合法声明与认证器都追加进装配状态。
func TestAppendExternalAuthRoutes_FiltersIncomplete(t *testing.T) {
	t.Parallel()
	h := &Handlers{logger: testLogger()}
	h.appendExternalAuthRoutes(RegisterRoutesOpts{
		ExternalAuthHandlers: []authn.ExternalAuthHandler{
			&fakeExternalAuthHandler{ // 全部非法：方法/路径/handler 缺失
				routes: []authn.ExternalAuthRoute{
					{Method: "GET", Pattern: "/auth/oidc/login"},
					{Method: "", Pattern: "/auth/oidc/cb", Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})},
				},
			},
		},
	})
	if len(h.externalAuthRoutes) != 0 {
		t.Fatalf("非法路由不得入表, got %d", len(h.externalAuthRoutes))
	}

	ok := &fakeExternalAuthHandler{routes: []authn.ExternalAuthRoute{{
		Method: "POST", Pattern: "/auth/ldap/login",
		Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	}}}
	h.appendExternalAuthRoutes(RegisterRoutesOpts{
		ExternalAuthHandlers: []authn.ExternalAuthHandler{nil, ok},
	})
	if len(h.externalAuthRoutes) != 1 || h.externalAuthRoutes[0].Pattern != "/auth/ldap/login" {
		t.Fatalf("合法路由应入表: %+v", h.externalAuthRoutes)
	}
}

// TestTrashGCTask_RunAppliesDefaultTTL 钉住回收站 GC 任务：TTL 每 tick 现读，未配置时用
// 7 天默认（不得因零值立刻清空全部回收站）。
func TestTrashGCTask_RunAppliesDefaultTTL(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	h := buildVolSetHandlers(t, cfg)

	cfg.Trash.GCInterval = time.Hour
	cfg.Trash.TTL = 0 // 未配置 ⇒ 任务内取 7 天默认
	task := h.trashGCTask(cfg)
	if task.Name != "trash-gc" || task.Interval != time.Hour || !task.MaintenanceOnly {
		t.Fatalf("任务装配不符: %+v", task)
	}
	if task.Run == nil {
		t.Fatal("任务执行体不得为 nil")
	}
	// 执行一次：不 panic、不因零值 TTL 误删（无 trash 目录 = 无操作）。
	task.Run(context.Background())

	cfg.Trash.TTL = time.Minute
	if got := h.trashGCTask(cfg).Run; got == nil {
		t.Fatal("配置 TTL 后任务执行体仍需存在")
	}
}

// TestVolumeRetentionInterval_MinAcrossVolumes 钉住保留期循环间隔 = 全部启用卷的最小
// gc_interval（最早 tick）；全零 ⇒ 0（调用方据此不启动循环）。
func TestVolumeRetentionInterval_MinAcrossVolumes(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: cfg.StorageRoot},
		{Name: "disk2", Root: t.TempDir(), Retention: VolumeRetentionConfig{GCInterval: 2 * time.Hour}},
	}
	h := buildVolSetHandlers(t, cfg)
	if got := h.volumeRetentionInterval(); got != 2*time.Hour {
		t.Fatalf("间隔=%v want 2h", got)
	}
	// 未启用（全零）⇒ 0（调用方据此不启动周期循环）。
	offCfg := Default()
	offCfg.StorageRoot = t.TempDir()
	offCfg.Volumes = []VolumeConfig{{Name: "main", Root: offCfg.StorageRoot}}
	off := buildVolSetHandlers(t, offCfg)
	if got := off.volumeRetentionInterval(); got != 0 {
		t.Fatalf("未配置 retention 时应 0, got %v", got)
	}
}

// TestHasMirrorStrategyAndIsExternal 钉住镜像启用判定与外部卷探测（两者决定周期任务是否启动、
// 是否走路径式镜像）。
func TestHasMirrorStrategyAndIsExternal(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: cfg.StorageRoot},
		{Name: "mirror", Root: t.TempDir()},
	}
	h := buildVolSetHandlers(t, cfg)
	if h.hasMirrorStrategy() {
		t.Fatal("未配置 mirror 时不应报告镜像策略")
	}
	if (&Handlers{}).hasMirrorStrategy() {
		t.Fatal("未装配卷集时应 false")
	}

	// 外部卷（无本地根）⇒ isExternalVolumeName=true；本地卷/未知卷 false。
	fs := newOpsFS()
	if err := h.volSet.AddExternalVolume(volume.Volume{Name: "ext", Type: "ops-ext"}, &opsBackend{fs: fs}); err != nil {
		t.Fatalf("AddExternalVolume: %v", err)
	}
	if !h.isExternalVolumeName("ext") {
		t.Fatal("外部卷应被识别")
	}
	if h.isExternalVolumeName("main") {
		t.Fatal("本地卷不得被识别为外部卷")
	}
	if h.isExternalVolumeName("") || h.isExternalVolumeName("nope") {
		t.Fatal("空名/未知卷应 false")
	}
}

// TestCopyViaService_WritesThroughDomain 钉住 /api/files/copy 的装配委托：未装配服务 ⇒ 400，
// 正常复制 ⇒ 200 + checksum，源不存在 ⇒ 域侧错误状态原样透传。
func TestCopyViaService_WritesThroughDomain(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	h := buildVolSetHandlers(t, cfg)
	writeTenantFile(t, h, "user/src.txt", "copy-payload")

	rec := httptest.NewRecorder()
	h.copyViaService(rec, httptest.NewRequest(http.MethodPost, "/api/files/copy", nil),
		"anonymous", "src.txt", "dst.txt", "main", "main", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("复制应成功 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp mirrorCopyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.Success {
		t.Fatalf("响应异常: %v (%s)", err, rec.Body.String())
	}
	if resp.Size == 0 || resp.Checksum == "" {
		t.Fatalf("复制结果应回报 size+checksum: %+v", resp)
	}

	rec = httptest.NewRecorder()
	h.copyViaService(rec, httptest.NewRequest(http.MethodPost, "/api/files/copy", nil),
		"anonymous", "missing.txt", "x.txt", "main", "main", false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("源不存在应 404, got %d (%s)", rec.Code, rec.Body.String())
	}

	// 未装配租户/卷集 ⇒ fail-closed 400（不 panic、不回落全局根）。
	rec = httptest.NewRecorder()
	(&Handlers{logger: testLogger()}).copyViaService(rec, httptest.NewRequest(http.MethodPost, "/", nil),
		"anonymous", "a", "b", "main", "main", false)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未装配应 fail-closed 400, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestDuExternalVolume_WalksExternalTree 钉住外部卷 du：授权/路径/遍历失败三态与成功统计。
func TestDuExternalVolume_WalksExternalTree(t *testing.T) {
	t.Parallel()
	fs := newOpsFS()
	fs.put("alice/user/dir/a.bin", "12345")
	fs.put("alice/user/dir/b.bin", "123")
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	h := buildVolSetHandlers(t, cfg)
	v := volume.Volume{Name: "ext", Type: "ops-ext"}
	if err := h.volSet.AddExternalVolume(v, &opsBackend{fs: fs}); err != nil {
		t.Fatalf("AddExternalVolume: %v", err)
	}

	rec := httptest.NewRecorder()
	h.duExternalVolume(rec, httptest.NewRequest(http.MethodGet, "/api/du?volume=ext", nil), "alice", v, "dir")
	if rec.Code != http.StatusOK {
		t.Fatalf("du 应 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Files int   `json:"files"`
			Size  int64 `json:"size"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.Success {
		t.Fatalf("响应异常: %v (%s)", err, rec.Body.String())
	}
	if resp.Data.Files != 2 || resp.Data.Size != 8 {
		t.Fatalf("du 统计=(%d,%d) want (2,8)", resp.Data.Files, resp.Data.Size)
	}

	rec = httptest.NewRecorder()
	h.duExternalVolume(rec, httptest.NewRequest(http.MethodGet, "/api/du", nil), "alice", v, "../escape")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法路径应 400, got %d", rec.Code)
	}

	fs.listErr["alice/user/dir"] = errTestInjected
	rec = httptest.NewRecorder()
	h.duExternalVolume(rec, httptest.NewRequest(http.MethodGet, "/api/du", nil), "alice", v, "dir")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("遍历失败应 500, got %d", rec.Code)
	}

	// 未装配卷（未知卷）⇒ 404。
	rec = httptest.NewRecorder()
	h.duExternalVolume(rec, httptest.NewRequest(http.MethodGet, "/api/du", nil), "alice", volume.Volume{Name: "nope"}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知卷应 404, got %d", rec.Code)
	}
}

// TestParseBackupRequest_RejectsMalformed 钉住备份请求解析三态：坏 JSON / target 空 / 正常。
func TestParseBackupRequest_RejectsMalformed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body string
		want       int
		ok         bool
	}{
		{"坏 JSON", "{", http.StatusBadRequest, false},
		{"空 target", `{"target":"   "}`, http.StatusBadRequest, false},
		{"正常", `{"target":"backup"}`, http.StatusOK, true},
	} {
		rec := httptest.NewRecorder()
		req, ok := parseBackupRequest(rec, httptest.NewRequest(http.MethodPost, "/api/backup", strings.NewReader(tc.body)))
		if ok != tc.ok {
			t.Fatalf("%s: ok=%v want %v", tc.name, ok, tc.ok)
		}
		if tc.ok {
			if req == nil || req.Target != "backup" {
				t.Fatalf("%s: 请求解析错误: %+v", tc.name, req)
			}
			continue
		}
		if rec.Code != tc.want {
			t.Fatalf("%s: 状态=%d want %d", tc.name, rec.Code, tc.want)
		}
	}
}

// TestBackupSourceRoot_ACLAndResolution 钉住备份源卷解析：无权卷 403、卷根不可用 400、
// 正常返回租户与 user 桶绝对路径。
func TestBackupSourceRoot_ACLAndResolution(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	h := buildVolSetHandlers(t, cfg)

	// 未知卷（volumeAllowedFor false）⇒ 403，不泄存在性。
	rec := httptest.NewRecorder()
	if _, _, ok := h.backupSourceRoot(rec, "anonymous", &BackupRequest{Source: "nope"}); ok {
		t.Fatal("未知源卷必须拒绝")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非法源卷应 403, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	tnt, abs, ok := h.backupSourceRoot(rec, "anonymous", &BackupRequest{})
	if !ok || tnt == nil || abs == "" {
		t.Fatalf("默认源卷应解析成功: ok=%v tnt=%v abs=%q", ok, tnt, abs)
	}

	// 未装配卷集 + 显式源 ⇒ 403（fail-closed，不回落全局根）。
	rec = httptest.NewRecorder()
	if _, _, ok := (&Handlers{logger: testLogger()}).backupSourceRoot(rec, "anonymous", &BackupRequest{Source: "main"}); ok {
		t.Fatal("未装配卷集时必须拒绝")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("未装配应 403, got %d", rec.Code)
	}
}

// errTestInjected 是夹具注入的通用故障（避免各用例重复构造错误值）。
var errTestInjected = errorString("injected test failure")

// errorString 是最小 error 实现（纯标准库，保持测试自足）。
type errorString string

func (e errorString) Error() string { return string(e) }
