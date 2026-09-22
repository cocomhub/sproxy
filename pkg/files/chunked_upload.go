// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/internal/shortid"
	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// validateChunkChecksum 校验 chunk_checksum 是否为有效的 64 位 hex 字符串。
// 使用 hex.DecodeString + 长度检查实现，一次调用即可完成验证。
func validateChunkChecksum(checksum string) bool {
	if len(checksum) != 64 {
		return false
	}
	_, err := hex.DecodeString(checksum)
	return err == nil
}

// chunkOverheadMargin 是 multipart 表单开销的预估余量，超过此值的 chunk 会被服务端裁剪。
// 客户端 chunk 实际数据 + 此余量必须 ≤ DefaultChunkBodyLimit。
const chunkOverheadMargin = 4 * 1024 // 4 KiB

// negotiateChunkSize 协商分块大小：使用客户端传入的值，但不超过服务端上限。
func negotiateChunkSize(clientChunkSize, cfgChunkSize int64) (chunkSize int64, adjusted bool) {
	chunkSize = clientChunkSize
	if chunkSize <= 0 {
		chunkSize = cfgChunkSize
	}
	if chunkSize <= 0 {
		chunkSize = size.DefaultChunkSize
	}
	if chunkSize > size.DefaultChunkBodyLimit-chunkOverheadMargin {
		chunkSize = size.DefaultChunkBodyLimit - chunkOverheadMargin
		adjusted = true
	}
	return chunkSize, adjusted
}

