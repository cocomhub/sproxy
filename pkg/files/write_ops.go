// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// write_ops.go 是**写面的域操作 API**：上传（`WriteFile`）、目录（`MakeDir` / `RemoveDir`）、
// 重命名（`RenameFile`）、删除（`DeleteFile`）。
//
// 与 `write.go` / `dirs.go` / `rename.go` / `delete.go` 的分工（同 read_ops.go）：本文件承载
// 领域逻辑（路径校验 → 并发互斥 → 重复/版本 → 卷路由与双账本 → 原子写与哈希 → 结算；
// 以及重命名/删除的跨卷定位、checksum 门禁、配额与台账释放），四个 HTTP 文件只做
// 「解析请求/头 → 调域方法 → 写状态码与响应体」。
//
// 为什么必须抽出：远程写（Y 二期）与任何新表面都要复用**同一份**写语义——checksum 门禁、
// mtime、原子改名、版本保存、配额双账本、文件级锁、卷路由、审计。若各写一份，这些不变量必然分叉。
package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// WriteFileInput 是上传（写文件）的领域入参。**不含任何 HTTP 类型**：`Mtime` 由调用方从
// `X-File-MTime` 头解析后传入（0 = 不设置）。
type WriteFileInput struct {
	// Owner 是操作主体（空 → anonymous，本方法内归一）。
	Owner string
	// RemotePath 是用户可见相对路径（含子目录）。
	RemotePath string
	// ExplicitVol 是 `?volume=` 显式卷（空 = 按 ACL/placement 自动路由）。
	ExplicitVol string
	// ExpectedChecksum 是客户端声明的 SHA-256（**必填**；不符即 400 并删除已写内容）。
	ExpectedChecksum string
	// ClientSize 是客户端声明的大小（用于成功文案与路由预检）。
	ClientSize int64
	// Mtime 是写入后要设置的文件修改时间（UnixNano；0 = 不设置）。
	Mtime int64
}

// WriteFileResult 是上传的领域结果。
type WriteFileResult struct {
	// VolumeName 是落盘卷名（空 = 无卷语义；调用方按需回 `X-Volume` 头）。
	VolumeName string
	// Checksum 是服务端计算的 SHA-256（幂等命中时为客户端声明的值）。
	Checksum string
	// Size 是落盘字节数（幂等命中时为已存在文件的大小）。
	Size int64
	// Idempotent 表示「同名同 checksum 已存在、未写盘」（历史语义：200 成功）。
	Idempotent bool
	// Message 是面向客户端的成功文案。
	//
	// **刻意由域侧给出**：两条成功路径的历史文案不同（幂等为「文件已上传成功，size: N」，
	// 正常为「文件上传成功，size: N」），把它们留在域侧可避免调用方各自拼文案而分叉。
	Message string
}

// WriteFile 把 src 写入目标卷租户根内的 `input.RemotePath`（原子写 + 流式哈希）。
//
// 执行顺序（与既有实现逐字一致，勿重排——顺序本身是不变量）：
//
//  1. 路径校验（pathguard + UserRel）→ 400
//  2. 文件级互斥（同 owner 同 rel 并发写拒绝）→ 409
//  3. 写前 home 定位 + 重复检测/版本保存（幂等 200 / 冲突 409 / 版本化覆盖）
//  4. 卷路由 + 双账本预留（ACL/placement/容量）→ 403/409/507/500
//  5. 建中间目录 → 原子写入 → 校验和比对（不符即删并 400）→ 双账本结算
//  6. checksum 台账写入 + mtime 落地
//
// 失败一律以 `*HTTPError` 表达（含可选 `Checksum`：上传冲突时附带服务端实际 checksum，
// 方便客户端决策——历史契约的一部分），调用方据此写响应。
func (s *Service) WriteFile(ctx context.Context, input WriteFileInput, src io.Reader) (WriteFileResult, error) {
	owner := normalizeOwner(input.Owner)
	logger := s.rt.logger()

	remotePath, rel, err := s.resolveWritePath(owner, input.RemotePath)
	if err != nil {
		return WriteFileResult{}, err
	}
	logger.DebugContext(ctx, "上传路径", "remote_path", remotePath)

	// 并发上传防护：防止同一 owner 同 rel 被多个上传请求同时写入导致 OOM。
	// 键空间与 delete / move / 分块 complete 共用（FileLocks 实现负责归一 owner 与分隔符）。
	releaseUpload, acquired := s.rt.fileLocks().TryMark(owner, rel, uploadingLockUpload)
	if !acquired {
		logger.WarnContext(ctx, "文件正在上传中，拒绝并发上传", "file_name", remotePath)
		return WriteFileResult{}, &HTTPError{Status: http.StatusConflict, Message: "文件正在上传中"}
	}
	defer releaseUpload()

	out := WriteFileResult{}

	// 重复检测与版本管理（自动路由）：先做「写前视图定位」确认 rel 的真实 home 卷（F1-A/B），
	// **只在 home 命中时**对其根做幂等/冲突/版本检查——幂等同 checksum 重传在容量/配额检查前
	// 直接 200（不因卷满误拒）；命中 → 覆盖写必须 stay-home 到 home 卷（容量路由只用于新文件，
	// 否则同 rel 跨卷双份 + owner 双计）。显式 volume 不做此检查：其语义是新文件定位，同名已
	// 存在（含同 checksum）一律由 routeUpload 唯一性 409。
	//
	// F-1（AD-6 写侧闭合）：home 以 owner 卷视图为界（LocateOwnerFile 只搜 AllowedVolumes）。
	// locate miss（rel 不在 owner 视图任何卷，含「默认卷被 ACL 排除但仍留 owner 遗留」）→
	// 该 rel 不属于 owner 逻辑树，**跳过 dup-check 视为新文件**交容量路由——不得对无权卷的
	// 遗留做版本化覆盖写（泄漏到无权卷 version/）或假 409/假幂等（误伤新写入）。
	// 写前 home 定位 + 重复检测/版本管理（自动路由）。详细语义见 resolveWriteHome。
	forceHomeVol, handled, dupRes, dupErr := s.resolveWriteHomeHandled(ctx, owner, rel, remotePath, input, logger)
	if handled {
		return dupRes, dupErr
	}

	// 卷路由 + 双账本预留（T4/T5）：RouteUpload 按 ACL/placement 选目标卷，在 owner 全局
	// Scope + 卷容量池双 TryReserve；显式 volume= 时做 ACL 校验与唯一性查重（403/409）；
	// forceHomeVol 非空时强制该 home 卷单候选（容量不足 507 不换卷）。
	route, routeErr := s.routeWriteUpload(ctx, logger, owner, rel, input, forceHomeVol, remotePath)
	if routeErr != nil {
		return WriteFileResult{}, routeErr
	}
	out.VolumeName = route.VolumeName
	// **外部卷写入（2026-10-05 用户裁定：普通上传进外部卷）**：route.Sink 非 nil =
	// 目标卷是外部后端（baidupcs/secretdata）。外部卷无本地 inode 语义——跳过原子写
	// （临时+rename）、硬链接去重、版本管理（这些依赖 *storage.Root）；直接
	// MakeDir + WriteFile 整流 + 校验和比对 + 双账本结算（简单路径，无去重/版本/台账）。
	if route.Sink != nil {
		res, werr := s.writeExternalSettle(ctx, logger, externalWriteArgs{
			owner: owner, rel: rel, input: input, route: route, src: src, remotePath: remotePath,
		})
		if werr != nil {
			return WriteFileResult{}, werr
		}
		res.VolumeName = route.VolumeName
		return res, nil
	}
	root := route.Tenant.Root()

	if mkdirErr := root.MkdirAll(filepath.Dir(rel), 0755); mkdirErr != nil {
		route.Release()
		logger.ErrorContext(ctx, errMsgCreateDirFailed, "error", mkdirErr.Error())
		return WriteFileResult{}, &HTTPError{Status: http.StatusInternalServerError, Message: errMsgCreateDirFailed}
	}

	// 覆盖写场景先统计旧文件大小 prev（双 Adjust 差分用）。
	prev := int64(0)
	if stat, statErr := root.Stat(rel); statErr == nil {
		prev = stat.Size()
	}

	// 内容寻址去重（roadmap P1，dedup.enabled）：
	//   - 覆盖写（prev>0）：先从台账摘除本 rel 的旧内容引用（旧 checksum 可能另有引用；
	//     归零时旧 inode 由后续写盘原子替换自然释放——旧物理文件被 rename 覆盖）。
	//     但硬链接引用下「rename 覆盖」会断开链接关系：本 rel 是链接时需先摘除再真删旧
	//     inode（否则 rename 覆盖的是 inode 自身，另一引用仍指向旧内容 → 链接断裂）。
	//   - 新文件（prev==0）：查同卷已有同 checksum → 硬链接零拷贝 + 台账追加引用；
	//     配额只计首份物理占用。命中时回滚本次预留并直接结算。
	//
	// 安全边界：台账 per-tenant（owner 隔离）；只同卷硬链（跨卷不合并）。
	f := &fileOp{ctx: ctx, root: root, owner: owner, remotePath: remotePath, logger: logger}
	if res, handled, dedupErr := s.tryDedupUploadHandled(f, rel, prev, input, route); handled {
		return res, dedupErr
	}

	// 原子写入 + 流式哈希（目标卷 root）+ SHA-256 比对 + 双账本结算 + 成功副作用。
	res, err := s.writeFileSettle(f, rel, prev, input, route, src)
	if err != nil {
		return WriteFileResult{}, err
	}
	res.VolumeName = route.VolumeName
	return res, nil
}

// fileOp 是单文件写/删操作（upload dedup / delete）的共享上下文（S107：收敛 write_ops
// 系列多参数函数）。只承载单文件操作全程不变的 ctx/root/owner/remotePath/logger，
// 各函数差异化参数（rel/input/route/prev/homeVol/quarRef/info 等）仍走方法签名。
type fileOp struct {
	//nolint:containedctx // S8242 已评估：单文件操作作用域共享 ctx（S107 收敛后），非请求侧驻留——与下行 NOSONAR 同源双抑制
	ctx        context.Context // NOSONAR: S8242 — 单文件写/删操作全程不变的共享 ctx（S107 收敛），非请求侧驻留
	root       *storage.Root
	owner      string
	remotePath string
	logger     *slog.Logger
}

// externalWriteArgs 是外部卷写入参数组（S107 收敛：writeExternalSettle 8 参数 → 3）。
type externalWriteArgs struct {
	owner      string
	rel        string
	input      WriteFileInput
	route      UploadRoute
	src        io.Reader
	remotePath string
}

