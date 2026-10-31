// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// transfer.go 是云归档的**转存核心**与归档产物**删除/配额释放**面（P1-c S3c）：
// 把打包流（tar[.ext][.aes]）转存到目标卷（单任务 / 批量 / 组归档共用），以及组归档
// 产物（租户 archive 桶归档名 / 外部卷转存 URL）的删除与占用释放。
//
// 本文件由装配层 pkg/server/cloud_download_handler.go + cloud_archive_handler.go 迁入，
// 行为零变化（docs/designs/2026-10-31-cloudtasks-deps-mapping.md §4/§9.5）：
//   - 目标 FS = Volumes.TransferFS（可信卷写视图 Guard(Wrap(...))，meta 恒生成、meta 桶隔离）；
//   - 目标键经 volume.ResolveLocation 唯一入口（恒 owner 前缀 + 路径安全），与读路径
//     ResolveUserLocation 同键空间；
//   - 预处理（压缩 / 可选加密）经 transfer registry 选型；
//   - 本地 staging 配额 + per-(owner,rel) 写锁（与 HTTP 上传/DAV/S3 同键空间）；
//   - owner 配额「写前探针 → 写 → 差分对账」与卷级容量池同源结算，顺序与删除侧一致；
//   - 审计动作/文案与迁移前逐字相同。
//
// archiveUsage 状态（owner → 归档名 → 已确认字节）随本片从装配层迁入：Service 单实例持有
// （装配层 cloudTaskService 懒构造一次），记录（归档落桶）与删除（按登记释放）同源。
package cloudtasks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/cocomhub/sproxy/pkg/compressx"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/rootfs"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/transfer"
)

// archiveUsage 按 owner 登记归档产物在租户 archive 桶的已确认占用（归档名 → 字节），
// 供删除时按登记释放（P5 审查重要 2：不依赖周期扫描自愈）。
//
// 并发安全（自持锁）：记录（ArchiveTask/ArchiveBatch/ArchiveGroup）与删除（组删除联动）
// 可能并发，装配层原来的 tenantMu 只用于保护懒创建。
type archiveUsage struct {
	mu sync.Mutex
	m  map[string]map[string]int64
}

// record 登记归档名的已确认占用（actual<=0 不登记：文件未落盘，无占用可释放）。
func (u *archiveUsage) record(owner, archiveName string, actual int64) {
	if actual <= 0 {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.m == nil {
		u.m = make(map[string]map[string]int64)
	}
	m := u.m[owner]
	if m == nil {
		m = make(map[string]int64)
		u.m[owner] = m
	}
	m[archiveName] = actual
}

// size 返回归档名的登记占用（0 = 未登记）。
func (u *archiveUsage) size(owner, archiveName string) int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.m[owner][archiveName]
}

// remove 清除归档名的登记（删除归档后调用）。
func (u *archiveUsage) remove(owner, archiveName string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if m := u.m[owner]; m != nil {
		delete(m, archiveName)
	}
}

// ArchiveUsage 报告 owner 的归档名在 archive 桶中的已登记字节（0 = 未登记）。
// 这是删除路径「按登记释放」的事实源，同时供装配层测试/诊断核验删除后登记已清除。
func (s *Service) ArchiveUsage(owner, archiveName string) int64 {
	return s.usage.size(storage.NormalizeOwner(owner), archiveName)
}

// recordArchiveUsage 登记归档文件在租户 archive 桶 Scope 中的已确认占用
// （P5 审查重要 2：删除归档时按登记释放，不再依赖周期扫描自愈）。
func (s *Service) recordArchiveUsage(owner, archiveName string, actual int64) {
	s.usage.record(storage.NormalizeOwner(owner), archiveName, actual)
}

