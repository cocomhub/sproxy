// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/rootfs"
	"github.com/cocomhub/sproxy/pkg/server/archive"
	"github.com/cocomhub/sproxy/pkg/server/cloudtasks"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// 归档 DTO 的唯一实现已迁入 cloudtasks（P1-c S3a）；装配层与既有测试经类型别名继续引用，
// 响应结构体因此仍只有一份定义（JSON 字段顺序/标签零变化）。
type (
	CloudArchiveRequest      = cloudtasks.CloudArchiveRequest
	CloudArchiveBatchRequest = cloudtasks.CloudArchiveBatchRequest
	CloudArchiveResult       = cloudtasks.CloudArchiveResult
)

// cloudArchiveReservePlaceholder 云归档打包预留占位（100MB）。
// tar 头（≤512B/文件）+ gzip 可能在极少数情况下产生少量膨胀，预留后按实际大小对账。
const cloudArchiveReservePlaceholder = int64(100 * 1024 * 1024)

// 云归档编排入口（P1-c S3a 已迁入 pkg/server/cloudtasks）。装配层只保留 owner 解析
// 适配器（`ActorFrom` 的 ctx key 是装配层私有，核心显式接收 owner）。
func (h *Handlers) archiveTaskHandler() http.HandlerFunc {
	return h.cloudTaskHandler(h.cloudTaskService().ArchiveTask)
}

func (h *Handlers) archiveBatchHandler() http.HandlerFunc {
	return h.cloudTaskHandler(h.cloudTaskService().ArchiveBatch)
}

// archiveSettleQuota 按磁盘实际大小对账预留配额的**非响应核心**（单任务 / 批量 / 组归档
// 共用）：scope 路径 = Reconcile(actual) + Commit()（多预留部分自动归还）；storageMgr 回退
// 保留 3 步（Release(pre)+TryReserve(actual)）。失败返回 status+message（响应由调用方写出，
// 文案与迁移前逐字一致）。
func (h *Handlers) archiveSettleQuota(root *storage.Root, rel string, res *quota.Settlement, pre int64, logger *slog.Logger) (actual int64, status int, message string) {
	if res != nil {
		info, statErr := root.Stat(rel)
		if statErr != nil {
			// stat 失败（罕见）：释放预留避免永久挂账（归档文件已落盘，下次扫描校准全局账本）。
			res.Release()
			return 0, 0, ""
		}
		actual = info.Size()
		commitArchiveSettlement(res, pre, actual, logger)
		return actual, 0, ""
	}
	if h.storageMgr == nil {
		return 0, 0, ""
	}
	h.storageMgr.Release(pre, capacity.CategoryCloud)
	info, statErr := root.Stat(rel)
	if statErr != nil {
		return 0, 0, ""
	}
	actual = info.Size()
	if rErr := h.storageMgr.TryReserve(actual, capacity.CategoryCloud); rErr != nil {
		logger.Error("storage full, removing archive to keep ledger consistent", "error", rErr)
		_ = root.Remove(rel)
		return 0, http.StatusInsufficientStorage, fmt.Sprintf("insufficient storage for archive: %v", rErr)
	}
	return actual, 0, ""
}

// archiveReserveQuota 是归档预留的**非响应核心**（单任务 / 批量 / 组归档共用）：先校验
// cloud_archive_max_bytes 上限，再按「scope 优先（ReserveWait 排队）、storageMgr 回退」预留。
// 失败返回 status+message（文案与迁移前逐字一致）；成功返回结算句柄（nil = 回退/未装配）
// 与预留量 pre。
func (h *Handlers) archiveReserveQuota(ctx context.Context, owner string, totalSourceSize int64) (*quota.Settlement, int64, int, string) {
	if maxBytes := (&cloudTasksAdapter{h: h}).MaxArchiveBytes(); maxBytes > 0 && totalSourceSize > maxBytes {
		return nil, 0, http.StatusBadRequest, fmt.Sprintf("archive exceeds cloud_archive_max_bytes: %d > %d", totalSourceSize, maxBytes)
	}
	pre := totalSourceSize + cloudArchiveReservePlaceholder
	// P5 收敛：双轨 TryReserve（storageMgr + Scope 同时预留同量字节）改为二选一——
	// 生产环境 scope 恒非 nil（全局兜底由 Scope 父链生效），storageMgr 仅作回退。
	// P1-b：scope 路径的「预留 → 对账 → 提交/回滚」改走 quota.Settlement（协议唯一实现）；
	// ReserveWait 保留「容量临时不足 ⇒ 排队等释放」的能力（#963 锁定的语义，不得退化为立即 507）。
	if scope := h.quotaBucketFor(owner, "archive"); scope != nil {
		st := quota.NewSettlement(scope.Pool())
		// h.quotaWaitTimeout = 0（生产恒 0）⇒ ReserveWait 内部按 quota.DefaultWaitTimeout（30s），
		// 语义不变。此前此处恒传 quota.DefaultWaitTimeout，测试注入的等待上限被忽略（#963 记录）。
		if reserveErr := st.ReserveWait(ctx, pre, h.quotaWaitTimeout); reserveErr != nil {
			// 失败时账本与句柄状态均不变（ReserveWait 契约）⇒ 直接丢弃句柄，无预留泄漏。
			return nil, 0, http.StatusInsufficientStorage, fmt.Sprintf("insufficient storage: %v", reserveErr)
		}
		return st, pre, 0, ""
	} else if h.storageMgr != nil {
		if reserveErr := h.storageMgr.WaitReserve(ctx, pre, capacity.CategoryCloud, capacity.DefaultWaitTimeout); reserveErr != nil {
			return nil, 0, http.StatusInsufficientStorage, fmt.Sprintf("insufficient storage: %v", reserveErr)
		}
	}
	// 回退路径（storageMgr）或未装配账本：无结算句柄需定案（回退预留按 pre 释放）。
	return nil, pre, 0, ""
}