// writeExternalSettle 外部卷整流写入（2026-10-05 用户裁定：普通上传进外部卷）：
// MakeDir + WriteFile + 流式哈希 + SHA-256 比对（不符删回 400）+ 双账本结算。
// 无原子写/去重/版本/台账——外部卷是远程后端，这些本地 inode 语义天然不适用。
func (s *Service) writeExternalSettle(ctx context.Context, logger *slog.Logger, a externalWriteArgs) (WriteFileResult, error) {
	// 覆盖写场景先统计旧文件大小 prev（双 Adjust 差分用）。
	prev := int64(0)
	if sz, ok, serr := a.route.Sink.Stat(ctx, a.rel); serr == nil && ok {
		prev = sz
	}
	if derr := a.route.Sink.MakeDir(ctx, filepath.Dir(a.rel)); derr != nil {
		a.route.Release()
		logger.ErrorContext(ctx, errMsgCreateDirFailed, "error", derr.Error(), "file_name", a.remotePath)
		return WriteFileResult{}, &HTTPError{Status: http.StatusInternalServerError, Message: errMsgCreateDirFailed}
	}
	// 整流写入 + 流式哈希（一次性流，写盘同时算 SHA-256——外部卷无原子写，写后比对）。
	hasher := sha256.New()
	limitedSrc := limitReader(s.rt.bandwidthLimiter(), a.owner, io.TeeReader(a.src, hasher))
	start := time.Now()
	if werr := a.route.Sink.WriteFile(ctx, a.rel, limitedSrc, a.input.ClientSize, a.input.Mtime); werr != nil {
		a.route.Release()
		logger.ErrorContext(ctx, "外部卷保存文件失败", "error", werr.Error(), "file_name", a.remotePath)
		return WriteFileResult{}, &HTTPError{Status: http.StatusInternalServerError, Message: errMsgSaveFailed}
	}
	serverChecksum := hex.EncodeToString(hasher.Sum(nil))
	if serverChecksum != a.input.ExpectedChecksum {
		_ = a.route.Sink.Remove(ctx, a.rel)
		a.route.Release()
		logger.WarnContext(ctx, "外部卷文件 SHA-256 校验失败", "server", serverChecksum, "client", a.input.ExpectedChecksum, "file_name", a.remotePath)
		return WriteFileResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgChecksumMismatch}
	}
	// 双账本结算：覆盖写 Adjust(prev, written) + Release；新文件 Commit(written)。
	a.route.Commit(prev, a.input.ClientSize)
	if mr := s.rt.metricsRecorder(); mr != nil {
		mr.RecordUpload(a.input.ClientSize)
	}
	logger.DebugContext(ctx, "外部卷上传成功", "file_name", a.remotePath, "size", a.input.ClientSize, "elapsed", time.Since(start))
	return WriteFileResult{Checksum: serverChecksum, Size: a.input.ClientSize, Message: "文件上传成功"}, nil
}

// writeFileSettle 原子写入 + 流式哈希 + SHA-256 比对（不符删文件回 400）+ 双账本结算
// + checksum 台账 + mtime 落地 + 去重新内容登记。返回最终成功结果。
func (s *Service) writeFileSettle(f *fileOp, rel string, prev int64, input WriteFileInput, route UploadRoute, src io.Reader) (WriteFileResult, error) {
	// 原子写入 + 流式哈希（目标卷 f.root）。
	// 带宽限速（roadmap §6 P1）：src 按 f.owner 桶包一层限速 reader；未装配限速器原样直通。
	limitedSrc := limitReader(s.rt.bandwidthLimiter(), f.owner, src)
	serverChecksum, written, wErr := writeFileAtomicallyRoot(f.ctx, f.root, rel, limitedSrc)
	if wErr != nil {
		route.Release()
		f.logger.ErrorContext(f.ctx, "保存文件失败", "error", wErr.Error(), "file_name", f.remotePath)
		return WriteFileResult{}, &HTTPError{Status: http.StatusInternalServerError, Message: errMsgSaveFailed}
	}

	if serverChecksum != input.ExpectedChecksum {
		// 清理已写入的校验失败文件，忽略错误（临时文件由 writeFileAtomicallyRoot 清理）
		_ = f.root.Remove(rel)
		route.Release()
		f.logger.WarnContext(f.ctx, errMsgChecksumMismatch, "server", serverChecksum, "client", input.ExpectedChecksum, "file_name", f.remotePath)
		return WriteFileResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgChecksumMismatch}
	}

	// 双账本结算：覆盖写 Adjust(prev, written) + Release（旧文件已占用 prev，差分收敛到
	// 新大小）；新文件 Commit(written)。f.owner 全局 Scope 与卷容量池同语义。
	route.Commit(prev, written)

	// 成功后的副作用：checksum 台账写入 + mtime 落地（原 setUploadResponseHeaders 的领域部分）。
	s.recordUploadSuccess(f.root, f.owner, f.remotePath, rel, serverChecksum, input.Mtime, f.logger)
	if ds := s.rt.dedupStore(f.owner); s.rt.dedupEnabled() && ds != nil {
		// 新文件与覆盖写都登记新 checksum 引用（覆盖写已在上方 prev>0 分支摘除旧引用；
		// 本处登记新内容引用，使后续去重可命中）。幂等/409 提前返回不经过此处。
		ds.Add(rel, route.VolumeName, serverChecksum)
	}
	// 可信卷（本地卷到达即建 meta）：写入成功后生成配套 FileMeta（从已落盘文件
	// 计算总 sha256+md5 + 分块）并落隐藏 `.meta`（占配额——经装配层实现写入计入账本）。
	// 失败不阻断主写成功（meta 缺失时读路径直算/Stat 兜底；用户裁定新文件保障立刻创建）。
	if s.rt.fileMetaEnabled() {
		if mErr := s.rt.writeMetaSidecar(f.ctx, f.root, rel); mErr != nil {
			f.logger.Warn("可信卷 meta 落盘失败（读路径直算兜底）", "file_name", rel, "error", mErr)
		}
	}

	return WriteFileResult{Checksum: serverChecksum, Size: written, Message: fmt.Sprintf("文件上传成功, size: %d", input.ClientSize)}, nil
}

// resolveWritePath 校验并映射写路径（pathguard + UserRel），失败以 *HTTPError{400} 表达。
//
// 与 read_ops 的差异：写路径的非法错误**沿用 pathguard 的原始文案**（历史契约：客户端看到
// 的是 ValidateFilePath 的具体原因），而 UserRel 失败回统一 errMsgInvalidPath。
// linkFile 在 root 内创建硬链接 oldRel → newRel。
// 测试接缝：s.linkFunc 非 nil 时委托它（FAT/exFAT 回退用例注入失败）；
// 生产（nil）走 root.Link 真实行为。
func (s *Service) linkFile(root *storage.Root, oldRel, newRel string) error {
	if s.linkFunc != nil {
		return s.linkFunc(oldRel, newRel)
	}
	return root.Link(oldRel, newRel)
}

// dedupFallbackCopy 在硬链接不可用（FAT/exFAT）时把 srcRel 内容原子复制到 rel。
// 返回复制后 checksum 与实际写入字节数；失败时清理临时文件并回滚。
func (s *Service) dedupFallbackCopy(ctx context.Context, root *storage.Root, srcRel, rel string) (string, int64, error) {
	srcFile, err := root.Open(srcRel)
	if err != nil {
		return "", 0, fmt.Errorf("打开去重源文件失败: %w", err)
	}
	defer func() { _ = srcFile.Close() }()
	// 复用原子写路径（临时文件 + fsync + rename）：复制 = 与普通上传相同的落盘语义。
	checksum, written, err := writeFileAtomicallyRoot(ctx, root, rel, srcFile)
	if err != nil {
		return "", 0, fmt.Errorf("去重回退复制失败: %w", err)
	}
	return checksum, written, nil
}

// tryDedupUpload 尝试内容寻址去重（dedup.enabled）：覆盖写（prev>0）先摘除旧引用；
// 新文件（prev==0）查同卷已有同 checksum → 硬链接零拷贝（成功回滚预留直接结算）/
// 回退复制（FAT/exFAT，按实际占用结算）。返回 handled=true 表示已产出最终结果
// （err 非 nil 时调用方直接返回该错误）。
func (s *Service) tryDedupUpload(f *fileOp, rel string, prev int64, input WriteFileInput, route UploadRoute) (WriteFileResult, bool, error) {
	ds := s.rt.dedupStore(f.owner)
	if !s.rt.dedupEnabled() || ds == nil {
		return WriteFileResult{}, false, nil
	}
	if prev > 0 {
		// 摘除旧引用（旧 checksum 从文件校验或台账 RelExists 判定）。
		// 本 rel 是硬链接（台账中该 rel 属于某 checksum 且非首份）时：rename 覆盖会
		// 断开 inode 链接（新内容替换该目录项，原 inode 仍被另一引用持有但目录项已
		// 脱离）→ 先移除本 rel 目录项（unlink），再以新内容原子写（原 inode 若归零
		// 即释放）。unlink 后 rename 覆盖即新 inode，无链接断裂。
		if oldCS, ok := ds.RelCS(rel, route.VolumeName); ok {
			ds.RemoveRef(rel, route.VolumeName, oldCS)
		}
	}
	if prev == 0 {
		if srcRel, ok := ds.FirstRel(route.VolumeName, input.ExpectedChecksum); ok {
			// 命中：先不释放预留（Link 成功 = 硬链接零拷贝不占新配额 → Release 回滚；
			// Link 失败 = 回退复制占实际配额 → Commit）。
			return s.tryDedupNewFile(f, srcRel, rel, input, route, ds)
		}
	}
	return WriteFileResult{}, false, nil
}

// tryDedupNewFile 新文件去重命中处理：硬链接零拷贝（成功回滚预留直接结算）/
// 回退复制（FAT/exFAT，按实际占用结算）。返回 handled=true 表示已产出最终结果。
func (s *Service) tryDedupNewFile(f *fileOp, srcRel, rel string, input WriteFileInput, route UploadRoute, ds *DedupStore) (WriteFileResult, bool, error) {
	linkErr := s.linkFile(f.root, srcRel, rel)
	if linkErr != nil {
		// FAT/exFAT 无硬链接（ENOTSUP/EPERM/EXDEV）：回退普通复制——
		// 源内容流式复制到 rel（原子写 + 流式哈希），台账仍登记引用计数，
		// 配额按实际占用量结算（复制形态 = 每文件独立物理，双计）。
		f.logger.WarnContext(f.ctx, "去重硬链接不可用，回退复制", "error", linkErr.Error(), "file_name", f.remotePath)
		fallbackChecksum, size, copyErr := s.dedupFallbackCopy(f.ctx, f.root, srcRel, rel)
		if copyErr != nil {
			route.Release()
			f.logger.ErrorContext(f.ctx, "去重回退复制失败", "error", copyErr.Error(), "file_name", f.remotePath)
			s.rt.recordFileAudit(f.ctx, "upload", f.remotePath, auditResultError, "去重回退复制失败")
			return WriteFileResult{}, true, &HTTPError{Status: http.StatusInternalServerError, Message: errMsgSaveFailed}
		}
		if fallbackChecksum != input.ExpectedChecksum {
			route.Release()
			_ = f.root.Remove(rel)
			f.logger.WarnContext(f.ctx, "去重回退复制校验失败", "file_name", f.remotePath)
			return WriteFileResult{}, true, &HTTPError{Status: http.StatusInternalServerError, Message: errMsgSaveFailed}
		}
		route.Commit(0, size)
		ds.Add(rel, route.VolumeName, input.ExpectedChecksum)
		s.recordUploadSuccess(f.root, f.owner, f.remotePath, rel, input.ExpectedChecksum, input.Mtime, f.logger)
		// 可信卷：去重回退复制路径也建 meta（C-MAJOR-3 修复——去重 rel 与普通上传一致有 sidecar）。
		if s.rt.fileMetaEnabled() {
			if mErr := s.rt.writeMetaSidecar(f.ctx, f.root, rel); mErr != nil {
				f.logger.Warn("可信卷 meta 落盘失败（去重回退复制）", "file_name", rel, "error", mErr)
			}
		}
		return WriteFileResult{Checksum: input.ExpectedChecksum, Size: size, Message: fmt.Sprintf("文件上传成功, size: %d", input.ClientSize)}, true, nil
	}
	// 硬链接成功：零拷贝不占新配额 → 回滚预留。
	route.Release()
	ds.Add(rel, route.VolumeName, input.ExpectedChecksum)
	s.recordUploadSuccess(f.root, f.owner, f.remotePath, rel, input.ExpectedChecksum, input.Mtime, f.logger)
	// 引用文件大小 = 已存首份大小（从硬链接目标 stat）。
	var linkedSize int64
	if fi, statErr := f.root.Stat(rel); statErr == nil {
		linkedSize = fi.Size()
	}
	return WriteFileResult{Checksum: input.ExpectedChecksum, Size: linkedSize, Message: fmt.Sprintf("文件上传成功, size: %d", input.ClientSize)}, true, nil
}

