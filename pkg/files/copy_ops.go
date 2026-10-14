// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// copy_ops.go 实现**通用复制**（CLI `cp`；`POST /api/volumes/copy` 的扩展形态）：同卷、跨卷、
// URI（由调用方归一）都支持，源始终保留。
//
// 两条路径：
//
// 两条路径（均在域侧判定，**配额口径统一**）：
//  1. **同卷 + 卷支持 blob 直拷**（如 secretdata）：直调卷能力（底层 blob 直拷 + 新 meta，
//     **不重新加解密**，同容器零 blob 写），但**先经 routeWriteUpload 做 owner Scope + 卷容量池
//     双预留**（与写入同口径 ⇒ 复制不是配额旁路），成功 Commit、失败 Release；
//  2. **其余情形**（跨卷 / 本地同卷 / 卷不支持）：**读源 → 统一写内核 `WriteFile`**（内核自带
//     双账本预留；跨卷目标卷用自己的密钥重新加密）。
//     跨卷时目标卷用自己的密钥重新加密（不可避免），并复用写内核的容量/配额/sidecar/写后校验。
//
// 完整性：源侧记录的明文 SHA-256（本地 sidecar/台账，或外部卷 `meta.Provider`）作为写内核的
// `ExpectedChecksum` ⇒ 复制**顺带校验**内容与源记录值一致（不一致写内核会 400 并清理）。
// 源无记录值时才退化为「先算一遍再写」（多读一遍，宁可慢也不跳过校验）。

// CopyFileInput 是复制的领域入参（HTTP 层解析 `?filename&to&from_volume&to_volume&force`）。
type CopyFileInput struct {
	Owner   string // 操作主体（空 → anonymous）
	From    string // 用户可见源相对路径
	To      string // 用户可见目标相对路径
	FromVol string // 源卷（空 = 按 owner 视图定位，回落默认卷）
	ToVol   string // 目标卷（空 = 与源同卷 ⇒ 同卷复制）
	Force   bool   // true = 允许覆盖已存在目标（默认 false → 409，绝不静默覆盖）
	// Src 是装配层解析好的**外部卷读取源**（源为外部/加密卷时非 nil）；本地卷源为 nil。
	Src DownloadSource
	// UniqueExemptVol 是**唯一性查重豁免卷**（仅跨卷移动使用 = 移动的**源卷**）：该卷上存在
	// 同 rel 属预期（它就是本次要搬走的源），不得据此判「同名文件已存在于卷 X」。
	// 非空时改用「显式卷定卷（不查重）」写路径，并在域侧自行做 AD-4 查重（目标卷已有同 rel →
	// 409；视图内**其它**卷已有同 rel → 409）——与本地卷跨卷移动核心 `moveCheckTargetUnique`
	// 同口径。空值 = 普通复制（保持既有 AD-4 查重，零回归）。
	UniqueExemptVol string
}

// CopyFileResult 是复制的领域出参。
type CopyFileResult struct {
	From, To   string
	VolumeName string
	Checksum   string
	Size       int64
	// VolumeCopy 表示走了「卷内 blob 直拷」（零重加密）路径。
	VolumeCopy bool
}