type fileWithRelPath struct {
	root   *storage.Root
	rel    string // 根内相对源路径（cloud/<taskID>/<file>）
	tarRel string // tar 内条目名
}

// createTarGz 将单个文件打包为 tar.gz 并返回流式计算的 SHA-256 checksum。
// 使用 succeeded 标记模式确保出错时清理输出文件。O_EXCL 创建，已存在返回 false 到 created。
// 源文件经 srcRoot 相对 srcRel 读取、输出经 outRoot 相对 outRel 打开（os.Root 防符号链接逃逸；O_EXCL 语义保留）。
func createTarGz(srcRoot *storage.Root, srcRel, srcTarName string, outRoot *storage.Root, outRel string, logger *slog.Logger) (checksum string, err error) {
	outputFile, created, err := openArchiveOutput(outRoot, outRel)
	if err != nil {
		return "", err
	}
	if !created {
		return "", errArchiveExists
	}
	defer outputFile.Close()

	// 出错时清理输出文件
	succeeded := false
	defer func() {
		if !succeeded {
			_ = outRoot.Remove(outRel)
		}
	}()

	hasher := sha256.New()
	multiWriter := io.MultiWriter(outputFile, hasher)
	gw := gzip.NewWriter(multiWriter)
	tw := tar.NewWriter(gw)

	// **收尾顺序必须显式**（P1 修复，2026-10-21）：此前 tw/gw 都是 `defer Close()`，而
	// checksum 在 defer 之前就计算 ⇒ ① 哈希漏掉 tar 的 EOF 两块 + gzip 尾（CRC32/ISIZE）
	// ⇒ 响应里的 checksum 100% 不等于落盘文件（客户端按它校验必然判「归档损坏」）；
	// ② Close 的错误被丢弃且 succeeded 已置 true ⇒ 收尾失败的**截断归档**被当成功返回
	// 且不清理。与 pkg/server/archive.go、pkg/transfer/registry.go 的既有口径统一。
	if err := archive.AddFileToTar(tw, srcRoot, srcRel, srcTarName, logger); err != nil {
		_ = tw.Close()
		_ = gw.Close()
		return "", fmt.Errorf("add file to tar: %w", err)
	}
	if err := tw.Close(); err != nil {
		_ = gw.Close()
		return "", fmt.Errorf("关闭 tar: %w", err)
	}
	if err := gw.Close(); err != nil {
		return "", fmt.Errorf("关闭 gzip: %w", err)
	}
	if err := outputFile.Close(); err != nil {
		return "", fmt.Errorf("关闭归档文件: %w", err)
	}

	succeeded = true
	checksum = hex.EncodeToString(hasher.Sum(nil))
	return checksum, nil
}

// createMultiFileTarGz 将多个文件打包为单个 tar.gz 并返回流式计算的 SHA-256 checksum。
// 使用 succeeded 标记模式确保出错时清理输出文件。O_EXCL 创建，已存在时置 created=false 并返回 errArchiveExists。
// 源文件按各自租户根（files[].root）相对读取、输出经 outRoot 相对 outRel 打开
// （os.Root 防符号链接逃逸；O_EXCL 语义保留）。
func createMultiFileTarGz(files []fileWithRelPath, outRoot *storage.Root, outRel string, logger *slog.Logger, created *bool) (checksum string, err error) {
	*created = true
	outputFile, createdOK, err := openArchiveOutput(outRoot, outRel)
	if err != nil {
		return "", err
	}
	if !createdOK {
		*created = false
		return "", errArchiveExists
	}
	defer outputFile.Close()

	// 出错时清理输出文件
	succeeded := false
	defer func() {
		if !succeeded {
			_ = outRoot.Remove(outRel)
		}
	}()

	hasher := sha256.New()
	multiWriter := io.MultiWriter(outputFile, hasher)
	gw := gzip.NewWriter(multiWriter)
	tw := tar.NewWriter(gw)

	// 与 createTarGz 同口径（P1 修复，2026-10-21）：显式收尾 + 收尾后再算 checksum。
	for _, f := range files {
		if err := archive.AddFileToTar(tw, f.root, f.rel, f.tarRel, logger); err != nil {
			_ = tw.Close()
			_ = gw.Close()
			return "", fmt.Errorf("add file %q to tar: %w", f.tarRel, err)
		}
	}
	if err := tw.Close(); err != nil {
		_ = gw.Close()
		return "", fmt.Errorf("关闭 tar: %w", err)
	}
	if err := gw.Close(); err != nil {
		return "", fmt.Errorf("关闭 gzip: %w", err)
	}
	if err := outputFile.Close(); err != nil {
		return "", fmt.Errorf("关闭归档文件: %w", err)
	}

	succeeded = true
	checksum = hex.EncodeToString(hasher.Sum(nil))
	return checksum, nil
}

// errArchiveExists 归档输出文件已存在（O_EXCL 创建失败）。
var errArchiveExists = errors.New("archive file already exists")

// openArchiveOutput 以 O_EXCL 语义打开归档输出文件（相对租户 root 的 rel 路径）。
// 跨平台统一用 errors.Is(err, os.ErrExist) 判已存在（Windows 上 O_EXCL 对已存在文件同样报错）。
func openArchiveOutput(root *storage.Root, rel string) (io.WriteCloser, bool, error) {
	// 归档输出是**用户内容**写面：加密卷须透明加密落盘（raw OpenFile 写明文，
	// 归档产物读回时按密文形态不完整；评审「用户可见入口」收敛面）。
	f, err := rootfs.WriteUserVisible(root, rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("create archive output: %w", err)
	}
	return f, true, nil
}
