// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// VersionInfo 版本信息。
type VersionInfo struct {
	Filename  string `json:"filename"`
	VersionID int64  `json:"version_id"` // 毫秒时间戳×1000 + 随机后缀（见 newVersionID）
	Size      int64  `json:"size"`
	Checksum  string `json:"checksum,omitempty"`
	CreatedAt string `json:"created_at"`
}

// 版本 ID 生成器的进程内单调兜底状态。
var (
	versionIDMu   sync.Mutex
	lastVersionID int64
)

// newVersionID 生成新的文件版本 ID：**毫秒时间戳 ×1000 + 3 位随机后缀（0-999），
// 冲突时单调递增兜底**。
//
// 溢出修复：旧实现为 time.Now().UnixNano()*1000 + rand.IntN(1000)。UnixNano()≈1.76e18，
// ×1000 后≈1.76e21，远超 int64 上限 9.22e18 —— 约每 213.5 天回绕一次且符号各半，当前
// 时段生成的 ID 恒为负（客户端/服务端按正数语义消费时不可用）。改用毫秒精度 ×1000 后
// 最大约 1.79e15，远小于 int64 上限，正数可用至约 29 万年。
//
// 唯一性：同一毫秒内提供 1000 个随机后缀槽位。但无节流的连续调用可在同一毫秒内产生
// 远超 1000 次调用（实测 10000 次仅得 1000 个唯一值），故在进程内加锁保证严格单调递增
// ——候选值不前进时取 lastVersionID+1，使高频连续调用下 ID 仍唯一。
//
// 范围界定：lastVersionID 的单调性仅在**本进程内**成立，跨进程或跨重启不保证全局单调；
// 那些场景由「毫秒时间戳 + 随机后缀」本身的低碰撞概率，以及落盘侧
// O_CREATE|O_EXCL（已存在则报错）兜底，不会静默覆盖既有版本。
func newVersionID() int64 {
	id := time.Now().UnixMilli()*1000 + int64(rand.IntN(1000))
	versionIDMu.Lock()
	if id <= lastVersionID {
		id = lastVersionID + 1
	}
	lastVersionID = id
	versionIDMu.Unlock()
	return id
}

// versionIDTime 由版本 ID 还原其创建时间。
// 版本 ID = 毫秒时间戳 ×1000 + 3 位随机后缀（见 newVersionID），故 /1000 得毫秒时间戳。
// 历史遗留 ID 均无法可靠还原时间，回落 fallback（调用方传版本文件 mtime）：
//   - 非正 ID：旧纳秒 ×1000 回绕为负；
//   - 正的巨值 ID：旧纳秒 ×1000 回绕为正（约一半概率），/1000 后解码为 ~公元 10500 年。
func versionIDTime(versionID int64, fallback time.Time) time.Time {
	if versionID <= 0 {
		return fallback
	}
	ts := time.UnixMilli(versionID / 1000)
	// 合理上限：ID 派生时间不应显著超前于当前——拦截上述回绕为正的巨值遗留 ID，
	// 避免把版本显示成遥远的未来时间。
	if ts.After(time.Now().Add(24 * time.Hour)) {
		return fallback
	}
	return ts
}