// deleteCloudArchive 删除租户 archive 桶下指定归档文件并释放其 Scope 占用。
// 供归档删除路径（DeleteGroup 联动 / 未来归档删除 API）调用；文件不存在视为幂等成功。
// 释放量以磁盘 stat 为准（与登记一致；登记缺失时按 stat 释放，防止 Scope 虚高）。
func (s *Service) DeleteCloudArchive(owner, archiveName string) error {
	owner = storage.NormalizeOwner(owner)
	tnt := s.deps.Tenants.TenantFor(owner)
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
		size = s.usage.size(owner, archiveName)
	}
	if scope := s.deps.Quota.BucketScope(owner, "archive"); scope != nil && size > 0 {
		scope.ReleaseUsage(size)
	}
	s.usage.remove(owner, archiveName)
	s.deps.Logger.Info("云归档已删除并释放配额", "owner", owner, "archive", archiveName, "size", size)
	return nil
}

// DeleteGroupArchive 删除组归档产物：外部卷转存 URL（含 "://"）→ 按卷可信 FS 删除
// （主文件 + sidecar）；否则按租户 archive 桶归档名删除（既有行为零回归）。
func (s *Service) DeleteGroupArchive(owner, archiveFile string) error {
	if !strings.Contains(archiveFile, "://") {
		return s.DeleteCloudArchive(owner, archiveFile)
	}
	u, err := url.Parse(archiveFile)
	if err != nil {
		return fmt.Errorf("解析归档 URL %q: %w", archiveFile, err)
	}
	volName := u.Host
	fsKey := strings.TrimPrefix(u.Path, "/")
	if volName == "" || fsKey == "" {
		return fmt.Errorf("归档 URL %q 缺少卷/路径", archiveFile)
	}
	fs, _, _, ok := s.deps.Volumes.TransferFS(volName)
	if !ok {
		return fmt.Errorf("归档目标卷 %q 未装配", volName)
	}
	// **配额释放（与写侧同源）**：组归档转存外部卷的产物在写入时经 `settleArchiveCharge` 计入
	// owner 账本（P2 修复，2026-10-21：此前删除路径只删对象、不释放 ⇒ 配额虚高，可能误 507；
	// 虽然磁盘对账会在 ≤30min 内冲掉，但属于可避免的窗口）。删除前**实测**记账字节（与写侧
	// `archiveTargetCharge` 同一函数 ⇒ 不会「未曾提交的释放」），删除成功后按同一 Scope 释放。
	charge := s.archiveTargetCharge(volName, owner, fsKey, userRelFromFSKey(owner, fsKey))
	if err := fs.Delete(context.Background(), fsKey); err != nil {
		return fmt.Errorf("删除外部卷归档 %q: %w", fsKey, err)
	}
	if charge > 0 {
		if scope := s.deps.Quota.ScopeFor(owner, path.Join("user", userRelFromFSKey(owner, fsKey))); scope != nil {
			scope.ReleaseUsage(charge)
		}
		// **卷级容量释放**（独立复审 Important-1）：本地卷产物写侧已补卷池 Adjust ⇒
		// 删除侧同源释放（pool.ReleaseCommitted）。外部卷（CapacityFS 层已记）不双扣。
		if s.deps.Volumes.IsLocal(volName) {
			if pool := s.deps.Volumes.LocalPool(volName); pool != nil {
				pool.ReleaseCommitted(charge)
			}
		}
	}
	return nil
}

// RecordBypassWrite 是云转存/归档转存产物派生状态的**统一入口**（装配层注入 cloud manager）。
//
// 分流依据是「该卷是否本地根」：
//   - 本地卷：结构/内容写事件已维护搜索索引 ⇒ 只补 checksum 台账（`RecordBypassChecksum`）；
//   - 外部/远端卷：无本地 Root 事件 ⇒ 走 `RecordBypassWrite`（checksum + 索引一次登记）。
//
// 这样本地卷不再重复 upsert（省一次整表 COW 克隆），也不会为「卷内并不存在的路径」造幽灵条目。
func (s *Service) RecordBypassWrite(owner, rel string, size int64, checksum, volume string, root *storage.Root) {
	if s.deps.Volumes.IsLocal(volume) {
		s.deps.Targets.RecordBypassChecksum(owner, rel, checksum, volume)
		return
	}
	s.deps.Targets.RecordBypassWrite(owner, rel, size, checksum, volume, root)
}

