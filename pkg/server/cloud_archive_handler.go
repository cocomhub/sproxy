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

	"github.com/cocomhub/sproxy/pkg/compressx"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/rootfs"
	"github.com/cocomhub/sproxy/pkg/server/archive"
	"github.com/cocomhub/sproxy/pkg/server/cloudtasks"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	"github.com/cocomhub/sproxy/pkg/transfer"
)

// 归档 DTO 的唯一实现已迁入 cloudtasks（P1-c S3a）；装配层与既有测试经类型别名继续引用，
// 响应结构体因此仍只有一份定义（JSON 字段顺序/标签零变化）。
type (
	CloudArchiveRequest      = cloudtasks.CloudArchiveRequest
	CloudArchiveBatchRequest = cloudtasks.CloudArchiveBatchRequest
	CloudArchiveResult       = cloudtasks.CloudArchiveResult
)

// archiveVolumeTransfer 是统一转存入参的类型别名（唯一实现在 cloudtasks，S3b 迁入后别名删除）。
type archiveVolumeTransfer = cloudtasks.ArchiveVolumeTransfer

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

// normalizeArchiveName 是组归档路径（S3b 前仍在装配层）对编排核心归一逻辑的取用壳：
// 规则唯一实现是 cloudtasks.NormalizeArchiveName（Base/后缀/255 字节），本函数只做响应写出。
func (h *Handlers) normalizeArchiveName(w http.ResponseWriter, archiveName string) (string, bool) {
	name, msg := cloudtasks.NormalizeArchiveName(archiveName)
	if msg != "" {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: msg}, http.StatusBadRequest)
		return "", false
	}
	return name, true
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

// cloudArchiveSettleQuota 是 archiveSettleQuota 的响应壳（组归档路径 S3b 前直接调用；
// 单任务/批量经 Quota.SettleArchive 取用同一核心）。TryReserve 拒绝时已删文件并回 507。
func (h *Handlers) cloudArchiveSettleQuota(w http.ResponseWriter, root *storage.Root, rel string, res *quota.Settlement, pre int64, logger *slog.Logger) (actual int64, ok bool) {
	actual, status, message := h.archiveSettleQuota(root, rel, res, pre, logger)
	if status != 0 {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: message}, status)
		return 0, false
	}
	return actual, true
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

// recordArchiveUsage 登记归档文件在租户 archive 桶 Scope 中的已确认占用
// （P5 审查重要 2：删除归档时按登记释放，不再依赖周期扫描自愈）。
// actual<=0 时不登记（文件未落盘，无占用可释放）。
func (h *Handlers) recordArchiveUsage(owner, archiveName string, actual int64) {
	if actual <= 0 {
		return
	}
	owner = normalizeOwner(owner)
	h.tenantMu.Lock()
	defer h.tenantMu.Unlock()
	if h.archiveUsage == nil {
		h.archiveUsage = make(map[string]map[string]int64)
	}
	m := h.archiveUsage[owner]
	if m == nil {
		m = make(map[string]int64)
		h.archiveUsage[owner] = m
	}
	m[archiveName] = actual
}

// deleteCloudArchive 删除租户 archive 桶下指定归档文件并释放其 Scope 占用。
// 供归档删除路径（DeleteGroup 联动 / 未来归档删除 API）调用；文件不存在视为幂等成功。
// 释放量以磁盘 stat 为准（与登记一致；登记缺失时按 stat 释放，防止 Scope 虚高）。
func (h *Handlers) deleteCloudArchive(owner, archiveName string) error {
	owner = normalizeOwner(owner)
	tnt := h.tenantFor(owner)
	if tnt == nil {
		return errors.New("租户不可用，无法删除归档")
	}
	rel, ok := tnt.FeatureRel("archive", archiveName)
	if !ok {
		return fmt.Errorf("非法归档名: %s", archiveName)
	}
	root := tnt.Root()
	size := int64(0)
	if info, err := root.Stat(rel); err == nil {
		size = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat 归档失败: %w", err)
	}
	if err := root.Remove(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("删除归档失败: %w", err)
	}
	// 释放 archive 桶 Scope 占用（按磁盘 stat 大小；文件已不存在时按登记释放）。
	if size <= 0 {
		h.tenantMu.Lock()
		if m := h.archiveUsage[owner]; m != nil {
			size = m[archiveName]
		}
		h.tenantMu.Unlock()
	}
	if scope := h.quotaBucketFor(owner, "archive"); scope != nil && size > 0 {
		scope.ReleaseUsage(size)
	}
	h.tenantMu.Lock()
	if m := h.archiveUsage[owner]; m != nil {
		delete(m, archiveName)
	}
	h.tenantMu.Unlock()
	h.logger.Info("云归档已删除并释放配额", "owner", owner, "archive", archiveName, "size", size)
	return nil
}

