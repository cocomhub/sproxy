// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// backup_api.go 是备份服务端端点（roadmap 12.2-3 备份 P2）：
// POST /api/backup —— 源=本地卷 user 桶 sync.FS → 目标=配置卷（本地卷或外部卷
// federated 写面，经 registry.ExternalBackend.FS() 取远端写面）。
// 复用 pkg/backup 引擎（#619 P1，Run 签名 + Report 结构——本文件只做装配/接线，
// 不改引擎）。增量语义由引擎 manifest 比对（size+mtime）保证；配额记账经
// backupQuotaFS 逐文件写前预留（owner user 桶 Scope + 目标卷容量池）。
// 部分失败：HTTP 200 + Report JSON（failed/errors 显式化，不静默）。

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/cocomhub/sproxy/pkg/backup"
	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/trusted"
)

// BackupRequest 是 POST /api/backup 的请求体。
type BackupRequest struct {
	// Target 是目标卷名（必填）：外部卷（federated/baidupcs 等）走其写面 FS；
	// 本地卷写该卷 owner user 桶。ACL 校验：请求者 owner 必须在目标卷视图内。
	Target string `json:"target"`
	// Source 是源卷名（可选；空 = 默认卷）。源恒为本地卷（owner user 桶）。
	Source string `json:"source,omitempty"`
	// Verify 传输后对目标 Stat 校验 size（对齐引擎 Options.Verify）。
	Verify bool `json:"verify,omitempty"`
	// Concurrent 并发传输文件数（<=0 = 引擎默认 4）。
	Concurrent int `json:"concurrent,omitempty"`
	// Exclude 排除 glob 模式（path.Match 语义，对齐 pkg/sync ParseFilters）。
	Exclude []string `json:"exclude,omitempty"`
}

// backupAPIReport 是服务端备份响应（引擎 Report 的 JSON 化：Err 序列化为字符串，
// 避免 error 接口在 JSON 里序列化空对象）。
type backupAPIReport struct {
	Files     int      `json:"files"`   // 成功传输文件数
	Bytes     int64    `json:"bytes"`   // 成功传输字节数
	Skipped   int      `json:"skipped"` // manifest 命中（size+mtime 相同）跳过
	Failed    int      `json:"failed"`  // 失败文件数（含校验失败与 manifest 写失败）
	Errors    []string `json:"errors,omitempty"`
	Truncated bool     `json:"truncated"` // ctx 取消/超时：仅部分完成
}

// backupReportToAPI 把引擎 Report 转成 JSON 友好的 API 报告。
func backupReportToAPI(rep *backup.Report) *backupAPIReport {
	out := &backupAPIReport{
		Files: rep.Files, Bytes: rep.Bytes,
		Skipped: rep.Skipped, Failed: rep.Failed, Truncated: rep.Truncated,
	}
	for _, fe := range rep.Errors {
		msg := fe.Path
		if fe.Err != nil {
			msg += ": " + fe.Err.Error()
		}
		out.Errors = append(out.Errors, msg)
	}
	return out
}

// backupHandler 处理 POST /api/backup（主 mux 经 authMiddleware；隧道内层裸注册）。
func (h *Handlers) backupHandler(w http.ResponseWriter, r *http.Request) {
	req, ok := parseBackupRequest(w, r)
	if !ok {
		return
	}
	owner := normalizeOwner(ownerFromRequest(r))

	// 源=本地卷 user 桶（backup 引擎泛化于 sync.FS，远端源不在本端点范围）。
	srcAbs, ok := h.backupSourceRoot(w, owner, req)
	if !ok {
		return
	}
	srcFS := syncpkg.NewLocalFS(filepath.ToSlash(srcAbs), h.logger)

	// 目标卷：ACL + 装配（外部卷写面 / 本地卷 user 桶）+ 配额记账包装。
	if !h.volumeAllowedFor(owner, req.Target) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgVolumeNotAllowed}, http.StatusForbidden)
		return
	}
	dstFS, err := h.backupTargetFS(r.Context(), owner, req.Target)
	if err != nil {
		h.sendBackupFSError(w, err)
		return
	}

	rep, err := backup.Run(r.Context(), srcFS, dstFS, backup.Options{
		Concurrent: req.Concurrent,
		Verify:     req.Verify,
		Exclude:    req.Exclude,
	})
	if err != nil {
		h.logger.Error("备份执行失败", "target", req.Target, "error", err)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "备份执行失败"}, http.StatusInternalServerError)
		return
	}

	// 审计（部分失败/截断 → error 结果，与 verify 同款语义）。
	h.recordBackupAudit(r.Context(), req.Target, rep)
	sendJSONResponse(w, backupReportToAPI(rep), http.StatusOK)
}