func (s *Service) resolveWritePath(owner, filename string) (remotePath, rel string, err error) {
	remotePath, vErr := pathguard.ValidateFilePath(filename)
	if vErr != nil {
		return "", "", &HTTPError{Status: http.StatusBadRequest, Message: vErr.Error()}
	}
	tnt := s.rt.tenantOf(owner)
	if tnt == nil {
		return "", "", &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}
	// 服务端内部目录访问防护收敛到 UserRel（逐段 ValidSegmentName 拒绝 .__ 前缀）：
	// 用户显式上传/重命名不得落到服务端内部目录，user/ 桶内 .__ 前缀段已被
	// ValidSegmentName 拒绝，无需再单独内部目录守卫。
	rel, ok := tnt.UserRel(remotePath)
	if !ok {
		return "", "", &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}
	return remotePath, rel, nil
}

// duplicateOutcome 是「同名已存在且 checksum 匹配」的幂等结果（非 nil 表示已处理完、无需写盘）。
type duplicateOutcome struct {
	Checksum string
	Size     int64
	Message  string
}

// resolveWriteHome 写前视图定位 + 重复检测/版本管理（自动路由）：
// 只在 home 命中时对其根做幂等/冲突/版本检查——幂等同 checksum 重传在容量/配额检查前
// 直接 200（不因卷满误拒）；命中 → 覆盖写必须 stay-home 到 home 卷（容量路由只用于新文件，
// 否则同 rel 跨卷双份 + owner 双计）。显式 volume 不做此检查：其语义是新文件定位，同名已
// 存在（含同 checksum）一律由 routeUpload 唯一性 409。
//
// F-1（AD-6 写侧闭合）：home 以 owner 卷视图为界（LocateOwnerFile 只搜 AllowedVolumes）。
// locate miss（rel 不在 owner 视图任何卷，含「默认卷被 ACL 排除但仍留 owner 遗留」）→
// 该 rel 不属于 owner 逻辑树，**跳过 dup-check 视为新文件**交容量路由——不得对无权卷的
// 遗留做版本化覆盖写（泄漏到无权卷 version/）或假 409/假幂等（误伤新写入）。
// 返回 (forceHomeVol, handled, result, err)——handled=true 表示已产出最终结果。
func (s *Service) resolveWriteHome(ctx context.Context, owner, rel, remotePath string, input WriteFileInput, logger *slog.Logger) (string, bool, WriteFileResult, error) {
	if input.ExplicitVol != "" {
		return "", false, WriteFileResult{}, nil
	}
	loc, found := s.rt.locateOwnerFile(owner, rel)
	if !found || loc.Tenant == nil {
		// locate miss → 新文件：交容量路由（无 dup-check；VolSet==nil 唯一根下 stat miss
		// 等价旧行为——handleDuplicateFile 在 miss 时本就返回 false）。
		return "", false, WriteFileResult{}, nil
	}
	forceHomeVol := loc.VolumeName
	idem, existed, dupErr := s.handleDuplicateFile(ctx, owner, loc.Tenant, rel, input.ExpectedChecksum, remotePath)
	if dupErr != nil {
		return "", true, WriteFileResult{}, dupErr
	}
	if idem != nil { // 幂等命中：已存在且 checksum 匹配，直接成功（不写盘）
		return forceHomeVol, true, WriteFileResult{
			VolumeName: forceHomeVol,
			Checksum:   idem.Checksum,
			Size:       idem.Size,
			Idempotent: true,
			Message:    idem.Message,
		}, nil
	}
	// existed=true = home 卷命中且继续（版本化覆盖写）→ stay-home（forceHomeVol 保留，
	// 容量不足 RouteUpload 直接 507 不换卷）。existed=false 是 locate 命中与 dup-check
	// 间 TOCTOU 的防御（理论竞态）→ 交容量路由。
	if !existed {
		forceHomeVol = ""
	}
	return forceHomeVol, false, WriteFileResult{}, nil
}

// resolveWriteHomeHandled 写前 home 定位 + 重复检测/版本管理（WriteFile 步骤 3）的
// handled 归一化：把「重复输出」折叠为 (handled=true, result, err) 形状，供调用方直接
// 返回（err 非 nil 时 result 为零值）。未处理（handled=false）时原样透传 forceHomeVol。
func (s *Service) resolveWriteHomeHandled(ctx context.Context, owner, rel, remotePath string, input WriteFileInput, logger *slog.Logger) (string, bool, WriteFileResult, error) {
	forceHomeVol, handled, dupRes, dupErr := s.resolveWriteHome(ctx, owner, rel, remotePath, input, logger)
	if !handled {
		return forceHomeVol, false, WriteFileResult{}, nil
	}
	return forceHomeVol, true, dupRes, dupErr
}