// archiveExtras 是「仅转存外部卷支持」的选项集合（校验入参收拢为结构体，避免长参数列表）。
type archiveExtras struct {
	TargetVolume string
	TargetPath   string
	Encrypt      bool
	Cipher       string
	Compression  string
}

// validateArchiveExtras 校验「仅转存外部卷支持」的选项：
//   - 未指定 target_volume 时不得设置 target_path/encrypt/cipher/compression
//     （archive 桶路径走既有 createTarGz，不加密也不认目标路径）；
//   - cipher 必须与 encrypt 同用（单独指定无任何加密效果，静默忽略会让用户误信已加密）。
//
// 失败时已写响应并返回 false。
func (h *Handlers) validateArchiveExtras(w http.ResponseWriter, ex archiveExtras) bool {
	if status, msg := archiveExtrasError(ex); status != 0 {
		return h.archiveBadRequest(w, msg)
	}
	return true
}

// archiveExtrasError 是「仅转存外部卷支持」选项校验的**非响应核心**（规则唯一实现：
// 装配层的组归档壳与 cloudtasks 经 Targets.ValidateArchiveExtras 共用）。
// 返回 (0, "") 表示通过；否则为应回应的状态码与文案。
func archiveExtrasError(ex archiveExtras) (int, string) {
	if ex.TargetVolume == "" {
		if ex.Encrypt || ex.Cipher != "" || ex.Compression != "" || ex.TargetPath != "" {
			return http.StatusBadRequest, "target_path/encrypt/cipher/compression 仅支持与 target_volume（转存外部卷）同用"
		}
		return 0, ""
	}
	if ex.Cipher != "" && !ex.Encrypt {
		return http.StatusBadRequest, "cipher 仅与 encrypt 同用（单独指定不加密，已拒绝以避免误信已加密）"
	}
	return 0, ""
}

// archiveBadRequest 回 400 并返回 false（统一归档端点的参数错误应答）。
func (h *Handlers) archiveBadRequest(w http.ResponseWriter, msg string) bool {
	sendJSONResponse(w, CloudArchiveResult{Success: false, Message: msg}, http.StatusBadRequest)
	return false
}

// archivePreprocessor 按请求构造打包预处理器（压缩 + 可选加密，经 transfer registry 选型）。
// 加密密钥来自 archive.key_file（与 /api/archive 同一约定）。失败已回响应。
func (h *Handlers) archivePreprocessor(w http.ResponseWriter, encrypt bool, cipher, compression string) (transfer.Preprocessor, bool) {
	algo := compressx.Gzip
	if compression != "" {
		parsed, err := compressx.Parse(compression)
		if err != nil {
			sendJSONResponse(w, CloudArchiveResult{Success: false, Message: err.Error()}, http.StatusBadRequest)
			return nil, false
		}
		algo = parsed
	}
	cfg := transfer.PreprocessorConfig{Compression: algo, Cipher: cipher}
	if encrypt {
		if cipher != "" {
			if _, ok := files.LookupCipher(cipher); !ok {
				sendJSONResponse(w, CloudArchiveResult{Success: false, Message: fmt.Sprintf("未知加密算法 %q", cipher)}, http.StatusBadRequest)
				return nil, false
			}
		}
		key := h.archiveKey()
		if len(key) != 32 {
			sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "归档加密密钥不可用（需配置 archive.key_file）"}, http.StatusBadRequest)
			return nil, false
		}
		cfg.EncryptKey = key
	}
	pre, err := transfer.New("tar", cfg)
	if err != nil {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: err.Error()}, http.StatusInternalServerError)
		return nil, false
	}
	return pre, true
}