// parseBackupRequest 解析备份请求体（MaxBytesReader 上限 + drain 哈希校验 + target 非空）。
// 失败已写响应，返回 false。
func parseBackupRequest(w http.ResponseWriter, r *http.Request) (*BackupRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB
	var req BackupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无法解析请求体"}, http.StatusBadRequest)
		return nil, false
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return nil, false
	}
	if strings.TrimSpace(req.Target) == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "target 卷名不能为空"}, http.StatusBadRequest)
		return nil, false
	}
	return &req, true
}

// backupSourceRoot 解析备份源卷：显式 Source 或默认卷；ACL 收口（AD-6：无权卷不泄不写），
// 并返回源卷 user 桶的绝对路径。失败已写响应，返回 false。
func (h *Handlers) backupSourceRoot(w http.ResponseWriter, owner string, req *BackupRequest) (string, bool) {
	// 源卷：显式 Source 或默认卷；ACL 收口（AD-6：无权卷不泄不写）。
	srcVol := req.Source
	if srcVol == "" && h.volSet != nil {
		srcVol = h.volSet.Default().Name
	}
	if srcVol == "" || !h.volumeAllowedFor(owner, srcVol) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgVolumeNotAllowed}, http.StatusForbidden)
		return "", false
	}
	srcTnt := h.volumeTenant(srcVol, owner)
	if srcTnt == nil || srcTnt.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "源卷根不可用"}, http.StatusBadRequest)
		return "", false
	}
	srcAbs, ok := srcTnt.Root().Abs(srcTnt.UserRoot())
	if !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "源卷根不可用"}, http.StatusBadRequest)
		return "", false
	}
	return srcAbs, true
}

// sendBackupFSError 写备份目标卷装配错误（远端不可达/拨号失败映射 502 Bad Gateway，显式报错）。
func (h *Handlers) sendBackupFSError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if strings.Contains(err.Error(), "不可达") || strings.Contains(err.Error(), "拨号") {
		status = http.StatusBadGateway // 远端写面装配失败（无远端可达）显式报错，不静默
	}
	sendJSONResponse(w, UploadResponse{Success: false, Message: err.Error()}, status)
}

// recordBackupAudit 记录备份审计（部分失败/截断 → 失败结果，与 verify 同款语义）。
func (h *Handlers) recordBackupAudit(ctx context.Context, target string, rep *backup.Report) {
	detail := fmt.Sprintf("target=%s files=%d bytes=%d skipped=%d failed=%d truncated=%t",
		target, rep.Files, rep.Bytes, rep.Skipped, rep.Failed, rep.Truncated)
	result := AuditResultSuccess
	if rep.Failed > 0 || rep.Truncated {
		result = AuditResultError
	}
	h.RecordAudit(ctx, AuditEvent{
		Action: "backup", ObjectType: "volume", Object: target,
		Result: result, Detail: detail,
	})
}

// backupTargetFS 装配备份目标 sync.FS：
//   - 外部卷（federated/baidupcs 等）：registry.ExternalBackend.FS()（写面随后端装配，
//     federated WithWriter(remote.Client.FS(ref)) 已由后端构造注入）；装配失败
//     （远端不可达）显式报错，绝不静默回落。
//   - 本地卷：owner user 桶 LocalFS。
//
// 返回的 FS 已包 quota 记账（backupQuotaFS）：owner user 桶 Scope + 目标卷容量池
// 逐文件写前预留（外部卷无 owner 桶，仅卷池；两者均未装配 = 不记账）。
func (h *Handlers) backupTargetFS(ctx context.Context, owner, target string) (syncpkg.FS, error) {
	if h.volSet == nil {
		return nil, fmt.Errorf("卷功能未装配")
	}
	fs, err := h.backupTargetFSBase(ctx, owner, target)
	if err != nil {
		return nil, err
	}
	// 配额记账：备份占用目标卷配额（owner user 桶 + 目标卷容量池）。
	// **外部卷不再叠加卷池**：卷池由 FS 层 capacity.CapacityFS 权威记账（backupQuotaFS
	// 与共享 Pool 双预留会使 committed += 2×size，Delete 只释放 1× → 池单调膨胀至该卷
	// 全部写被误拒；P2 对抗评审）。本地卷目标无 CapacityFS，保留 pool 记账。
	scope := h.quotaScopeFor(owner, "user")
	pool := h.volSet.Pool(target)
	if h.volSet.External(target) != nil {
		pool = nil
	}
	if scope == nil && pool == nil {
		return fs, nil
	}
	return &backupQuotaFS{inner: fs, scope: scope, pool: pool}, nil
}

