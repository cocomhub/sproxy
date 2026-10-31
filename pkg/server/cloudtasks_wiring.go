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
	"time"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/cloudfilename"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/server/cloudtasks"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
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

func (a *cloudTasksAdapter) SetGroupArchiveFile(groupID, archiveFile string) {
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