// routeWriteUpload 卷路由 + 双账本预留（WriteFile 步骤 4）：RouteUpload 按 ACL/placement
// 选目标卷，在 owner 全局 Scope + 卷容量池双 TryReserve。失败返回 *HTTPError
// （路由拒绝按类型映射 403/409/507，内部错误 → 500）；route.Tenant 不可用 → 400。
// 返回的 route 必满足 route.Tenant 与 route.Tenant.Root() 非 nil（调用方可直接落盘）。
func (s *Service) routeWriteUpload(ctx context.Context, logger *slog.Logger, owner, rel string, input WriteFileInput, forceHomeVol, remotePath string) (UploadRoute, error) {
	route, routeErr := s.rt.routeUpload(owner, rel, input.ExplicitVol, input.ClientSize, forceHomeVol)
	if routeErr != nil {
		logger.WarnContext(ctx, "上传卷路由拒绝", "file_name", remotePath, "error", routeErr.Error())
		if he, ok := errors.AsType[*HTTPError](routeErr); ok {
			return UploadRoute{}, he
		}
		logger.ErrorContext(ctx, "上传卷路由失败", "file_name", remotePath, "error", routeErr.Error())
		return UploadRoute{}, &HTTPError{Status: http.StatusInternalServerError, Message: errMsgSaveFailed}
	}
	if route.Tenant == nil && route.Sink == nil {
		route.Release()
		return UploadRoute{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}
	return route, nil
}

// tryDedupUploadHandled 内容寻址去重（WriteFile 步骤 6）的 handled 归一化：返回 handled=true
// 表示已产出最终结果（dedupErr 非 nil 时调用方直接返回该错误）。
func (s *Service) tryDedupUploadHandled(f *fileOp, rel string, prev int64, input WriteFileInput, route UploadRoute) (WriteFileResult, bool, error) {
	res, handled, dedupErr := s.tryDedupUpload(f, rel, prev, input, route)
	if !handled {
		return WriteFileResult{}, false, nil
	}
	return res, true, dedupErr
}

// handleDuplicateFile 检查文件是否存在，处理重复上传和版本管理逻辑。
//
// 返回：
//   - (非 nil, _, nil)：幂等命中（已存在且 checksum 匹配）——调用方直接回成功，不写盘；
//   - (nil, true, nil)：home 卷命中且应继续「覆盖写」（版本化启用时已保存旧版本）；
//   - (nil, false, nil)：未命中（新文件 / 非本卷 home）——继续正常上传；
//   - (nil, _, err)：冲突且 versioning 关闭 → *HTTPError{409}（可能附服务端 checksum）。
//
// homeTnt 为已定位的 home 卷租户；rel 为租户根内相对路径。
func (s *Service) handleDuplicateFile(ctx context.Context, owner string, homeTnt *storage.Tenant, rel, expectedChecksum, remotePath string) (*duplicateOutcome, bool, error) {
	if homeTnt == nil || homeTnt.Root() == nil {
		return nil, false, nil
	}
	root := homeTnt.Root()
	stat, statErr := root.Stat(rel)
	if statErr != nil {
		return nil, false, nil // 文件不存在，继续正常上传（新文件或非本卷 home）
	}
	if verifyFileWithChecksumRoot(root, rel, expectedChecksum) {
		// 幂等上传：文件已存在且 checksum 匹配，直接返回成功（不保存版本）
		return &duplicateOutcome{
			Checksum: expectedChecksum,
			Size:     stat.Size(),
			Message:  fmt.Sprintf("文件已上传成功, size: %d", stat.Size()),
		}, true, nil
	}
	// 与基线 `cfg := h.cfgPtr.Load(); if cfg.Versioning.Enabled` 逐字同源（接缝的
	// VersioningEnabled 即该形态，cfg 未装配时同样 panic——见 pkg/server/handlers.go 的
	// 接缝注释），故本处**无控制流残差**。
	if s.rt.versioningEnabled() {
		// 版本管理启用时，checksum 不匹配视为有意覆盖旧版本（homeTnt 即旧文件所在卷）
		s.saveVersionBeforeOverwrite(owner, remotePath, homeTnt)
		// 审查 I-3：覆盖动作记审计（含旧版本已保存的信息）。
		s.rt.recordFileAudit(ctx, "overwrite", remotePath, auditResultSuccess, "覆盖现有文件（版本已保存）")
		return nil, true, nil // 继续执行写入流程，用新内容覆盖现有文件（home=本卷）
	}
	// checksum 不匹配：冲突，需保留现有文件
	s.rt.logger().WarnContext(ctx, "文件已存在，但校验失败", "file_name", remotePath)
	// 审查 I-3：versioning 关闭时同名覆盖是静默数据丢失，记审计（当前走冲突拒绝
	// 分支——保留现有文件，不覆盖；此处为 denied 留痕）。
	s.rt.recordFileAudit(ctx, "overwrite", remotePath, auditResultDenied, "文件已存在且 checksum 不匹配（versioning 关闭，拒绝覆盖）")
	// 附带服务端文件的实际 checksum，方便客户端决策
	he := &HTTPError{Status: http.StatusConflict, Message: "文件已存在，但校验失败"}
	if serverCS, csErr := fileChecksumRoot(root, rel); csErr == nil {
		he.Checksum = serverCS
	}
	return nil, true, he
}

// recordUploadSuccess 记录上传成功后的领域副作用：checksum 台账写入 + mtime 落地。
//
// 抽自原 setUploadResponseHeaders（其"写响应头"部分留回处理器，领域只保留台账与落盘副作用）。
func (s *Service) recordUploadSuccess(root *storage.Root, owner, remotePath, rel, serverChecksum string, mtime int64, logger *slog.Logger) {
	if cs := s.rt.checksumStore(owner); cs != nil {
		cs.Set(rel, serverChecksum)
	} else {
		logger.WarnContext(context.Background(), "per-tenant checksum store 不可用，跳过记录", "file_name", remotePath)
	}
	if mtime > 0 {
		modTime := time.Unix(0, mtime)
		if err := root.Chtimes(rel, modTime, modTime); err != nil {
			logger.WarnContext(context.Background(), "设置文件时间戳失败", "file_name", remotePath, "error", err)
		}
	}
	// 搜索索引增量 upsert（roadmap P0）：与 checksum 台账同生命周期（单次上传与分块
	// complete 共用本函数）。rel 是租户根相对（含 user/ 前缀），索引 key 为相对 user
	// 桶路径（去 user/ 前缀）；mtime 用磁盘实际（Chtimes 后已生效）。
	if s.index != nil {
		if modTime, err := root.Stat(rel); err == nil {
			// 索引 key 相对 user 桶（去 user/ 前缀）：从 owner 默认租户派生 user 桶名。
			userRoot := "user"
			if tnt := s.rt.tenantOf(owner); tnt != nil {
				userRoot = tnt.UserRoot()
			}
			s.index.upsert(owner, strings.TrimPrefix(rel, userRoot+"/"),
				modTime.Size(), modTime.ModTime().UnixNano(), s.volumeNameForRoot(root), root, rel)
		}
	}
	// 文件变更事件（roadmap §2 P1）：upload 成功（含覆盖写/分块 complete 共用本函数）。
	s.rt.publishFileEvent(EventUpload, owner, rel, func() int64 {
		if fi, err := root.Stat(rel); err == nil {
			return fi.Size()
		}
		return 0
	}())
}

// volumeNameForRoot 返回 root 所在卷名（多卷）；旧装配（volSet nil）或未命中返回空。
// 用于索引条目记录物理归属（与搜索结果 volume 字段语义一致：目录为空，文件带卷名）。
func (s *Service) volumeNameForRoot(root *storage.Root) string {
	if root == nil || s.rt.volSet() == nil {
		return ""
	}
	for _, v := range s.rt.volSet().All() {
		if s.rt.volSet().Root(v.Name) == root {
			return v.Name
		}
	}
	return ""
}

// ---- 目录族域操作（mkdir / rmdir）----
//
// 命名说明：域操作用 MakeDir / RemoveDir（与 sync.FS.MakeDir 同词），HTTP 处理器仍叫
// Mkdir / Rmdir——前者是领域动作，后者是 HTTP 面，两者同名会互相遮蔽（且无法同时存在）。

// MakeDirResult 是建目录的领域结果。
type MakeDirResult struct {
	// RemotePath 是校验后的用户可见相对路径（调用方据此组文案/回包）。
	RemotePath string
}

// MakeDir 在 owner 视图内**首个卷**（默认卷优先）创建目录树。
//
// 错误语义（*HTTPError）：400 = dirname 为空 / 路径非法 / 租户不可用；500 = 创建失败。
func (s *Service) MakeDir(owner, dirname string) (MakeDirResult, error) {
	if dirname == "" {
		return MakeDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: "dirname 不能为空"}
	}
	remotePath, err := pathguard.ValidateFilePath(dirname)
	if err != nil {
		return MakeDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: "无效的目录名: " + err.Error()}
	}
	// 路径映射与卷无关（user/<path> 相对各卷租户根），用默认租户做纯路径校验；
	// 实际落盘目录选 owner 视图内首个卷（默认卷优先——默认卷开放时即默认租户，零回归；
	// 默认卷被 ACL 排除时落到视图卷，绝不经默认租户直写默认卷遗留，AD-6 闭合）。
	owner = normalizeOwner(owner)
	tnt0 := s.rt.tenantOf(owner)
	if tnt0 == nil || tnt0.Root() == nil {
		return MakeDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidDirPath}
	}
	rel, ok := tnt0.UserRel(remotePath)
	if !ok {
		return MakeDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidDirPath}
	}
	target := s.primaryViewTenant(owner)
	if target == nil || target.Root() == nil {
		return MakeDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidDirPath}
	}
	if mkErr := target.Root().MkdirAll(rel, 0755); mkErr != nil {
		s.rt.logger().Error(errMsgCreateDirFailed, "dir", remotePath, "error", mkErr)
		return MakeDirResult{}, &HTTPError{Status: http.StatusInternalServerError, Message: errMsgCreateDirFailed}
	}
	// 索引增量维护：mkdir 后索引补登记目录条目（父目录链由 upsert 顺带补全），
	// 否则 List 走索引时新目录不出现（#423 修复：TestFiles_Mkdir E2E 红→绿）。
	// 索引 key 相对 user 桶（去 user/ 前缀），与 upsert/List 索引空间一致。
	if s.index != nil {
		userRoot := "user"
		if tnt := s.rt.tenantOf(owner); tnt != nil {
			userRoot = tnt.UserRoot()
		}
		s.index.upsertDir(owner, strings.TrimPrefix(rel, userRoot+"/"))
	}
	s.rt.logger().Info("目录已创建", "dir", remotePath)
	// 文件变更事件：mkdir 成功。
	s.rt.publishFileEvent(EventMkdir, owner, rel, 0)
	return MakeDirResult{RemotePath: remotePath}, nil
}

// RemoveDirResult 是删目录的领域结果。
type RemoveDirResult struct {
	// RemotePath 是校验后的用户可见相对路径。
	RemotePath string
}

// RemoveDir 递归删除目录树（owner 视图内**每一个**存在该目录的卷都删）。
//
// 多卷语义：同一子目录可能因换卷在多个卷上并存（目录非文件，不受 AD-4 唯一性约束），
// 故逐卷定位、逐卷删除；文件级配额按 per-file rel 释放（跨卷合计正确），卷容量池按卷释放。
//
// 错误语义（*HTTPError）：
//   - 400：dirname 为空 / 路径非法 / 租户不可用 / 目标不是目录 / 目标为符号链接 / 未带 force
//   - 404：目录不存在（任何卷上都没有）
//   - 500：访问目录失败 / 删除失败
func (s *Service) RemoveDir(owner, dirname string, force bool) (RemoveDirResult, error) {
	if dirname == "" {
		return RemoveDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: "dirname 不能为空"}
	}
	remotePath, err := pathguard.ValidateFilePath(dirname)
	if err != nil {
		return RemoveDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: "无效的目录名: " + err.Error()}
	}
	// 归一 owner（空 → anonymous）：目录探测/删除与列表/写路径同键，未认证请求归属 anonymous。
	owner = normalizeOwner(owner)
	// 路径映射与卷无关，用默认租户做纯路径校验；卷感知只决定目录落到哪些卷的租户根。
	tnt0 := s.rt.tenantOf(owner)
	if tnt0 == nil || tnt0.Root() == nil {
		return RemoveDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidDirPath}
	}
	rel, ok := tnt0.UserRel(remotePath)
	if !ok {
		return RemoveDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidDirPath}
	}

	// 收集 owner 视图内存在该目录的卷租户（默认卷优先；目录不存在于任何卷 → 404）。
	targets := s.collectRmdirTargets(owner, rel, tnt0)
	if len(targets) == 0 {
		return RemoveDirResult{}, &HTTPError{Status: http.StatusNotFound, Message: "目录不存在"}
	}

	// 符号链接 / 非目录检查与 TOCTOU 二次检查：在首个命中卷（默认卷优先）执行，错误语义与
	// 单卷一致（目录不存在 404 / 符号链接或非目录 400）。
	primary := targets[0]
	if pErr := rmdirPrimaryCheck(primary.tnt.Root(), rel); pErr != nil {
		return RemoveDirResult{}, pErr
	}

	// force 必须为 true 才执行删除（避免误删）
	if !force {
		return RemoveDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: "请使用 ?force=true 确认删除"}
	}

	// 逐卷删除存在该目录的子树；每卷删除前收集树内文件 {rel,size}（per-file 分键释放 owner
	// 全局 Scope——跨卷合计语义正确，文件 rel 唯一故不双计），并按所在卷释放卷容量池。
	var allFiles []rmdirFileStat
	for _, tg := range targets {
		dirFiles, rmErr := s.removeDirOnVolume(tg, rel, remotePath)
		if rmErr != nil {
			return RemoveDirResult{}, rmErr
		}
		allFiles = append(allFiles, dirFiles...)
	}

	// 删除成功后收尾：per-file 配额释放 + checksum 台账子树清理 + 搜索索引子树删除
	// （语义见 cleanupRemovedDir）。
	s.cleanupRemovedDir(owner, rel, allFiles)

	s.rt.logger().Info("目录已删除", "dir", remotePath)
	// 文件变更事件：rmdir 成功。
	s.rt.publishFileEvent(EventRmdir, owner, rel, 0)
	return RemoveDirResult{RemotePath: remotePath}, nil
}

// cleanupRemovedDir 目录删除成功后的收尾：按各文件实际子 Scope（按 rel 解析）释放配额
// 占用（per-file 分键释放 owner 全局 Scope，跨卷合计语义正确）；清理 per-tenant checksum
// store 中该目录下所有文件的记录（key = rel，无 owner 前缀，"/" 分隔符与 ChecksumStore
// 约定一致）；搜索索引同步删除子树（rmdir 后子树不可见）。
func (s *Service) cleanupRemovedDir(owner, rel string, allFiles []rmdirFileStat) {
	for _, f := range allFiles {
		if scope := s.rt.quotaScope(owner, f.rel); scope != nil {
			scope.ReleaseUsage(f.size)
		}
	}
	if cs := s.rt.checksumStore(owner); cs != nil {
		cs.DeletePrefix(rel + "/")
		// 清理目录自身的 checksum 记录（如果存在）
		cs.Delete(rel)
	}
	// 搜索索引同步删除子树（roadmap P0）：rmdir 后子树不可见。
	if s.index != nil {
		s.index.removePrefix(owner, strings.TrimPrefix(rel, "user/"))
	}
}

