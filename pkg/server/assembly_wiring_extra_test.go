// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// assembly_wiring_extra_test.go 覆盖装配层剩余的**装配原语与运维路径**：备份目标卷装配、
// 自备份判定、旧审计目录迁移、owner 默认 secrets 桶读取、单卷归档源校验、卷导出源枚举、
// 镜像循环停止、容量账本恢复。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// TestBackupTargetFS_LocalAndExternal 钉住备份目标装配：本地卷经 Guard(Wrap) + user/ 前缀 +
// 配额记账；外部卷同形且不叠加卷池；未知卷/未装配 fail-closed。
func TestBackupTargetFS_LocalAndExternal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	h := buildVolSetHandlers(t, cfg)

	tgt, err := h.backupTargetFS(ctx, "anonymous", "main")
	if err != nil {
		t.Fatalf("本地目标装配失败: %v", err)
	}
	if tgt.data == nil || tgt.manifest == nil {
		t.Fatal("本地目标应同时给数据写面与 manifest 写面")
	}
	if err := tgt.data.WriteFile(ctx, syncpkg.LogicalKey{}, "f.bin", strings.NewReader("1234567890"), 10, 0); err != nil {
		t.Fatalf("写备份数据: %v", err)
	}
	pool := h.volSet.Pool("main")
	if pool == nil {
		t.Fatal("主卷应有容量池")
	}
	if got := pool.Usage(); got == 0 {
		t.Fatal("备份写应记入目标卷容量池")
	}
	// 自备份判定：目标租户根 === 源 user 桶的父目录 ⇒ true；其它卷/外部卷 ⇒ false。
	tnt := h.volumeTenant("main", "anonymous")
	srcAbs, ok := tnt.Root().Abs(tnt.UserRoot())
	if !ok {
		t.Fatal("源 user 桶绝对路径不可解析")
	}
	if !h.backupSelfTarget("anonymous", "main", srcAbs) {
		t.Fatal("同一实体应识别为自备份")
	}
	if aerr := h.volSet.AddExternalVolume(volume.Volume{Name: "ext", Type: "ops-ext"}, &opsBackend{fs: newOpsFS()}); aerr != nil {
		t.Fatalf("AddExternalVolume: %v", aerr)
	}
	if h.backupSelfTarget("anonymous", "ext", srcAbs) {
		t.Fatal("外部卷恒异体，不得判自备份")
	}
	if h.backupSelfTarget("anonymous", "nope", srcAbs) {
		t.Fatal("未知卷不得判自备份")
	}
	if got, _, err := h.backupTargetFSBase(ctx, "anonymous", "ext"); err != nil || got == nil {
		t.Fatalf("外部卷目标应装配成功: %v", err)
	}
	if _, _, err := h.backupTargetFSBase(ctx, "anonymous", "nope"); err == nil {
		t.Fatal("未知卷目标必须报错（不静默回落）")
	}
	if _, err := (&Handlers{logger: testLogger()}).backupTargetFS(ctx, "anonymous", "main"); err == nil {
		t.Fatal("未装配卷集必须报错")
	}
}

// TestSendBackupFSError_MapsRemoteUnreachable 钉住备份目标装配错误映射：远端不可达/拨号失败
// ⇒ 502（显式暴露），其余 ⇒ 400。
func TestSendBackupFSError_MapsRemoteUnreachable(t *testing.T) {
	t.Parallel()
	h := &Handlers{logger: testLogger()}
	for _, tc := range []struct {
		msg  string
		want int
	}{
		{"备份目标卷 \"x\" 远端不可达: dial tcp", http.StatusBadGateway},
		{"拨号失败", http.StatusBadGateway},
		{"备份目标卷 \"x\" 不可用", http.StatusBadRequest},
	} {
		rec := httptest.NewRecorder()
		h.sendBackupFSError(rec, errors.New(tc.msg))
		if rec.Code != tc.want {
			t.Fatalf("%q ⇒ %d want %d", tc.msg, rec.Code, tc.want)
		}
	}
}