// backupTargetFSBase 装配备份目标卷的裸 sync.FS（未包配额记账）：
//   - 外部卷（federated/baidupcs 等）：registry.ExternalBackend.FS()（写面随后端装配，
//     federated WithWriter(remote.Client.FS(ref)) 已由后端构造注入）；装配失败
//     （远端不可达）显式报错，绝不静默回落。
//   - 本地卷：owner user 桶 LocalFS。
func (h *Handlers) backupTargetFSBase(ctx context.Context, owner, target string) (syncpkg.FS, error) {
	if be := h.volSet.External(target); be != nil {
		if probe, ok := be.(registry.HealthProbe); ok {
			if perr := probe.Ping(ctx); perr != nil && !errors.Is(perr, syncpkg.ErrUnsupported) {
				return nil, fmt.Errorf("备份目标卷 %q 远端不可达: %w", target, perr)
			}
		}
		raw := be.FS()
		if raw == nil {
			return nil, fmt.Errorf("备份目标卷 %q 无文件系统视图（装配错误）", target)
		}
		// P1-9 修复：备份写外部卷也须 Wrap（meta 恒生成，与上传/转存一致）；
		// P1-4 修复：Guard 现透传 StagingQuotaExempt/Capable，s3 豁免/baidupcs 自管可命中。
		fs := trusted.Guard(trusted.Wrap(raw, h.trustedWrapOpts()))
		// 本地 staging 配额强制接线（用户裁定 2026-10-10：外部卷统一过独立 staging
		// 配额——备份写本地暂存防打满；s3 流式显式 Exempt，baidupcs 自管 StagingQuotaCapable）。
		return h.stagingQuotaFS(owner, fs), nil
	}
	tnt := h.volumeTenant(target, owner)
	if tnt == nil || tnt.Root() == nil {
		return nil, fmt.Errorf("备份目标卷 %q 不可用", target)
	}
	userAbs, ok := tnt.Root().Abs(tnt.UserRoot())
	if !ok {
		return nil, fmt.Errorf("备份目标卷 %q 根不可用", target)
	}
	return syncpkg.NewLocalFS(filepath.ToSlash(userAbs), h.logger), nil
}

// backupQuotaFS 是备份目标写面的配额记账装饰器：WriteFile 前对文件 size 在
// owner user 桶 Scope 与目标卷容量池上双预留（与 syncexec.quotaLocalFS 同构），
// 超限拒绝（该文件计入 Report.Failed，不中止整体）；写成功 Commit、失败 Release。
// 其余方法透传 inner。
type backupQuotaFS struct {
	inner syncpkg.FS
	scope *quota.Scope
	pool  *quota.Pool
}

// backupReservation 是 scope+pool 双预留的聚合（任一为空则跳过）。
type backupReservation struct {
	scope, pool *quota.Reservation
}

func (r *backupReservation) Commit(actual int64) {
	if r.scope != nil {
		r.scope.Commit(actual)
	}
	if r.pool != nil {
		r.pool.Commit(actual)
	}
}

func (r *backupReservation) Release() {
	if r.scope != nil {
		r.scope.Release()
	}
	if r.pool != nil {
		r.pool.Release()
	}
}

func (q *backupQuotaFS) reserve(size int64) (*backupReservation, error) {
	if q.scope == nil && q.pool == nil {
		return nil, nil
	}
	var res backupReservation
	if q.scope != nil {
		s, err := q.scope.TryReserve(size)
		if err != nil {
			return nil, err
		}
		res.scope = s
	}
	if q.pool != nil {
		p, err := q.pool.TryReserve(size)
		if err != nil {
			if res.scope != nil {
				res.scope.Release()
			}
			return nil, err
		}
		res.pool = p
	}
	return &res, nil
}

func (q *backupQuotaFS) WriteFile(ctx context.Context, relPath string, r io.Reader, size, mtime int64) error {
	res, err := q.reserve(size)
	if err != nil {
		return fmt.Errorf("备份目标配额不足: %w", err)
	}
	if res == nil {
		return q.inner.WriteFile(ctx, relPath, r, size, mtime)
	}
	if werr := q.inner.WriteFile(ctx, relPath, r, size, mtime); werr != nil {
		res.Release()
		return werr
	}
	res.Commit(size)
	return nil
}

func (q *backupQuotaFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	return q.inner.ListDir(ctx, p)
}

func (q *backupQuotaFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	return q.inner.Stat(ctx, p)
}

func (q *backupQuotaFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	return q.inner.OpenRead(ctx, p)
}

func (q *backupQuotaFS) Rename(ctx context.Context, from, to string) error {
	return q.inner.Rename(ctx, from, to)
}

func (q *backupQuotaFS) Delete(ctx context.Context, p string) error {
	return q.inner.Delete(ctx, p)
}

func (q *backupQuotaFS) MakeDir(ctx context.Context, p string) error {
	return q.inner.MakeDir(ctx, p)
}

// _ 编译期断言：backupQuotaFS 实现 sync.FS。
var _ syncpkg.FS = (*backupQuotaFS)(nil)