// rmTarget 是删除目录的目标卷（卷名 + 该卷上 owner 租户）。
type rmTarget struct {
	volName string
	tnt     *storage.Tenant
}

// collectRmdirTargets 收集 owner 视图内存在该目录的卷租户（默认卷优先）；目录在
// 任何卷上都不存在 → 返回空（调用方按 404）。
func (s *Service) collectRmdirTargets(owner, rel string, tnt0 *storage.Tenant) []rmTarget {
	var targets []rmTarget
	if s.rt.volSet() == nil {
		targets = append(targets, rmTarget{volName: "", tnt: tnt0})
	} else {
		view := volume.AllowedVolumes(s.rt.volSet().All(), owner)
		for _, v := range view {
			rt := s.rt.volSet().Root(v.Name)
			if rt == nil {
				continue
			}
			// 探测用卷根相对 <owner>/<rel>（不创建租户目录）；确认存在再取租户操作。
			if _, statErr := rt.Stat(owner + "/" + rel); statErr != nil {
				continue
			}
			tnt := s.rt.volumeTenant(v.Name, owner)
			if tnt == nil || tnt.Root() == nil {
				continue
			}
			targets = append(targets, rmTarget{volName: v.Name, tnt: tnt})
		}
	}
	return targets
}

// rmdirPrimaryCheck 检查首个命中卷（默认卷优先）的目标目录：目录不存在 404 /
// 符号链接 400 / 非目录 400 / 其它 500（TOCTOU 二次检查，错误语义与单卷一致）。
func rmdirPrimaryCheck(root *storage.Root, rel string) error {
	if vErr := validateRmdirTarget(root, rel); vErr != nil {
		switch {
		case os.IsNotExist(vErr):
			return &HTTPError{Status: http.StatusNotFound, Message: "目录不存在"}
		case errors.Is(vErr, errRmdirSymlink):
			return &HTTPError{Status: http.StatusBadRequest, Message: "不允许删除符号链接"}
		case errors.Is(vErr, errRmdirNotDir):
			return &HTTPError{Status: http.StatusBadRequest, Message: "指定路径不是目录"}
		default:
			return &HTTPError{Status: http.StatusInternalServerError, Message: "访问目录失败"}
		}
	}
	return nil
}

// removeDirOnVolume 删除单个卷上的目录子树并返回树内文件 {rel,size}（per-file 分键释放
// owner 全局 Scope 用）；同时按所在卷释放卷容量池。
func (s *Service) removeDirOnVolume(tg rmTarget, rel, remotePath string) ([]rmdirFileStat, error) {
	root := tg.tnt.Root()
	var dirFiles []rmdirFileStat
	sumRootDirFiles(root, rel, &dirFiles)
	if rmErr := root.RemoveAll(rel); rmErr != nil {
		s.rt.logger().Error("删除目录失败", "dir", remotePath, "error", rmErr)
		return nil, &HTTPError{Status: http.StatusInternalServerError, Message: "删除目录失败"}
	}
	// 卷容量池释放：本卷被删字节 = dirFiles 之和。
	if tg.volName != "" && s.rt.volSet() != nil {
		if pool := s.rt.volSet().Pool(tg.volName); pool != nil {
			var volBytes int64
			for _, f := range dirFiles {
				volBytes += f.size
			}
			if volBytes > 0 {
				pool.ReleaseCommitted(volBytes)
			}
		}
	}
	return dirFiles, nil
}

// ---- 重命名族域操作（rename）----
//
// 命名说明：域操作 RenameFile，HTTP 处理器 Rename（批量族 BatchRename 在 P2-c 收敛到本方法）。

// RenameFileInput 是重命名的领域入参。**不含任何 HTTP 类型**。
type RenameFileInput struct {
	// Owner 是操作主体（空 → anonymous，本方法内归一）。
	Owner string
	// From / To 是用户可见相对路径（原始值；本方法内校验，非法 → 400）。
	From string
	To   string
	// ExpectedChecksum 是客户端声明的源文件 SHA-256（**必填**，不符即拒绝）。
	ExpectedChecksum string
	// ExplicitVol 是 `?volume=` 显式卷（空 = 按 owner 卷视图定位源）。
	ExplicitVol string
	// Origin 是调用来源标记（"" = 单条 API；auditOriginBatch = 批量族），只影响审计行的
	// 来源标记，不改变任何响应语义。
	Origin string
}

// RenameFileResult 是重命名的领域结果。
type RenameFileResult struct {
	// From / To 是**校验后**的用户可见路径（调用方据此组文案）。
	From string
	To   string
	// Checksum 是成功时回显的客户端 checksum；「同源同目标」分支留空（历史契约：该分支不携带 checksum）。
	Checksum string
	// Message 是面向客户端的成功文案（由域侧给出，两条成功路径文案不同）。
	Message string
	// NoOp 表示「源与目标相同，无需移动」的短路成功：此时 Message 即历史专属文案，
	// 调用方应**原样透传**（批量族也不再改写为「重命名成功」）。
	NoOp bool
}

// RenameFile 在源文件所在 home 卷内完成重命名（同卷移动；跨卷移动走 move API）。
//
// 执行顺序（与既有处理器逐字一致，勿重排——顺序本身是不变量）：
//  1. 路径校验（空 from/to → 400；pathguard 非法 → 400）
//  2. 同源同目标 → 直接成功（**在校验 checksum 之前**短路，且不回显 checksum）
//  3. checksum 头缺失 → 400
//  4. 租户 / UserRel 解析失败 → 400
//  5. 跨卷定位 home：显式卷未命中 / 默认卷被 ACL 排除 → 404（fail-closed，AD-6）
//  6. AD-4 唯一性：目标 rel 已在其它卷存在 → 409
//  7. renameInHome：源存在 → 目标不存在 → checksum 门禁 → 建父目录 → 配额转移 → 原子改名
//
// 失败一律以 *HTTPError 表达（状态码与文案即对外契约）。
func (s *Service) RenameFile(ctx context.Context, input RenameFileInput) (RenameFileResult, error) {
	logger := s.rt.logger()

	from, to, err := validateRenamePaths(input.From, input.To)
	if err != nil {
		return RenameFileResult{}, err
	}
	if from == to {
		return RenameFileResult{From: from, To: to, Message: "源与目标相同，无需移动", NoOp: true}, nil
	}
	if input.ExpectedChecksum == "" {
		return RenameFileResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgMissingChecksum, Reason: reasonChecksumMissing}
	}

	owner := normalizeOwner(input.Owner)
	fromRel, toRel, tnt, ok := s.resolveRenamePaths(owner, from, to)
	if !ok || tnt == nil || tnt.Root() == nil {
		return RenameFileResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath, Reason: reasonPathInvalid}
	}

	// 文件级互斥（与单次上传 / 删除 / 跨卷 move / 分块 complete 共用同一 rel 锁池）：
	// 「目标不存在检查 → 原子改名」是两步，中间是 TOCTOU 窗口——并发写入者可在窗口内创建
	// 目标，随后 `Rename` 会**静默覆盖**它（POSIX 替换语义）⇒ 数据丢失，且「目标路径已存在」
	// 的 409 门禁被绕过。同理源侧被持锁（该路径正在上传）时改名会搬走半成品。
	// 两把锁均**非阻塞**取（Acquire）⇒ 与其它单锁路径无死锁风险；归一化后 from/to 键相同
	// 时只取一把（避免自冲突）。
	release, locked := s.acquireRenameLocks(ctx, logger, owner, fromRel, toRel, from, to)
	if !locked {
		return RenameFileResult{}, &HTTPError{Status: http.StatusConflict, Message: errMsgRenameBusy}
	}
	defer release()

	// 跨卷定位源 home（任务 5）：文件可能因换卷落在非默认卷，rename 在 home 卷内完成
	// （同卷；跨卷移动走 T6 move API）。带显式 ?volume= 只在指定卷定位源（不在 → 404）。
	// 全视图未命中仅当默认卷对 owner 授权才回落默认租户；默认卷被 ACL 排除时不得回落
	// （ACL bypass，AD-6）。语义见 resolveRenameHome。
	homeVol, root, locErr := s.resolveRenameHome(ctx, owner, fromRel, input.ExplicitVol, tnt)
	if locErr != nil {
		return RenameFileResult{}, locErr
	}

	// AD-4 唯一性：目标 rel 不得已存在于 owner 视图其它卷（否则 rename 后同逻辑路径跨卷
	// 双份）。目标已在同一 home 卷由 renameInHome 的 Stat 捕获（409）；目标在其它卷 →
	// 直接 409（跨卷移动非本任务语义）。单卷/无卷语义时 homeVol 空 → 跳过。
	if dstErr := s.rejectRenameDestOnOtherVolume(owner, toRel, homeVol); dstErr != nil {
		return RenameFileResult{}, dstErr
	}

	if renErr := s.renameInHome(ctx, renameHomeArgs{
		owner:            owner,
		root:             root,
		fromRel:          fromRel,
		toRel:            toRel,
		from:             from,
		to:               to,
		expectedChecksum: input.ExpectedChecksum,
		origin:           input.Origin,
		logger:           logger,
	}); renErr != nil {
		return RenameFileResult{}, renErr
	}

	s.rt.recordFileAudit(ctx, "rename", from, auditResultSuccess, "to="+to)
	logger.InfoContext(ctx, "文件已重命名", "from", from, "to", to, "checksum", input.ExpectedChecksum)
	// 文件变更事件：rename 成功（rel = to 目标路径；from 由事件内容表达）。
	s.rt.publishFileEvent(EventRename, normalizeOwner(input.Owner), to, 0)
	return RenameFileResult{
		From:     from,
		To:       to,
		Checksum: input.ExpectedChecksum,
		Message:  fmt.Sprintf("文件已重命名: %s -> %s", from, to),
	}, nil
}

// validateRenamePaths 校验 from/to 并返回**校验后**的路径（失败以 *HTTPError{400} 表达）。
// 三类文案与既有 parseRenameParams 逐字一致。
func validateRenamePaths(from, to string) (string, string, error) {
	if from == "" || to == "" {
		return "", "", &HTTPError{Status: http.StatusBadRequest, Message: "from 和 to 都不能为空"}
	}
	vFrom, err := pathguard.ValidateFilePath(from)
	if err != nil {
		return "", "", &HTTPError{Status: http.StatusBadRequest, Message: "无效的源路径"}
	}
	vTo, err := pathguard.ValidateFilePath(to)
	if err != nil {
		return "", "", &HTTPError{Status: http.StatusBadRequest, Message: "无效的目标路径"}
	}
	return vFrom, vTo, nil
}

// resolveRenamePaths 计算 from 和 to 在指定 owner 租户 user 桶下的相对路径。
// from 与 to 必须落在同一租户内（UserRel 保证 user/ 桶内）。返回租户与两条 rel。
func (s *Service) resolveRenamePaths(owner, from, to string) (fromRel, toRel string, tnt *storage.Tenant, ok bool) {
	tnt = s.rt.tenantOf(owner)
	if tnt == nil {
		return "", "", nil, false
	}
	var fok, tok bool
	fromRel, fok = tnt.UserRel(from)
	toRel, tok = tnt.UserRel(to)
	if !fok || !tok {
		return "", "", nil, false
	}
	return fromRel, toRel, tnt, true
}