// saveVersion 在上传覆盖前保存当前文件版本。
// 返回保存的版本 ID（毫秒时间戳×1000 + 随机后缀，见 newVersionID），如果没有旧文件则返回 0。
// userRel 是相对 user 桶的路径（如 dir/f.txt，无 user/ 前缀）；tnt 为请求者租户。
// 版本文件落 version 桶（version/<userRel>/<id>），checksum key = version/<userRel>/<id>
// （相对租户根，无 owner 前缀，per-tenant store）——消除旧 __version__ 前缀的 R4 碰撞。
func (h *Handlers) saveVersion(userRel string, tnt *storage.Tenant, owner string) (int64, error) {
	if tnt == nil || tnt.Root() == nil {
		return 0, fmt.Errorf("保存版本: 租户不可用")
	}
	root := tnt.Root()
	fullRel, ok := tnt.UserRel(userRel)
	if !ok {
		return 0, fmt.Errorf("保存版本: 无效的文件路径: %s", userRel)
	}
	srcInfo, statErr := root.Stat(fullRel)
	if os.IsNotExist(statErr) {
		return 0, nil // 新文件，无需保存版本
	} else if statErr != nil {
		return 0, fmt.Errorf("检查源文件失败: %w", statErr)
	}
	srcSize := srcInfo.Size()

	versionID := newVersionID()
	verDir, ok := tnt.FeatureRel("version", userRel)
	if !ok {
		return 0, fmt.Errorf("保存版本: 无效的版本目录路径: %s", userRel)
	}
	if err := root.MkdirAll(verDir, 0o755); err != nil {
		return 0, fmt.Errorf("创建版本目录失败: %w", err)
	}

	verRel := verDir + "/" + strconv.FormatInt(versionID, 10)

	// P5 版本桶配额（双账本 reserve-then-commit，AD-7）：写版本文件前在 owner 全局 version 桶
	// Scope 与 home 卷容量池**同时预留**源文件大小（版本是旧文件拷贝，字节计入 version 桶
	// Scope + 该卷容量池），写入成功后 Commit(actual)；失败/放弃双 Release。任一侧配额不足即
	// 拒绝保存版本（调用方语义与单卷 owner 全局 version Scope 满一致：覆盖写 best-effort 跳过
	// 版本、恢复路径 500 中止）——卷容量对版本字节由预留封顶（T6c 安全②：不再事后 Adjust
	// fail-open，防借版本反复写把卷堆满）。
	pool := h.volumePoolForTenant(tnt)
	var scopeRes, poolRes *quota.Reservation
	if scope := h.quotaBucketFor(owner, "version"); scope != nil {
		rr, reserveErr := scope.TryReserve(srcSize)
		if reserveErr != nil {
			return 0, fmt.Errorf("保存版本: 存储配额不足: %w", reserveErr)
		}
		scopeRes = rr
	}
	if pool != nil {
		rr, reserveErr := pool.TryReserve(srcSize)
		if reserveErr != nil {
			if scopeRes != nil {
				scopeRes.Release()
			}
			return 0, fmt.Errorf("保存版本: 卷容量不足: %w", reserveErr)
		}
		poolRes = rr
	}
	releaseRes := func() {
		if scopeRes != nil {
			scopeRes.Release()
		}
		if poolRes != nil {
			poolRes.Release()
		}
	}

	src, err := root.Open(fullRel)
	if err != nil {
		releaseRes()
		return 0, fmt.Errorf("打开源文件失败: %w", err)
	}
	defer src.Close()

	dst, err := root.OpenFile(verRel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		releaseRes()
		return 0, fmt.Errorf("创建版本文件失败: %w", err)
	}
	defer dst.Close()

	// 流式计算 checksum：一边复制一边计算 SHA-256，避免重复读取。
	// 审查 #9 结论（勿再分析）：此处是**单遍**复制+哈希（io.MultiWriter），并无
	// "先哈希再复制"的重复读取；old 文件 checksum 记录须独立重算（旧版本删过
	// checksum，store 中无可靠值），新文件由上传方提供 expectedChecksum 校验。
	// 无重复哈希可省，维持现状。
	hasher := sha256.New()
	multiWriter := io.MultiWriter(dst, hasher)
	written, err := io.Copy(multiWriter, src)
	if err != nil {
		_ = root.Remove(verRel)
		releaseRes()
		return 0, fmt.Errorf("复制版本文件失败: %w", err)
	}
	checksum := hex.EncodeToString(hasher.Sum(nil))

	// 写入 checksumStore（per-tenant store，key = version/<userRel>/<id>，无 owner 前缀）
	csKey := verRel
	if cs := h.checksumStoreFor(owner); cs != nil {
		cs.Set(csKey, checksum)
	} else {
		h.logger.Warn("per-tenant checksum store 不可用，跳过版本 checksum 记录", "file_name", userRel)
	}

	// 显式 fsync 版本文件，确保崩溃时不会丢失已保存的版本
	if err := dst.Sync(); err != nil {
		_ = root.Remove(verRel)
		releaseRes()
		return 0, fmt.Errorf("同步版本文件失败: %w", err)
	}

	// P5 配额对账：双 Commit(actual)（多预留部分自动归还，预留转 committed）。
	if scopeRes != nil {
		scopeRes.Commit(written)
		scopeRes = nil
	}
	if poolRes != nil {
		poolRes.Commit(written)
		poolRes = nil
	}

	// 清理超出上限的旧版本（删除的旧版本按文件大小释放 version 桶 Scope + 卷容量池）。
	h.cleanupOldVersions(userRel, tnt, owner)

	h.logger.Info("文件版本已保存", "file_name", userRel, "version_id", versionID)
	return versionID, nil
}

