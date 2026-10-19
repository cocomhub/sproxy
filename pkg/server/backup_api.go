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
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/cocomhub/sproxy/pkg/backup"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
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
	srcTnt, srcAbs, ok := h.backupSourceRoot(w, owner, req)
	if !ok {
		return
	}
	// **源经 `storage.Root`**（评审 P0 残余闭环）：键空间与目标一致（`user/<rel>` 前缀映射，
	// 源条目仍是裸 rel）；经 Root 即自动获得 **at-rest 透明解密**与**封印**语义
	// ——此前源是裸 `NewLocalFS(srcAbs)`（纯 os I/O）⇒ 加密卷源读到密文（备份产物不可用），
	// 故当时对加密源显式 409 拒绝；now 该拒绝不再需要（加密卷与封印卷均可作为备份源）。
	srcFS := syncpkg.NewPrefixFS(NewRootFS(srcTnt.Root()), srcTnt.UserRoot()+"/")

	// 自备份守卫：源与目标为同一实体时拒绝（与 manifest 落点无关的独立防线）。
	if h.backupSelfTarget(owner, req.Target, srcAbs) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "备份源与目标为同一卷（不支持自备份）"}, http.StatusBadRequest)
		return
	}

	// 目标卷：ACL + 装配（外部卷写面 / 本地卷 user 桶）+ 配额记账包装。
	if !h.volumeAllowedFor(owner, req.Target) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgVolumeNotAllowed}, http.StatusForbidden)
		return
	}
	tgt, err := h.backupTargetFS(r.Context(), owner, req.Target)
	if err != nil {
		h.sendBackupFSError(w, err)
		return
	}

	rep, err := backup.Run(r.Context(), srcFS, tgt.data, backup.Options{
		Concurrent: req.Concurrent,
		Verify:     req.Verify,
		Exclude:    req.Exclude,
		// manifest 落**独立 backup 功能桶**（用户 API 不可寻址的命名空间）：
		// 否则 manifest 是用户可读写路径（可覆写/伪造 → 增量静默跳过、覆盖用户同名文件）。
		ManifestFS:  tgt.manifest,
		ManifestRel: backupManifestKey,
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
// 并返回源卷租户（供经 Root 构造源视图）与 user 桶绝对路径（自备份判据用）。
// 失败已写响应，返回 false。
func (h *Handlers) backupSourceRoot(w http.ResponseWriter, owner string, req *BackupRequest) (*storage.Tenant, string, bool) {
	// 源卷：显式 Source 或默认卷；ACL 收口（AD-6：无权卷不泄不写）。
	srcVol := req.Source
	if srcVol == "" && h.volSet != nil {
		srcVol = h.volSet.Default().Name
	}
	if srcVol == "" || !h.volumeAllowedFor(owner, srcVol) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgVolumeNotAllowed}, http.StatusForbidden)
		return nil, "", false
	}
	srcTnt := h.volumeTenant(srcVol, owner)
	if srcTnt == nil || srcTnt.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "源卷根不可用"}, http.StatusBadRequest)
		return nil, "", false
	}
	srcAbs, ok := srcTnt.Root().Abs(srcTnt.UserRoot())
	if !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "源卷根不可用"}, http.StatusBadRequest)
		return nil, "", false
	}
	return srcTnt, srcAbs, true
}