// CopyFile 执行复制（源保留）。错误语义（*HTTPError）：400 路径/租户/校验不可得；
// 404 源不存在；409 目标已存在且未 Force；5xx 读写失败。
func (s *Service) CopyFile(ctx context.Context, input CopyFileInput) (CopyFileResult, error) {
	owner := normalizeOwner(input.Owner)
	from, err := pathguard.ValidateFilePath(input.From)
	if err != nil {
		return CopyFileResult{}, &HTTPError{Status: 400, Message: errMsgInvalidPath}
	}
	to, err := pathguard.ValidateFilePath(input.To)
	if err != nil {
		return CopyFileResult{}, &HTTPError{Status: 400, Message: errMsgInvalidPath}
	}
	tnt0 := s.rt.tenantOf(owner)
	if tnt0 == nil || tnt0.Root() == nil {
		return CopyFileResult{}, &HTTPError{Status: 400, Message: errMsgInvalidPath}
	}
	fromRel, ok1 := tnt0.UserRel(from)
	toRel, ok2 := tnt0.UserRel(to)
	if !ok1 || !ok2 {
		return CopyFileResult{}, &HTTPError{Status: 400, Message: errMsgInvalidPath}
	}

	// 源定位：外部读源（装配层已解析）优先；否则本地（显式卷 → 该卷租户；空 → 视图/默认卷）。
	srcVol := input.FromVol
	var root *storage.Root
	if input.Src == nil {
		homeVol, r, locErr := s.resolveRenameHome(ctx, owner, fromRel, input.FromVol, tnt0)
		if locErr != nil {
			return CopyFileResult{}, locErr
		}
		srcVol, root = homeVol, r
	}
	dstVol := input.ToVol
	if dstVol == "" {
		dstVol = srcVol // cp 默认语义：不加目标卷 ⇒ 本卷内复制
	}

	// **同卷 + 卷支持 blob 直拷**（如 secretdata 加密卷）：直调卷能力（底层 blob 直拷 + 新 meta，
	// 不重新加解密）。**配额与写入同口径**：先经 routeWriteUpload 在 owner Scope + 卷容量池做
	// 双预留（配额不足 → 507/403 fail-closed，绝不在预留前动数据），成功后 Commit 转正。
	if res, handled, cerr := s.copyWithinVolume(ctx, owner, srcVol, dstVol, root, from, to, toRel, input.Force); handled || cerr != nil {
		return res, cerr
	}

	// 读源 → 统一写内核写目标（跨卷 / 本地同卷 / 卷不支持 blob 直拷）。
	return s.copyViaKernel(ctx, owner, copyArgs{
		from: from, to: to, fromRel: fromRel, toRel: toRel,
		srcVol: srcVol, dstVol: dstVol, src: input.Src, root: root, force: input.Force,
		uniqueExemptVol: input.UniqueExemptVol,
	})
}

// copyWithinVolume 尝试「同卷内 blob 直拷」（卷能力接缝 + 配额预检/结算）。handled=true 表示
// 已裁决。**为什么配额必须在此做**：blob 直拷不经统一写内核（无 WriteFile ⇒ 无 routeUpload），
// 若不预留，同卷复制就会成为**配额旁路**（复制不计数）。此处复用写内核同一组原语
// （`routeWriteUpload` 的 Scope/Pool 双 TryReserve + `Commit`/`Release`）⇒ 与写入同口径。
func (s *Service) copyWithinVolume(ctx context.Context, owner, srcVol, dstVol string, root *storage.Root,
	from, to, toRel string, force bool) (CopyFileResult, bool, error) {
	ops := s.rt.externalVolOps()
	vs := s.rt.volSet()
	if ops == nil || vs == nil || root != nil || srcVol == "" || dstVol != srcVol {
		return CopyFileResult{}, false, nil // 本地卷 / 跨卷 / 未装配接缝 → 写内核
	}
	v, ok := vs.ByName(srcVol)
	if !ok || !v.Authorize(owner) {
		return CopyFileResult{}, true, &HTTPError{Status: 404, Message: errMsgFileNotFound}
	}
	if vs.Root(srcVol) != nil {
		return CopyFileResult{}, false, nil // 本地卷 → 写内核
	}
	size, checksum, serr := ops.ExternalStat(ctx, owner, srcVol, from)
	if serr != nil {
		return CopyFileResult{}, true, serr
	}
	if size < 0 {
		return CopyFileResult{}, true, &HTTPError{Status: 404, Message: errMsgFileNotFound}
	}
	// 配额预检（双账本预留）：新文件语义（prev=0）⇒ 预留 size 字节。失败即 fail-closed（未动数据）。
	logger := s.rt.logger()
	route, rerr := s.routeWriteUpload(ctx, logger, owner, toRel, WriteFileInput{
		Owner: owner, RemotePath: to, ExplicitVol: dstVol, ClientSize: size,
	}, "", to)
	if rerr != nil {
		return CopyFileResult{}, true, rerr
	}
	cres, cerr := ops.ExternalCopy(ctx, owner, srcVol, from, from, to, toRel, force)
	if cerr != nil {
		route.Release()
		return CopyFileResult{}, true, cerr
	}
	if !cres.Supported {
		route.Release() // 不支持 blob 直拷 → 归还预留，回落写内核（内核会重新预留）
		return CopyFileResult{}, false, nil
	}
	route.Commit(0, s.settleSize(route, owner, to, size)) // 预留转正（与写入同口径）
	s.rt.recordFileAudit(ctx, "copy", from, auditResultSuccess, "to="+to+" 卷内直拷 volume="+srcVol)
	s.rt.publishFileEvent(EventUpload, owner, to, size)
	if cres.Checksum != "" {
		checksum = cres.Checksum
	}
	if cres.Size > 0 {
		size = cres.Size
	}
	return CopyFileResult{From: from, To: to, VolumeName: srcVol, Checksum: checksum, Size: size, VolumeCopy: true}, true, nil
}