// acquireRenameLocks 对 from/to 两个 rel 非阻塞取文件级互斥（归一化后相同时只取一把），
// 返回合并 release 与 locked。任一冲突已记日志并返回 (nil, false)——调用方按 409 处理。
// 释放顺序与既有双 defer 一致（先 to 后 from，LIFO）。
func (s *Service) acquireRenameLocks(ctx context.Context, logger *slog.Logger, owner, fromRel, toRel, from, to string) (func(), bool) {
	releaseFrom, locked := s.rt.fileLocks().Acquire(owner, fromRel)
	if !locked {
		logger.WarnContext(ctx, "文件正在移动/上传中，拒绝重命名", "from", from, "to", to)
		return nil, false
	}
	if toRel != fromRel {
		releaseTo, toLocked := s.rt.fileLocks().Acquire(owner, toRel)
		if !toLocked {
			releaseFrom()
			logger.WarnContext(ctx, "目标路径正在移动/上传中，拒绝重命名", "from", from, "to", to)
			return nil, false
		}
		return func() {
			releaseTo()
			releaseFrom()
		}, true
	}
	return releaseFrom, true
}

// resolveRenameHome 跨卷定位源 home 卷根（RenameFile 步骤 5）：locateForRead 命中 → 该卷
// 租户根；显式卷未命中 / 默认卷被 ACL 排除 → 404（fail-closed，AD-6）；否则回落默认租户。
func (s *Service) resolveRenameHome(ctx context.Context, owner, fromRel, explicitVol string, tnt *storage.Tenant) (string, *storage.Root, error) {
	loc, found := s.locateForRead(owner, fromRel, explicitVol)
	switch {
	case found && loc.Tenant != nil:
		return loc.VolumeName, loc.Tenant.Root(), nil
	case explicitVol != "":
		return "", nil, &HTTPError{Status: http.StatusNotFound, Message: errMsgSrcNotExist}
	case !s.defaultVolumeAllows(owner):
		return "", nil, &HTTPError{Status: http.StatusNotFound, Message: errMsgSrcNotExist}
	default:
		return "", tnt.Root(), nil
	}
}

// rejectRenameDestOnOtherVolume AD-4 唯一性：目标 rel 已在 owner 视图其它卷存在 → 409
// （目标已在同一 home 卷由 renameInHome 的 Stat 捕获）。单卷/无卷语义时 homeVol 空 → 跳过。
func (s *Service) rejectRenameDestOnOtherVolume(owner, toRel, homeVol string) error {
	if homeVol != "" && s.rt.volSet() != nil {
		if dstLoc, dstFound := s.rt.locateOwnerFile(owner, toRel); dstFound && dstLoc.VolumeName != homeVol {
			return &HTTPError{Status: http.StatusConflict, Message: errMsgDestExists}
		}
	}
	return nil
}

// renameHomeArgs 是 renameInHome 的参数集合（go:S107：参数过多时收敛为结构体）。
// 只放**领域**入参——不含 http.ResponseWriter（域方法不写响应，失败以 *HTTPError 表达）。
type renameHomeArgs struct {
	owner            string
	root             *storage.Root
	fromRel          string
	toRel            string
	from             string
	to               string
	expectedChecksum string
	// origin 是调用来源标记（"" = 单条 API；auditOriginBatch = 批量 API），**只**影响审计行
	// Detail 的来源标记，便于从审计里区分是哪一族触发的（历史行为：批量族带 (batch) 标记）。
	origin string
	logger *slog.Logger
}

// auditOriginBatch 是批量族的调用来源标记（审计 Detail 里显示为「（batch）」）。
const auditOriginBatch = "batch"

// renameAuditDetail 组装重命名族审计 Detail：`base` [+「（batch）」] [+「: to=<to>」]。
//
// P2-c 归一化（原先两族各写一套、信息量不一致）：统一为「<base>[（batch）]: to=<to>」——
// 单条与批量都能看出目标路径，且仍能区分来源。`to == ""` 时不追加目标段。
func renameAuditDetail(base, to, origin string) string {
	detail := base
	if origin == auditOriginBatch {
		detail += "（batch）"
	}
	if to != "" {
		detail += ": to=" + to
	}
	return detail
}

// renameInHome 在给定 home 卷租户内完成一次重命名（含跨子目录配额对称转移）。
//
// 返回 nil 表示成功；失败返回 *HTTPError（状态码与文案即对外契约，由调用方写响应）。
// 除响应外的副作用一律保留：审计、业务日志、checksum 台账改名、配额对称转移。
func (s *Service) renameInHome(ctx context.Context, a renameHomeArgs) error {
	a.logger.InfoContext(ctx, "开始重命名", "from", a.fromRel, "to", a.toRel)
	if _, err := a.root.Stat(a.fromRel); os.IsNotExist(err) {
		s.rt.recordFileAudit(ctx, "rename", a.from, auditResultError, errMsgSrcNotExist)
		return &HTTPError{Status: http.StatusNotFound, Message: errMsgSrcNotExist}
	}
	// 目标不存在检查（409 门禁）。**本 Stat 与下方 Rename 之间已不是 TOCTOU 窗口**：
	// 调用方 RenameFile 在进入本函数前已对 from/to 两个 rel 取锁（FileLocks，与上传/删除/
	// 分块 complete 共用锁池），同 rel 的并发操作会先在锁上 409；本检查即锁内的权威判定。
	if _, err := a.root.Stat(a.toRel); err == nil {
		s.rt.recordFileAudit(ctx, "rename", a.from, auditResultDenied, renameAuditDetail(errMsgDestExists, a.to, a.origin))
		// 审查 I-1：必须返回非 nil 错误——原 `return err`（err 恰为 nil）让调用方误判
		// 成功并追加一条假的 success 审计行（被拒绝的 rename 记为成功，破坏审计可信度）。
		return &HTTPError{Status: http.StatusConflict, Message: errMsgDestExists}
	}
	if !verifyFileWithChecksumRoot(a.root, a.fromRel, a.expectedChecksum) {
		s.rt.recordFileAudit(ctx, "rename", a.from, auditResultDenied, renameAuditDetail("checksum 不匹配", a.to, a.origin))
		a.logger.WarnContext(ctx, "rename checksum 校验失败", "from", a.from)
		return &HTTPError{Status: http.StatusBadRequest, Message: errMsgSrcChecksumFailed}
	}
	if err := a.root.MkdirAll(filepath.Dir(a.toRel), 0755); err != nil {
		s.rt.recordFileAudit(ctx, "rename", a.from, auditResultError, renameAuditDetail("创建父目录失败", a.to, a.origin))
		a.logger.ErrorContext(ctx, errMsgCreateParentDirFailed, "to", a.to, "error", err.Error())
		return &HTTPError{Status: http.StatusInternalServerError, Message: errMsgCreateParentDirFailed, Reason: reasonMkdirFailed}
	}
	// 配额：rename 在 user 桶内移动字节（总量不变，桶/租户级天然正确）。但跨 bucket_limits
	// 子目录时 committed 归属需对称转移——源目录键释放、目标目录键入账（子目录配额对 rename
	// 同样封顶，防止"先传受限目录外再 rename 进来"绕过）。非跨键（同目录/同键）零操作。
	// 两键相同时无需记账（释放+入账互相抵消）；装配层配额未装配（QuotaScopeFor 返回 nil）
	// 时退化为无配额记账（旧行为，仅总量正确）。
	if handled, err := s.renameWithQuotaTransfer(ctx, a); handled {
		return err
	}
	// 无配额记账路径（同键/未装配配额/源 stat 失败）：直接原子改名 + 台账/索引收尾。
	if err := atomicRenameRoot(a.root, a.fromRel, a.toRel); err != nil {
		s.rt.recordFileAudit(ctx, "rename", a.from, auditResultError, renameAuditDetail("重命名失败", a.to, a.origin))
		a.logger.ErrorContext(ctx, "重命名失败", "from", a.from, "to", a.to, "error", err.Error())
		return &HTTPError{Status: http.StatusInternalServerError, Message: "重命名失败"}
	}
	s.renameChecksumIndex(a)
	return nil
}

// renameWithQuotaTransfer 跨子目录配额对称转移路径的重命名（renameInHome 子集）：
// 源/目标 Scope 均存在且不同时，先 TryReserve 目标配额 → 原子 Rename → 目标入账 +
// 源释放。返回 (handled, err)：非跨键/未装配配额/源 stat 失败时返回 handled=false，
// 调用方走无配额原链路；handled=true 表示配额路径已完成（err 即结果错误）。
func (s *Service) renameWithQuotaTransfer(ctx context.Context, a renameHomeArgs) (bool, error) {
	fromScope := s.rt.quotaScope(a.owner, a.fromRel)
	if fromScope == nil {
		return false, nil
	}
	toScope := s.rt.quotaScope(a.owner, a.toRel)
	if toScope == nil || fromScope == toScope {
		return false, nil
	}
	srcInfo, statErr := a.root.Stat(a.fromRel)
	if statErr != nil {
		return false, nil // 源文件 stat 失败（理论不可达）：不记账，继续原链路。
	}
	size := srcInfo.Size()
	// 目标键先 TryReserve（子目录/租户/全局逐级检查，配额不足拒绝移动避免超限），
	// 成功后再原子 Rename，最后源键 ReleaseUsage。若 Rename 失败则 Release 归还目标预留。
	toRes, err := toScope.TryReserve(size)
	if err != nil {
		s.rt.recordFileAudit(ctx, "rename", a.from, auditResultDenied, renameAuditDetail("目标目录配额不足", a.to, a.origin))
		a.logger.WarnContext(ctx, "rename 目标目录配额不足", "from", a.fromRel, "to", a.toRel, "size", size)
		return true, &HTTPError{Status: http.StatusInsufficientStorage, Message: "目标目录配额不足"}
	}
	if err := atomicRenameRoot(a.root, a.fromRel, a.toRel); err != nil {
		toRes.Release() // Rename 失败归还目标预留（源键未动）。
		s.rt.recordFileAudit(ctx, "rename", a.from, auditResultError, renameAuditDetail("重命名失败", a.to, a.origin))
		a.logger.ErrorContext(ctx, "重命名失败", "from", a.from, "to", a.to, "error", err.Error())
		return true, &HTTPError{Status: http.StatusInternalServerError, Message: "重命名失败"}
	}
	toRes.Commit(size)
	fromScope.ReleaseUsage(size)
	s.renameChecksumIndex(a)
	return true, nil
}