// releaseVersionUsage 释放 version 桶 Scope 中已确认占用的版本文件字节（P5）。
// 删除版本文件后按删除前 stat 的文件大小释放，避免 version 桶 committed 虚高
// 依赖周期扫描自愈。tnt 为版本文件所在卷租户——版本字节同时释放所在卷容量池
// （T6c 发现-3 双账本，与 saveVersion 写侧 Adjust 对称）。size<=0 时为空操作。
func (h *Handlers) releaseVersionUsage(tnt *storage.Tenant, owner string, size int64) {
	if size <= 0 {
		return
	}
	if scope := h.quotaBucketFor(owner, "version"); scope != nil {
		scope.ReleaseUsage(size)
	}
	if pool := h.volumePoolForTenant(tnt); pool != nil {
		pool.ReleaseCommitted(size)
	}
}

// cleanupOldVersions 删除超出 max_versions 的旧版本。
// userRel 为相对 user 桶的路径；版本文件在 version/<userRel>/ 目录下。
// P5：删除的旧版本按文件大小释放 version 桶 Scope（不依赖周期扫描自愈）。
func (h *Handlers) cleanupOldVersions(userRel string, tnt *storage.Tenant, owner string) {
	cfg := h.cfgPtr.Load()
	if cfg.Versioning.MaxVersions <= 0 {
		return
	}
	if tnt == nil || tnt.Root() == nil {
		return
	}
	root := tnt.Root()
	verDir, ok := tnt.FeatureRel("version", userRel)
	if !ok {
		return
	}
	abs, ok := root.Abs(verDir)
	if !ok {
		return
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return
	}

	if len(entries) <= cfg.Versioning.MaxVersions {
		return
	}

	// 按文件名（版本 ID 为单调递增数值）排序，删除最旧的
	// 使用 ParseInt 解析为 int64 后做数值比较，消除字符串字典序与数值序不一致的隐患。
	// 使用 SliceStable 保持相等元素的原始顺序，避免排序不稳定带来的不确定性。
	sort.SliceStable(entries, func(i, j int) bool {
		vi, erri := strconv.ParseInt(entries[i].Name(), 10, 64)
		vj, errj := strconv.ParseInt(entries[j].Name(), 10, 64)
		if erri != nil && errj != nil {
			return false
		}
		if erri != nil {
			return false
		}
		if errj != nil {
			return true
		}
		return vi < vj
	})
	excess := len(entries) - cfg.Versioning.MaxVersions
	for i := range excess {
		delRel := verDir + "/" + entries[i].Name()
		// 删除旧版本前记录文件大小，删除后释放 version 桶 Scope。
		var delSize int64
		if info, sErr := root.Stat(delRel); sErr == nil {
			delSize = info.Size()
		}
		if err := root.Remove(delRel); err != nil {
			h.logger.Warn("删除旧版本文件失败", "path", delRel, "error", err)
			continue
		}
		h.releaseVersionUsage(tnt, owner, delSize)
	}
}