// copyArgs 是 copyViaKernel 的参数集合（S107：参数过多收敛为结构体）。
type copyArgs struct {
	from, to       string
	fromRel, toRel string
	srcVol, dstVol string
	src            DownloadSource
	root           *storage.Root
	force          bool
	// uniqueExemptVol 非空 = 跨卷移动（值为移动**源卷**）：唯一性查重豁免该卷 + 改用
	// 「显式卷定卷（不查重）」写路径（查重由 moveUniqueConflict 按源卷豁免口径完成）。
	uniqueExemptVol string
}

// copyViaKernel 读源 → 统一写内核写目标。
func (s *Service) copyViaKernel(ctx context.Context, owner string, a copyArgs) (CopyFileResult, error) {
	size, checksum, err := s.copySourceFacts(ctx, a)
	if err != nil {
		return CopyFileResult{}, err
	}
	if checksum == "" {
		return CopyFileResult{}, &HTTPError{Status: 400,
			Message: fmt.Sprintf("源文件 %s 的 checksum 不可得，复制已 fail-closed 拒绝", a.from)}
	}
	if !a.force {
		// **默认不覆盖**：目标已存在 → 409（cp 语义：绝不静默覆盖用户内容）。
		// 本地卷经租户根 Stat；外部卷经接缝 ExternalStat（size>=0 = 存在）。
		if exists, xerr := s.copyTargetExists(ctx, owner, a); xerr != nil {
			return CopyFileResult{}, xerr
		} else if exists {
			return CopyFileResult{}, &HTTPError{Status: 409, Message: errMsgDestExists}
		}
	} else {
		// 覆盖前移除已存在目标：删除门禁要求 expectedChecksum ⇒ 带上目标**当前**的明文
		// checksum（顺带把「目标在我们读过之后被改动」也拦下，TOCTOU 窗口最小化）；
		// 目标不存在 → AllowMissing 幂等跳过。外部卷目标不做预删（其写路径/接缝自带唯一性语义）。
		if root, ok := s.localDstRoot(owner, a); ok {
			sum, _ := fileChecksumRoot(root, a.toRel)
			if _, derr := s.DeleteFile(ctx, DeleteFileInput{
				Owner: owner, RemotePath: a.to, ExplicitVol: a.dstVol,
				ExpectedChecksum: sum, AllowMissing: true,
			}); derr != nil {
				return CopyFileResult{}, derr
			}
		}
	}
	// 写目标：
	//   - 普通复制（uniqueExemptVol 空）：`WriteFile` 显式卷路由（**带 AD-4 唯一性查重**，防跨卷双份）；
	//   - 跨卷移动（uniqueExemptVol = 源卷）：统一写内核 `WriteTarget`（显式卷定卷、不查重）——
	//     查重已由域侧 moveUniqueConflict 按「源卷豁免」口径完成（否则源卷自己持有同 rel 会被
	//     装配层的查重误判 409，这正是「跨卷移动涉及外部卷恒失败」的根因之一）。
	if a.uniqueExemptVol != "" {
		return s.copyViaTargetForMove(ctx, owner, a, checksum, size)
	}
	rc, oerr := s.openCopyStream(ctx, a)
	if oerr != nil {
		return CopyFileResult{}, oerr
	}
	defer func() { _ = rc.Close() }()
	res, werr := s.WriteFile(ctx, WriteFileInput{
		Owner: owner, RemotePath: a.to, ExplicitVol: a.dstVol,
		ExpectedChecksum: checksum, ClientSize: size,
	}, rc)
	if werr != nil {
		return CopyFileResult{}, werr
	}
	s.rt.recordFileAudit(ctx, "copy", a.from, auditResultSuccess, "to="+a.to+" volume="+res.VolumeName)
	s.rt.publishFileEvent(EventUpload, owner, a.to, res.Size)
	return CopyFileResult{From: a.from, To: a.to, VolumeName: res.VolumeName, Checksum: res.Checksum, Size: res.Size}, nil
}