// checkExistingFileForInit 检查目标文件（租户 user 桶内）是否已存在。
// tnt 非 nil、rel 为租户根内 user 桶相对路径（uploadInit 已解析）。返回 true 表示已处理（调用方应 return）。
func (s *Service) checkExistingFileForInit(w http.ResponseWriter, tnt *storage.Tenant, rel, filename, fileChecksum string) bool {
	if tnt == nil || tnt.Root() == nil {
		s.sendJSON(w, ChunkedInitResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return true
	}
	root := tnt.Root()
	stat, err := root.Stat(rel)
	if err != nil {
		return false // 文件不存在，继续正常流程
	}
	if verifyFileWithChecksumRoot(root, rel, fileChecksum) {
		s.rt.logger().Info("文件已存在，跳过上传", "file_name", filename, "size", stat.Size(), "checksum", shortid.ShortHash(fileChecksum))
		s.sendJSON(w, ChunkedInitResponse{
			Success:  true,
			UploadID: "already_exists",
			Message:  fmt.Sprintf(errFmtFileExists, stat.Size()),
		}, http.StatusOK)
		return true
	}
	// 文件存在但 checksum 不匹配：versioning 开启时视为有意覆盖旧版本（进入分块流程，
	// 由 complete 先 SaveVersion 备份再覆盖，配额完整对账）；否则不允许覆盖。
	if s.rt.versioningEnabled() {
		s.rt.logger().Info("同名文件已存在但 checksum 不匹配，versioning 开启视为覆盖",
			"file_name", filename, "old_size", stat.Size())
		return false
	}
	s.rt.logger().Warn("同名文件已存在但 checksum 不匹配", "file_name", filename)
	s.sendJSON(w, ChunkedInitResponse{Success: false, Message: "同名文件已存在但 checksum 不匹配"}, http.StatusConflict)
	return true
}

// parseChunkFormParams 解析分块上传请求中的表单参数。
func parseChunkFormParams(r *http.Request) (uploadID string, chunkIndex int, chunkChecksum string, ok bool) {
	uploadID = r.FormValue("upload_id")
	chunkIndexStr := r.FormValue("chunk_index")
	chunkChecksum = r.FormValue("chunk_checksum")

	if uploadID == "" || chunkIndexStr == "" {
		return "", 0, "", false
	}
	if !validateChunkChecksum(chunkChecksum) {
		return "", 0, "", false
	}
	if _, err := fmt.Sscanf(chunkIndexStr, "%d", &chunkIndex); err != nil {
		return "", 0, "", false
	}
	return uploadID, chunkIndex, chunkChecksum, true
}

// maxTotalChunks 是单个分块上传会话允许的最大分块数（服务端元数据上界，DoS 防护）。
//
// 为何需要上界：ChunkedUploadSession 按 total_chunks **等长分配**两块元数据——
// ReceivedChunks([]bool) 与 ChunkChecksums([]string)，约 17 B/块（1 B 位 + 16 B 字符串头），
// 而 init 此前只校验 total_chunks > 0 ⇒ 单个请求即可让服务端为一个会话分配 GiB 级内存
// （实测：total_chunks=2^24 ⇒ 堆增长 ~528 MiB，线性放大）。
//
// 取值 1<<16（65536）的依据（两侧夹逼，非拍脑袋）：
//   - 不得拒掉合法大文件：本仓客户端 pkg/client/chunked.go 的分块协商把分块数控制在
//     **~512 量级**（chunkSize 从首选值翻倍直到 chunkSize*512 >= fileSize，上限
//     size.DefaultMaxChunkSize=64 MiB）⇒ 65536 是其 128 倍余量。
//   - 元数据必须小：65536 块 ⇒ 约 2 MiB/会话。
//
// 边界与裁剪交互（复核 S1，均经推导/实测）：拒绝 ⇔ `ceil(total_size / chunk_size) > maxTotalChunks`，
// 其中第二道校验用的是**裁剪后**的 chunk_size（上限 `DefaultChunkBodyLimit - chunkOverheadMargin`
// = 67,104,768 B）⇒ 各 chunk_size 下可上传的最大单文件：
//
//	64 MiB（客户端自适应上限）→ 65536 × 67,104,768 ≈ 3.9990 TiB
//	4 MiB（SDK / sclient / Web UI 默认起点）→ 256 GiB
//	1 MiB（`--chunk-size 1MiB` 合法取值）→ 64 GiB
//	4 KiB → 256 MiB
//
// 因此「恰好 4 TiB」会被拒：声明 64 MiB 时 65536 块过第一道，裁剪到 67,104,768 后 ceil 得 65,546
// ⇒ 第二道 400（这正是第二道校验的真实作用）。默认客户端不受影响（`pkg/client` 的 calcChunkSize
// 自适应 4→64 MiB，≤32 GiB 文件恒 ~512 块）；受影响的是「用户自设极小 chunk_size + 大文件」与
// TiB 级单文件，属针对 DoS 的必要取舍，已写入 docs/api.md。
// 另：单测 `TestMaxTotalChunksDerivedFromLimits` 钉住「上界不得小到装不下 UploadBodyLimit 级别的文件」，
// 降低上界必须同步改文档/契约。
const maxTotalChunks = 1 << 16

// validateChunkPlan 校验客户端声明的分块计划：上下界 + 乘法溢出 + 覆盖性
// （chunk_size*total_chunks >= total_size）。返回的 error 文案直接作为对外 400 消息
// （与拆分前的四条校验文案逐字一致，避免客户端/用例感知变化）。
// 抽成纯函数是为了让边界与溢出可被确定性单测覆盖（HTTP 层只做接线）。
func validateChunkPlan(totalSize, chunkSize int64, totalChunks int) error {
	if totalSize <= 0 {
		return errors.New("total_size 必须大于 0")
	}
	if chunkSize <= 0 {
		return errors.New("chunk_size 必须大于 0")
	}
	if totalChunks <= 0 {
		return errors.New("total_chunks 必须大于 0")
	}
	if totalChunks > maxTotalChunks {
		return fmt.Errorf("total_chunks 超出上限 %d", maxTotalChunks)
	}
	// 溢出安全：chunk_size 与 total_chunks 都是客户端输入，直接相乘可能 int64 回绕（回绕后的值
	// 小于 total_size 时下面的覆盖性检查会误判）。先判乘法是否溢出，再比较。
	// 注（复核 S5 修正口径）：真正乘积 > MaxInt64 时乘积必然 ≥ total_size，所以回绕**不会**让
	// 「覆盖不足」的计划被放行；这条判据的意义是拦下荒谬声明（如 chunk_size=MaxInt64）并让 int64
	// 语义明确，而不是补一个可被绕过的漏洞（旧注释的说法过强）。
	if chunkSize > math.MaxInt64/int64(totalChunks) {
		return errors.New("chunk_size 过大：chunk_size * total_chunks 超出 int64 范围")
	}
	if chunkSize*int64(totalChunks) < totalSize {
		return errors.New("chunk_size * total_chunks 应 >= total_size")
	}
	return nil
}

// UploadInit 初始化一个分块上传会话。
func (s *Service) UploadInit(w http.ResponseWriter, r *http.Request) {
	// 限制请求体大小
	r.Body = http.MaxBytesReader(w, r.Body, size.MultipartBufSize) // 1MB 足够
	var req ChunkedInitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendJSON(w, ChunkedInitResponse{Success: false, Message: "请求体解析失败"}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		s.sendJSON(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}

	s.rt.logger().Debug("uploadInit 请求", "file_name", req.Filename, "total_size", req.TotalSize,
		"chunk_size", req.ChunkSize, "total_chunks", req.TotalChunks,
		"file_checksum", shortid.ShortHash(req.FileChecksum), "upload_id", req.UploadID)

	// 校验字段
	if req.UploadID == "" {
		s.sendJSON(w, ChunkedInitResponse{Success: false, Message: "缺少 upload_id"}, http.StatusBadRequest)
		return
	}
	// upload_id 用裸 id，过 pkg/storage.ValidSegmentName（段名校验单一权威）防路径穿越：
	// 拒绝 / \、".."、".__" 前缀、Windows 非法字符与保留设备名、尾点/尾空格、超长。
	// 租户隔离由 per-tenant chunk 桶物理保证（会话只在本租户 chunk/ 下创建），无需 owner 前缀。
	if !storage.ValidSegmentName(req.UploadID) {
		s.sendJSON(w, ChunkedInitResponse{Success: false, Message: "无效的 upload_id"}, http.StatusBadRequest)
		return
	}
	if _, err := pathguard.ValidateFilePath(req.Filename); err != nil {
		s.sendJSON(w, ChunkedInitResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}
	// 分块计划校验（上下界 + 乘法溢出 + 覆盖性）：ChunkedUploadSession 按 total_chunks
	// **等长分配**两块元数据（ReceivedChunks/ChunkChecksums）⇒ 无上界即单请求内存放大，
	// 且 chunk_size*total_chunks 可回绕绕过覆盖性检查。判据见 maxTotalChunks/validateChunkPlan。
	if err := validateChunkPlan(req.TotalSize, req.ChunkSize, req.TotalChunks); err != nil {
		s.sendJSON(w, ChunkedInitResponse{Success: false, Message: err.Error()}, http.StatusBadRequest)
		return
	}
	if !validateChunkChecksum(req.FileChecksum) {
		s.sendJSON(w, ChunkedInitResponse{Success: false, Message: "file_checksum 不是有效的 hex"}, http.StatusBadRequest)
		return
	}

	// 取 owner 的租户与 per-tenant UploadStore（会话目录 <默认卷根>/<owner>/chunk/<id>/；
	// 会话元数据统一默认卷 chunk 桶，temp 整文件在目标卷 user 桶，见 routeUpload 注释）。
	owner := s.rt.actorOf(r)
	store := s.rt.uploadStore(owner)
	if store == nil {
		s.rt.logger().Error("获取 per-tenant UploadStore 失败", "owner", owner)
		s.sendJSON(w, ChunkedInitResponse{Success: false, Message: "创建上传会话失败"}, http.StatusInternalServerError)
		return
	}
	defTnt := s.rt.tenantOf(owner)
	if defTnt == nil || defTnt.Root() == nil {
		s.sendJSON(w, ChunkedInitResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	rel, ok := defTnt.UserRel(req.Filename)
	// tnt 是目标卷租户（temp 文件写盘根）。新会话由 routeUpload 定卷后赋值；续传会话首个
	// init 已定卷（session.Volume），仅按需派生（见 !reused 块）。
	var tnt *storage.Tenant
	if !ok {
		s.sendJSON(w, ChunkedInitResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}

	// 排他上传检查：同一文件不能并发上传（与单次上传共用 FileLocks 键空间）
	releaseUpload, acquired := s.rt.fileLocks().TryMark(owner, rel, req.UploadID)
	if !acquired {
		s.sendJSON(w, ChunkedInitResponse{Success: false, Message: "该文件正在上传中"}, http.StatusConflict)
		return
	}
	defer releaseUpload()

	// 目标卷路由准备（AD-5：init 即定卷——chunk/temp/complete 与最终 user 文件同卷，rename
	// 原子）。语义与 upload handler 对齐：
	//   - 显式 volume（非空）→ 只该卷候选（routeUpload 唯一性 409），不做 home dup-check；
	//   - auto（显式空）→ 写前 locateOwnerFile 定位 rel home 卷：命中 → 在该 home 卷根做
	//     幂等/冲突/版本检查，覆盖写 stay-home（forceHomeVol=home）；locate miss → 新文件
	//     交容量路由（无 dup-check）。volSet nil（旧装配单卷）跳过定位（唯一根即 home）。
	//   - 实际 routeUpload（预留）延到 GetOrCreateSession 之后仅对新会话执行（续传会话复用
	//     已定卷与预留，避免双计）。
	explicitVol := req.Volume
	if explicitVol == "" {
		explicitVol = r.URL.Query().Get("volume")
	}
	forceHomeVol := ""
	if explicitVol == "" {
		// home dup-check 只在「文件确实存在于 owner 视图」时执行（幂等 200 / versioning 关
		// checksum 冲突 409）；locate miss（视图内无此 rel）视为新文件跳过——防对无权卷遗留
		// 文件做版本化覆盖写/假冲突（AD-6 写侧闭合，F-1 同族）。volSet nil（旧装配唯一根）
		// 恒在默认租户 dup-check（单卷零回归）。
		var homeTnt *storage.Tenant
		if s.rt.volSet() == nil {
			homeTnt = defTnt
		} else if loc, found := s.rt.locateOwnerFile(owner, rel); found && loc.Tenant != nil {
			forceHomeVol = loc.VolumeName
			homeTnt = loc.Tenant
		}
		if homeTnt != nil {
			if s.checkExistingFileForInit(w, homeTnt, rel, req.Filename, req.FileChecksum) {
				return // 幂等 200 / checksum 冲突 409（versioning 关）已回包
			}
		}
	}

	// 分块大小协商
	chunkSize, adjusted := negotiateChunkSize(req.ChunkSize, s.rt.chunkSize())
	if adjusted {
		s.rt.logger().Info("chunk_size 超出服务端上限，自动裁剪",
			"client_chunk_size", req.ChunkSize,
			"max_chunk_upload_bytes", size.DefaultChunkBodyLimit,
			"file_name", req.Filename,
			"upload_id", shortid.ShortHash(req.UploadID))
		// 重算后的分块数同样必须过校验：这条路径的输入（total_size）仍来自客户端，
		// 否则「超大 total_size + 被裁剪的 chunk_size」可以绕过上界。
		// 用除法算 ceil 而非 (size+chunkSize-1)/chunkSize，后者对超大 total_size 会溢出。
		totalChunks := req.TotalSize / chunkSize
		if req.TotalSize%chunkSize != 0 {
			totalChunks++
		}
		req.TotalChunks = int(totalChunks)
		if err := validateChunkPlan(req.TotalSize, chunkSize, req.TotalChunks); err != nil {
			s.sendJSON(w, ChunkedInitResponse{Success: false, Message: err.Error()}, http.StatusBadRequest)
			return
		}
	}

	// 任务 4 设计决策②：同名存活 session 同 checksum 复用续传、不同 checksum 直拒（Conflict）。
	// 预检：存在未完成同名会话但 checksum/大小不一致 → 拒绝（避免同目标两个在途会话）。
	if existing := store.GetSessionByFilename(req.Filename); existing != nil {
		if existing.FileChecksum != req.FileChecksum || existing.TotalSize != req.TotalSize {
			s.rt.logger().Warn("同名会话已存在但 checksum 不匹配，拒绝创建新会话",
				"file_name", req.Filename, "upload_id", req.UploadID)
			s.sendJSON(w, ChunkedInitResponse{Success: false, Message: "同名文件正在上传中且 checksum 不一致"}, http.StatusConflict)
			return
		}
	}

	// 会话直接以裸 id 创建于本租户 store（无 owner 前缀；隔离靠 per-tenant chunk 桶）
	session, reused, err := store.GetOrCreateSession(req.UploadID, req.Filename,
		req.TotalSize, chunkSize, req.TotalChunks, req.FileChecksum, req.FileModTime)
	if err != nil {
		s.rt.logger().Error("创建/续传上传会话失败", "upload_id", req.UploadID, "error", err)
		s.sendJSON(w, ChunkedInitResponse{Success: false, Message: "创建上传会话失败"}, http.StatusInternalServerError)
		return
	}

	if !reused {
		// 任务 4：在途整文件（user 桶目标同目录）与配额预留。
		// 多卷（AD-5/AD-7）：routeUpload 先定卷 + owner 全局 Scope + 卷容量池双 TryReserve
		// （507 时清理 session 返回 507，不创建临时名）；tnt 为 route 目标卷租户（temp/complete
		// 同卷 rename 原子）。单卷旧装配（volSet nil）：routeUpload 回落既有 scope TryReserve
		// 语义；scope 未装配（quota nil）时回退旧 storageMgr 预留（与改造前一致，零回归）。
		route, routeErr := s.rt.routeUpload(owner, rel, explicitVol, req.TotalSize, forceHomeVol)
		if routeErr != nil {
			store.cleanupSessionIfCurrent(session.UploadID, session)
			s.sendUploadRouteError(w, r, req.Filename, routeErr)
			return
		}
		if route.Tenant == nil || route.Tenant.Root() == nil {
			route.Release()
			store.cleanupSessionIfCurrent(session.UploadID, session)
			s.sendJSON(w, ChunkedInitResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
			return
		}
		tnt = route.Tenant
		// 定卷/预留写回 store 持有的会话对象：必须经锁内 setter（该对象会被并发请求经
		// GetSession/PersistNow 整结构深拷贝，直接改字段即数据竞争，审计 C-8）。
		// 三处发布均走**身份门控** setter（按注册世代判定「表内该 id 仍是本次注册的会话」）：
		// 同 id 已被新会话接管时返回 false，避免把本次状态发布到接管会话上
		// （RV9-CHUNK-FINAL Q7）。
		routePublished := store.setSessionRouteIfCurrent(session, route.VolumeName, route.ScopeRes, route.Pool, route.PoolRes)
		// p5Reserved/p5Published 记录 P5 回退预留的字节数与登记结果（供发布失败时回滚；
		// 未走 P5 分支时 p5Published 恒为 true、p5Reserved 为 0）。
		var p5Reserved int64
		p5Published := true
		if route.ScopeRes == nil && s.rt.storageManager() != nil {
			// P5 回退：quota 未装配（route.ScopeRes nil，volSet nil 旧装配 / globalPool nil）
			// 时回退旧 storageMgr 全局预留；未完成会话删除/过期时按 StorageMgrReserved 释放
			// （已完成会话不释放，见 DeleteSession）。
			if err := s.rt.storageManager().TryReserveChunked(session.TotalSize); err != nil {
				store.cleanupSessionIfCurrent(session.UploadID, session)
				s.rt.logger().Warn("storage full, chunked upload rejected",
					"file_name", req.Filename,
					"total_size", session.TotalSize,
					"current_usage", s.rt.storageManager().Usage(),
					"max_bytes", s.rt.storageManager().MaxBytes(),
				)
				s.sendJSON(w, ChunkedInitResponse{Success: false, Message: "存储空间不足"}, http.StatusInsufficientStorage)
				return
			}
			// P5 回退预留登记：**未完成**会话删除/过期时按此释放（DeleteSession/cleanupExpired）；
			// 已完成会话不释放——temp 已 rename 为正式文件，字节仍在磁盘（见 DeleteSession）。
			p5Reserved = session.TotalSize
			p5Published = store.setSessionStorageMgrReservedIfCurrent(session, session.TotalSize)
		}

		// 创建在途整临时文件（user 桶 target 同目录，O_EXCL 防跨 worker 冲突），
		// Truncate(TotalSize) 预先占位；失败按 500，临时名未创建无需清理配额。
		// tempRel = user/<dir>/.inflight-<hash16>-<upload_id>.part（散列取 rel 全路径）。
		tempRel := TempRelForUser(session, rel)
		if tempRel == "" {
			s.rt.logger().Error("派生在途临时文件路径失败", "upload_id", session.UploadID, "file_name", session.Filename)
			store.cleanupSessionIfCurrent(session.UploadID, session)
			s.sendJSON(w, ChunkedInitResponse{Success: false, Message: "创建上传会话失败"}, http.StatusInternalServerError)
			return
		}
		// 确保临时名父目录存在（user/<dir> 桶目标同目录）。
		if err := tnt.Root().MkdirAll(filepath.Dir(tempRel), 0o755); err != nil {
			s.rt.logger().Error("创建在途临时文件父目录失败", "upload_id", session.UploadID, "error", err)
			store.cleanupSessionIfCurrent(session.UploadID, session)
			s.sendJSON(w, ChunkedInitResponse{Success: false, Message: "创建上传会话失败"}, http.StatusInternalServerError)
			return
		}
		tmpFile, err := tnt.Root().OpenFile(tempRel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			s.rt.logger().Error("创建在途临时文件失败", "upload_id", session.UploadID, "error", err)
			store.cleanupSessionIfCurrent(session.UploadID, session)
			s.sendJSON(w, ChunkedInitResponse{Success: false, Message: "创建上传会话失败"}, http.StatusInternalServerError)
			return
		}
		if err := tmpFile.Truncate(session.TotalSize); err != nil {
			tmpFile.Close()
			_ = tnt.Root().Remove(tempRel)
			s.rt.logger().Error("预占在途临时文件失败", "upload_id", session.UploadID, "error", err)
			store.cleanupSessionIfCurrent(session.UploadID, session)
			s.sendJSON(w, ChunkedInitResponse{Success: false, Message: "创建上传会话失败"}, http.StatusInternalServerError)
			return
		}
		if err := tmpFile.Close(); err != nil {
			_ = tnt.Root().Remove(tempRel)
			s.rt.logger().Error("关闭在途临时文件失败", "upload_id", session.UploadID, "error", err)
			store.cleanupSessionIfCurrent(session.UploadID, session)
			s.sendJSON(w, ChunkedInitResponse{Success: false, Message: "创建上传会话失败"}, http.StatusInternalServerError)
			return
		}
		// 临时名发布同样走身份门控 setter（同上：并发读者会深拷贝会话）。
		tempPublished := store.setSessionTempPathIfCurrent(session, tempRel)
		// 回写 session.json 持久化 tempPath（重启后据此恢复续传）。
		// 同样走**身份门控**：会话已被接管时本请求已注定回滚 ⇒ 不去动**接管会话**的持久化文件；
		// 该情形随后会被下面的发布结果校验判定并回 409（回滚路径自带告警），故此处只记 Debug。
		if err := store.PersistNowIfCurrent(session); err != nil {
			if errors.Is(err, errSessionNotCurrent) {
				s.rt.logger().Debug("会话已被接管，跳过持久化在途临时文件路径", "upload_id", session.UploadID)
			} else {
				s.rt.logger().Warn("持久化在途临时文件路径失败", "upload_id", session.UploadID, "error", err)
			}
		}
		// 发布结果校验（审计 P2-2 + RV9-CHUNK-FINAL Q7）：三处**身份门控** setter 返回 false
		// ⇔ 本次 init 的会话已不在表中——已被并发删除（cancel / 过期清理）**或**同 id 已被新会话
		// 接管。两者都意味着 routeUpload 预留、P5 回退预留与刚创建的在途临时文件没有（属于本次的）
		// 会话登记 ⇒ 既不会被删除也不会被释放（永久孤儿）。fail-closed：回滚本次全部产物并回 409。
		// 注意「部分发布已生效」也必须整体回滚（route 已发布 → P5/temp 失败同样走本支）。
		if !routePublished || !p5Published || !tempPublished {
			releaseP5 := int64(0)
			if !p5Published {
				releaseP5 = p5Reserved
			}
			s.abortInitOrphanRollback(w, store, route, tnt, tempRel, releaseP5, req.Filename, session.UploadID)
			return
		}
	}

	msg := "上传会话已创建"
	if reused {
		missing := MissingChunks(session)
		msg = fmt.Sprintf("续传会话已恢复，缺失 %d 个分块", len(missing))
		s.rt.logger().Info("续传会话", "upload_id", session.UploadID, "file_name", req.Filename,
			"missing", len(missing), "total", session.TotalChunks)
	} else {
		s.rt.logger().Info("新上传会话", "upload_id", session.UploadID, "file_name", req.Filename,
			"total_size", req.TotalSize, "total_chunks", session.TotalChunks)
	}

	s.sendJSON(w, ChunkedInitResponse{
		Success:   true,
		UploadID:  session.UploadID,
		ChunkSize: session.ChunkSize,
		Message:   msg,
	}, http.StatusOK)
}

// abortInitOrphanRollback 回滚「已无会话登记」的 init 遗留产物：删除刚创建的在途临时文件、
// 归还本次定卷/容量预留（以及从未登记进会话的 P5 回退预留），最后以 409 让客户端重新初始化。
//
// 审计背景（#304 复核 P2-2 登记项）：init 的三处**身份门控** setter 在会话被并发删除（cancel /
// 过期清理）**或同 id 已被新会话接管**（世代不同，见 setSessionRouteIfCurrent）时返回 false，
// 而原实现忽略返回值 ⇒ routeUpload 的预留与在途临时文件都没有任何清理路径（既不删除也不释放），
// 且仍对客户端回 200 Success:true。
// **不得**在此调用 DeleteSession(uploadID)：该 id 可能已被新会话接管（审计 C-7 同类身份问题），
// 按 id 删除会误删新会话的目录与临时名。
// 同理，**在途临时文件的删除也必须过身份闸门**（RV9-CHUNK-FINAL F-2）：临时名只依赖
// (rel, uploadID) ⇒ 同 id 复用即同路径；若该 id 已被新会话接管且它记录的在途临时名正是 tempRel，
// 则该文件已属于新会话，按路径删除会让新会话的 session.json 指向消失的临时名（叠加审计 C-2
// 的「temp 丢失不可修复」会拖到 TTL）。
// releaseP5 只在「P5 回退预留从未登记进会话」时非 0：ReleaseChunked 是直接累减（非幂等），
// 已登记的预留由会话删除负责归还，重复归还会让容量账少算。
func (s *Service) abortInitOrphanRollback(w http.ResponseWriter, store *UploadStore, route UploadRoute,
	tnt *storage.Tenant, tempRel string, releaseP5 int64, filename, uploadID string,
) {
	if tempRel != "" && tnt != nil && tnt.Root() != nil {
		// 身份闸门：当前会话记录的临时名与 tempRel 相同时，该文件归新会话所有 ⇒ 跳过删除。
		// 判据用「记录值与路径」而非按 id 删：新会话可能尚未发布 TempPath（此时文件仍是本次
		// 遗留的孤儿，删除它是安全的，且能避免残留件阻断新会话的 O_EXCL 创建）。
		// 判定与删除在**同一次 us.mu.RLock 内**完成（RemoveUnclaimedTemp）：若先取副本、放锁后再删，
		// 两拍之间新会话可能恰好发布同一 TempPath，就会删掉它的在途文件（RV9-CHUNK-FINAL 建议①）。
		claimed, rmErr := store.RemoveUnclaimedTemp(uploadID, tempRel, func() error {
			return tnt.Root().Remove(tempRel)
		})
		switch {
		case claimed:
			s.rt.logger().Warn("回滚 init 遗留产物：该 id 已由新会话接管同一在途临时名，跳过删除",
				"file_name", filename, "upload_id", shortid.ShortHash(uploadID))
		case rmErr != nil && !os.IsNotExist(rmErr):
			s.rt.logger().Warn("回滚 init 遗留产物：删除在途临时文件失败", "upload_id", uploadID, "error", rmErr)
		}
	}
	route.Release()
	if releaseP5 > 0 {
		if sm := s.rt.storageManager(); sm != nil {
			sm.ReleaseChunked(releaseP5)
		}
	}
	s.rt.logger().Warn("init 期间会话已被并发删除，已回滚定卷/容量预留与在途临时文件",
		"file_name", filename, "upload_id", shortid.ShortHash(uploadID),
		"temp_created", tempRel != "", "p5_released_bytes", releaseP5)
	s.sendJSON(w, ChunkedInitResponse{Success: false, Message: "上传会话已被并发取消，请重新初始化"}, http.StatusConflict)
}

// TempRelForUser 生成租户 user 桶内分块在途整文件的存储根相对路径：
// user/<dir>/.inflight-<sha256(rel)前16hex>-<upload_id>.part，与正式名同目录。
// rel 为 tnt.UserRel(filename) 结果（user/... 相对路径）；inflightTempName 对 rel 取散列
// 并拼 uploadID 段，返回段名安全（散列 + uploadID 均无非法字符）。dir 由 filepath.Dir
// 导出（rel 内路径段已由 UserRel 校验合法）；返回空串表示非法 rel。
func TempRelForUser(session *ChunkedUploadSession, rel string) string {
	userRel := strings.TrimPrefix(rel, "user/")
	dir := filepath.Dir(filepath.FromSlash(userRel))
	if dir == "." {
		dir = ""
	}
	tempName := InflightTempName(rel, session.UploadID)
	if dir == "" {
		return "user/" + tempName
	}
	return "user/" + filepath.ToSlash(dir) + "/" + tempName
}

// chunkLenAt 返回会话中第 i 个分片的实际长度（末片可能短于 chunk_size）。
func chunkLenAt(session *ChunkedUploadSession, i int) int64 {
	offset := int64(i) * session.ChunkSize
	if offset >= session.TotalSize {
		return 0
	}
	if remaining := session.TotalSize - offset; remaining < session.ChunkSize {
		return remaining
	}
	return session.ChunkSize
}

// openSessionTemp 打开会话的在途整临时文件（绝对路径，root.OpenFile 相对保证不逃逸）。
// 只读（供 complete 全文件读取与恢复校验）；写入由 chunk 写路径单独以 O_WRONLY 打开。
func (s *Service) openSessionTemp(tnt *storage.Tenant, session *ChunkedUploadSession) (*os.File, error) {
	abs, ok := tnt.Root().Abs(session.TempPath)
	if !ok {
		return nil, fmt.Errorf("在途临时文件路径越界: %s", session.TempPath)
	}
	return os.Open(abs)
}

// openSessionTempWrite 打开会话在途整临时文件用于 seek 直写（O_WRONLY）。
func (s *Service) openSessionTempWrite(tnt *storage.Tenant, session *ChunkedUploadSession) (*os.File, error) {
	abs, ok := tnt.Root().Abs(session.TempPath)
	if !ok {
		return nil, fmt.Errorf("在途临时文件路径越界: %s", session.TempPath)
	}
	return os.OpenFile(abs, os.O_WRONLY, 0)
}

// UploadChunk 上传单个分块。
func (s *Service) UploadChunk(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// 限制请求体大小（含 multipart 开销）
	r.Body = http.MaxBytesReader(w, r.Body, size.DefaultChunkBodyLimit)

	// 解析 multipart
	//nolint:gosec // G120 误报：请求体已由上一行 http.MaxBytesReader 限定为 DefaultChunkBodyLimit
	// （64 MiB），并非无界解析。实测：同一代码留在 pkg/server 时不报、迁入本包即报（gosec 该规则是
	// Sanitizers 为空的 taint 规则，无法识别 MaxBytesReader 的限定）。**触发条件已定因**：污点分析
	// 的入口是「**处理器在包内没有调用者**」——路由注册留在装配层，故本包的处理器无包内调用者；
	// 对照探针：只把基线的处理器改名（仍被路由注册调用）不复现，而加一个包内无调用者的方法
	// 无论导出与否都复现。
	if err := r.ParseMultipartForm(size.DefaultChunkBodyLimit); err != nil {
		s.rt.logger().Warn("uploadChunk parse multipart 失败", "error", err.Error(), "content_type", r.Header.Get("Content-Type"), "content_length", r.ContentLength)
		s.sendJSON(w, ChunkUploadResponse{Success: false, Message: "解析 multipart 失败"}, http.StatusRequestEntityTooLarge)
		return
	}
	// I-3：multipart 解析不读到 EOF，读完全部 body 触发 bodyValidator 哈希校验。
	if err := drainAndVerifyBody(r); err != nil {
		s.sendJSON(w, ChunkUploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}
	s.rt.logger().Debug("uploadChunk multipart 解析完成", "content_type", r.Header.Get("Content-Type"))

	uploadID, chunkIndex, chunkChecksum, ok := parseChunkFormParams(r)
	if !ok {
		s.sendJSON(w, ChunkUploadResponse{Success: false, Message: "缺少 upload_id、chunk_index 或 chunk_checksum 无效"}, http.StatusBadRequest)
		return
	}

	s.rt.logger().Debug("uploadChunk 请求", "upload_id", uploadID, "chunk_index", chunkIndex, "content_type", r.Header.Get("Content-Type"))

	// 租户隔离靠 per-tenant store：会话只在本租户 chunk/ 桶下创建，跨租户同裸 id 互不可见
	owner := s.rt.actorOf(r)
	store := s.rt.uploadStore(owner)
	if store == nil {
		s.sendJSON(w, ChunkUploadResponse{Success: false, Message: errMsgUploadIDNotFound}, http.StatusNotFound)
		return
	}

	// 获取 session
	session := store.GetSession(uploadID)
	if session == nil {
		s.sendJSON(w, ChunkUploadResponse{Success: false, Message: errMsgUploadIDNotFound}, http.StatusNotFound)
		return
	}

	if session.Completed {
		s.sendJSON(w, ChunkUploadResponse{Success: false, Message: "上传已完成，不接受新分块"}, http.StatusGone)
		return
	}
	// 合并中屏障（C-3）：complete 的「全文件校验 → rename」期间不得再接受分块，
	// 否则该分块可能在 rename 之后改写临时文件（落盘内容 != 刚校验通过的内容）。
	// 此处必须**立即**拒绝（而非在 merge 锁上排队）。
	// ShouldRetry：合并是**瞬态**窗口（通常毫秒级），必须置 true 让 SDK 退避重试——否则 SDK
	// （pkg/client/chunked.go 仅在 should_retry=true 时重试）会把它当永久失败直接判死整个上传。
	// 该窗口在多进程共用同一会话时可达：SDK 的 uploadID 是 filename|size|mtime|checksum 的确定性散列。
	if session.Completing {
		s.sendJSON(w, ChunkUploadResponse{Success: false, ShouldRetry: true, Message: "上传正在合并中，暂不接受新分块"}, http.StatusConflict)
		return
	}

	if chunkIndex < 0 || chunkIndex >= session.TotalChunks {
		s.sendJSON(w, ChunkUploadResponse{Success: false, Message: fmt.Sprintf("chunk_index %d 超出范围 [0, %d)", chunkIndex, session.TotalChunks)}, http.StatusBadRequest)
		return
	}

	file, _, err := r.FormFile("chunk")
	if err != nil {
		s.sendJSON(w, ChunkUploadResponse{Success: false, Message: "读取分块文件失败"}, http.StatusBadRequest)
		return
	}
	defer file.Close()

	// 幂等：如果该块已接收且 checksum 匹配，直接返回成功
	if session.ReceivedChunks[chunkIndex] && session.ChunkChecksums[chunkIndex] == chunkChecksum {
		s.rt.logger().Debug("chunk 已存在，跳过", "upload_id", uploadID, "chunk_index", chunkIndex, "checksum", shortid.ShortHash(chunkChecksum))
		s.sendJSON(w, ChunkUploadResponse{Success: true, ChunkIndex: chunkIndex, Message: "分块已存在，跳过"}, http.StatusOK)
		return
	}

	// 获取 chunk IO 读锁（任务 4：并发分段写各自 seek 固定 offset + BoundWriter 防越界，
	// 锁域仍按 uploadID 划分避免同会话 bitmap 更新与完成读的竞态；complete 用写锁读全文件）。
	unlockIO := store.LockChunkIO(uploadID)
	defer unlockIO()

	// 持锁后重新获取 session，用最新副本做幂等检查
	session = store.GetSession(uploadID)
	if session == nil {
		s.sendJSON(w, ChunkUploadResponse{Success: false, Message: errMsgUploadIDNotFound}, http.StatusNotFound)
		return
	}
	if session.Completed {
		s.sendJSON(w, ChunkUploadResponse{Success: false, Message: "上传已完成，不接受新分块"}, http.StatusGone)
		return
	}
	// 持锁后的权威复查（C-3 屏障）：早期检查之后、取锁之前 complete 可能已进入合并阶段。
	// ShouldRetry 语义同上（瞬态窗口，让 SDK 退避重试而非判死整个上传）。
	if session.Completing {
		s.sendJSON(w, ChunkUploadResponse{Success: false, ShouldRetry: true, Message: "上传正在合并中，暂不接受新分块"}, http.StatusConflict)
		return
	}
	if chunkIndex < 0 || chunkIndex >= session.TotalChunks {
		s.sendJSON(w, ChunkUploadResponse{Success: false, Message: fmt.Sprintf("chunk_index %d 超出范围 [0, %d)", chunkIndex, session.TotalChunks)}, http.StatusBadRequest)
		return
	}
	if session.ReceivedChunks[chunkIndex] && session.ChunkChecksums[chunkIndex] == chunkChecksum {
		s.rt.logger().Debug("chunk 已存在，跳过", "upload_id", uploadID, "chunk_index", chunkIndex, "checksum", shortid.ShortHash(chunkChecksum))
		s.sendJSON(w, ChunkUploadResponse{Success: true, ChunkIndex: chunkIndex, Message: "分块已存在，跳过"}, http.StatusOK)
		return
	}

	// 任务 4：seek+BoundWriter 直写整临时文件。不写独立 .chunk 文件。
	// 流程：读块到内存 → 块 checksum 校验 → 清空读取句柄（读指针已 EOF）→
	// Seek(i*chunkSize) → BoundWriter(limit=该分片实际长度) 写入（防越界写坏相邻分片）→
	// MarkChunkReceived(i, checksum)。请求体已受 MaxBytesReader(DefaultChunkBodyLimit)
	// 限制，单块 ≤ ~60 MiB（测试 4KiB），内存缓冲可控。
	// 乱序安全：seek 固定 offset + BoundWriter 逐段写，互不覆盖；并发分段写沿用锁。
	// 多卷（AD-5）：temp 整文件在会话定卷的 user 桶（init 定卷），chunk 直写须经该卷租户。
	tnt := s.rt.volumeTenant(session.Volume, owner)
	if tnt == nil || tnt.Root() == nil {
		s.sendJSON(w, ChunkUploadResponse{Success: false, Message: "上传会话缺少在途临时文件"}, http.StatusInternalServerError)
		return
	}
	if session.TempPath == "" {
		// 任务 4：会话缺临时名（旧磁盘遗留/篡改）。本分片无法直写——拒绝并提示
		// 客户端重试 init（重新创建临时名），不静默吞掉分片。
		s.sendJSON(w, ChunkUploadResponse{Success: false, ChunkIndex: chunkIndex, ShouldRetry: true, Message: "上传会话缺少在途临时文件，请重新初始化"}, http.StatusInternalServerError)
		return
	}

	// 读请求块到内存并计算 SHA-256（一次性，双用：校验 + 直写数据源）。
	// 所有权语义：data 直接指向池条目（零拷贝），release 在 sha256 + 直写完成后归还
	// （同一 goroutine 顺序执行，归还后无引用 —— 见 readChunkBodyOwned 的说明）。
	data, release, err := readChunkBodyOwned(file)
	if err != nil {
		s.rt.logger().Error("读取分块失败", "upload_id", uploadID, "chunk_index", chunkIndex, "error", err)
		s.sendJSON(w, ChunkUploadResponse{Success: false, ChunkIndex: chunkIndex, ShouldRetry: true, Message: "读取分块失败"}, http.StatusInternalServerError)
		return
	}
	defer release() // 所有返回路径（含 checksum 不匹配 / 写入失败）都归还池条目，不泄漏

	if closeErr := file.Close(); closeErr != nil {
		s.rt.logger().Error("关闭分块读取句柄失败", "upload_id", uploadID, "chunk_index", chunkIndex, "error", closeErr)
		s.sendJSON(w, ChunkUploadResponse{Success: false, ChunkIndex: chunkIndex, ShouldRetry: true, Message: "读取分块失败"}, http.StatusInternalServerError)
		return
	}
	serverChecksum := fmt.Sprintf("%x", sha256.Sum256(data))
	if serverChecksum != chunkChecksum {
		s.rt.logger().Warn("chunk SHA-256 不匹配", "upload_id", uploadID, "chunk_index", chunkIndex,
			"server", shortid.ShortHash(serverChecksum), "client", shortid.ShortHash(chunkChecksum),
			"session_chunk_size", session.ChunkSize)
		s.sendJSON(w, ChunkUploadResponse{
			Success:     false,
			ChunkIndex:  chunkIndex,
			ShouldRetry: true,
			Message:     "SHA-256 校验不匹配",
		}, http.StatusOK)
		return
	}

	// 限长分片直写：limit=该分片实际长度（末片短于 chunk_size）。
	offset := int64(chunkIndex) * session.ChunkSize
	limit := chunkLenAt(session, chunkIndex)
	written, err := s.writeChunkDirect(session, tnt, offset, limit, data)
	if err != nil {
		s.rt.logger().Error("写入在途临时文件失败", "upload_id", uploadID, "chunk_index", chunkIndex, "error", err)
		s.sendJSON(w, ChunkUploadResponse{Success: false, ChunkIndex: chunkIndex, ShouldRetry: true, Message: "写入分块失败"}, http.StatusInternalServerError)
		return
	}

	// 更新 session
	if err := store.MarkChunkReceived(uploadID, chunkIndex, serverChecksum); err != nil {
		s.rt.logger().Error("标记分块已接收失败", "upload_id", uploadID, "chunk_index", chunkIndex, "error", err)
		s.sendJSON(w, ChunkUploadResponse{Success: false, ChunkIndex: chunkIndex, ShouldRetry: true, Message: "更新状态失败"}, http.StatusInternalServerError)
		return
	}

	s.rt.logger().Info("uploadChunk 耗时", "upload_id", uploadID, "chunk_index", chunkIndex,
		"total", time.Since(start).String(), "size", written)
	s.sendJSON(w, ChunkUploadResponse{
		Success:    true,
		ChunkIndex: chunkIndex,
		Message:    fmt.Sprintf("分块 %d 已接收并校验通过", chunkIndex),
	}, http.StatusOK)
}

// readChunkBodyOwned 读取 multipart 分块文件到内存（供 SHA-256 校验与直写数据源），
// **返回池条目所有权**而非独立拷贝：data 直接指向池条目底层数组（零拷贝返回），
// release() 归还池条目（调用方在**同一 goroutine 顺序**完成 sha256 与直写后调用，
// 归还后不得再持有对 data 的任何引用）。
//
// 与 #457（先拷贝后归还）的关系：当年为修 DATA RACE 引入「append 拷贝 + 立即归还」，
// 代价是每 chunk 多一次整块拷贝且池条目在 UploadChunk 内从不归还（返回值是独立拷贝，
// 调用方的 putChunkBody(&data) 是双重归还被删——池条目从此泄漏，仅靠 GC 回收）。
// 本实现把「归还时机」交给唯一使用方（UploadChunk 单 goroutine 顺序执行）：sha256 与
// writeChunkDirect 完成后才 release，归还后无引用 —— race 安全性与 #457 等价，且
// 消除每 chunk 的整块拷贝（Benchmark ChunkedUpload B/op 4MiB/op 级下降）并让池真正复用。
func readChunkBodyOwned(file multipart.File) (data []byte, release func(), err error) {
	bufp, _ := chunkBodyPool.Get().(*[]byte) //nolint:errcheck // pool 无错误返回，断言防御
	if bufp == nil {
		bufp = new([]byte)
		*bufp = make([]byte, 0, 64*1024)
	}
	buf := (*bufp)[:0]
	for {
		// 复用缓冲逐段读取；cap 不足时扩容（与 bytes.Buffer 同策略，避免逐字节增长）。
		// 扩容**原地更新池条目指向的切片**（*bufp = nb），始终只存在一个切片头——
		// 归还时 Put 同一个 bufp 指针，池内不会出现指向同一底层数组的多个切片（并发
		// Get 拿到不同条目即不同数组，无 race；曾因 Put(&buf) 存局部变量地址导致
		// 同数组被两个 goroutine 并发读写，CI -race 实测 DATA RACE）。
		if len(buf) == cap(buf) {
			nb := make([]byte, len(buf), 2*cap(buf))
			copy(nb, buf)
			buf = nb
			*bufp = nb
		}
		n, rerr := file.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			putChunkBody(bufp)
			return nil, nil, rerr
		}
	}
	// 所有权交给调用方：release 归还池条目（调用方用毕后调用，归还后无引用）。
	// 注意 data 的 len 与 cap：cap 可能 > len（扩容余量），release 后池条目可能被
	// 另一 goroutine Get 并复用底层数组 —— 调用方必须保证 release 前不再读 data。
	return buf, func() { putChunkBody(bufp) }, nil
}

// putChunkBody 归还 readChunkBodyOwned 分配的缓冲到 chunkBodyPool。
// bufp 恒为 readChunkBodyOwned 持有的**池条目指针**（Get 所得、扩容后仍同指针），
// 故每个池条目唯一对应一个底层数组，并发 Get 不会共享数组。
// 未清零：池内缓冲被再次取出时按长度切片使用，读取的数据会覆盖旧内容。
func putChunkBody(bufp *[]byte) {
	if bufp != nil {
		chunkBodyPool.Put(bufp)
	}
}

// chunkBodyPool 复用以 readChunkBodyOwned 为主的分块读取缓冲（默认 64 KiB 起步，自动增长
// 到分块实际大小）。池化避免每个分块请求都从零分配整块缓冲。
var chunkBodyPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 64*1024)
		return &b
	},
}

// writeChunkDirect 把已通过 checksum 校验的分片数据 seek 直写进在途整临时文件。
// root 相对 TempPath → Abs 派生绝对路径（防符号链接逃逸）+ os.OpenFile O_WRONLY；
// NewBoundWriter(offset, limit) 在 [offset, offset+limit) 限长写入（超限 io.EOF，防越界
// 写坏相邻分片），offset 保证乱序直写互不覆盖。返回实际写入字节数；data 超长时截断，
// 不足 limit 时按实际写（末片短于 chunk_size 属正常）。
func (s *Service) writeChunkDirect(session *ChunkedUploadSession, tnt *storage.Tenant, offset, limit int64, data []byte) (int64, error) {
	tmpFile, err := s.openSessionTempWrite(tnt, session)
	if err != nil {
		return 0, err
	}
	defer tmpFile.Close()

	bw := quota.NewBoundWriter(tmpFile, offset, limit, 0)
	n, err := bw.Write(data)
	if err != nil && err != io.EOF {
		return int64(n), fmt.Errorf("限长写入分片失败: %w", err)
	}
	return int64(n), nil
}

// UploadSessions 列出所有未完成上传会话的元信息。
// 归一为 {success:true, sessions:[{upload_id,filename,total_size,received_count,total_chunks,file_checksum,file_mod_time,status}]}。
// 已完成会话（Completed=true，complete 后 CleanupSessionAfter 延迟清理前的窗口）不列出，
// 故 status 恒为 "uploading"（取值域 uploading|completed，completed 在此被 handler 过滤）。
func (s *Service) UploadSessions(w http.ResponseWriter, r *http.Request) {
	// per-tenant store 的 ListSessions() 天然只含本租户会话，无需 owner 过滤。
	owner := s.rt.actorOf(r)
	store := s.rt.uploadStore(owner)
	if store == nil {
		s.sendJSON(w, ChunkSessionsResponse{Success: true, Sessions: []UploadSessionInfo{}}, http.StatusOK)
		return
	}
	meta := store.ListSessions()
	sessions := make([]UploadSessionInfo, 0, len(meta))
	for _, m := range meta {
		if m.Completed {
			continue
		}
		info := UploadSessionInfo{
			UploadID:      m.UploadID,
			Filename:      m.Filename,
			TotalSize:     m.TotalSize,
			ReceivedCount: m.ReceivedCount,
			TotalChunks:   m.TotalChunks,
			FileChecksum:  m.FileChecksum,
			FileModTime:   m.FileModTime,
			Status:        "uploading",
		}
		sessions = append(sessions, info)
	}
	s.sendJSON(w, ChunkSessionsResponse{Success: true, Sessions: sessions}, http.StatusOK)
}

// UploadStatus 查询上传会话状态。
func (s *Service) UploadStatus(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	uploadID := params.Get("upload_id")
	filename := params.Get("filename")
	owner := s.rt.actorOf(r)

	// 1. 按 upload_id 查 session（per-tenant store，天然只含本租户会话）
	if uploadID != "" {
		if s.lookupUploadIDStatus(w, owner, uploadID, filename) {
			return
		}
	}

	// 2. 按 filename 查找未完成的 session（本租户作用域）
	if filename != "" {
		if s.lookupFilenameStatus(w, owner, filename) {
			return
		}
	}

	// 什么都没找到
	s.sendJSON(w, ChunkStatusResponse{Success: false, Message: "未找到文件或上传会话"}, http.StatusNotFound)
}

// lookupUploadIDStatus 按 upload_id 查询上传会话状态。返回 true 表示已处理请求。
func (s *Service) lookupUploadIDStatus(w http.ResponseWriter, owner, uploadID, filename string) bool {
	store := s.rt.uploadStore(owner)
	if store == nil {
		if filename == "" {
			s.sendJSON(w, ChunkStatusResponse{Success: false, Message: errMsgUploadIDNotFound}, http.StatusNotFound)
			return true
		}
		return false
	}
	session := store.GetSession(uploadID)
	if session != nil {
		missing := MissingChunks(session)
		s.sendJSON(w, ChunkStatusResponse{
			Success:       true,
			UploadID:      session.UploadID,
			ReceivedCount: len(session.ReceivedChunks) - len(missing),
			TotalChunks:   session.TotalChunks,
			MissingChunks: missing,
			Completed:     session.Completed,
			FileChecksum:  session.FileChecksum,
			Filename:      session.Filename,
			Message:       fmt.Sprintf("会话%d/%d分块已接收", len(session.ReceivedChunks)-len(missing), session.TotalChunks),
		}, http.StatusOK)
		return true
	}
	// upload_id 存在但 session 不存在
	if filename == "" {
		s.sendJSON(w, ChunkStatusResponse{Success: false, Message: errMsgUploadIDNotFound}, http.StatusNotFound)
		return true
	}
	return false
}

// lookupFilenameStatus 按 filename 查找上传会话或检查文件是否已存在。返回 true 表示已处理请求。
func (s *Service) lookupFilenameStatus(w http.ResponseWriter, owner, filename string) bool {
	// 防御性校验：防止路径穿越
	if _, err := pathguard.ValidateFilePath(filename); err != nil {
		s.sendJSON(w, ChunkStatusResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return true
	}
	store := s.rt.uploadStore(owner)
	if store == nil {
		return s.checkFileExistsStatus(w, owner, filename)
	}
	session := store.GetSessionByFilename(filename)
	if session != nil {
		missing := MissingChunks(session)
		s.sendJSON(w, ChunkStatusResponse{
			Success:       true,
			UploadID:      session.UploadID,
			ReceivedCount: len(session.ReceivedChunks) - len(missing),
			TotalChunks:   session.TotalChunks,
			MissingChunks: missing,
			Completed:     session.Completed,
			FileChecksum:  session.FileChecksum,
			Filename:      session.Filename,
		}, http.StatusOK)
		return true
	}

	return s.checkFileExistsStatus(w, owner, filename)
}

// checkFileExistsStatus 检查租户 user 桶内文件是否已存在且 checksum 匹配。
// 多卷（T6b）：已完成文件按文件名跨卷定位（locateOwnerFile）——非默认卷文件可探测；
// 默认卷被 ACL 排除时默认卷遗留不可见 → 未命中返回 false（调用方 404，fail-closed 不泄存在性）。
// 返回 true 表示已处理请求。
func (s *Service) checkFileExistsStatus(w http.ResponseWriter, owner, filename string) bool {
	tnt := s.rt.tenantOf(owner)
	if tnt == nil || tnt.Root() == nil {
		s.sendJSON(w, ChunkStatusResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return true
	}
	rel, ok := tnt.UserRel(filename)
	if !ok {
		s.sendJSON(w, ChunkStatusResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return true
	}
	var root *storage.Root
	if s.rt.volSet() != nil {
		loc, found := s.rt.locateOwnerFile(owner, rel)
		if !found || loc.Tenant == nil || loc.Tenant.Root() == nil {
			return false
		}
		root = loc.Tenant.Root()
	} else {
		root = tnt.Root()
	}
	stat, err := root.Stat(rel)
	if err != nil {
		return false
	}
	if cs := s.rt.checksumStore(owner); cs != nil {
		if checksum, ok := cs.Get(rel); ok {
			s.sendJSON(w, ChunkStatusResponse{
				Success:      true,
				Completed:    true,
				FileChecksum: checksum,
				Filename:     filename,
				Message:      fmt.Sprintf(errFmtFileExists, stat.Size()),
			}, http.StatusOK)
			return true
		}
	}
	// 有文件但无 checksum 记录（意外情况），实时计算
	if cs, err := fileChecksumRoot(root, rel); err == nil {
		s.sendJSON(w, ChunkStatusResponse{
			Success:      true,
			Completed:    true,
			FileChecksum: cs,
			Filename:     filename,
			Message:      fmt.Sprintf(errFmtFileExists, stat.Size()),
		}, http.StatusOK)
		return true
	}
	return false
}

// validateCompleteSession 校验 complete 请求的 session 是否有效。
// 如果校验失败，已发送错误响应，返回 (nil, false)。
func (s *Service) validateCompleteSession(w http.ResponseWriter, store *UploadStore, owner, uploadID string) (*ChunkedUploadSession, bool) {
	if uploadID == "" {
		s.sendJSON(w, ChunkCompleteResponse{Success: false, Message: "缺少 upload_id"}, http.StatusBadRequest)
		return nil, false
	}
	// 租户隔离靠 per-tenant store：跨租户同裸 id 会话在此 store 中不存在 → 404
	session := store.GetSession(uploadID)
	if session == nil {
		s.sendJSON(w, ChunkCompleteResponse{Success: false, Message: errMsgUploadIDNotFound}, http.StatusNotFound)
		return nil, false
	}

	s.rt.logger().Info("uploadComplete 开始", "upload_id", uploadID, "file_name", session.Filename,
		"received", countReceived(session.ReceivedChunks), "total", session.TotalChunks)

	if session.Completed {
		s.rt.logger().Info("上传已完成（幂等）", "upload_id", uploadID, "file_name", session.Filename)
		s.sendJSON(w, ChunkCompleteResponse{
			Success:      true,
			Filename:     session.Filename,
			FileChecksum: session.FileChecksum,
			Message:      "上传已完成",
		}, http.StatusOK)
		return nil, false
	}

	if !store.AllChunksReceived(uploadID) {
		session = store.GetSession(uploadID)
		missing := MissingChunks(session)
		s.rt.logger().Warn("合并请求时还有分块未接收", "upload_id", uploadID, "missing", len(missing))
		s.sendJSON(w, ChunkCompleteResponse{
			Success: false,
			Message: fmt.Sprintf("还有 %d 个分块未接收", len(missing)),
		}, http.StatusBadRequest)
		return nil, false
	}

	return session, true
}

// recordCompleteMetadata 记录「落盘后副作用」（mtime + checksum 台账）并清理上传 session。
//
// 前两项**必须**与单次上传（`WriteFile`）共用同一份实现 `recordUploadSuccess`——这条路径
// 的 mtime 语义（`FileModTime == 0` 表示「不设置」，不是 epoch）与台账 key（租户根相对 rel）
// 此前是复制粘贴的另一份，极易与单次上传分叉且无用例会红。门禁见
// `internal/archcheck/upload_side_effect_test.go`（设置 mtime 的调用全仓只允许一处）。
//
// 文件在会话定卷（session.Volume；空 = 默认卷）的 user 桶——mtime 须经该卷租户；checksum
// store 是 owner 逻辑命名空间（默认卷 meta 单一权威），与物理卷无关。
func (s *Service) recordCompleteMetadata(owner, uploadID string, session *ChunkedUploadSession, finalChecksum string) {
	tnt := s.rt.volumeTenant(session.Volume, owner)
	if tnt == nil || tnt.Root() == nil {
		s.rt.logger().Warn("记录完成元数据失败：租户不可用", "owner", owner)
		return
	}
	rel, ok := tnt.UserRel(session.Filename)
	if !ok {
		s.rt.logger().Warn("记录完成元数据失败：文件名映射失败", "owner", owner, "file_name", session.Filename)
		return
	}

	// 上传成功的共同副作用内核（mtime + checksum 台账），与单次上传同源。
	s.recordUploadSuccess(tnt.Root(), owner, session.Filename, rel, finalChecksum, session.FileModTime, s.rt.logger())

	// 标记完成（延迟清理 session 目录）
	store := s.rt.uploadStore(owner)
	if store == nil {
		s.rt.logger().Warn("per-tenant UploadStore 不可用，跳过 session 清理", "owner", owner)
		return
	}
	if err := store.CompleteSession(uploadID); err != nil {
		s.rt.logger().Warn("标记 session 完成失败", "upload_id", uploadID, "error", err)
	}
	// 异步清理 session 目录
	store.CleanupSessionAfter(uploadID, 5*time.Second)
}

// UploadComplete 合并所有分块完成上传。
func (s *Service) UploadComplete(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, size.CompleteBodyLimit)
	var req ChunkedCompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendJSON(w, ChunkCompleteResponse{Success: false, Message: "请求体必须是合法 JSON（无法解析）"}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		s.sendJSON(w, ChunkCompleteResponse{Success: false, Message: "请求体校验失败（签名哈希不匹配或 JSON 语法错误）"}, http.StatusBadRequest)
		return
	}

	owner := s.rt.actorOf(r)
	store := s.rt.uploadStore(owner)
	if store == nil {
		s.sendJSON(w, ChunkCompleteResponse{Success: false, Message: errMsgUploadIDNotFound}, http.StatusNotFound)
		return
	}
	session, ok := s.validateCompleteSession(w, store, owner, req.UploadID)
	if !ok {
		return
	}

	// 合并中屏障（C-3）：置位后拒绝新分块，使「全文件校验 → rename」期间临时文件不再被改写。
	// 并发的第二个 complete 也在此被拦住（防两个 complete 同时合并/rename 同一会话）。
	// 失败/中断路径（含 mismatch 重传）由 defer 清除，客户端可重试 complete。
	if !store.BeginComplete(req.UploadID) {
		s.sendJSON(w, ChunkCompleteResponse{Success: false, Filename: session.Filename, Message: "该上传正在合并中，请稍后重试"}, http.StatusConflict)
		return
	}
	// **无条件**清「合并中」标记（复核 S4）：终态由 Completed 承担（UploadChunk 见到 Completed
	// 返回 410），故成功路径清掉 Completing 无害；而「按是否成功决定清不清」会留下死锁窗口——
	// rename 已成功但后续落元数据早退（recordCompleteMetadata 的「租户不可用」/「文件名映射失败」
	// 两处 return 不调 CompleteSession）时，会话会停在 Completed=false ∧ Completing=true：
	// 此后该会话 chunk 恒 409、complete 恒 409，只能等 24h TTL / 进程重启 / cancel。
	defer store.EndComplete(req.UploadID)

	// 合并不随客户端断开而取消：Using WithoutCancel 派生独立 context。
	// （complete 内部仍保守检查 ctx.Done；recovery 兜底走进程级。）
	mergeCtx := context.WithoutCancel(r.Context())

	// 取会话目标卷租户与 user 桶相对路径（覆盖写 ReleaseUsage / complete / SaveVersion 用）。
	// 多卷（AD-5）：init 定卷，temp + rename + version 全在目标卷（session.Volume 空 = 默认卷）。
	tnt := s.rt.volumeTenant(session.Volume, owner)
	rel := ""
	if tnt != nil && tnt.Root() != nil {
		if r, ok := tnt.UserRel(session.Filename); ok {
			rel = r
		}
	}

	// 文件级互斥（T6c move 锁架构延伸）：complete 会把 temp 原子 rename 为最终文件，与并发
	// move（复制→删源）共用同 rel 锁——无锁时 move 删源后 complete 仍可把文件落回源卷，与
	// 目标卷副本并存（AD-4 破坏）；持锁后 move 期间 complete 409（客户端可稍后重试，
	// session/temp/预留均保留）。
	if rel != "" {
		release, locked := s.rt.fileLocks().Acquire(owner, rel)
		if !locked {
			s.sendJSON(w, ChunkCompleteResponse{Success: false, Filename: session.Filename, Message: "文件正在移动/上传中，请稍后重试"}, http.StatusConflict)
			return
		}
		defer release()
	}

	// 全文件校验临时名内容 == session.FileChecksum：
	//  校验通过 → rename 为正式名 → 写 checksum store → 覆盖写 ReleaseUsage(old)；
	//  校验失败 → 逐分片 seek 重算 → mismatch_chunks（失败保留 session+临时名+预留供重传，
	//   不释放——重传还要写临时名；只有取消/过期/放弃才释放，见 DeleteSession/cleanupExpired）。
	mismatch, err := s.prepareMergedTemp(mergeCtx, store, tnt, session)
	if err != nil {
		// 全文件校验失败且已定位坏分片 → 400 + mismatch_chunks；IO/内部错误 → 500。
		if mismatch != nil {
			s.rt.logger().Warn("complete 校验失败，客户端按 mismatch_chunks 重传坏分片",
				"upload_id", req.UploadID, "file_name", session.Filename, "mismatch", mismatch)
			s.sendJSON(w, ChunkCompleteResponse{
				Success:        false,
				Filename:       session.Filename,
				Message:        fmt.Sprintf("%d 个分片校验失败，请重传这些分片后再次完成", len(mismatch)),
				MismatchChunks: mismatch,
			}, http.StatusBadRequest)
			return
		}
		s.rt.logger().Error("合并分块失败", "upload_id", req.UploadID, "file_name", session.Filename, "error", err)
		s.sendJSON(w, ChunkCompleteResponse{Success: false, Message: "合并文件失败"}, http.StatusInternalServerError)
		return
	}

	// 覆盖写（versioning enabled + 目标存同名旧文件）先备份版本。SaveVersion 把旧文件
	// 复制进 version 桶（version 桶 Scope 记账），不改 user 桶 committed；失败 best-effort。
	// 任务 8 O-1：覆盖动作记审计（沿用 upload_handler 覆盖写审计写法，Action=overwrite）。
	overwrote := false
	if rel != "" && tnt != nil && tnt.Root() != nil {
		if s.rt.versioningEnabled() {
			if _, sErr := tnt.Root().Stat(rel); sErr == nil {
				if _, vErr := s.SaveVersion(strings.TrimPrefix(rel, "user/"), tnt, owner); vErr != nil {
					s.rt.logger().Warn("保存文件版本失败", "file_name", session.Filename, "error", vErr)
				} else {
					overwrote = true
				}
			}
		}
	}

	// 与单次上传的三处**有意保留的差异**（P2-d 逐条记录，勿「顺手统一」）：
	//  ① 版本保存时机：此处「目标存在即备份」（客户端主动整文件重传 = 有意覆盖）；
	//     单次上传在 `handleDuplicateFile` 里**仅在 checksum 不同**时备份（同 checksum 走幂等 200）。
	//  ② 配额结算形式：此处「Commit(total) + ReleaseUsage(prev)」显式对账（I1 修复），
	//     单次上传用 `UploadRoute.Commit(prev, written)` 的 Adjust 差分；两者终态 committed 相同。
	//  ③ 卷池结算：两处同为 Adjust(prev,total)/Commit(total)（此处与单次上传一致，无差异）。
	//
	// rename 前 stat 旧文件大小（覆盖写）；新文件场景 old=0。
	prev := int64(0)
	if rel != "" && tnt != nil && tnt.Root() != nil {
		if st, statErr := tnt.Root().Stat(rel); statErr == nil {
			prev = st.Size()
		}
	}
	finalChecksum := session.FileChecksum
	if err := atomicRenameRoot(tnt.Root(), session.TempPath, rel); err != nil {
		s.rt.logger().Error("重命名最终文件失败", "upload_id", req.UploadID, "file_name", session.Filename, "error", err)
		s.sendJSON(w, ChunkCompleteResponse{Success: false, Message: "重命名文件失败"}, http.StatusInternalServerError)
		return
	}

	// P4/P5 配额对账（I1）：user 桶 Scope——init 已 TryReserve(TotalSize) 预留新文件全部
	// 字节（容量已在 init 校验），此处把预留 Commit 成 user 桶 committed（新文件大小），
	// 覆盖写再 ReleaseUsage(old) 释放已无磁盘实体的旧文件字节。净效果：committed 恰好等于
	// 新文件真实大小（显式对账，替代 Adjust 差分）。Release 原子生效一次——CompleteSession
	// 后 CleanupSessionAfter 删除会话时的额外 Release/Commit 为空操作。
	// scope 按文件实际 rel 解析（与 init 同一键，EnsureScope 缓存复用→同对象），子目录配额
	// 与 user 桶/租户逐级检查一致性由父链聚合保证。
	if scope := s.rt.quotaScope(owner, rel); scope != nil {
		if session.Reservation != nil {
			// 先提交新文件字节（reserved → committed），再释放旧文件字节。
			session.Reservation.Commit(session.TotalSize)
			session.Reservation = nil
		}
		if prev > 0 {
			// 覆盖写：rename 已原子替换，旧文件字节从磁盘消失 → ReleaseUsage(old)。
			scope.ReleaseUsage(prev)
		}
	}
	// 卷容量池双账本结算（AD-7，routeUpload 双预留之一）：新文件 Commit(total)；
	// 覆盖写 Adjust(prev, total) 差分收敛 + 释放预留（与 write.go 单次上传的 UploadRoute.Commit
	// 语义一致，
	// 防卷池 Usage 虚高/路由误判）。
	if session.Pool != nil && session.PoolRes != nil {
		if prev > 0 {
			session.Pool.Adjust(prev, session.TotalSize)
			session.PoolRes.Release()
		} else {
			session.PoolRes.Commit(session.TotalSize)
		}
		session.Pool = nil
		session.PoolRes = nil
	}

	// checksum 台账与 mtime 由 recordCompleteMetadata 经共享内核 recordUploadSuccess 一次写入
	// （此处原有一份重复写入，P2-d 删除：同一 (rel, checksum) 写两遍无收益，且是「两处实现
	// 各自演化」的温床）。

	// 任务 8 O-1：分块上传覆盖写（rename 已原子替换旧文件）记审计，与 write.go 单次上传的
	// 覆盖写审计同形（同 action/object_type/result）；无覆盖（新文件）不审计（普通上传成功
	// 也不记 audit，保持一致）。
	//
	// **Detail 文案是外部可观察的审计产物**（`/api/audit` 直出给 Web UI）：与合并前经
	// RecordOverwriteAudit 落盘的那条逐字相同，由 pkg/server 的
	// TestCompleteOverwriteReleaseUsage 钉住（断言恰好一条 overwrite 审计及其全部字段）。
	if overwrote {
		s.rt.recordFileAudit(r.Context(), "overwrite", session.Filename, auditResultSuccess, "分块上传覆盖现有文件（版本已保存）")
	}

	s.recordCompleteMetadata(owner, req.UploadID, session, finalChecksum)

	s.rt.logger().Info("文件合并完成", "file_name", session.Filename, "checksum", shortid.ShortHash(finalChecksum), "size", session.TotalSize)
	s.sendJSON(w, ChunkCompleteResponse{
		Success:      true,
		Filename:     session.Filename,
		FileChecksum: finalChecksum,
		Message:      "文件合并并校验通过",
	}, http.StatusOK)
}

// prepareMergedTemp 在 complete 期对临时名做全文件校验（== file_checksum）并逐分片准确
// 报告 mismatch。校验通过返回 (nil, nil)；全文件校验失败返回 (mismatchList, err)；
// 临时文件被外部删除/不可读返回 (mismatchList, err)（findMismatchChunks 对缺失返回全部分片
// mismatch → 调用方按 400 返回 mismatch_chunks，客户端整文件重传，而非 500 永久挂起）。
// 做法：持 LockChunkMerge 排他（防 chunk 并发 seek 写）后单遍哈希整临时文件比对
// file_checksum —— 不匹配再逐分片 seek 重算（带长度语义 offset=i*ChunkSize、length=
// chunkLenAt，与写侧/恢复侧一致）→ 精确定位坏分片 → ClearChunksReceived 落盘 bitmap
// （status 亦反映需重传列表）。不复用上传期的独立 .chunk 文件（任务 4 起不存在）。
func (s *Service) prepareMergedTemp(ctx context.Context, store *UploadStore, tnt *storage.Tenant, session *ChunkedUploadSession) ([]int, error) {
	if tnt == nil || tnt.Root() == nil || session.TempPath == "" {
		return nil, fmt.Errorf("会话缺少在途临时文件，无法完成上传")
	}
	unlockMerge := store.LockChunkMerge(session.UploadID)
	defer unlockMerge()

	src, err := s.openSessionTemp(tnt, session)
	if err != nil {
		// 任务 8 M-3：临时文件缺失/不可读 → findMismatchChunks 返回全部分片 index（客户端
		// 整文件重传），而非 500（临时名命中 isInflightTempName 不入列表，此处按 mismatch 显式化）。
		if os.IsNotExist(err) {
			return allMismatchIndices(session), err
		}
		return nil, fmt.Errorf("打开在途临时文件失败: %w", err)
	}
	defer src.Close()

	// 单遍整文件哈希。ctx 由 WithoutCancel 派生，永不 cancel；保守检查保留。
	hf := sha256.New()
	if _, err := io.Copy(hf, src); err != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		return nil, fmt.Errorf("读取在途临时文件失败: %w", err)
	}
	if hex.EncodeToString(hf.Sum(nil)) == session.FileChecksum {
		return nil, nil // 全文件校验通过
	}

	// 全文件校验失败：逐分片 seek 重算 mismatch（I-2：重叠/越界写坏单片被精确定位）。
	mismatch := store.findMismatchChunks(session)
	if len(mismatch) == 0 {
		// 理论不可达（整文件哈希不同但每个分片哈希都匹配），防御：全部视为 mismatch。
		mismatch = allMismatchIndices(session)
	}
	// 落盘 bitmap：坏分片清位（重复 complete 仍返回同样的 mismatch；status 反映需重传）。
	if err := store.ClearChunksReceived(session.UploadID, mismatch); err != nil {
		s.rt.logger().Error("complete mismatch 清位失败", "upload_id", session.UploadID, "error", err)
	}
	return mismatch, fmt.Errorf("分块校验失败：%d 个分片不匹配", len(mismatch))
}

// sendUploadRouteError 把 RouteUpload 返回的错误映射为 HTTP 响应（403/409/507/400）。
// 非 HTTPError 视为内部错误（500）。
//
// 与 pkg/server.sendUploadRouteError 语义一致（同状态码、同文案来源、同日志级别）；
// 装配层已把 *routeError 映射为 HTTPError（见 service.go 的 HTTPError）。
func (s *Service) sendUploadRouteError(w http.ResponseWriter, r *http.Request, remotePath string, err error) {
	if he, ok := errors.AsType[*HTTPError](err); ok {
		s.rt.logger().WarnContext(r.Context(), "上传卷路由拒绝", "file_name", remotePath, "status", he.Status, "reason", err.Error())
		s.sendJSON(w, UploadResponse{Success: false, Message: he.Message}, he.Status)
		return
	}
	s.rt.logger().ErrorContext(r.Context(), "上传卷路由失败", "file_name", remotePath, "error", err.Error())
	s.sendJSON(w, UploadResponse{Success: false, Message: errMsgSaveFailed}, http.StatusInternalServerError)
}