// renameChecksumIndex 重命名成功后的台账与索引改名（checksum store + 搜索索引）。
func (s *Service) renameChecksumIndex(a renameHomeArgs) {
	if cs := s.rt.checksumStore(a.owner); cs != nil {
		cs.Rename(a.fromRel, a.toRel)
	}
	if s.index != nil {
		s.index.rename(a.owner, strings.TrimPrefix(a.fromRel, "user/"), strings.TrimPrefix(a.toRel, "user/"))
	}
	// 可信卷：重命名主文件联动移动配套 .meta（服务端内部文件随主文件一起动）。
	if s.rt.fileMetaEnabled() {
		if a.root != nil {
			_ = a.root.Rename(meta.MetaPath(a.fromRel), meta.MetaPath(a.toRel))
		}
	}
}

// ---- 删除族域操作（delete）----

// DeleteFileInput 是单文件删除的领域入参。**不含任何 HTTP 类型**。
//
// 注意与 `RemoveDir` 的分工：本方法删**单个文件**（checksum 门禁 + 幂等由调用方决定语义），
// `RemoveDir` 删**目录子树**（无 checksum，逐卷删除）。
type DeleteFileInput struct {
	// Owner 是操作主体（空 → anonymous，本方法内归一）。
	Owner string
	// RemotePath 是用户可见相对路径（原始值；本方法内校验，非法 → 400）。
	RemotePath string
	// ExpectedChecksum 是客户端声明的 SHA-256（**必填**且必须匹配，不符即拒绝删除）。
	ExpectedChecksum string
	// ExplicitVol 是 `?volume=` 显式卷（空 = 按 owner 卷视图定位）。
	ExplicitVol string
	// AllowMissing 为 true 时「文件不存在（含默认卷被 ACL 排除而对 owner 不可见）」按**幂等
	// 成功**返回（Idempotent=true），false 时返回 404。批量族用 true（重放安全），单条 API 用 false。
	AllowMissing bool
	// SoftDelete 为 true 时软删：checksum 校验成功后移到 trash 桶（回收站，roadmap P2）
	// 而非删除；不释放配额（可恢复）。默认 false 零回归。
	SoftDelete bool
	// SkipFileLock 为 true 时**不取**文件级互斥。**只**给批量族用：批量语义是「逐条立即给结果、
	// 不因并发上传把整批变成 409」（历史行为，逐字保留；单条 API 必须留 false）。
	// 现状（P2-c 审计结论，2026-09-14）：**rename 族**已统一取锁——RenameFile 对 from/to
	// 两个 rel 非阻塞取锁，冲突即该条 409，批量重命名按逐条结果聚合，无需新语义；删除族仍保留本
	// 开关（批量删除继续"逐条立即给结果"）。若将来批量删除也要统一取锁，须先定 409 聚合语义。
	SkipFileLock bool
}

// DeleteFileResult 是单文件删除的领域结果。
type DeleteFileResult struct {
	// RemotePath 是**校验后**的用户可见路径。
	RemotePath string
	// Message 是面向客户端的成功文案（由域侧给出，避免调用方各自拼文案）。
	Message string
	// Idempotent 表示「文件不存在、按幂等成功返回」（仅 AllowMissing=true 时可能为 true）。
	Idempotent bool
}

// DeleteFile 删除单个文件（含 checksum 门禁、文件级互斥、跨卷定位、配额/卷池/台账释放）。
//
// 执行顺序（与既有处理器逐字一致，勿重排——顺序本身是不变量）：
//  1. 路径校验（空 / pathguard / UserRel）→ 400
//  2. checksum 头缺失 → 400（**触盘前**拒绝）
//  3. 文件级互斥（与上传/move/restore/分块 complete 共用同 rel 锁）→ 409
//  4. 跨卷定位 home 卷 → 404（显式卷未命中 / 默认卷被 ACL 排除，fail-closed）
//  5. 开 fd → Stat → checksum 校验（不符 400，**保留文件**）→ 关闭 → 删除
//  6. 释放配额占用 + 卷容量池 + checksum 台账 + 计量 + 审计
//
// 失败一律以 *HTTPError 表达（状态码与文案即对外契约）。
func (s *Service) DeleteFile(ctx context.Context, input DeleteFileInput) (DeleteFileResult, error) {
	logger := s.rt.logger()

	filename := input.RemotePath
	if filename == "" {
		return DeleteFileResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgEmptyFilename}
	}
	// owner 归一后再解析租户：与 rename / mkdir 同一键（空 owner → anonymous）。
	owner := normalizeOwner(input.Owner)
	remotePath, vErr := pathguard.ValidateFilePath(filename)
	if vErr != nil {
		return DeleteFileResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidFilename, Reason: reasonPathInvalid}
	}
	tnt0 := s.rt.tenantOf(owner)
	if tnt0 == nil {
		return DeleteFileResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidFilename, Reason: reasonPathInvalid}
	}
	rel, relOK := tnt0.UserRel(remotePath)
	if !relOK {
		return DeleteFileResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidFilename, Reason: reasonPathInvalid}
	}

	expectedChecksum := input.ExpectedChecksum
	if expectedChecksum == "" {
		logger.WarnContext(ctx, "X-File-Checksum 为空", "file_name", remotePath)
		return DeleteFileResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgMissingChecksum, Reason: reasonChecksumMissing}
	}

	// 跨卷定位（任务 5）：文件可能因换卷落在非默认卷。带显式 ?volume= 只在指定卷定位
	// （不在视图/卷上无此文件 → 404，fail-closed）。全视图未命中仅当默认卷对 owner 授权才
	// 回落默认租户（由 Open 产出 404/500，与单卷既有错误语义一致）；默认卷被 ACL 排除时不得
	// 回落——否则 owner 可经默认租户 Open 删除默认卷自身路径的遗留文件（ACL bypass，AD-6）。

	// 文件级互斥（T6c move 锁架构延伸）：与单次上传 / 跨卷 move / 版本 restore / 分块 complete
	// 共用同 rel 锁。无锁时 delete 可在 move「复制成功 → 删源」窗口内先删源（move 侧虽有
	// IsNotExist 兜底，但语义依赖时序）；持锁后并发 move 直接 409，窗口闭合。
	if !input.SkipFileLock {
		release, locked := s.acquireDeleteLock(ctx, owner, rel, remotePath)
		if !locked {
			return DeleteFileResult{}, &HTTPError{Status: http.StatusConflict, Message: "文件正在移动/上传中，请稍后重试"}
		}
		defer release()
	}

	homeVol, root, handled, res, locErr := s.locateDeleteTarget(ctx, owner, rel, input)
	if handled {
		return res, locErr
	}

	// ---- TOCTOU 加固（2026-09-17）：rename-to-quarantine ----
	// 原实现「基于 fd 校验 checksum → 关闭 → root.Remove(rel)」存在窗口：校验与删除之间
	// 并发写者可把 rel 替换为新文件，删除会**静默作用于替换后的新对象**（校验的是旧对象）。
	// 现改为：先把 rel 原子重命名到独立中间路径（rel + ".deleting.<nano>"），校验 quarantine
	// 内容匹配才删除——窗口内的路径替换只影响原 rel，不影响被校验/被删除的对象。
	// 与 rename 面 #259 的 quarantine 手法同源（先锁定路径归属，再校验，再动手）。
	//
	// 选择依据：atomicRenameRoot 是替换语义且重试有界（storage.Rename），quarantine 名带
	// 纳秒时间戳与同 rel 并发删除者天然隔离（两者都会先 rename，后到者 rename 失败返回错误）。
	quarRel, handled, res, qErr := s.renameToDeleteQuarantine(ctx, root, rel, remotePath, input, logger)
	if handled {
		return res, qErr
	}

	// 测试接缝（仅 TOCTOU 用例注入；生产恒 nil）。hook 内按需重算路径。
	if s.deleteBeforeRemoveHook != nil {
		s.deleteBeforeRemoveHook()
	}

	// 基于 fd 校验 quarantine 内容（打开失败 → 恢复 rel 并 500）。
	info, cs, qErr := s.verifyDeleteQuarantine(ctx, root, quarRel, rel, expectedChecksum, remotePath)
	if qErr != nil {
		return DeleteFileResult{}, qErr
	}

	// checksum 匹配：删除 quarantine（原子；此时原 rel 已被并发写者占据也不受影响）。
	// 内容寻址去重（dedup.enabled）：先摘除引用计数——还有其它引用时只摘引用（inode 保留，
	// 配额不减），引用归零才真正删除 inode + 释放配额。
	f := &fileOp{ctx: ctx, root: root, owner: owner, remotePath: remotePath, logger: logger}
	return s.deleteQuarantinedFile(f, homeVol, rel, quarRel, info, cs, input)
}

// acquireDeleteLock 对删除目标 rel 非阻塞取文件级互斥（与单次上传 / 跨卷 move / 版本
// restore / 分块 complete 共用同 rel 锁）。已记录冲突审计；返回 release 与 locked。
func (s *Service) acquireDeleteLock(ctx context.Context, owner, rel, remotePath string) (func(), bool) {
	release, locked := s.rt.fileLocks().Acquire(owner, rel)
	if !locked {
		s.rt.recordFileAudit(ctx, "delete", remotePath, auditResultDenied, "文件正在移动/上传中")
		return nil, false
	}
	return release, true
}

// renameToDeleteQuarantine 把 rel 原子重命名到独立 quarantine 路径（TOCTOU 加固：
// 先锁定路径归属，再校验，再动手）。原 rel 不存在时按 AllowMissing 幂等成功 / 404；
// rename 其它失败 500。返回 (quarRel, handled, result, err)——handled=true 表示已产出
// 最终结果（调用方直接返回）。
func (s *Service) renameToDeleteQuarantine(ctx context.Context, root *storage.Root, rel, remotePath string, input DeleteFileInput, logger *slog.Logger) (string, bool, DeleteFileResult, error) {
	quarRel := rel + ".deleting." + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := atomicRenameRoot(root, rel, quarRel); err != nil {
		// errors.Is 而非 os.IsNotExist：atomicRenameRoot 返回的是 fmt.Errorf 包装错误，
		// os.IsNotExist 不解包 %w 链（实测对包装错误恒 false），必须用 errors.Is。
		if errors.Is(err, os.ErrNotExist) {
			if input.AllowMissing {
				res, missErr := s.idempotentMissingDelete(ctx, remotePath)
				return "", true, res, missErr
			}
			s.rt.recordFileAudit(ctx, "delete", remotePath, auditResultError, "文件不存在")
			return "", true, DeleteFileResult{}, &HTTPError{Status: http.StatusNotFound, Message: "文件不存在"}
		}
		s.rt.recordFileAudit(ctx, "delete", remotePath, auditResultError, errMsgDeleteFile)
		logger.ErrorContext(ctx, errMsgDeleteFile, "file_name", remotePath, "error", err.Error())
		return "", true, DeleteFileResult{}, &HTTPError{Status: http.StatusInternalServerError, Message: errMsgDeleteFile, Reason: reasonRemoveFailed}
	}
	return quarRel, false, DeleteFileResult{}, nil
}