// copyTargetExists 判定目标是否已存在（本地卷经租户根；外部卷经 ExternalStat 接缝）。
func (s *Service) copyTargetExists(ctx context.Context, owner string, a copyArgs) (bool, error) {
	if root, ok := s.localDstRoot(owner, a); ok {
		if _, err := root.Stat(a.toRel); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, &HTTPError{Status: 500, Message: errMsgStatFailed}
		}
		return false, nil
	}
	ops := s.rt.externalVolOps()
	if ops == nil || a.dstVol == "" {
		return false, nil
	}
	size, _, err := ops.ExternalStat(ctx, owner, a.dstVol, a.to)
	if err != nil {
		return false, err
	}
	return size >= 0, nil
}

// localDstRoot 返回**本地**目标卷的租户根（外部卷/无卷语义 → ok=false，调用方跳过预删）。
func (s *Service) localDstRoot(owner string, a copyArgs) (*storage.Root, bool) {
	if a.dstVol == "" {
		tnt := s.rt.tenantOf(owner)
		if tnt == nil || tnt.Root() == nil {
			return nil, false
		}
		return tnt.Root(), true
	}
	vs := s.rt.volSet()
	if vs != nil && vs.Root(a.dstVol) == nil {
		return nil, false // 外部卷
	}
	tnt := s.rt.volumeTenant(a.dstVol, owner)
	if tnt == nil || tnt.Root() == nil {
		return nil, false
	}
	return tnt.Root(), true
}

// copySourceFacts 取源的大小与**明文** checksum：外部卷经读源（含 meta.Provider 的明文哈希）；
// 本地卷经租户根（先查台账/摘要，缺失再读一遍算）。
func (s *Service) copySourceFacts(ctx context.Context, a copyArgs) (int64, string, error) {
	if a.src != nil {
		return s.externalSourceFacts(ctx, a)
	}
	return s.localSourceFacts(ctx, a)
}

// externalSourceFacts 外部卷源：Stat 取大小；明文 checksum 优先来自读取源的 ChecksumSource
// （meta.Provider），缺失时退化为「先读一遍算」（宁可多读一遍，不跳过校验）。
func (s *Service) externalSourceFacts(ctx context.Context, a copyArgs) (int64, string, error) {
	info, serr := a.src.Stat(ctx)
	if serr != nil {
		return 0, "", &HTTPError{Status: 404, Message: errMsgFileNotFound}
	}
	if css, ok := a.src.(ChecksumSource); ok {
		if sum, ok2 := css.Checksum(ctx); ok2 && sum != "" {
			return info.Size(), sum, nil
		}
	}
	sum, herr := s.hashSourceStream(ctx, a)
	if herr != nil {
		return 0, "", herr
	}
	return info.Size(), sum, nil
}

