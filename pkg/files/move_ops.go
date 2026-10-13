// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"context"
	"errors"
	"net/http"
	"os"

	"github.com/cocomhub/sproxy/pkg/volume"
)

// move_ops.go 实现**跨卷移动的域侧组合**（覆盖「本地卷 ↔ 外部/加密卷」）。
//
// 背景：装配层既有的跨卷移动核心（`moveFileBetweenVolumes`）依赖**写盘 root** 与本地原子
// rename ⇒ 只覆盖本地卷；涉及外部/加密卷（无 `*storage.Root`）的移动此前直接
// `400 无效的文件路径`（用户只能手工 download + upload + delete）。
//
// 本方法把「复制 + 删源」收进域侧：复制复用 `CopyFile`（同卷 blob 直拷 / 统一写内核两条路径，
// 含配额双账本、sidecar、写后校验），删源：外部卷经 `ExternalDelete` 接缝硬删、本地卷经既有
// `DeleteFile`（回收站/meta/sidecar/配额释放口径与 `delete` 一致）。⇒ 装配层只负责解析
// **只读源**，配额与完整性口径不出现第二套。
//
// **非原子**：复制成功后删源失败会返回错误（**不谎报成功**），此时目标已写入、源仍在——
// 调用方按「已复制、源未删」处理（重试或清理）。唯一例外：删源返回 404（源已被并发删除）按
// 成功处理（移动目标已达成，幂等）。

// MoveFileInput 是跨卷移动的领域入参（HTTP 层解析 `?filename&from_volume&to_volume`）。
type MoveFileInput struct {
	// Owner 是操作主体（空 → anonymous，本方法内归一）。
	Owner string
	// From / To 是用户可见相对路径（本方法内校验，非法 → 400）。当前 move API 为同 rel 语义
	// （To = From），字段保留以对齐 CopyFile。
	From, To string
	// FromVol / ToVol 是源/目标卷名。
	FromVol, ToVol string
	// Force 透传给 CopyFile（true = 允许覆盖已存在目标；move API 默认 false）。
	Force bool
	// Src 是装配层解析好的**外部卷读取源**（源为外部/加密卷时非 nil）；本地卷源为 nil。
	// 同时是「源是否外部卷」的判据——决定删源走接缝（硬删）还是本地 delete 路径。
	Src DownloadSource
}

// MoveFileResult 是跨卷移动的领域出参。
type MoveFileResult struct {
	From, To   string
	VolumeName string
	Checksum   string
	Size       int64
	// VolumeCopy 表示复制段走了「卷内 blob 直拷」（零重加密）路径。
	VolumeCopy bool
}

// MoveFile 执行跨卷移动：先复制（源保留），**写成功之后**才删源。
//
// 错误语义（*HTTPError）：与 CopyFile 相同（400 路径/租户、404 源不存在、409 目标已存在、
// 5xx 读写失败）；删源失败 → 沿用该错误的状态码，文案标明「已复制到目标，源删除失败」。
func (s *Service) MoveFile(ctx context.Context, input MoveFileInput) (MoveFileResult, error) {
	owner := normalizeOwner(input.Owner)
	res, err := s.CopyFile(ctx, CopyFileInput{
		Owner: owner, From: input.From, To: input.To,
		FromVol: input.FromVol, ToVol: input.ToVol, Force: input.Force, Src: input.Src,
		// 源卷豁免 AD-4 唯一性查重：移动时「源卷持有同 rel」是预期状态（它就是被搬走的那个），
		// 装配层的显式卷查重（checkVolumeUniqueness）不认该豁免 ⇒ 会在 move 上误判 409。
		UniqueExemptVol: input.FromVol,
	})
	if err != nil {
		return MoveFileResult{}, err
	}
	if derr := s.deleteMovedSource(ctx, owner, input, res); derr != nil {
		if errors.Is(derr, errMoveSourceGone) {
			// 源已被并发删除：移动目标已达成（目标已写入、源不存在）→ 幂等成功。
			return MoveFileResult{
				From: res.From, To: res.To, VolumeName: input.ToVol,
				Checksum: res.Checksum, Size: res.Size, VolumeCopy: res.VolumeCopy,
			}, nil
		}
		status, msg := http.StatusInternalServerError, derr.Error()
		if he, ok := errors.AsType[*HTTPError](derr); ok {
			status, msg = he.Status, he.Message
		}
		return MoveFileResult{}, &HTTPError{
			Status:  status,
			Message: "移动未完成（已复制到目标，源删除失败）：" + msg,
		}
	}
	return MoveFileResult{
		From: res.From, To: res.To, VolumeName: input.ToVol,
		Checksum: res.Checksum, Size: res.Size, VolumeCopy: res.VolumeCopy,
	}, nil
}