// listVersionsHandler 处理 GET /api/versions?filename=xxx。
func (h *Handlers) listVersionsHandler(w http.ResponseWriter, r *http.Request) {
	filename := r.URL.Query().Get("filename")
	if filename == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "filename 不能为空"}, http.StatusBadRequest)
		return
	}
	remotePath, err := pathguard.ValidateFilePath(filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}

	cfg := h.cfgPtr.Load()
	if !cfg.Versioning.Enabled {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgVersioningDisabled}, http.StatusNotImplemented)
		return
	}

	owner := ownerFromRequest(r)
	baseTnt := h.tenantFor(owner)
	if baseTnt == nil || baseTnt.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		return
	}
	rel, relOK := baseTnt.UserRel(remotePath)
	if !relOK {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	verDir, dirOK := baseTnt.FeatureRel("version", remotePath)
	if !dirOK {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}

	// A2-D 跨卷合并：扫描 owner 视图各卷的 version/<rel> 目录并合并（version id 去重）——
	// 文件被 move 到别的卷后，留在原卷的版本仍可见。
	entries, collErr := h.collectVersionEntries(owner, remotePath)
	if collErr != nil {
		h.logger.Error("读取版本目录失败", "file_name", remotePath, "error", collErr)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "读取版本目录失败"}, http.StatusInternalServerError)
		return
	}
	// 与旧语义一致：无可列版本时，user 文件在视图内可见或默认卷对 owner 开放 → 200 空列表；
	// 否则 404 fail-closed（默认卷被 ACL 排除且视图内无该文件，不泄存在性）。
	if len(entries) == 0 {
		_, fileFound := h.locateOwnerFile(owner, rel)
		if !fileFound && !h.defaultVolumeAllows(owner) {
			sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
			return
		}
	}

	csStore := h.checksumStoreFor(owner)
	versions := make([]VersionInfo, 0, len(entries))
	for _, e := range entries {
		fi := VersionInfo{
			Filename:  filepath.ToSlash(remotePath),
			VersionID: e.versionID,
			Size:      e.info.Size(),
			// 版本 ID 为毫秒时间戳×1000+随机后缀（见 newVersionID），/1000 还原毫秒时间戳；
			// 历史遗留的非正 ID 无法还原时间，回落版本文件 mtime。
			CreatedAt: versionIDTime(e.versionID, e.info.ModTime()).Format(time.RFC3339),
		}
		// 尝试获取 checksum（per-tenant store，key = version/<rel>/<id>，与卷无关）
		if csStore != nil {
			if cs, ok := csStore.Get(verDir + "/" + e.name); ok {
				fi.Checksum = cs
			}
		}
		versions = append(versions, fi)
	}

	sendJSONResponse(w, map[string]any{"versions": versions}, http.StatusOK)
}

