// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// cloudtasks_wiring.go 是 P1-c S1 的装配层适配器：把 *Handlers 的既有能力包成
// pkg/server/cloudtasks 的 7 个窄接口。
//
// 本文件只做**薄委托**（一行一方法），不复制任何逻辑；S1 不接任何调用点，
// 编排核心迁移（S2/S3）才把 cloudtasks.Deps 构造并注入。方法语义与
// docs/designs/2026-10-31-cloudtasks-deps-mapping.md §5 逐条对应。
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/cloudfilename"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/server/cloudtasks"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/transfer"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// cloudTasksAdapter 把 *Handlers 适配为 cloudtasks 的 7 个接口。
//
// 一个类型实现全部接口是刻意的：装配点只需构造一次（`*Handlers` 是唯一实现源），
// 消费方仍按窄接口取用。生产/测试替身的分裂点在接口，不在适配器。
type cloudTasksAdapter struct {
	h *Handlers
}

// 编译期断言：适配器覆盖全部 7 个接口（S1 的验收锚点）。
var (
	_ cloudtasks.Tasks   = (*cloudTasksAdapter)(nil)
	_ cloudtasks.Groups  = (*cloudTasksAdapter)(nil)
	_ cloudtasks.Tenants = (*cloudTasksAdapter)(nil)
	_ cloudtasks.Volumes = (*cloudTasksAdapter)(nil)
	_ cloudtasks.Quota   = (*cloudTasksAdapter)(nil)
	_ cloudtasks.Targets = (*cloudTasksAdapter)(nil)
	_ cloudtasks.Audit   = (*cloudTasksAdapter)(nil)
)

// ---- Tasks ----

func (a *cloudTasksAdapter) AllowPrivate() bool { return a.h.cloudMgr.AllowPrivate() }

func (a *cloudTasksAdapter) MaxBatchURLs() int { return a.h.cloudMgr.MaxBatchURLs() }

func (a *cloudTasksAdapter) SubmitAndStart(method, url, filename string, totalSize int64,
	syncCtx context.Context, owner string, params cloud.TaskParams) (*cloud.CloudTask, error) {
	return a.h.cloudMgr.SubmitAndStart(method, url, filename, totalSize, syncCtx, owner, params)
}

func (a *cloudTasksAdapter) SnapshotTask(id, owner string) (*cloud.CloudTask, bool) {
	return a.h.cloudMgr.SnapshotTask(id, owner)
}

func (a *cloudTasksAdapter) SnapshotTasks(ids []string, owner string) []*cloud.CloudTask {
	return a.h.cloudMgr.SnapshotTasks(ids, owner)
}

func (a *cloudTasksAdapter) ListTasks(status string, offset, limit int, owner string) ([]*cloud.CloudTask, int) {
	return a.h.cloudMgr.ListTasks(status, offset, limit, owner)
}

func (a *cloudTasksAdapter) CancelTask(id, owner string) error {
	return a.h.cloudMgr.CancelTask(id, owner)
}

func (a *cloudTasksAdapter) DeleteTask(id, owner string) error {
	return a.h.cloudMgr.DeleteTask(id, owner)
}

func (a *cloudTasksAdapter) ResumeTask(id string, force bool, owner string) error {
	return a.h.cloudMgr.ResumeTask(id, force, owner)
}

// ---- Groups ----

func (a *cloudTasksAdapter) SubmitAndStartGroup(name string, urls []cloudfilename.Entry,
	owner string, params cloud.TaskParams) (*cloud.CloudTaskGroup, error) {
	return a.h.cloudMgr.SubmitAndStartGroup(name, urls, owner, params)
}

func (a *cloudTasksAdapter) GetGroup(id, owner string) (*cloud.CloudTaskGroup, bool) {
	return a.h.cloudMgr.GetGroup(id, owner)
}

func (a *cloudTasksAdapter) ListGroups(status string, offset, limit int, owner string) ([]*cloud.CloudTaskGroup, int) {
	return a.h.cloudMgr.ListGroups(status, offset, limit, owner)
}

func (a *cloudTasksAdapter) CancelGroup(groupID, owner string) error {
	return a.h.cloudMgr.CancelGroup(groupID, owner)
}

func (a *cloudTasksAdapter) DeleteGroup(groupID, owner string) error {
	return a.h.cloudMgr.DeleteGroup(groupID, owner)
}

func (a *cloudTasksAdapter) ResumeGroup(groupID string, force bool, owner string) error {
	return a.h.cloudMgr.ResumeGroup(groupID, force, owner)
}

