// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cloudtasks 是云下载/云归档编排核心（P1-c）的依赖契约包。
//
// 本包只声明编排核心需要的窄接口与构造期注入的 Deps，不包含编排逻辑，也不
// 依赖装配根 pkg/server；装配层负责把 *Handlers 适配成这些接口（见
// pkg/server/cloudtasks_wiring.go）。依赖映射与 S1–S4 拆分见
// docs/designs/2026-10-31-cloudtasks-deps-mapping.md。
//
// 接口命名按 S1 任务契约使用领域名词（Tasks/…）；设计文档 §5.1–§5.7 中的
// TaskStore/GroupStore/… 与之一一对应，不另设别名（同一概念只保留一个导出名）。
package cloudtasks

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/cloudfilename"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/transfer"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// Deps 是编排核心的全部装配依赖（8 字段 = 7 个窄接口 + 日志）。
//
// 字段数就是切片入边宽度：R52 的现行上限是 12，但 P1 设计目标是 ≤8，包内
// deps_test.go 用反射守住 8。确需第 9 项时拆包，禁止在构造器另塞 logger 或
// 放宽门禁。
type Deps struct {
	Tasks   Tasks
	Groups  Groups
	Tenants Tenants
	Volumes Volumes
	Quota   Quota
	Targets Targets
	Audit   Audit
	Logger  *slog.Logger
}

// Tasks 是云任务存储面（覆设计文档 §5.1）。
type Tasks interface {
	AllowPrivate() bool
	MaxBatchURLs() int
	SubmitAndStart(method, url, filename string, totalSize int64,
		syncCtx context.Context, owner string, params cloud.TaskParams) (*cloud.CloudTask, error)
	SnapshotTask(id, owner string) (*cloud.CloudTask, bool)
	SnapshotTasks(ids []string, owner string) []*cloud.CloudTask
	ListTasks(status string, offset, limit int, owner string) ([]*cloud.CloudTask, int)
	CancelTask(id, owner string) error
	DeleteTask(id, owner string) error
	ResumeTask(id string, force bool, owner string) error
}

// Groups 是云任务组面（覆设计文档 §5.2）。
//
// UpdateGroupStatus 有刷新副作用，调用顺序是语义的一部分：
// GetGroup → UpdateGroupStatus → GetGroup（owner 可见性先判），
// ListGroups("", -1, 0) → 逐个 UpdateGroupStatus → ListGroups(status, offset, limit)。
//
// DeleteGroupArchive 是组删除时的归档产物清理：归档自身的读写在 S3 才迁入本包，
// 故 S2b 期间由装配层实现（原 cloud_download_handler.go 的 deleteGroupArchive 原地
// 保留）；S3 迁入归档面后替换该委托。
type Groups interface {
	SubmitAndStartGroup(name string, urls []cloudfilename.Entry, owner string, params cloud.TaskParams) (*cloud.CloudTaskGroup, error)
	GetGroup(id, owner string) (*cloud.CloudTaskGroup, bool)
	ListGroups(status string, offset, limit int, owner string) ([]*cloud.CloudTaskGroup, int)
	CancelGroup(groupID, owner string) error
	DeleteGroup(groupID, owner string) error
	ResumeGroup(groupID string, force bool, owner string) error
	SetGroupArchiveFile(groupID, archiveFile string)
	UpdateGroupStatus(groupID string)
	DeleteGroupArchive(owner, archiveFile string) error
}

// Tenants 解析 owner 的租户（覆设计文档 §5.3）。空 owner → anonymous；失败 nil。
type Tenants interface {
	TenantFor(owner string) *storage.Tenant
}

// Volumes 是卷 ACL、转存目标 FS 与物理足迹的唯一查询面（覆设计文档 §5.4）。
type Volumes interface {
	Allowed(owner, volume string) bool
	TransferFS(volume string) (fs sync.FS, scheme string, vol volume.Volume, ok bool)
	IsLocal(volume string) bool
	LocalPool(volume string) *quota.Pool
	PhysFootprint(volume, owner, userRel string) (int64, bool)
}