// restoreVersionHandler 处理 POST /api/versions/restore?filename=xxx&version_id=xxx。
func (h *Handlers) restoreVersionHandler(w http.ResponseWriter, r *http.Request) {
	filename := r.URL.Query().Get("filename")
	versionIDStr := r.URL.Query().Get("version_id")
	if filename == "" || versionIDStr == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "filename 和 version_id 不能为空"}, http.StatusBadRequest)
		return
	}

	remotePath, err := pathguard.ValidateFilePath(filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}

	cfg := h.cfgPtr.Load()
	if !cfg.Versioning.Enabled {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgVersioningDisabled}, http.StatusNotImplemented)
		return
	}

	owner := ownerFromRequest(r)
	baseTnt := h.tenantFor(owner)
	if baseTnt == nil || baseTnt.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		return
	}
	targetRel, ok := baseTnt.UserRel(remotePath)
	if !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}

	// 文件级互斥（T6c move 锁架构延伸）：restore 会写回 user 文件（可能与其 home 卷不同），
	// 与并发 move（复制→删源）共用同 rel 锁——无锁时 move 删源后 restore 可能把文件写回源卷，
	// 与目标卷副本并存（AD-4 破坏）；持锁后并发 move 期间 restore 409。
	release, locked := h.acquireFileLock(owner, targetRel)
	if !locked {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultDenied, Detail: "文件正在移动/上传中",
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件正在移动/上传中，请稍后重试"}, http.StatusConflict)
		return
	}
	defer release()

	// A2-D 跨卷定位：版本文件可能留在 user 文件曾所在的卷（跨卷 move 后版本不迁移）。
	verLoc, verRel, verInfo, found, ferr := h.findVersionFile(owner, remotePath, versionIDStr)
	if ferr != nil {
		h.logger.Error("stat 版本文件失败", "file_name", remotePath, "error", ferr)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "访问版本文件失败"}, http.StatusInternalServerError)
		return
	}
	if !found {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "版本文件不存在: " + versionIDStr,
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "版本文件不存在"}, http.StatusNotFound)
		return
	}

	// 目标卷 = user 文件**当前**所在卷（AD-4：不因跨卷恢复分叉出双份；版本字节不迁移，
	// 配额仍计在原卷池）。文件已不可定位（已删除）→ 回落版本文件所在卷（与旧孤儿恢复一致）。
	dstTnt, dstVol := verLoc.tenant, verLoc.volumeName
	if floc, ffound := h.locateOwnerFile(owner, targetRel); ffound && floc != nil && floc.tenant != nil && floc.tenant.Root() != nil {
		dstTnt, dstVol = floc.tenant, floc.volumeName
	}
	dstRoot := dstTnt.Root()

	// 先保存当前版本（回滚前备份）到目标卷（文件所在卷），备份失败时返回 500 拒绝执行恢复
	if _, err = h.saveVersion(remotePath, dstTnt, owner); err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "恢复前备份失败: " + versionIDStr,
		})
		h.logger.Error("恢复版本前备份失败", "file_name", remotePath, "error", err)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "恢复版本前备份失败，已中止"}, http.StatusInternalServerError)
		return
	}

	// P4/P5 配额（I3 修复）：恢复把版本文件拷回 user 桶（覆盖当前文件），本质是新增
	// user 桶字节——缺失配额可反复 restore 突破租户上限。与 upload 对齐：TryReserve(版本大小)
	// 预留 → 拷贝成功后 Adjust(prev, actual)（覆盖写）/ Commit(actual)（新文件）；失败 Release()。
	// scope 按目标 user 桶 rel 解析（与 upload 同一键），子目录配额逐级检查自动生效。
	// 卷容量池同族（T6c 安全② reserve-then-commit）：恢复新增/覆盖的 user 字节先 TryReserve
	// **目标卷**容量池（user 字节物理落在目标卷，写前封顶），任一侧配额不足 → 507 拒绝恢复。
	scope := h.quotaScopeFor(owner, targetRel)
	pool := h.volumePoolForTenant(dstTnt)
	prev := int64(0)
	if st, statErr := dstRoot.Stat(targetRel); statErr == nil {
		prev = st.Size()
	}
	var res, poolRes *quota.Reservation
	if scope != nil {
		rr, reserveErr := scope.TryReserve(verInfo.Size())
		if reserveErr != nil {
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "version_restore", ObjectType: "file", Object: remotePath,
				Result: AuditResultError, Detail: "存储配额不足，拒绝恢复",
			})
			sendJSONResponse(w, UploadResponse{Success: false, Message: "存储配额不足"}, http.StatusInsufficientStorage)
			return
		}
		res = rr
	}
	if pool != nil {
		rr, reserveErr := pool.TryReserve(verInfo.Size())
		if reserveErr != nil {
			if res != nil {
				res.Release()
			}
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "version_restore", ObjectType: "file", Object: remotePath,
				Result: AuditResultError, Detail: "卷容量不足，拒绝恢复",
			})
			sendJSONResponse(w, UploadResponse{Success: false, Message: "存储配额不足"}, http.StatusInsufficientStorage)
			return
		}
		poolRes = rr
	}
	releaseRes := func() {
		if res != nil {
			res.Release()
		}
		if poolRes != nil {
			poolRes.Release()
		}
	}

	// 拷贝版本文件到目标位置：同卷 → 原地覆盖（既有语义，权限 0644）；跨卷 → 跨根复制
	// （temp + fsync + 原子 rename，MkdirAll 目标父目录），避免把文件写回源卷造成双份。
	var written int64
	if dstVol == verLoc.volumeName {
		src, oerr := verLoc.tenant.Root().Open(verRel)
		if oerr != nil {
			releaseRes()
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "version_restore", ObjectType: "file", Object: remotePath,
				Result: AuditResultError, Detail: "打开版本文件失败: " + versionIDStr,
			})
			sendJSONResponse(w, UploadResponse{Success: false, Message: "打开版本文件失败"}, http.StatusInternalServerError)
			return
		}
		defer src.Close()

		dst, derr := dstRoot.OpenFile(targetRel, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if derr != nil {
			releaseRes()
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "version_restore", ObjectType: "file", Object: remotePath,
				Result: AuditResultError, Detail: "创建目标文件失败: " + versionIDStr,
			})
			sendJSONResponse(w, UploadResponse{Success: false, Message: "创建目标文件失败"}, http.StatusInternalServerError)
			return
		}
		defer dst.Close()

		written, err = io.Copy(dst, src)
		if err != nil {
			releaseRes()
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "version_restore", ObjectType: "file", Object: remotePath,
				Result: AuditResultError, Detail: "恢复文件失败: " + versionIDStr,
			})
			sendJSONResponse(w, UploadResponse{Success: false, Message: "恢复文件失败"}, http.StatusInternalServerError)
			return
		}
		if syncErr := dst.Sync(); syncErr != nil {
			releaseRes()
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "version_restore", ObjectType: "file", Object: remotePath,
				Result: AuditResultError, Detail: "同步文件失败: " + versionIDStr,
			})
			sendJSONResponse(w, UploadResponse{Success: false, Message: "同步文件失败"}, http.StatusInternalServerError)
			return
		}
	} else {
		written, err = crossVolumeCopy(r.Context(), verLoc.tenant.Root(), dstRoot, verRel, targetRel)
		if err != nil {
			releaseRes()
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "version_restore", ObjectType: "file", Object: remotePath,
				Result: AuditResultError, Detail: "跨卷恢复文件失败: " + versionIDStr,
			})
			h.logger.Error("跨卷恢复版本失败", "file_name", remotePath, "from", verLoc.volumeName,
				"to", dstVol, "error", err)
			sendJSONResponse(w, UploadResponse{Success: false, Message: "恢复文件失败"}, http.StatusInternalServerError)
			return
		}
	}

	// P4/P5 配额对账：覆盖写 Adjust(prev, written) + Release 预留；新文件 Commit(written)。
	// owner 全局 scope 与卷容量池同语义（卷池侧同样 reserve-then-commit，物理字节入卷池）。
	if res != nil {
		if prev > 0 {
			scope.Adjust(prev, written)
			res.Release()
		} else {
			res.Commit(written)
		}
	}
	if poolRes != nil {
		if prev > 0 {
			pool.Adjust(prev, written)
			poolRes.Release()
		} else {
			poolRes.Commit(written)
		}
	}

	// 更新 checksum（per-tenant store，key = user 桶相对路径）
	checksum, err := FileChecksumRoot(dstRoot, targetRel)
	if err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "计算文件校验和失败: " + versionIDStr,
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "计算文件校验和失败"}, http.StatusInternalServerError)
		return
	}
	if cs := h.checksumStoreFor(ownerFromRequest(r)); cs != nil {
		cs.Set(targetRel, checksum)
	}

	h.RecordAudit(r.Context(), AuditEvent{
		Action: "version_restore", ObjectType: "file", Object: remotePath,
		Result: AuditResultSuccess, Detail: "version_id=" + versionIDStr,
	})
	h.logger.Info("文件版本已恢复", "file_name", remotePath, "version_id", versionIDStr)
	sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("已恢复版本 %s", versionIDStr), Checksum: checksum}, http.StatusOK)
}