// archiveToVolume 是统一转存的**共享执行核心**（单任务 / 批量 / 组归档共用）：
// 把打包流（tar[.ext][.aes]）转存到目标卷。
//
// 接线纪律（复用 master 可信卷能力，与 cloud 下载转存同款）：
//   - 目标 FS = Volumes.TransferFS（trusted.Guard(Wrap(...))）——meta 恒生成 +
//     meta 桶隔离（裸 FS 会让转存产物不是可信卷、且可触达 meta 凭据桶）；
//   - 目标键经 volume.ResolveLocation 唯一入口计算（恒 owner 前缀 + 路径安全）——与读
//     路径 ResolveUserLocation 同键空间，产物可经 /download 命中（外部卷不进本地文件
//     索引，故不出现在 /api/files 列表）；
//   - 预处理（压缩 / 可选加密）经 transfer registry 选型；
//   - 本地 staging 配额 + per-(owner,rel) 写锁（与 HTTP 上传/DAV/S3 同键空间）。
//
// t.Entries 已由调用方打开 source Reader（本函数负责关闭）；t.TotalSize 用于
// cloud_archive_max_bytes 上限校验。外部卷不消耗租户 archive 桶（产物落目标卷，容量
// 由目标卷 ReserveSpace / staging 配额管），故不做 archive 桶 Scope 预留。
func (s *Service) archiveToVolume(
	w http.ResponseWriter, r *http.Request,
	t ArchiveVolumeTransfer,
	logger *slog.Logger,
	onSuccess func(url string),
) {
	defer closeEntries(t.Entries)
	// 预处理器选型（压缩 + 可选加密，经 transfer registry）——失败已回响应。
	pre, ok := s.archivePreprocessor(w, t.Encrypt, t.Cipher, t.Compression)
	if !ok {
		return
	}
	// 目标卷解析（可信卷写视图；未装/元数据不可解析 → fail-closed）。
	// U3：本地卷现也可作转存目标（`Volumes.TransferFS` 走 `VolumeView`，
	// 本地卷返回 Guard(Wrap(NewRootFS))——trusted 对本地启用）；scheme 空按 local 归一。
	fs, scheme, vol, ok := s.deps.Volumes.TransferFS(t.TargetVolume)
	if !ok {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: "target volume not assembled"}, http.StatusNotFound)
		return
	}
	if scheme == "" {
		scheme = "local"
	}
	// NM6（与 cloud 下载转存 checkTransferVolumePreflight 一致）：secrets 是密钥管理卷
	// （secret 名禁路径分隔符），非通用转存目标——转存 rel 恒含 /，secrets:// 无法解析
	// 且写路径绕过 Secrets Manager 校验。
	if scheme == "secrets" {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: "目标卷是 secrets 密钥卷（非转存目标，请用 secretdata/普通卷）"}, http.StatusBadRequest)
		return
	}
	// 目标键：volume 唯一入口（权限门 + 路径安全 + 恒 owner 前缀），与读路径
	// resolveExternalDownload 的 ResolveUserLocation 同键空间。targetPath 为客户端由卷 URI
	// （cd/mv 同规约）归一后的 user 桶相对路径（可含子目录）；空 → 默认 archive/<name>。
	// 加密产物按 /api/archive 同口径加 .aes 后缀（`<name>.tar.gz.aes`）——密文不该伪装成
	// 可直接 gunzip 的 .tar.gz。
	archiveName := t.ArchiveName
	if t.Encrypt && !strings.HasSuffix(archiveName, ".aes") {
		archiveName += ".aes"
	}
	loc, lerr := vol.ResolveUserLocation(t.Owner, archiveTargetRel(t.TargetPath, archiveName))
	if lerr != nil {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: "invalid target path: " + lerr.Error()}, http.StatusBadRequest)
		return
	}
	rel := loc.FSPath()
	// 本地 staging 配额统一接线（与 cloud 下载转存一致；无独立配额 → 直通零回归）。
	// 打包流长度未知（size=0）由 gate 的 reservingReader 按实际读取量增量预留（#767 原生
	// 支持）——不得再在装配层额外预留（会双计）。
	fs = s.deps.Targets.StagingFS(t.Owner, fs)
	// 总量上限校验（不占租户 archive 桶 → 不做桶 Scope 预留，避免桶近满时误拒）。
	if maxBytes := s.deps.Targets.MaxArchiveBytes(); maxBytes > 0 && t.TotalSize > maxBytes {
		s.writeJSON(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("archive exceeds cloud_archive_max_bytes: %d > %d", t.TotalSize, maxBytes),
		}, http.StatusBadRequest)
		return
	}
	// 容量预检（ReserveSpace 装饰器链未实现 → ErrUnsupported 跳过）+ per-(owner,rel) 写锁（与
	// HTTP 上传/删除/改名/DAV/S3 同键空间）——转存写可信卷（主文件 + sidecar meta）与其它
	// 写面并发同 rel 会产出错位固化态。
	release, status, msg := s.archiveVolumeWriteGate(r.Context(), fs, t.Owner, rel, t.TotalSize)
	if status != 0 {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: msg}, status)
		return
	}
	defer release()
	// owner 配额结算的**前置测量**（独立审核 P1，2026-10-18）：写成功后必须按**与删除侧同一
	// Scope 解析器 + 同一物理足迹测量函数**结算——否则删除该产物时 releaseExternalDeleteQuota
	// 会做「**未曾提交的释放**」（幻影释放 ⇒ 扣同账本其它占用 ⇒ 少计 ⇒ 可超 owner 配额）。
	// 键空间与 cloud 下载转存一致：Scope 键 = `<bucket>/<path>`（即 `user/<rel>`）。
	chargeScope := s.deps.Quota.ScopeFor(t.Owner, path.Join(string(loc.Bucket()), string(loc.Path())))
	// R14 C6：**写前配额探针**（探测式：预留后立即释放，不排队）。此前本路径只做写后
	// `settleArchiveCharge`→`scope.Adjust`（永不失败）⇒ 配额已满时**仍会写入并越界**。
	// 对照 cloud 下载转存（`pkg/cloud/transfer.go` 的 ProbeReserve）同口径。
	// TotalSize 是源可见总量（上界近似）；未知（0/负）时跳过探针，由写路径自身兜底。
	if perr := probeArchiveQuota(chargeScope, t.TotalSize); perr != nil {
		s.writeJSON(w, CloudArchiveResult{
			Success: false, Message: "目标卷配额不足: " + perr.Error(),
		}, http.StatusInsufficientStorage)
		return
	}
	chargeUserRel := archiveTargetRel(t.TargetPath, archiveName)
	chargeBefore := s.archiveTargetCharge(t.TargetVolume, t.Owner, rel, chargeUserRel)
	url, terr := transfer.Execute(r.Context(), transfer.TransferRequest{
		Entries:     t.Entries,
		Preprocess:  pre,
		Destination: fs,
		Scheme:      scheme,
		Volume:      t.TargetVolume,
		Rel:         rel,
		// 写后读回校验受既有 trusted_volume.skip_verify 约束（独立复审 P1，2026-10-18）：
		// 该开关的文档（docs/config.md）与启动告警（volumes.go warnIfVerifySkipped）都宣称覆盖
		// 「下载/**转存**读侧数据校验」，但本轮新增的内核读回默认执行 ⇒ 对远端卷（OpenRead =
		// 一次全量下载）无法关闭、且文档与行为矛盾。此处接线到同一配置源（与 cloud 转存一致）。
		SkipVerify: s.deps.Targets.SkipVerify(),
	})
	// 统一转存审计（F8）：本路径是**同步 HTTP**（不经 cloud manager ⇒ 无任务级审计作用域 /
	// download·transfer span）。按备份等旁路写面的既有口径补一条服务端审计事件，保证
	// 「谁在何时把什么转存到哪个卷」可经 /api/audit 追溯。
	s.recordArchiveTransferAudit(r.Context(), t, scheme, rel, len(t.Entries), terr)
	if terr != nil {
		// 目标已存在（拒绝静默覆盖）是客户端可解冲突 → 409（非 500）。
		if errors.Is(terr, transfer.ErrExists) {
			s.writeJSON(w, CloudArchiveResult{Success: false, Message: terr.Error()}, http.StatusConflict)
			return
		}
		logger.Error("unified transfer to volume failed", "volume", t.TargetVolume, "error", terr)
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: "transfer failed: " + terr.Error()}, http.StatusInternalServerError)
		return
	}
	// 写成功 ⇒ 计入 owner 配额（差分：覆盖写用 before/after）。
	chargeAfter := s.archiveTargetCharge(t.TargetVolume, t.Owner, rel, chargeUserRel)
	SettleArchiveCharge(chargeScope, chargeBefore, chargeAfter)
	// **卷级容量记账**（独立复审 Important-1，2026-10-28）：本地卷 rootfs 无 ReserveSpace
	// ⇒ 转存写不经 CapacityFS ⇒ 卷池漏记 ⇒ 长期漂移。按与 owner Scope **同源**的实测差分
	// 结算卷池（同一 `archiveTargetCharge` 测量）。外部卷（CapacityFS 层已记）不双记。
	s.settleVolumePoolCharge(t.TargetVolume, chargeBefore, chargeAfter)
	// **派生状态登记**（R16 缺陷族修复 B）：归档转存产物写目标卷 user 桶不经 pkg/files 写内核
	// ⇒ 需显式登记 checksum 台账 + 搜索索引（checksum 从可信卷 meta sidecar 读；仅本地卷目标
	// ——外部卷 list 走卷视图，checksum 由卷自报 `sync.Entry.Checksum` 透传）。
	s.recordArchiveBypassState(t, fs, rel, chargeAfter)
	if onSuccess != nil {
		onSuccess(url)
	}
	s.writeJSON(w, CloudArchiveResult{Success: true, File: url, TaskCount: len(t.Entries)}, http.StatusOK)
}