// idempotentMissingDelete 返回「文件不存在」的**幂等成功**结果（批量族语义），并留审计行。
//
// 批量族的历史语义：删一个不存在的文件视为成功（重放安全），且**不**计删除计量（无实际删除）。
// 留审计行是 P2-c 的补充：原先批量族在这条路径上完全不写审计（单条族会写 error 行）⇒ 留白。
func (s *Service) idempotentMissingDelete(ctx context.Context, remotePath string) (DeleteFileResult, error) {
	s.rt.logger().WarnContext(ctx, "删除：文件不存在（幂等删除）", "file_name", remotePath)
	s.rt.recordFileAudit(ctx, "delete", remotePath, auditResultSuccess, "文件不存在（幂等删除）")
	return DeleteFileResult{RemotePath: remotePath, Message: "文件不存在（幂等删除）", Idempotent: true}, nil
}

// locateDeleteTarget 跨卷定位删除目标（任务 5）：locateForRead 命中 → 用该卷租户根；
// 未命中时仅当默认卷对 owner 授权才回落默认租户（显式卷未命中 / 默认卷被 ACL 排除 →
// AllowMissing ? 幂等成功 : 404，fail-closed，AD-6）。返回 handled=true 表示已产出结果。
func (s *Service) locateDeleteTarget(ctx context.Context, owner, rel string, input DeleteFileInput) (homeVol string, root *storage.Root, handled bool, result DeleteFileResult, err error) {
	loc, found := s.locateForRead(owner, rel, input.ExplicitVol)
	if found && loc.Tenant != nil {
		return loc.VolumeName, loc.Tenant.Root(), false, DeleteFileResult{}, nil
	}
	// 未命中（locate 失败/未在视图）时：与旧实现一致，只有默认卷在 owner 视图内才回落
	// 默认租户（locate 的 stat 失败 → 由后续 rename 的 IsNotExist 裁决幂等/404）。
	if input.ExplicitVol != "" {
		if input.AllowMissing {
			res, err := s.idempotentMissingDelete(ctx, input.RemotePath)
			return "", nil, true, res, err
		}
		return "", nil, true, DeleteFileResult{}, &HTTPError{Status: http.StatusNotFound, Message: "文件不存在"}
	}
	if !s.defaultVolumeAllows(owner) {
		if input.AllowMissing {
			res, err := s.idempotentMissingDelete(ctx, input.RemotePath)
			return "", nil, true, res, err
		}
		return "", nil, true, DeleteFileResult{}, &HTTPError{Status: http.StatusNotFound, Message: "文件不存在"}
	}
	tnt := s.rt.tenantOf(owner)
	if tnt == nil || tnt.Root() == nil {
		return "", nil, true, DeleteFileResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}
	return "", tnt.Root(), false, DeleteFileResult{}, nil
}

// verifyDeleteQuarantine 基于 fd 校验 quarantine 内容（打开失败 → 恢复 rel 并 500）。
// 返回文件元信息与实际 checksum；任一失败已尽力恢复原 rel 并返回 *HTTPError。
func (s *Service) verifyDeleteQuarantine(ctx context.Context, root *storage.Root, quarRel, rel, expectedChecksum, remotePath string) (os.FileInfo, string, error) {
	qf, qErr := root.Open(quarRel)
	if qErr != nil {
		_ = atomicRenameRoot(root, quarRel, rel) // 尽力恢复：quarantine 已在手，rel 必可回写
		s.rt.recordFileAudit(ctx, "delete", remotePath, auditResultError, errMsgOpenFile)
		s.rt.logger().ErrorContext(ctx, errMsgOpenFile, "file_name", remotePath, "error", qErr.Error())
		return nil, "", &HTTPError{Status: http.StatusInternalServerError, Message: errMsgOpenFile}
	}
	info, qErr := qf.Stat()
	if qErr != nil {
		qf.Close()
		_ = atomicRenameRoot(root, quarRel, rel)
		s.rt.recordFileAudit(ctx, "delete", remotePath, auditResultError, "stat 失败")
		s.rt.logger().ErrorContext(ctx, "stat 文件失败", "file_name", remotePath, "error", qErr.Error())
		return nil, "", &HTTPError{Status: http.StatusInternalServerError, Message: "stat 失败"}
	}
	cs, qErr := checksumReader(qf)
	qf.Close()
	if qErr != nil {
		_ = atomicRenameRoot(root, quarRel, rel)
		s.rt.recordFileAudit(ctx, "delete", remotePath, auditResultError, "计算 checksum 失败")
		s.rt.logger().ErrorContext(ctx, "计算文件 checksum 失败", "file_name", remotePath, "error", qErr.Error())
		return nil, "", &HTTPError{Status: http.StatusInternalServerError, Message: errMsgFileChecksum}
	}
	if !checksum.Equal(cs, expectedChecksum) {
		// 恢复原路径：用户数据必须保留（不丢不删），并返回拒绝。
		_ = atomicRenameRoot(root, quarRel, rel)
		s.rt.recordFileAudit(ctx, "delete", remotePath, auditResultDenied, "checksum 不匹配")
		s.rt.logger().WarnContext(ctx, errMsgFileChecksum, "file_name", remotePath)
		return nil, "", &HTTPError{Status: http.StatusBadRequest, Message: errMsgFileChecksum}
	}
	return info, cs, nil
}

// deleteQuarantinedFile 处理已通过 checksum 校验的 quarantine 文件：内容寻址去重先摘除
// 引用计数（仍有其它引用时只 unlink，配额不减）；引用归零 → 软删到回收站 / 硬删 +
// 配额与卷池释放；再统一收尾（checksum 台账 / 索引 / 计量 / 审计 / 事件）。
func (s *Service) deleteQuarantinedFile(f *fileOp, homeVol, rel, quarRel string, info os.FileInfo, cs string, input DeleteFileInput) (DeleteFileResult, error) {
	// 可信卷：**删除成功后**联动删除配套 .meta（C-MINOR-3 修复：主文件删除失败/软删
	// 时 meta 保留——先在成功路径统一删；此处只删当次 rel 的 sidecar，防止软删可恢复
	// 场景丢失校验凭证）。
	refCount := 0
	if ds := s.rt.dedupStore(f.owner); s.rt.dedupEnabled() && ds != nil {
		refCount = ds.RemoveRef(rel, homeVol, cs)
	}
	if refCount == 0 {
		// 引用归零：真正删除 inode + 释放配额（软删则移到回收站）。
		if res, handled, rmErr := s.removeOrSoftDelete(f, homeVol, rel, quarRel, info, input); handled {
			return res, rmErr
		}
	} else {
		// 仍有其它引用：unlink 本 rel 目录项（inode 链接数-1，其余引用仍指向同一 inode），
		// 配额不减。硬链接下 Remove(quarRel) 即 unlink——另一引用（b.txt）的 inode 保留。
		if err := f.root.Remove(quarRel); err != nil {
			f.logger.ErrorContext(f.ctx, "摘除去重引用失败", "file_name", f.remotePath, "error", err.Error())
		}
	}
	if csStore := s.rt.checksumStore(f.owner); csStore != nil {
		csStore.Delete(rel)
	}
	if s.index != nil {
		s.index.remove(f.owner, strings.TrimPrefix(rel, "user/"))
	}
	if s.rt.metricsRecorder() != nil {
		s.rt.metricsRecorder().RecordDelete()
	}
	// 可信卷：**删除成功收尾**联动删除配套 .meta（服务端内部文件随主文件一起删，
	// 防残留；此时主文件已确认删除/软删成功——软删场景保留 meta 供恢复，见下）。
	if s.rt.fileMetaEnabled() && !input.SoftDelete {
		_ = f.root.Remove(meta.MetaPath(rel))
	}
	s.rt.recordFileAudit(f.ctx, "delete", f.remotePath, auditResultSuccess, "")
	f.logger.InfoContext(f.ctx, "文件已删除", "file_name", f.remotePath)
	// 文件变更事件：delete 成功（幂等删除不推送——无实际变更）。
	s.rt.publishFileEvent(EventDelete, f.owner, rel, 0)
	return DeleteFileResult{RemotePath: f.remotePath, Message: fmt.Sprintf("文件删除成功: %s", f.remotePath)}, nil
}

// removeOrSoftDelete 引用归零后的真正删除：软删（quarantine → trash 桶，保留原 rel 供
// 恢复）或硬删 + 配额与卷容量池释放。返回 handled=true 表示已产出最终结果（软删/硬删失败
// 时 err 非 nil）。
func (s *Service) removeOrSoftDelete(f *fileOp, homeVol, rel, quarRel string, info os.FileInfo, input DeleteFileInput) (DeleteFileResult, bool, error) {
	if input.SoftDelete {
		// 软删：quarantine → trash 桶（保留原 rel 供恢复）。
		trashRel, terr := s.softDeleteToTrash(f.ctx, f.root, quarRel, rel, info)
		if terr != nil {
			f.logger.ErrorContext(f.ctx, "软删失败", "file_name", f.remotePath, "error", terr.Error())
			s.rt.recordFileAudit(f.ctx, "delete", f.remotePath, auditResultError, "软删失败")
			return DeleteFileResult{}, true, &HTTPError{Status: 500, Message: "软删失败"}
		}
		s.rt.recordFileAudit(f.ctx, "delete", f.remotePath, auditResultSuccess, "软删到回收站")
		f.logger.InfoContext(f.ctx, "文件已软删到回收站", "file_name", f.remotePath, "trash", trashRel)
		return DeleteFileResult{RemotePath: f.remotePath, Message: "文件已移入回收站"}, true, nil
	}
	if err := f.root.Remove(quarRel); err != nil {
		// 审查 M-4：Detail 不含 err.Error()（os.Remove 错误含绝对路径，暴露服务端
		// 文件系统布局）；错误详情记业务日志，审计行用固定文案。
		f.logger.ErrorContext(f.ctx, errMsgDeleteFile, "file_name", f.remotePath, "error", err.Error())
		s.rt.recordFileAudit(f.ctx, "delete", f.remotePath, auditResultError, errMsgDeleteFile)
		return DeleteFileResult{}, true, &HTTPError{Status: http.StatusInternalServerError, Message: errMsgDeleteFile, Reason: reasonRemoveFailed}
	}
	// P4 配额对账：删除即释放已确认占用（按删除前 stat 的文件大小）；按文件实际 rel
	// 解析子 Scope（与写入同一键，父链聚合释放到子目录/user/租户各层）。
	if scope := s.rt.quotaScope(f.owner, rel); scope != nil {
		scope.ReleaseUsage(info.Size())
	}
	// 卷容量池双 Release（AD-7）：写入经双账本预留/提交，删除须释放文件所在卷池，否则卷池
	// Usage 虚高（路由/换卷误判），依赖 reconcile 才自愈。homeVol 空（无卷语义旧装配）跳过。
	// 用 ReleaseCommitted 原子扣减（PR-C 终审 Minor：替代「读 Usage 两次 + Adjust」非原子序列）。
	if homeVol != "" && s.rt.volSet() != nil {
		if pool := s.rt.volSet().Pool(homeVol); pool != nil {
			pool.ReleaseCommitted(info.Size())
		}
	}
	return DeleteFileResult{}, false, nil
}