// deleteVersionHandler 处理 DELETE /api/versions?filename=xxx&version_id=xxx。
func (h *Handlers) deleteVersionHandler(w http.ResponseWriter, r *http.Request) {
	filename := r.URL.Query().Get("filename")
	versionIDStr := r.URL.Query().Get("version_id")
	if filename == "" || versionIDStr == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "filename 和 version_id 不能为空"}, http.StatusBadRequest)
		return
	}

	remotePath, err := pathguard.ValidateFilePath(filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}

	cfg := h.cfgPtr.Load()
	if !cfg.Versioning.Enabled {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgVersioningDisabled}, http.StatusNotImplemented)
		return
	}

	owner := ownerFromRequest(r)
	baseTnt := h.tenantFor(owner)
	if baseTnt == nil || baseTnt.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		return
	}
	userRel, relOK := baseTnt.UserRel(remotePath)
	if !relOK {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}

	// 文件级互斥（T6c move 锁架构延伸）：版本删除与同 rel 的 move/上传/恢复共用锁，
	// 避免与并发 restore（恢复前备份）等操作交错。
	release, locked := h.acquireFileLock(owner, userRel)
	if !locked {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_delete", ObjectType: "file", Object: remotePath,
			Result: AuditResultDenied, Detail: "文件正在移动/上传中",
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件正在移动/上传中，请稍后重试"}, http.StatusConflict)
		return
	}
	defer release()

	// A2-D 跨卷定位：版本文件可能留在 user 文件曾所在的卷（跨卷 move 后版本不迁移）。
	verLoc, verRel, verInfo, found, ferr := h.findVersionFile(owner, remotePath, versionIDStr)
	if ferr != nil {
		h.logger.Error("stat 版本文件失败", "file_name", remotePath, "error", ferr)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "访问版本文件失败"}, http.StatusInternalServerError)
		return
	}
	if !found {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_delete", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "版本文件不存在: " + versionIDStr,
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "版本文件不存在"}, http.StatusNotFound)
		return
	}
	// P5 版本桶配额：删除前记录文件大小，删除成功后释放**版本所在卷**的版本桶 Scope 与卷池。
	delSize := verInfo.Size()
	if err := verLoc.tenant.Root().Remove(verRel); err != nil {
		if os.IsNotExist(err) {
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "version_delete", ObjectType: "file", Object: remotePath,
				Result: AuditResultError, Detail: "版本文件不存在: " + versionIDStr,
			})
			sendJSONResponse(w, UploadResponse{Success: false, Message: "版本文件不存在"}, http.StatusNotFound)
		} else {
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "version_delete", ObjectType: "file", Object: remotePath,
				Result: AuditResultError, Detail: "删除版本文件失败: " + versionIDStr,
			})
			sendJSONResponse(w, UploadResponse{Success: false, Message: "删除版本文件失败"}, http.StatusInternalServerError)
		}
		return
	}
	h.releaseVersionUsage(verLoc.tenant, owner, delSize)

	// 清理 checksumStore 中对应的版本记录（key = version/<rel>/<id>，无 owner 前缀，与卷无关）
	if verDir, dirOK := baseTnt.FeatureRel("version", remotePath); dirOK {
		if cs := h.checksumStoreFor(owner); cs != nil {
			cs.Delete(verDir + "/" + versionIDStr)
		}
	}

	h.RecordAudit(r.Context(), AuditEvent{
		Action: "version_delete", ObjectType: "file", Object: remotePath,
		Result: AuditResultSuccess, Detail: "version_id=" + versionIDStr,
	})
	sendJSONResponse(w, UploadResponse{Success: true, Message: "版本已删除"}, http.StatusOK)
}