// archivePreprocessor 按请求构造打包预处理器（压缩 + 可选加密，经 transfer registry 选型）。
// 加密密钥来自 archive.key_file（与 /api/archive 同一约定）。失败已回响应。
func (s *Service) archivePreprocessor(w http.ResponseWriter, encrypt bool, cipher, compression string) (transfer.Preprocessor, bool) {
	algo := compressx.Gzip
	if compression != "" {
		parsed, err := compressx.Parse(compression)
		if err != nil {
			s.writeJSON(w, CloudArchiveResult{Success: false, Message: err.Error()}, http.StatusBadRequest)
			return nil, false
		}
		algo = parsed
	}
	cfg := transfer.PreprocessorConfig{Compression: algo, Cipher: cipher}
	if encrypt {
		if cipher != "" {
			if _, ok := files.LookupCipher(cipher); !ok {
				s.writeJSON(w, CloudArchiveResult{Success: false, Message: fmt.Sprintf("未知加密算法 %q", cipher)}, http.StatusBadRequest)
				return nil, false
			}
		}
		key := s.deps.Targets.ArchiveKey()
		if len(key) != 32 {
			s.writeJSON(w, CloudArchiveResult{Success: false, Message: "归档加密密钥不可用（需配置 archive.key_file）"}, http.StatusBadRequest)
			return nil, false
		}
		cfg.EncryptKey = key
	}
	pre, err := transfer.New("tar", cfg)
	if err != nil {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: err.Error()}, http.StatusInternalServerError)
		return nil, false
	}
	return pre, true
}