// errMoveSourceGone 标记「源已不存在」（删源 404）——MoveFile 据此按幂等成功处理。
var errMoveSourceGone = errors.New("move: 源已不存在")

// deleteMovedSource 删除移动的源对象（**必须在目标写入成功之后**调用）。
//   - 外部卷源（input.Src 非 nil）：经 ExternalDelete 接缝**硬删**（不经回收站——远端后端无
//     原子 rename）；expectedChecksum 用复制时校验过的源码 SHA-256 交叉比对，防「复制后源被
//     并发改写仍被删」的 TOCTOU 丢数据。
//   - 本地卷源：复用 DeleteFile（回收站/meta/sidecar/配额释放口径与 `delete` 完全一致）。
func (s *Service) deleteMovedSource(ctx context.Context, owner string, input MoveFileInput, res CopyFileResult) error {
	if input.Src != nil {
		ops := s.rt.externalVolOps()
		if ops == nil {
			return &HTTPError{Status: http.StatusInternalServerError, Message: "外部卷删除未装配"}
		}
		removed, err := ops.ExternalDelete(ctx, owner, input.FromVol, input.From, res.Checksum)
		if err != nil {
			return err
		}
		if !removed {
			return errMoveSourceGone
		}
		return nil
	}
	if _, err := s.DeleteFile(ctx, DeleteFileInput{
		Owner: owner, RemotePath: input.From, ExplicitVol: input.FromVol,
		ExpectedChecksum: res.Checksum, SkipChecksumVerify: res.Checksum == "",
	}); err != nil {
		if he, ok := errors.AsType[*HTTPError](err); ok && he.Status == http.StatusNotFound {
			return errMoveSourceGone
		}
		return err
	}
	return nil
}

// moveUniqueConflict 是跨卷移动的 **AD-4 目标唯一性**判定（与本地卷跨卷移动核心
// `moveCheckTargetUnique` 同口径）：目标卷已有同 rel → 返回目标卷名；owner 视图内**其它**卷
// （**源卷豁免**——它就是本次要搬走的源；目标卷已单独判过）已有同 rel → 返回该卷名。
// 两种情况调用方都映射 409（「防跨卷双份 / 静默覆盖」）。
//
// 为什么域侧自行判定：移动的目标写入走「显式卷定卷（不查重）」路由（否则源卷自己的同 rel 触发
// 装配层的 AD-4 查重 ⇒ 误判 409），故查重责任随之回到域侧；事实源与装配层一致——本地卷经
// `*storage.Root.Stat`、外部卷经 `ExternalStat` 接缝（接缝未装配则跳过该卷，不误报）。
func (s *Service) moveUniqueConflict(ctx context.Context, owner string, a copyArgs) (string, error) {
	if exists, err := s.copyTargetExists(ctx, owner, a); err != nil {
		return "", err
	} else if exists {
		return a.dstVol, nil
	}
	vs := s.rt.volSet()
	if vs == nil {
		return "", nil
	}
	for _, v := range volume.AllowedVolumes(vs.All(), owner) {
		if v.Name == a.srcVol || v.Name == a.dstVol {
			continue
		}
		hit, herr := s.volumeHoldsRel(ctx, owner, v.Name, a)
		if herr != nil {
			return "", herr
		}
		if hit {
			return v.Name, nil
		}
	}
	return "", nil
}

// volumeHoldsRel 判定该卷是否已有同 rel（本地卷经 *storage.Root.Stat、外部卷经 ExternalStat
// 接缝；接缝未装配 → 视为不命中，不误报）。
func (s *Service) volumeHoldsRel(ctx context.Context, owner, volName string, a copyArgs) (bool, error) {
	if root := s.rt.volSet().Root(volName); root != nil {
		if _, err := root.Stat(owner + "/" + a.toRel); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, &HTTPError{Status: http.StatusInternalServerError, Message: "探测目标卷失败"}
		}
		return false, nil
	}
	ops := s.rt.externalVolOps()
	if ops == nil {
		return false, nil
	}
	size, _, err := ops.ExternalStat(ctx, owner, volName, a.to)
	if err != nil {
		return false, err
	}
	return size >= 0, nil
}