// backupSelfTarget 报告备份目标是否与源为**同一实体**（自备份）。
// 判据：目标为本地卷（外部卷恒异体）且其租户根 == 源的租户根（= 源 user 桶根的父目录）。
// 自备份无意义且危险：manifest/数据回灌源命名空间、目标产物被再次枚举。
func (h *Handlers) backupSelfTarget(owner, target, srcUserRootAbs string) bool {
	if h.volSet == nil || h.volSet.External(target) != nil {
		return false
	}
	tnt := h.volumeTenant(target, owner)
	if tnt == nil || tnt.Root() == nil {
		return false
	}
	rootAbs, ok := tnt.Root().Abs("")
	if !ok {
		return false
	}
	src := filepath.Clean(filepath.FromSlash(srcUserRootAbs))
	dst := filepath.Clean(filepath.FromSlash(rootAbs))
	return dst == filepath.Dir(src)
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

// backupManifestBucket 是备份 manifest 的**功能桶名**（与 pkg/storage 的 featureBuckets
// "backup" 对齐）：本地卷落在 `<租户根>/backup/`，外部卷落在 `<owner>/backup/`。
// 用户 API 只寻址 user 桶 ⇒ 用户看不到/改不了 manifest（可信备份正确性的前提）。
const backupManifestBucket = "backup"

// backupManifestKey 是 manifest 在功能桶内的键。
const backupManifestKey = "manifest.json"

// backupTarget 是备份目标的装配结果：数据写面 + manifest 写面（两个命名空间）。
type backupTarget struct {
	data     syncpkg.FS // 数据写面（已包配额记账）
	manifest syncpkg.FS // manifest 功能桶写面（独立命名空间；不包配额——manifest 体量有界）
}

// backupTargetFS 装配备份目标：
//   - 外部卷（federated/baidupcs 等）：registry.ExternalBackend.FS()（写面随后端装配，
//     federated WithWriter(remote.Client.FS(ref)) 已由后端构造注入）；装配失败
//     （远端不可达）显式报错，绝不静默回落。
//   - 本地卷：owner user 桶 LocalFS。
//
// 返回的 data 已包 quota 记账（backupQuotaFS）：owner user 桶 Scope + 目标卷容量池
// 逐文件写前预留（外部卷无 owner 桶，仅卷池；两者均未装配 = 不记账）。
func (h *Handlers) backupTargetFS(ctx context.Context, owner, target string) (backupTarget, error) {
	if h.volSet == nil {
		return backupTarget{}, fmt.Errorf("卷功能未装配")
	}
	fs, mfs, err := h.backupTargetFSBase(ctx, owner, target)
	if err != nil {
		return backupTarget{}, err
	}
	// 配额记账：备份占用目标卷配额（owner user 桶 + 目标卷容量池）。
	// **外部卷不再叠加卷池**：卷池由 FS 层 capacity.CapacityFS 权威记账（backupQuotaFS
	// 与共享 Pool 双预留会使 committed += 2×size，Delete 只释放 1× → 池单调膨胀至该卷
	// 全部写被误拒；P2 对抗评审）。本地卷目标无 CapacityFS，保留 pool 记账。
	scope := h.quotaScopeFor(owner, "user")
	pool := h.volSet.Pool(target)
	var sm *capacity.StorageManager
	if h.volSet.External(target) != nil {
		pool = nil
	} else {
		// M9b（强一致）：本地卷目标的**持久容量账本**（capacity.StorageManager——/stats 与
		// ScanAndRecalculate 的权威账本）必须随写同步记账。原先只有 Scope/卷池两条内存账，
		// 无 vol_capacity 的本地卷靠周期扫描对账 ⇒ 扫描前统计低估（与云端归档/云下载的
		// storageMgr 结算语义对齐）。
		sm = h.storageMgr
	}
	if scope != nil || pool != nil || sm != nil {
		// 记账口径：**本地卷目标**按该卷的物理口径折算（加密卷 = 密文长度；明文卷 = 恒等）；
		// 外部卷无本地密文语义（远端 Stat 即真值）⇒ 恒等。必须与上传路径同源（`files.RootPhysSizer`）。
		var phys func(int64) int64
		if h.volSet.External(target) == nil {
			if tnt := h.volumeTenant(target, owner); tnt != nil {
				phys = files.RootPhysSizer(tnt.Root())
			}
		}
		fs = &backupQuotaFS{inner: fs, scope: scope, pool: pool, sm: sm, cat: capacity.CategoryChunked, log: h.logger, phys: phys}
	}
	return backupTarget{data: fs, manifest: mfs}, nil
}

// backupTargetFSBase 装配备份目标的裸 FS：数据写面（未包配额）与 manifest 功能桶写面。
//   - 外部卷（federated/baidupcs 等）：registry.ExternalBackend.FS()（写面随后端装配，
//     federated WithWriter(remote.Client.FS(ref)) 已由后端构造注入）；装配失败
//     （远端不可达）显式报错，绝不静默回落。
//   - 本地卷：owner user 桶 LocalFS。
func (h *Handlers) backupTargetFSBase(ctx context.Context, owner, target string) (syncpkg.FS, syncpkg.FS, error) {
	if be := h.volSet.External(target); be != nil {
		if probe, ok := be.(registry.HealthProbe); ok {
			if perr := probe.Ping(ctx); perr != nil && !errors.Is(perr, syncpkg.ErrUnsupported) {
				return nil, nil, fmt.Errorf("备份目标卷 %q 远端不可达: %w", target, perr)
			}
		}
		raw := be.FS()
		if raw == nil {
			return nil, nil, fmt.Errorf("备份目标卷 %q 无文件系统视图（装配错误）", target)
		}
		// P1-9 修复：备份写外部卷也须 Wrap（meta 恒生成，与上传/转存一致）；
		// P1-4 修复：Guard 现透传 StagingQuotaExempt/Capable，s3 豁免/baidupcs 自管可命中。
		// FS-CORE-3 修复：经 prefixFS 把源相对路径映射到外部卷桶键空间 `<owner>/user/...`
		// （否则 Guard 误判 meta 目录、sidecar 落用户可见目录、跨 owner 同名互覆）。
		fs := syncpkg.FS(trusted.Guard(trusted.Wrap(raw, h.trustedWrapOpts())))
		fs = syncpkg.NewPrefixFS(fs, storage.NormalizeOwner(owner)+"/user/")
		// manifest 写面：同后端键空间的功能桶 `<owner>/backup/`（不 Wrap：manifest 不需 sidecar），
		// 仍经 Guard（防意外触碰 meta 桶；标签桶非 meta，正常放行）。
		mfs := syncpkg.FS(trusted.Guard(raw))
		mfs = syncpkg.NewPrefixFS(mfs, storage.NormalizeOwner(owner)+"/"+backupManifestBucket+"/")
		// 本地 staging 配额强制接线（用户裁定 2026-10-10：外部卷统一过独立 staging
		// 配额——备份写本地暂存防打满；s3 流式显式 Exempt，baidupcs 自管 StagingQuotaCapable）。
		return h.stagingQuotaFS(owner, fs), mfs, nil
	}
	tnt := h.volumeTenant(target, owner)
	if tnt == nil || tnt.Root() == nil {
		return nil, nil, fmt.Errorf("备份目标卷 %q 不可用", target)
	}
	// 本地卷目标：root 在 tenant 根（`<storage_root>/<owner>`），键空间 = `user/<rel>`。
	// 经 Guard(Wrap)（TV-BACKUP-LOCAL-BYPASS 修复：本地目标同样建 sidecar，与上传/转存
	// 一致）；prefixFS 把源相对路径 rel 映射到 `user/rel`，Wrap 写 sidecar 到 `meta/rel.meta`。
	if _, ok := tnt.Root().Abs(""); !ok {
		return nil, nil, fmt.Errorf("备份目标卷 %q 根不可用", target)
	}
	// **经 Root 适配器**（评审第 2 轮 P1）：裸 LocalFS 会在加密目标卷上写明文、并绕过封印。
	fs := trusted.Guard(trusted.Wrap(NewRootFS(tnt.Root()), h.trustedWrapOpts()))
	// manifest 写面：租户根的 `backup/` 功能桶（与 user/meta 并列，用户 API 不可寻址）；
	// 同经 Root（manifest 含文件名，加密卷上不应明文落盘）。
	mfs := syncpkg.NewPrefixFS(NewRootFS(tnt.Root()), backupManifestBucket+"/")
	// 封装卷封印由底座统一收口：写经 Root（本地卷封印在装配层注入；外部卷封印在装配处套
	// SealFS）⇒ 命中封印象限的写恒拒 ErrSealed（阶段 2 迁移删除了过渡层在此叠加的装饰器）。
	return syncpkg.NewPrefixFS(fs, "user/"), mfs, nil
}

// backupQuotaFS 是备份目标写面的配额记账装饰器：WriteFile 前对文件 size 在三本账上预留
// ——owner user 桶 Scope、目标卷容量池（与 syncexec.quotaLocalFS 同构）、以及本地卷目标的
// **持久容量账本** capacity.StorageManager（M9b，强一致），超限拒绝（该文件计入
// Report.Failed，不中止整体）；写成功 Commit、失败 Release。其余方法透传 inner。
type backupQuotaFS struct {
	inner syncpkg.FS
	scope *quota.Scope
	pool  *quota.Pool
	sm    *capacity.StorageManager // 持久容量账本（仅本地卷目标；nil = 未装配或外部卷）
	cat   capacity.StorageCategory // 记账类别（与用户文件写面一致）
	log   *slog.Logger
	// phys 是本卷的**记账口径折算**（nil = 恒等）：加密卷 ⇒ `storage.EncryptedPhysicalSize`。
	//
	// 为什么必须有（2026-10-16 高风险区审核发现）：本装饰器的记账口径必须与**上传路径**同源
	// （`files.settleSize` ⇒ 物理），否则加密卷上「**备份按逻辑记账、上传按物理记账**」⇒
	// owner Scope **少记**（磁盘实际占用 = 物理 > 逻辑），且 owner 配额侧**永不收敛**（只有卷池
	// 靠磁盘扫描收敛）⇒ **可超真实配额**（违反 G1「绝不超真实配额」）。
	//
	// 链路依据：本地目标 = `PrefixFS(Guard(Wrap(NewRootFS(tnt.Root()))))` ⇒ `RootFS.Stat` 返回
	// `LogicalSize`（**逻辑**）、`RootFS.WriteFile` 经 `writeUserVisible` **透明加密** ⇒ 落盘为
	// 物理。故所有进入账本的尺寸都必须先经本字段折算。
	phys func(int64) int64
}

// physSize 把**逻辑**字节折算为本卷的**记账口径**（物理占用）；nil / 非正值 ⇒ 原样返回。
// **每个值只折算一次**（调用点：`statSize` 与 `WriteFile` 入口），避免双重折算。
func (q *backupQuotaFS) physSize(n int64) int64 {
	if n <= 0 || q.phys == nil {
		return n
	}
	return q.phys(n)
}

// backupReservation 是 scope+pool+持久账本三预留的聚合（任一为空则跳过）。
type backupReservation struct {
	scope, pool *quota.Reservation
	sm          *capacity.StorageManager
	cat         capacity.StorageCategory
	log         *slog.Logger
	smPre       int64 // 持久账本上已预留字节；0 = 未预留
}

func (r *backupReservation) Commit(actual int64) {
	if r.scope != nil {
		r.scope.Commit(actual)
	}
	if r.pool != nil {
		r.pool.Commit(actual)
	}
	r.settleSM(actual)
}

func (r *backupReservation) Release() {
	if r.scope != nil {
		r.scope.Release()
	}
	if r.pool != nil {
		r.pool.Release()
	}
	r.releaseSM()
}

// settleSM 把持久账本上的预留调到实际落盘大小（Commit(actual) 的持久账本版）：
// 账本无 Adjust，走「Release(pre) + TryReserve(actual)」（与云端归档/云下载回退路径同款）。
// actual>pre 且 TryReserve 失败属罕见（声明失真）：文件已在盘上，账本待下次扫描校准，
// 此处只告警不改变写入结果（禁静默）。
func (r *backupReservation) settleSM(actual int64) {
	if r.sm == nil || r.smPre <= 0 {
		return
	}
	pre := r.smPre
	r.smPre = 0
	r.sm.Release(pre, r.cat)
	if actual <= 0 {
		return
	}
	if err := r.sm.TryReserve(actual, r.cat); err != nil && r.log != nil {
		r.log.Warn("备份目标容量账本对账未达（已落盘，待下次扫描校准）", "error", err, "actual", actual)
	}
}

// releaseSM 回退持久账本预留（写失败/取消）。
func (r *backupReservation) releaseSM() {
	if r.sm == nil || r.smPre <= 0 {
		return
	}
	r.sm.Release(r.smPre, r.cat)
	r.smPre = 0
}

// smAdopt 覆盖写：把账本上的旧字节 prev 换成本次写入（预留在前，写成功后调用）；与
// Scope/Pool 的 Adjust(prev,size) 同效果——预留的 size 转为已确认占用。
func (r *backupReservation) smAdopt(prev int64) {
	if r.sm == nil {
		return
	}
	if prev > 0 {
		r.sm.Release(prev, r.cat)
	}
	r.smPre = 0
}

// reserveMemory 预留 Scope 与卷池（二者可空，任一失败自动回退已成功的一侧）。
func (r *backupReservation) reserveMemory(ctx context.Context, size int64, scope *quota.Scope, pool *quota.Pool) error {
	if scope != nil {
		s, err := scope.WaitReserve(ctx, size, quota.DefaultWaitTimeout)
		if err != nil {
			return err
		}
		r.scope = s
	}
	if pool != nil {
		p, err := pool.WaitReserve(ctx, size, quota.DefaultWaitTimeout)
		if err != nil {
			r.releaseScope()
			return err
		}
		r.pool = p
	}
	return nil
}

func (r *backupReservation) releaseScope() {
	if r.scope != nil {
		r.scope.Release()
		r.scope = nil
	}
}

func (r *backupReservation) releasePool() {
	if r.pool != nil {
		r.pool.Release()
		r.pool = nil
	}
}

func (q *backupQuotaFS) reserve(ctx context.Context, size int64) (*backupReservation, error) {
	if q.scope == nil && q.pool == nil && q.sm == nil {
		return nil, nil
	}
	res := backupReservation{sm: q.sm, cat: q.cat, log: q.log}
	if err := res.reserveMemory(ctx, size, q.scope, q.pool); err != nil {
		return nil, err
	}
	if q.sm != nil {
		if err := q.sm.TryReserve(size, q.cat); err != nil {
			res.releaseScope()
			res.releasePool()
			return nil, err
		}
		res.smPre = size
	}
	return &res, nil
}

func (q *backupQuotaFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, relPath string, r io.Reader, size, mtime int64) error {
	// 记账口径折算（见 `phys` 字段注释）：**入账前**把逻辑长度折成本卷物理占用。
	// 只在此处折一次，下方 reserve/Adjust/Commit/smAdopt 均以折算后的值为准。
	size = q.physSize(size)
	// 覆盖写差分（P1）：备份引擎对 manifest 不匹配的同 rel 做覆盖写、且每轮重写 manifest；
	// 原实现只 TryReserve+Commit(size) 而无 prev 差分 → 旧字节永不释放，Scope/池随每轮备份
	// 单调上涨 → 后续上传/备份被 507 误拒（对照上传路径 Commit(prev,written) 与 multipart
	// Adjust(prev,total)）。
	prev := q.statSize(ctx, relPath)
	res, err := q.reserve(ctx, size)
	if err != nil {
		return fmt.Errorf("备份目标配额不足: %w", err)
	}
	if res == nil {
		return q.inner.WriteFile(ctx, key, relPath, r, size, mtime)
	}
	if werr := q.inner.WriteFile(ctx, key, relPath, r, size, mtime); werr != nil {
		res.Release()
		return werr
	}
	if prev > 0 {
		// 覆盖：占用从 prev 调到 size（负 diff 沿父链按本层实际生效量传播）。
		if q.scope != nil {
			q.scope.Adjust(prev, size)
			res.scope.Release()
		}
		if q.pool != nil {
			q.pool.Adjust(prev, size)
			res.pool.Release()
		}
		res.smAdopt(prev)
	} else {
		res.Commit(size)
	}
	return nil
}

// statSize 取目标已存在文件大小（不存在/目录/失败 → 0）。
//
// 口径：`inner.Stat`（链尾 `RootFS`）返回的是 **`LogicalSize`（明文长度）** ⇒ 必须折算为物理，
// 否则覆盖写的 `prev` 与结算值不同源 ⇒ 每次覆盖写泄漏 `(物理 − 逻辑)`（幽灵占用/反向漂移）。
func (q *backupQuotaFS) statSize(ctx context.Context, relPath string) int64 {
	e, err := q.inner.Stat(ctx, relPath)
	if err != nil || e == nil || e.IsDir {
		return 0
	}
	return q.physSize(e.Size)
}

// releaseBytes 从 scope/pool 扣减已确认占用 n（删除/覆盖改名时对称释放，C-MAJOR-2 同语义）。
func (q *backupQuotaFS) releaseBytes(n int64) {
	if n <= 0 {
		return
	}
	if q.scope != nil {
		q.scope.Adjust(n, 0)
	}
	if q.pool != nil {
		q.pool.Adjust(n, 0)
	}
	if q.sm != nil {
		q.sm.Release(n, q.cat)
	}
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
	// 覆盖改名（manifest .tmp → 正式名）：目标旧字节已计入账本 → 释放（源字节已在
	// WriteFile 时计入，改名后仍代表同一批字节，不再重计）。
	prevDest := q.statSize(ctx, to)
	if err := q.inner.Rename(ctx, from, to); err != nil {
		return err
	}
	q.releaseBytes(prevDest)
	return nil
}

func (q *backupQuotaFS) Delete(ctx context.Context, p string) error {
	prev := q.statSize(ctx, p)
	if err := q.inner.Delete(ctx, p); err != nil {
		return err
	}
	q.releaseBytes(prev)
	return nil
}

// Inner 暴露被包装 FS（供 syncpkg.Innermost 下探最内层原始卷——staging 能力判定依赖）。
func (q *backupQuotaFS) Inner() syncpkg.FS { return q.inner }

// WithStagingQuota 转发到内层（P1：外层不转发会让 ApplyStagingQuota 静默不包 gate）。
func (q *backupQuotaFS) WithStagingQuota(t syncpkg.StagingQuotaTracker) {
	if sc, ok := q.inner.(syncpkg.StagingQuotaCapable); ok {
		sc.WithStagingQuota(t)
	}
}

func (q *backupQuotaFS) MakeDir(ctx context.Context, p string) error {
	return q.inner.MakeDir(ctx, p)
}

// _ 编译期断言：backupQuotaFS 实现 sync.FS。
var _ syncpkg.FS = (*backupQuotaFS)(nil)