// localSourceFacts 本地卷源：Stat 取大小；checksum 先试台账/sidecar，缺失再读一遍算。
func (s *Service) localSourceFacts(ctx context.Context, a copyArgs) (int64, string, error) {
	if a.root == nil {
		return 0, "", &HTTPError{Status: 404, Message: errMsgFileNotFound}
	}
	info, serr := a.root.Stat(a.fromRel)
	if serr != nil {
		if os.IsNotExist(serr) {
			return 0, "", &HTTPError{Status: 404, Message: errMsgFileNotFound}
		}
		return 0, "", &HTTPError{Status: 500, Message: errMsgStatFailed}
	}
	if cs, cerr := fileChecksumRoot(a.root, a.fromRel); cerr == nil && cs != "" {
		return info.Size(), cs, nil
	}
	sum, herr := s.hashSourceStream(ctx, a)
	if herr != nil {
		return 0, "", herr
	}
	return info.Size(), sum, nil
}

// hashSourceStream 打开源流算一遍 SHA-256（记录值缺失时的兜底；多读一遍换“不跳过校验”）。
func (s *Service) hashSourceStream(ctx context.Context, a copyArgs) (string, error) {
	rc, err := s.openCopyStream(ctx, a)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	if _, cerr := io.Copy(h, rc); cerr != nil {
		return "", &HTTPError{Status: 500, Message: errMsgOpenFileFailed}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// openCopyStream 打开源读取流（外部卷经装配层读源；本地卷经租户根，加密卷透明解密）。
func (s *Service) openCopyStream(ctx context.Context, a copyArgs) (io.ReadCloser, error) {
	if a.src != nil {
		rc, err := a.src.Open(ctx)
		if err != nil {
			return nil, &HTTPError{Status: 500, Message: errMsgOpenFileFailed}
		}
		return rc, nil
	}
	if a.root == nil {
		return nil, &HTTPError{Status: 404, Message: errMsgFileNotFound}
	}
	rc, err := a.root.OpenDecrypted(a.fromRel) // 非加密卷 = 普通读；加密卷 = 透明解密
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &HTTPError{Status: 404, Message: errMsgFileNotFound}
		}
		return nil, &HTTPError{Status: 500, Message: errMsgOpenFileFailed}
	}
	return rc, nil
}

// copyViaTargetForMove 执行**跨卷移动**的目标写入：域侧 AD-4 查重（源卷豁免）+ 统一写内核
// `WriteTarget`（显式卷定卷、不查重）。抽独立方法的原因：让 copyViaKernel 保持单一直线路径
// （普通复制）与可读的复杂度预算。
func (s *Service) copyViaTargetForMove(ctx context.Context, owner string, a copyArgs, checksum string, size int64) (CopyFileResult, error) {
	conflictVol, cerr := s.moveUniqueConflict(ctx, owner, a)
	if cerr != nil {
		return CopyFileResult{}, cerr
	}
	if conflictVol != "" {
		return CopyFileResult{}, &HTTPError{Status: http.StatusConflict,
			Message: "同名文件已存在于卷 \"" + conflictVol + "\""}
	}
	rc, oerr := s.openCopyStream(ctx, a)
	if oerr != nil {
		return CopyFileResult{}, oerr
	}
	defer func() { _ = rc.Close() }()
	tres, terr := s.WriteTarget(ctx, WriteTargetInput{
		Owner: owner, RemotePath: a.to, Volume: a.dstVol,
		Verify: true, ExpectedChecksum: checksum, ClientSize: size,
	}, rc)
	if terr != nil {
		return CopyFileResult{}, terr
	}
	s.rt.recordFileAudit(ctx, "move", a.from, auditResultSuccess, "to="+a.to+" volume="+tres.VolumeName)
	return CopyFileResult{From: a.from, To: a.to, VolumeName: tres.VolumeName,
		Checksum: tres.Checksum, Size: tres.Size}, nil
}