// SetGroupArchiveFile 记录组归档引用。cloudMgr 未装配（装配层直调转存路径的测试夹具）时
// 空操作——与迁移前 `if h.cloudMgr != nil` 判等（HTTP 路径必经 GetGroup，恒已装配）。
func (a *cloudTasksAdapter) SetGroupArchiveFile(groupID, archiveFile string) {
	if a.h.cloudMgr == nil {
		return
	}
	a.h.cloudMgr.SetGroupArchiveFile(groupID, archiveFile)
}

func (a *cloudTasksAdapter) UpdateGroupStatus(groupID string) {
	a.h.cloudMgr.UpdateGroupStatus(groupID)
}

// DeleteGroupArchive 委托装配层的归档清理（归档读写在 S3 才迁入 cloudtasks，S2b 期间
// 只经此一行取用 `deleteGroupArchive`；行为与迁移前逐字一致）。
func (a *cloudTasksAdapter) DeleteGroupArchive(owner, archiveFile string) error {
	return a.h.deleteGroupArchive(owner, archiveFile)
}

// ---- Tenants ----

func (a *cloudTasksAdapter) TenantFor(owner string) *storage.Tenant {
	return a.h.tenantFor(owner)
}

// ---- Volumes ----

func (a *cloudTasksAdapter) Allowed(owner, name string) bool {
	return a.h.volumeAllowedFor(owner, name)
}

func (a *cloudTasksAdapter) TransferFS(name string) (syncpkg.FS, string, volume.Volume, bool) {
	return a.h.externalVolumeTransferFS(name)
}

func (a *cloudTasksAdapter) IsLocal(name string) bool {
	return a.h.volSet != nil && a.h.volSet.HasLocalRoot(name)
}

func (a *cloudTasksAdapter) LocalPool(name string) *quota.Pool {
	if a.h.volSet == nil {
		return nil
	}
	return a.h.volSet.Pool(name)
}

func (a *cloudTasksAdapter) PhysFootprint(name, owner, userRel string) (int64, bool) {
	return filesPhysFootprinter{h: a.h}.PhysFootprint(name, owner, userRel)
}

// ---- Quota ----

func (a *cloudTasksAdapter) BucketScope(owner, bucket string) *quota.Scope {
	return a.h.quotaBucketFor(owner, bucket)
}

func (a *cloudTasksAdapter) ScopeFor(owner, rel string) *quota.Scope {
	return a.h.quotaScopeFor(owner, rel)
}

func (a *cloudTasksAdapter) WaitTimeout() time.Duration {
	return a.h.quotaWaitTimeout
}

// ReserveFallback 使用容量回退账本（scope==nil 时的既有分支）；未装配时为空操作。
func (a *cloudTasksAdapter) ReserveFallback(ctx context.Context, n int64, timeout time.Duration) error {
	if a.h.storageMgr == nil {
		return nil
	}
	return a.h.storageMgr.WaitReserve(ctx, n, capacity.CategoryCloud, timeout)
}

func (a *cloudTasksAdapter) ReleaseFallback(n int64) {
	if a.h.storageMgr != nil {
		a.h.storageMgr.Release(n, capacity.CategoryCloud)
	}
}

func (a *cloudTasksAdapter) TryReserveFallback(n int64) error {
	if a.h.storageMgr == nil {
		return nil
	}
	return a.h.storageMgr.TryReserve(n, capacity.CategoryCloud)
}

// ---- Quota（归档，P1-c S3a 临时接缝） ----

// ReserveArchive 委托装配层的归档预留（含 cloud_archive_max_bytes 上限校验）。
func (a *cloudTasksAdapter) ReserveArchive(ctx context.Context, owner string, totalSourceSize int64) (*quota.Settlement, int64, int, string) {
	return a.h.archiveReserveQuota(ctx, owner, totalSourceSize)
}

// SettleArchive 委托装配层的归档对账（scope: Reconcile+Commit；回退: Release+TryReserve）。
func (a *cloudTasksAdapter) SettleArchive(root *storage.Root, rel string, reservation *quota.Settlement, pre int64, logger *slog.Logger) (int64, int, string) {
	return a.h.archiveSettleQuota(root, rel, reservation, pre, logger)
}

// ReleaseArchive 委托装配层的预留释放（幂等）。
func (a *cloudTasksAdapter) ReleaseArchive(reservation *quota.Settlement, pre int64) {
	releaseArchiveReservation(reservation, a.h.storageMgr, pre)
}

// RecordArchiveUsage 委托装配层的 archive 桶占用登记（状态仍在装配层，S3b 随组归档迁入）。
func (a *cloudTasksAdapter) RecordArchiveUsage(owner, archiveName string, actual int64) {
	a.h.recordArchiveUsage(owner, archiveName, actual)
}

// ---- Targets ----

func (a *cloudTasksAdapter) StagingFS(owner string, fs syncpkg.FS) syncpkg.FS {
	return a.h.stagingQuotaFS(owner, fs)
}