// TestMigrateLegacyAuditDir_MovesOnlyAuditSegments 钉住旧审计目录一次性迁移：只迁审计段、
// 不覆盖目标已有文件、目录不存在时静默跳过（保留原处，不丢数据）。
func TestMigrateLegacyAuditDir_MovesOnlyAuditSegments(t *testing.T) {
	t.Parallel()
	legacy := t.TempDir()
	newDir := filepath.Join(t.TempDir(), "audit")
	for name, body := range map[string]string{
		"current-audit.log":   "a",
		"segment.audit.log.1": "b",
		"unrelated.txt":       "c",
	} {
		if err := os.WriteFile(filepath.Join(legacy, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(legacy, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	migrateLegacyAuditDir(legacy, newDir, testLogger())

	if _, err := os.Stat(filepath.Join(newDir, "current-audit.log")); err != nil {
		t.Fatalf("审计热段应迁入新目录: %v", err)
	}
	if _, err := os.Stat(filepath.Join(newDir, "segment.audit.log.1")); err != nil {
		t.Fatalf("审计归档段应迁入新目录: %v", err)
	}
	if _, err := os.Stat(filepath.Join(newDir, "unrelated.txt")); !os.IsNotExist(err) {
		t.Fatal("非审计文件不得迁移")
	}

	// 目标已存在 ⇒ 不覆盖（旧文件保留原处）。
	other := t.TempDir()
	dup := filepath.Join(other, "current-audit.log")
	if err := os.WriteFile(dup, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "current-audit.log"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	migrateLegacyAuditDir(legacy, other, testLogger())
	if data, err := os.ReadFile(dup); err != nil || string(data) != "existing" {
		t.Fatalf("目标已存在时不得覆盖: %s (%v)", data, err)
	}

	// 旧目录不存在：无历史可迁，静默返回。
	migrateLegacyAuditDir(filepath.Join(legacy, "missing"), filepath.Join(newDir, "x"), testLogger())
}

// TestReadOwnerDefaultSecret_FailsClosedWithoutVolumes 钉住 owner 默认 secrets 读取的
// fail-closed 边界：卷集未装配 / secret 名不存在都必须报错（绝不返回空成功）。
func TestReadOwnerDefaultSecret_FailsClosedWithoutVolumes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, err := ReadOwnerDefaultSecret(ctx, nil, "alice", "s", testLogger()); !errors.Is(err, errSecretsTenantUnavailable) {
		t.Fatalf("未装配卷集应 errSecretsTenantUnavailable, got %v", err)
	}

	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	h := buildVolSetHandlers(t, cfg)
	if _, err := ReadOwnerDefaultSecret(ctx, h.volSet, "alice", "missing-secret", testLogger()); err == nil {
		t.Fatal("不存在的 secret 必须报错")
	}
}

// TestArchiveDirSingleVolumeSource_ValidatesDirectory 钉住单卷归档源校验三态。
func TestArchiveDirSingleVolumeSource_ValidatesDirectory(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	h := buildVolSetHandlers(t, cfg)
	tnt := h.tenants.TenantFor("anonymous")
	if err := tnt.Root().MkdirAll("user/dir", 0o755); err != nil {
		t.Fatal(err)
	}
	if f, err := tnt.Root().OpenFile("user/file.bin", os.O_CREATE|os.O_WRONLY, 0o644); err != nil {
		t.Fatal(err)
	} else {
		_ = f.Close()
	}

	rec := httptest.NewRecorder()
	srcs, ok := archiveDirSingleVolumeSource(rec, tnt, "user/dir")
	if !ok || len(srcs) != 1 || srcs[0].userRel != "user/dir" {
		t.Fatalf("目录应以单一源返回: ok=%v srcs=%+v", ok, srcs)
	}

	rec = httptest.NewRecorder()
	if _, ok := archiveDirSingleVolumeSource(rec, tnt, "user/missing"); ok {
		t.Fatal("目录不存在必须拒绝")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在应 404, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	if _, ok := archiveDirSingleVolumeSource(rec, tnt, "user/file.bin"); ok {
		t.Fatal("文件路径不得作为归档目录")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非目录应 400, got %d", rec.Code)
	}
}

// TestListVolumeExportSources_CoversAssemblyModes 钉住卷导出源枚举三种装配形态：
// 显式卷（ACL 收口）/ 全部可见卷 / 旧装配（无卷集，唯一根）。
func TestListVolumeExportSources_CoversAssemblyModes(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	h := buildVolSetHandlers(t, cfg)
	writeTenantFile(t, h, "user/a.txt", "abc")

	srcs, err := h.listVolumeExportSources("anonymous", "main")
	if err != nil || len(srcs) != 1 || srcs[0].relName != "a.txt" {
		t.Fatalf("显式卷导出源=(%+v,%v) want 单文件 a.txt", srcs, err)
	}
	if _, lerr := h.listVolumeExportSources("anonymous", "nope"); lerr == nil {
		t.Fatal("未知卷必须报错（不泄存在性）")
	}
	if aerr := h.volSet.AddExternalVolume(volume.Volume{Name: "ext", Type: "ops-ext"}, &opsBackend{fs: newOpsFS()}); aerr != nil {
		t.Fatalf("AddExternalVolume: %v", aerr)
	}
	if esrcs, xerr := h.listVolumeExportSources("anonymous", "ext"); xerr != nil || len(esrcs) != 0 {
		t.Fatalf("外部卷无本地根应跳过（不导、不报错）: (%d,%v)", len(esrcs), xerr)
	}
	all, err := h.listVolumeExportSources("anonymous", "")
	if err != nil || len(all) == 0 {
		t.Fatalf("未指定卷应枚举全部可见本地卷: (%d,%v)", len(all), err)
	}

	// 旧装配（volSet nil）：唯一根 = 默认租户。
	legacyCfg := Default()
	legacyCfg.StorageRoot = t.TempDir()
	legacy := buildVolSetHandlers(t, legacyCfg)
	writeTenantFile(t, legacy, "user/legacy.bin", "x")
	legacyOnly := &Handlers{logger: testLogger(), tenants: legacy.tenants}
	got, err := legacyOnly.listVolumeExportSources("anonymous", "")
	if err != nil || len(got) != 1 || got[0].relName != "legacy.bin" {
		t.Fatalf("旧装配枚举=(%+v,%v)", got, err)
	}
	if _, err := (&Handlers{logger: testLogger()}).listVolumeExportSources("anonymous", ""); err == nil {
		t.Fatal("租户不可用必须报错")
	}
}

// TestMirrorVolumeLoop_ExitsOnStop 钉住镜像循环的停止语义（Close 后不泄漏 goroutine）。
func TestMirrorVolumeLoop_ExitsOnStop(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	cfg.MirrorInterval = time.Millisecond
	h := buildVolSetHandlers(t, cfg)
	// 手工装配停止信号（startMirrorLoop 仅在配了 mirror_to 策略时才建），由生产路径
	// h.Close() 关闭（closeOnce 幂等，cleanup 里再次 Close 不会双重 close）。
	h.mirrorStop = make(chan struct{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.mirrorVolumeLoop()
	}()
	// 通过生产路径停止（Close 内 closeOnce 关闭 mirrorStop）；不可预先 close
	// mirrorStop，否则与 Close 形成双重 close（panic: close of closed channel）。
	h.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("mirrorVolumeLoop 未响应停止信号")
	}
}

// TestRecoverVolumeLedgers_SkipsNonLedgerVolumes 钉住启动账本恢复：非账本装配的卷被跳过
// （不误报失败），ctx 取消即中止。
func TestRecoverVolumeLedgers_SkipsNonLedgerVolumes(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	h := buildVolSetHandlers(t, cfg)

	n, err := recoverVolumeLedgers(context.Background(), h, nil)
	if err != nil || n != 0 {
		t.Fatalf("无外部卷应 (0,nil), got (%d,%v)", n, err)
	}
	n, err = recoverVolumeLedgers(context.Background(), h, []string{"main", "nope"})
	if err != nil || n != 0 {
		t.Fatalf("非 LedgerCounter 卷应跳过: (%d,%v)", n, err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := recoverVolumeLedgers(canceled, h, []string{"main"}); err == nil {
		t.Fatal("ctx 取消必须中止并报错")
	}
}