// validateArchiveExtras 校验「仅转存外部卷支持」的选项；失败已写 400 响应。
func (s *Service) validateArchiveExtras(w http.ResponseWriter, extras ArchiveExtras) bool {
	status, msg := archiveExtrasError(extras)
	if status != 0 {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: msg}, status)
		return false
	}
	return true
}

// archiveExtrasError 是「仅转存外部卷支持」选项校验的**规则唯一实现**（响应写出在调用方）：
//   - 未指定 target_volume 时不得设置 target_path/encrypt/cipher/compression
//     （archive 桶路径走既有 createTarGz，不加密也不认目标路径）；
//   - cipher 必须与 encrypt 同用（单独指定无任何加密效果，静默忽略会让用户误信已加密）。
//
// 返回 (0, "") 表示通过；否则为应回应的状态码与文案。
func archiveExtrasError(ex ArchiveExtras) (int, string) {
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

// archiveVolumeWriteGate 是统一转存的写前门：目标卷容量预检（ReserveSpace 可选能力，
// 装饰器链未实现 → ErrUnsupported 跳过）+ 转存取锁。取锁经 Targets.Lock（唯一归一入口：
// 卷 FS 键 → 域侧锁键）——与 HTTP 上传/删除/改名/DAV/S3、cloud 下载转存**同一键空间**
// （否则共享外部卷上互不可见，同 rel 并发写产出主文件/sidecar 错位固化）。
// 返回 (release, status, msg)：status==0 表示通过（release 必非 nil）；否则按 status/msg 回包。
// 抽离以控制 archiveToVolume 的认知复杂度（gocognit 门禁）。
//
// size 语义：打包流的**真实产物长度打包前不可知**，此处以源可见总量作上界近似（预检，
// 非预留，无需释放）；权威强制在写路径本身——capacity.CapacityFS 按实际写入记账，
// 外部卷后端按自身配额 API 拒写。
func (s *Service) archiveVolumeWriteGate(ctx context.Context, fs syncpkg.FS, owner, rel string, size int64) (func(), int, string) {
	if rs, ok := fs.(syncpkg.ReserveSpace); ok {
		if err := rs.ReserveSpace(ctx, rel, size); err != nil && !errors.Is(err, syncpkg.ErrUnsupported) {
			return func() {}, http.StatusInsufficientStorage, "目标卷容量不足: " + err.Error()
		}
	}
	release, locked := s.deps.Targets.Lock(owner, rel)
	if !locked {
		return func() {}, http.StatusConflict, "目标正被其他写者占用，请稍后重试"
	}
	return release, 0, ""
}

// recordArchiveTransferAudit 记录统一转存（归档 → 外部卷）的服务端审计事件（F8）。
// 同步 HTTP 路径没有 cloud 任务级审计作用域（见 archiveToVolume），以服务端审计行补齐
// 「谁把什么转存到哪个卷」的可追溯性——与 backup 旁路写面同口径（recordBackupAudit）。
// 文案只含卷名/键/scheme 等非敏感结构信息，不含 URL/凭据。
func (s *Service) recordArchiveTransferAudit(ctx context.Context, t ArchiveVolumeTransfer, scheme, rel string, count int, terr error) {
	result, detail := auditResultSuccess, fmt.Sprintf("scheme=%s key=%s files=%d encrypt=%t", scheme, rel, count, t.Encrypt)
	if terr != nil {
		result = auditResultError
		detail += " error=" + terr.Error()
	}
	s.deps.Audit.Record(ctx, "cloud_archive", "volume", t.TargetVolume, result, detail)
}

// recordArchiveBypassState 归档转存（本地卷目标）的派生状态登记（R16 缺陷族修复 B）。
// V4：索引改由 `storage.Root` 事件自动维护 ⇒ 这里只补 checksum 台账。
func (s *Service) recordArchiveBypassState(t ArchiveVolumeTransfer, fs syncpkg.FS, rel string, size int64) {
	if !s.deps.Volumes.IsLocal(t.TargetVolume) {
		return
	}
	cs := ""
	if pv, ok := fs.(meta.Provider); ok {
		if fm, ferr := pv.FileMeta(context.Background(), rel); ferr == nil && fm != nil {
			cs = fm.TotalSHA256
		}
	}
	// 本地卷：索引已由 Root 事件（结构 + 内容写）维护 ⇒ 只补 checksum（避免重复 upsert）。
	s.deps.Targets.RecordBypassChecksum(t.Owner, rel, cs, t.TargetVolume)
}

// settleVolumePoolCharge 把归档转存产物计入**本地卷**的卷级容量池（差分；外部卷跳过——
// 其卷级记账在 FS 层 CapacityFS，双记会重复占用）。
func (s *Service) settleVolumePoolCharge(volName string, before, after int64) {
	if !s.deps.Volumes.IsLocal(volName) || after <= before {
		return
	}
	if pool := s.deps.Volumes.LocalPool(volName); pool != nil {
		pool.Adjust(before, after)
	}
}

// archiveTargetCharge 返回归档转存产物的**记账字节**：① 卷自报物理足迹优先（与删除侧
// files.physChargeFor 同一测量函数 filesPhysFootprinter.PhysFootprint）——secretdata 的
// pad/parity 逻辑不可推，两处必须同源；② 无足迹能力（多数外部卷）⇒ 回落卷内实测大小
// （与删除侧 ExternalStat 回落同档）。fsRel 是卷内完整键（`<owner>/user/<rel>`），userRel 是
// 用户桶相对路径（足迹解析用）。
func (s *Service) archiveTargetCharge(volName, owner, fsRel, userRel string) int64 {
	if n, ok := s.deps.Volumes.PhysFootprint(volName, owner, userRel); ok && n > 0 {
		return n
	}
	fs, _, _, ok := s.deps.Volumes.TransferFS(volName)
	if !ok || fs == nil {
		return 0
	}
	if e, err := fs.Stat(context.Background(), fsRel); err == nil && e != nil {
		return e.Size
	}
	return 0
}

// userRelFromFSKey 把外部卷内部键 `<owner>/user/<rel>` 还原为用户桶相对路径 `<rel>`
// （`archiveTargetCharge` 的 PhysFootprint 需要该口径；与写侧 `archiveTargetRel` 同键空间）。
// 形态不符（非 `<owner>/user/` 前缀）时原样返回，交由调用方的足迹/Stat 兜底。
func userRelFromFSKey(owner, fsKey string) string {
	prefix := owner + "/user/"
	if after, ok := strings.CutPrefix(fsKey, prefix); ok {
		return after
	}
	if rest, ok := strings.CutPrefix(fsKey, "user/"); ok {
		return rest
	}
	return fsKey
}

// archiveEntries 把源文件描述转为 transfer.SourceEntry（打开源文件作 Reader；size/mtime 填真值）。
// 打开中途失败时关闭已打开的 Reader（防句柄泄漏）。
func archiveEntries(sources []ArchiveSource) ([]transfer.SourceEntry, error) {
	out := make([]transfer.SourceEntry, 0, len(sources))
	for _, f := range sources {
		// R25：用户可见读入口（加密卷透明解密）——不得直调 root.Open。
		rc, err := rootfs.OpenUserVisibleRead(f.Root, f.Rel)
		if err != nil {
			closeEntries(out)
			return nil, err
		}
		st, serr := f.Root.Stat(f.Rel)
		var size, mtime int64
		// 用户可见（逻辑）长度：加密卷取明文长度，否则等同 Stat。
		if sv, vErr := rootfs.UserVisibleSize(f.Root, f.Rel); vErr == nil {
			size = sv
		} else if serr == nil && st != nil {
			size = st.Size()
		}
		if serr == nil && st != nil {
			mtime = st.ModTime().Unix()
		}
		out = append(out, transfer.SourceEntry{Rel: f.TarRel, Size: size, ModTime: mtime, Reader: rc})
	}
	return out, nil
}

// closeEntries 关闭所有 SourceEntry 的 Reader（io.Closer）。
func closeEntries(entries []transfer.SourceEntry) {
	for _, e := range entries {
		if e.Reader != nil {
			if rc, ok := e.Reader.(io.Closer); ok {
				rc.Close()
			}
		}
	}
}

// archiveTotalSize 汇总源文件的用户可见（逻辑）大小。
func archiveTotalSize(sources []ArchiveSource) int64 {
	var total int64
	for _, f := range sources {
		if sv, err := rootfs.UserVisibleSize(f.Root, f.Rel); err == nil {
			total += sv
		}
	}
	return total
}

// archiveTargetRel 派生转存目标在**目标卷 user 桶内**的相对路径：
//   - targetPath 空 → 默认 archive/<archive_name>；
//   - targetPath 以 "/" 结尾 → 视为目录，拼上 archiveName；
//   - 否则直接用 targetPath（去前导 "/"）。
//
// 路径安全（../ 逃逸 / 绝对路径 / 非法段）由 volume.ResolveUserLocation 唯一入口校验。
func archiveTargetRel(targetPath, archiveName string) string {
	p := strings.TrimPrefix(strings.TrimSpace(targetPath), "/")
	switch {
	case p == "":
		return relOf(archiveName)
	case strings.HasSuffix(p, "/"):
		return p + archiveName
	default:
		return p
	}
}

// relOf 派生转存目标默认路径（archive/<name>.tar.gz，卷内 user 桶相对路径；
// owner 前缀/路径安全由 volume.ResolveLocation 唯一入口拼接）。已带归档扩展名
// （`.tar.gz` / 加密产物 `.tar.gz.aes`）时不再重复追加。
func relOf(archiveName string) string {
	rel := "archive/" + archiveName
	if !strings.HasSuffix(rel, ".tar.gz") && !strings.HasSuffix(rel, ".tar.gz.aes") {
		rel += ".tar.gz"
	}
	return rel
}

// probeArchiveQuota 是归档转存的**写前配额探针**（R14 C6）：探测式（预留后立即释放、不排队）。
// 抽成函数以避免给 archiveToVolume 增加分支（gocognit 门禁）。scope 为 nil（未装配配额）
// 或 size ≤ 0（打包流真实长度不可知）时跳过 —— 由写路径自身兜底。
func probeArchiveQuota(scope *quota.Scope, size int64) error {
	if scope == nil || size <= 0 {
		return nil
	}
	return scope.ProbeReserve(size)
}

// settleArchiveCharge 把归档转存产物计入 owner 配额。before = 写入前实测、after = 写入后实测
// （覆盖写按差分，不双计）；scope 为 nil（未装配配额）或 after == before ⇒ 无操作（零回归）。
//
// **必须与删除侧同源**（独立审核 P1，2026-10-18）：删除侧 releaseExternalDeleteQuota 会从同一
// Scope 释放 physChargeFor 实测字节；若写侧不结算，那次释放就是「**未曾提交的释放**」⇒ 扣掉同
// 账本其它对象的占用 ⇒ 少计 ⇒ 后续写入沿父链检查时放行 ⇒ **可超出 owner 配额**。
//
// 导出理由：装配层既有纯函数用例（cloud_archive_quota_settle_test.go）继续驱动同一实现，
// 避免为测试保留第二份差分语义。
func SettleArchiveCharge(scope *quota.Scope, before, after int64) {
	if scope == nil || after == before {
		return
	}
	scope.Adjust(before, after)
}