func (a *cloudTasksAdapter) Lock(owner, fsKey string) (func(), bool) {
	return a.h.transferWriteLock(owner, fsKey)
}

func (a *cloudTasksAdapter) RecordBypassChecksum(owner, rel, checksum, volume string) {
	a.h.fileService().RecordBypassChecksum(owner, rel, checksum, volume)
}

func (a *cloudTasksAdapter) RecordBypassWrite(owner, rel string, size int64, checksum, volume string, root *storage.Root) {
	a.h.fileService().RecordBypassWrite(owner, rel, size, checksum, volume, root)
}

func (a *cloudTasksAdapter) SkipVerify() bool {
	return a.h.verifySkipped()
}

func (a *cloudTasksAdapter) MaxArchiveBytes() int64 {
	if a.h.cfgPtr == nil {
		return 0
	}
	cfg := a.h.cfgPtr.Load()
	if cfg == nil {
		return 0
	}
	return cfg.CloudArchiveMaxBytes
}

func (a *cloudTasksAdapter) ArchiveKey() []byte {
	if a.h.cfgPtr == nil {
		return nil
	}
	return a.h.archiveKey()
}

// ---- Targets（归档，P1-c S3a 临时接缝） ----

// ValidateArchiveExtras 委托装配层的「仅转存外部卷支持」选项校验（规则只有一份）。
func (a *cloudTasksAdapter) ValidateArchiveExtras(extras cloudtasks.ArchiveExtras) (int, string) {
	return archiveExtrasError(archiveExtras{
		TargetVolume: extras.TargetVolume, TargetPath: extras.TargetPath,
		Encrypt: extras.Encrypt, Cipher: extras.Cipher, Compression: extras.Compression,
	})
}

// CreateArchiveTar 委托装配层的单文件 tar.gz 打包（O_EXCL）。
func (a *cloudTasksAdapter) CreateArchiveTar(srcRoot *storage.Root, srcRel, tarName string, outRoot *storage.Root, outRel string, logger *slog.Logger) (string, bool, error) {
	checksum, err := createTarGz(srcRoot, srcRel, tarName, outRoot, outRel, logger)
	if errors.Is(err, errArchiveExists) {
		return "", true, nil
	}
	return checksum, false, err
}

// CreateMultiArchiveTar 委托装配层的多文件 tar.gz 打包（O_EXCL）。
func (a *cloudTasksAdapter) CreateMultiArchiveTar(sources []cloudtasks.ArchiveSource, outRoot *storage.Root, outRel string, logger *slog.Logger) (string, bool, error) {
	created := false
	checksum, err := createMultiFileTarGz(archiveFilesFrom(sources), outRoot, outRel, logger, &created)
	if errors.Is(err, errArchiveExists) {
		return "", true, nil
	}
	if err != nil {
		return "", false, err
	}
	return checksum, false, nil
}

// ArchiveEntries 委托装配层打开源文件 Reader（用户可见读入口，R25）。
func (a *cloudTasksAdapter) ArchiveEntries(sources []cloudtasks.ArchiveSource) ([]transfer.SourceEntry, error) {
	return archiveEntries(archiveFilesFrom(sources))
}

// ArchiveTotalSize 委托装配层的用户可见总量汇总（与 tar 条目 Size 同口径）。
func (a *cloudTasksAdapter) ArchiveTotalSize(sources []cloudtasks.ArchiveSource) int64 {
	return archiveTotalSize(archiveFilesFrom(sources))
}

// ArchiveToVolume 委托装配层的统一转存执行核心（S3b 迁入后删除本方法）。
func (a *cloudTasksAdapter) ArchiveToVolume(w http.ResponseWriter, r *http.Request, t cloudtasks.ArchiveVolumeTransfer, logger *slog.Logger, onSuccess func(url string)) {
	a.h.cloudArchiveToVolume(w, r, t, logger, onSuccess)
}

// archiveFilesFrom 把编排核心的源文件描述适配为装配层的 fileWithRelPath（tar 助手入参）。
func archiveFilesFrom(sources []cloudtasks.ArchiveSource) []fileWithRelPath {
	out := make([]fileWithRelPath, 0, len(sources))
	for _, src := range sources {
		out = append(out, fileWithRelPath{root: src.Root, rel: src.Rel, tarRel: src.TarRel})
	}
	return out
}

// ---- Audit ----

func (a *cloudTasksAdapter) Record(ctx context.Context, action, objectType, object, result, detail string) {
	a.h.RecordAudit(ctx, AuditEvent{
		Action:     action,
		ObjectType: objectType,
		Object:     object,
		Result:     result,
		Detail:     detail,
	})
}