// versionLocation 是一次版本定位结果：版本（目录）所在卷名 + 该卷上 owner 租户。
// volumeName 空 = 旧装配路径（volSet nil，唯一根无卷语义）。
type versionLocation struct {
	volumeName string
	tenant     *storage.Tenant
}

// versionDirLocations 返回 owner 视图内**存在** version/<remotePath> 目录的卷位置（卷声明序，
// 默认卷在前）。跨卷版本可见性（A2-D）：文件被跨卷 move 到别的卷后，其版本仍留在原卷——
// 此处不再只认「user 文件的 home 卷」，而是扫描 owner 视图各卷的版本目录，使 list/restore/
// delete 在文件位于任意卷时都可用（版本字节不迁移、配额仍计在原卷池 + owner 全局）。
//
// ACL（AD-6）：仅 owner 视图内卷参与扫描，默认卷被排除则不列（不泄无权卷版本）。
// 只读：先以卷根 Stat 探测版本目录，存在才经 volumeTenant 取租户（目录已在 → MkdirAll 空操作，
// GET 不会凭空创建租户目录）。
// volSet nil（旧装配单卷唯一根）：直接返回默认租户（唯一根，零回归）。
func (h *Handlers) versionDirLocations(owner, remotePath string) []*versionLocation {
	owner = normalizeOwner(owner)
	baseTnt := h.tenantFor(owner)
	if baseTnt == nil || baseTnt.Root() == nil {
		return nil
	}
	verDir, ok := baseTnt.FeatureRel("version", remotePath)
	if !ok {
		return nil
	}
	if h.volSet == nil {
		return []*versionLocation{{tenant: baseTnt}}
	}
	var out []*versionLocation
	for _, v := range volume.AllowedVolumes(h.volSet.All(), owner) {
		rt := h.volSet.Root(v.Name)
		if rt == nil {
			continue
		}
		// 卷根下路径 = <owner>/<功能桶 rel>（verDir 是租户根相对路径）。
		if _, err := rt.Stat(owner + "/" + verDir); err != nil {
			continue // 该卷无此版本目录（正常：版本随文件原卷）
		}
		tnt := h.volumeTenant(v.Name, owner)
		if tnt == nil || tnt.Root() == nil {
			continue
		}
		out = append(out, &versionLocation{volumeName: v.Name, tenant: tnt})
	}
	return out
}