// Quota 是配额 Scope 解析与容量回退账本（覆设计文档 §5.5）。
//
// 三个 Fallback 方法在装配层绑定 capacity.CategoryCloud；回退账本未装配时
// 是空操作（与既有 scope==nil && storageMgr==nil 分支等价）。
//
// 归档配额四方法（P1-c S3a）是「预留 → 写 → 对账 → 提交/回滚」的唯一取用面：装配层
// 保留既有实现（组归档与转存路径 S3b 前仍直接调用），本包只消费结果。失败以
// (status, message) 表达，响应由调用方写出，避免把 http.ResponseWriter 传进配额面。
type Quota interface {
	BucketScope(owner, bucket string) *quota.Scope
	ScopeFor(owner, rel string) *quota.Scope
	WaitTimeout() time.Duration

	ReserveFallback(ctx context.Context, n int64, timeout time.Duration) error
	ReleaseFallback(n int64)
	TryReserveFallback(n int64) error

	// ReserveArchive 预留归档输出配额（含 cloud_archive_max_bytes 上限校验）。
	// 成功返回结算句柄（nil = storageMgr 回退或未装配账本）与预留量；失败返回 status+message。
	ReserveArchive(ctx context.Context, owner string, totalSourceSize int64) (reservation *quota.Settlement, pre int64, status int, message string)
	// SettleArchive 按落盘实际对账预留（scope: Reconcile+Commit；回退: Release+TryReserve）。
	SettleArchive(root *storage.Root, rel string, reservation *quota.Settlement, pre int64, logger *slog.Logger) (actual int64, status int, message string)
	// ReleaseArchive 释放未定案的预留（O_EXCL 冲突 / 打包失败路径，幂等）。
	ReleaseArchive(reservation *quota.Settlement, pre int64)
	// RecordArchiveUsage 登记归档文件在 archive 桶 Scope 的已确认占用（actual<=0 跳过）。
	RecordArchiveUsage(owner, archiveName string, actual int64)
}

// Targets 是转存目标写面：staging gate、写锁、派生状态登记与归档策略
// （覆设计文档 §5.6）。
//
// 策略 getter（SkipVerify/MaxArchiveBytes/ArchiveKey）每次调用都必须重新读
// 配置/文件（支持热更新），不得在构造期缓存。
type Targets interface {
	StagingFS(owner string, fs sync.FS) sync.FS
	Lock(owner, fsKey string) (release func(), ok bool)
	RecordBypassChecksum(owner, rel, checksum, volume string)
	RecordBypassWrite(owner, rel string, size int64, checksum, volume string, root *storage.Root)
	SkipVerify() bool
	MaxArchiveBytes() int64
	ArchiveKey() []byte

	// 归档打包/转存六方法（P1-c S3a 临时接缝，S3b 增 ArchiveTotalSize）：实现仍在装配层
	// （tar 助手与 cloudArchiveToVolume 属 S3c 迁移面），本包消费结果；ArchiveToVolume 因
	// 执行核心尚未迁入暂保留 HTTP 参数，S3c 迁入后删除该方法。
	//
	// ValidateArchiveExtras 校验「仅转存外部卷支持」的选项，成功返回 status=0。
	ValidateArchiveExtras(extras ArchiveExtras) (status int, message string)
	// CreateArchiveTar 打包单个文件（O_EXCL）；exists=true 表示同名归档已存在（409）。
	CreateArchiveTar(srcRoot *storage.Root, srcRel, tarName string, outRoot *storage.Root, outRel string,
		logger *slog.Logger) (checksum string, exists bool, err error)
	// CreateMultiArchiveTar 打包多个文件（O_EXCL）；exists 语义同上。
	CreateMultiArchiveTar(sources []ArchiveSource, outRoot *storage.Root, outRel string,
		logger *slog.Logger) (checksum string, exists bool, err error)
	// ArchiveEntries 把源文件转成 transfer.SourceEntry（打开 Reader；失败时已关闭已打开项）。
	ArchiveEntries(sources []ArchiveSource) ([]transfer.SourceEntry, error)
	// ArchiveTotalSize 汇总源文件的**用户可见（逻辑）大小**（加密卷取明文长度）。
	// 转存路径的 TotalSize 用该口径（与 tar 条目 Size 同源），不用 Stat 物理大小。
	ArchiveTotalSize(sources []ArchiveSource) int64
	// ArchiveToVolume 执行统一转存（可信卷写视图 + owner 前缀 + staging/写锁 + 配额结算）。
	ArchiveToVolume(w http.ResponseWriter, r *http.Request, t ArchiveVolumeTransfer, logger *slog.Logger, onSuccess func(url string))
}

// Audit 是业务审计唯一入口（覆设计文档 §5.7）。
type Audit interface {
	Record(ctx context.Context, action, objectType, object, result, detail string)
}