// findVersionFile 跨卷定位 version/<remotePath>/<versionIDStr>（owner 视图，卷声明序默认卷优先）。
// 返回版本文件所在位置、其相对卷根的 rel 与文件信息；未命中 found=false；stat 非「不存在」错误
// （权限/IO）→ err 非空（调用方 500 fail-closed，不把探测失败当「版本不存在」）。
// 同一 version id 若异常地出现在多卷，首次命中（默认卷优先）胜出（版本文件不跨卷复制，
// 正常不可达；见 A2-D 报告）。
func (h *Handlers) findVersionFile(owner, remotePath, versionIDStr string) (*versionLocation, string, os.FileInfo, bool, error) {
	for _, loc := range h.versionDirLocations(owner, remotePath) {
		verDir, ok := loc.tenant.FeatureRel("version", remotePath)
		if !ok {
			continue
		}
		verRel := verDir + "/" + versionIDStr
		info, err := loc.tenant.Root().Stat(verRel)
		if err == nil {
			return loc, verRel, info, true, nil
		}
		if !os.IsNotExist(err) {
			return nil, "", nil, false, fmt.Errorf("stat 版本文件失败（卷 %q）: %w", loc.volumeName, err)
		}
	}
	return nil, "", nil, false, nil
}

// versionEntry 是合并列表中的一个版本条目：版本 ID + 目录项原始名（checksum key 用）+ 文件信息。
type versionEntry struct {
	versionID int64
	name      string
	info      os.FileInfo
}

// collectVersionEntries 合并 owner 各卷 version/<remotePath> 目录条目（卷声明序 + 各卷
// ReadDir 名序；单卷形态与旧行为逐条一致）。同一 version id 重复（异常）时首次命中胜出。
// ReadDir 遇「目录不存在」（IsNotExist，路径被并发删除/从不存在的探查残留）→ 按空目录跳过；
// 其它错误（权限/IO）→ 返回错误（调用方 500 fail-closed，不把「读不到」当「无版本」静默给空列表）。
func (h *Handlers) collectVersionEntries(owner, remotePath string) ([]versionEntry, error) {
	var out []versionEntry
	seen := make(map[int64]bool)
	for _, loc := range h.versionDirLocations(owner, remotePath) {
		verDir, ok := loc.tenant.FeatureRel("version", remotePath)
		if !ok {
			continue
		}
		abs, ok := loc.tenant.Root().Abs(verDir)
		if !ok {
			continue
		}
		dirEntries, err := os.ReadDir(abs)
		if os.IsNotExist(err) {
			continue // 目录不存在 → 空目录（容忍「Stat 之后被删」竞态；volSet nil 旧装配 Get 不退化 500）
		}
		if err != nil {
			return nil, fmt.Errorf("读取卷 %q 版本目录失败: %w", loc.volumeName, err)
		}
		for _, e := range dirEntries {
			versionID, perr := strconv.ParseInt(e.Name(), 10, 64)
			if perr != nil || seen[versionID] {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			seen[versionID] = true
			out = append(out, versionEntry{versionID: versionID, name: e.Name(), info: info})
		}
	}
	return out, nil
}

// saveVersionBeforeOverwrite 在文件即将被覆盖前保存旧版本。
// 在 upload handler 中调用，如果版本管理启用则保存当前版本。tnt 为旧文件实际所在卷的租户
// （覆盖写 stay-home 定位后的 home 卷；单卷 = 默认租户）。version/ 桶随 user/ 文件同卷（AD-5）。
func (h *Handlers) saveVersionBeforeOverwrite(r *http.Request, remotePath string, tnt *storage.Tenant) {
	cfg := h.cfgPtr.Load()
	if !cfg.Versioning.Enabled {
		return
	}
	if tnt == nil || tnt.Root() == nil {
		h.logger.Warn("saveVersionBeforeOverwrite: 租户不可用", "remote_path", remotePath)
		return
	}
	fullRel, ok := tnt.UserRel(remotePath)
	if !ok {
		h.logger.Warn("saveVersionBeforeOverwrite: 无效路径", "remote_path", remotePath)
		return
	}
	if _, err := tnt.Root().Stat(fullRel); err != nil {
		if os.IsNotExist(err) {
			return
		}
		h.logger.Warn("saveVersionBeforeOverwrite: 检查文件失败", "remote_path", remotePath, "error", err)
		return
	}
	userRel := strings.TrimPrefix(fullRel, tnt.UserRoot()+"/")
	if _, err := h.saveVersion(userRel, tnt, ownerFromRequest(r)); err != nil {
		h.logger.Warn("保存文件版本失败", "file_name", remotePath, "error", err)
	}
}
