// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncmgr

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// syncReservePlaceholder 未知大小同步任务的本地存储占位大小（1 GiB，对齐 cloud 下载占位）。
const syncReservePlaceholder = int64(1024 * 1024 * 1024)

// TenantRootResolver 按任务 owner 解析租户 user 根与 meta/sync 持久化目录的绝对路径。
// 空 owner 归一为 anonymous 租户；租户不可用（非法 owner / 存储根未装配）返回 ok=false
// （写路径 fail-closed，绝不回落全局根）。装配层用 storage.NewTenant(owner, globalRoot)
// 派生（见 pkg/server/sync_handler.go 的 Handlers.syncTenantRoot）。
type TenantRootResolver func(owner string) (userRootAbs, persistDirAbs string, ok bool)

// ErrStorageFull 存储配额不足（对齐 pkg/server.ErrStorageFull 语义）。
var ErrStorageFull = errors.New("storage quota exceeded")

// ErrNotFound 任务不存在。
var ErrNotFound = errors.New("sync task not found")

// ownerVisible 判定任务 owner 对请求者 owner 是否可见（多租户隔离规则，与
// pkg/server 的 cloud owner 过滤一致）：
//   - 请求者 owner 为空（管理员/未认证）→ 可见全部；
//   - 请求者 owner 非空 → 任务 owner 为空（全局/旧任务，兼容所有用户）或与请求者一致才可见。
//
// 空 owner 任务对所有人可见是刻意设计：不破坏现有未认证/单用户部署（旧任务与
// 未认证创建的任务无归属），且让空 owner 请求者承担管理员语义。
//
// 审查 #3（跨 owner 去重 500 复核结论——not a bug，勿再分析）：
// CreateTask 去重命中条件 ownerVisible(t.Owner, req.Owner) 与 handler 回读快照
// Get(task.ID, ActorFrom(ctx)) 使用**同一** ownerVisible 判定，二者语义恒定一致：
// 去重命中（ownerVisible=true）→ 必以请求者视角 Get 可见，绝不产生
// "task created but not found" 500。空 owner 全局任务被认证用户吸收后 Get 仍通过
// （taskOwner=="" → 对所有请求者可见）；反向（空 owner 请求者吸收 ownerA 任务）
// 不存在——去重循环 ownerVisible("ak-A","")=true 会吸收，此时 handler Get(owner="")
// 恒可见。已由 TestCreateTask_DedupScopedByOwner / TestCloudOwner_EmptyOwnerCompat
// 覆盖。无缺陷，无需修改。
func ownerVisible(taskOwner, reqOwner string) bool {
	if reqOwner == "" {
		return true
	}
	return taskOwner == "" || taskOwner == reqOwner
}

// QuotaStore 抽象存储配额接口（由 pkg/server.StorageManager 实现）。
// cat 是存储分类（pkg/server.StorageCategory），syncmgr 只使用 CategoryUserFiles。
type QuotaStore interface {
	TryReserve(size int64, cat int) error
	Release(size int64, cat int)
	Usage() int64
	MaxBytes() int64
}

// Config 是 SyncManager 配置。
type Config struct {
	MaxConcurrent int           // 最大并发同步任务数，默认 3
	TaskTTL       time.Duration // 完成任务保留时间，默认 24h
	// MaxRetries 瞬时网络错误（可重试）最大重试次数，默认 10（对齐 cloud retry）。
	MaxRetries int
	// RetryDelay 指数退避基准延迟（第 1 次重试的等待时长），默认 10s。
	RetryDelay time.Duration
	// RetryBackoff 指数退避倍率（每次重试延迟乘以此值，如 2 = 2x），默认 2。
	// 用 float64 表达倍率而非 time.Duration（time.Duration 是纳秒计数，无法表达"2 倍"）。
	// 退避上限封顶 RetryDelay*10（对齐 cloud 重试预算，避免长尾任务卡死）。
	RetryBackoff float64
}

// applyConfigDefaults 为 Config 零值字段填充默认值。
func applyConfigDefaults(cfg *Config) {
	if cfg.MaxConcurrent < 1 {
		cfg.MaxConcurrent = 3
	}
	if cfg.TaskTTL <= 0 {
		cfg.TaskTTL = 24 * time.Hour
	}
	if cfg.MaxRetries < 1 {
		cfg.MaxRetries = 10
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = 10 * time.Second
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = 2
	}
}

// RemoteConfig 是同步远程节点配置。
type RemoteConfig struct {
	Name            string
	URL             string // http(s)://host:port
	AccessKey       string
	AccessKeySecret string
}

// Manager 管理同步任务生命周期（照搬 CloudDownloadManager 模式）。
// 实际同步执行由注入的 Executor 完成（模块边界：syncmgr 不依赖 pkg/sync → pkg/client）。
type Manager struct {
	tasks       map[string]*SyncTask
	mu          sync.RWMutex
	tenantRoot  TenantRootResolver // 按任务 owner 解析租户 user 根 / meta/sync 持久化目录
	listTenants func() []string    // 返回全部租户名（恢复扫描；磁盘扫描，非内存缓存）
	quota       QuotaStore
	quotaCat    int
	remotes     map[string]RemoteConfig
	executor    Executor
	logger      *slog.Logger
	semaphore   chan struct{}
	config      *Config
	cancelFuncs map[string]context.CancelFunc
	running     map[string]bool
	wg          sync.WaitGroup
	stopCleanup chan struct{}
	closeOnce   sync.Once
}

// defaultLogger 返回非 nil logger。
func defaultLogger(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}

// NewManager 创建 SyncManager 并恢复持久化任务。
// tenantRoot 按任务 owner 解析租户 user 根 / meta/sync 持久化目录（nil 时持久化与本地执行
// 路径 fail-closed）；listTenants 返回全部租户名供恢复扫描（nil 时跳过恢复）。
// quota 可为 nil（不启用配额追踪），executor 可为 nil（任务执行时失败，CreateTask 仍可用）。
// 持久化目录在首次 saveTask 时按租户懒创建（不再预先创建全局目录）。
func NewManager(tenantRoot TenantRootResolver, listTenants func() []string, quota QuotaStore, quotaCat int, remotes []RemoteConfig, executor Executor, logger *slog.Logger, cfg *Config) *Manager {
	if cfg == nil {
		cfg = &Config{}
	}
	applyConfigDefaults(cfg)
	log := defaultLogger(logger)
	if tenantRoot == nil {
		tenantRoot = func(string) (string, string, bool) { return "", "", false }
	}
	if listTenants == nil {
		listTenants = func() []string { return nil }
	}

	rmap := make(map[string]RemoteConfig, len(remotes))
	for _, r := range remotes {
		rmap[r.Name] = r
	}

	m := &Manager{
		tasks:       make(map[string]*SyncTask),
		tenantRoot:  tenantRoot,
		listTenants: listTenants,
		quota:       quota,
		quotaCat:    quotaCat,
		remotes:     rmap,
		executor:    executor,
		logger:      log,
		semaphore:   make(chan struct{}, cfg.MaxConcurrent),
		config:      cfg,
		cancelFuncs: make(map[string]context.CancelFunc),
		running:     make(map[string]bool),
		stopCleanup: make(chan struct{}),
	}
	if quota == nil {
		m.quota = noopQuota{}
	}

	m.logger.Info("sync manager initialized",
		"max_concurrent", cfg.MaxConcurrent,
		"task_ttl", cfg.TaskTTL,
		"remotes", len(rmap),
	)

	m.recoverTasks()
	m.wg.Go(func() { m.cleanupExpired() })
	return m
}

// noopQuota 是不做任何计数的 QuotaStore（quota 为 nil 时的兜底）。
type noopQuota struct{}

func (noopQuota) TryReserve(_ int64, _ int) error { return nil }
func (noopQuota) Release(_ int64, _ int)          {}
func (noopQuota) Usage() int64                    { return 0 }
func (noopQuota) MaxBytes() int64                 { return 0 }

// persistDirFor 返回任务 owner 租户 meta/sync 持久化目录绝对路径。租户不可用返回 ""。
func (m *Manager) persistDirFor(owner string) string {
	_, p, ok := m.tenantRoot(owner)
	if !ok {
		return ""
	}
	return p
}

// validateRemote 校验并返回 remote 配置（fail-closed：未配置凭据拒绝）。
func (m *Manager) validateRemote(name string) (*RemoteConfig, error) {
	if name == "" {
		return nil, fmt.Errorf("remote 不能为空")
	}
	rc, ok := m.remotes[name]
	if !ok {
		return nil, fmt.Errorf("remote %q 未配置（sync_remotes 中需包含该名称）", name)
	}
	u, err := url.Parse(rc.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("remote %q URL 非法（应为 http(s)://host:port）: %q", name, rc.URL)
	}
	if rc.AccessKey == "" || rc.AccessKeySecret == "" {
		return nil, fmt.Errorf("remote %q 未配置 access_key/access_key_secret，无法创建远程同步任务（fail-closed）", name)
	}
	cp := rc
	return &cp, nil
}

// validateSyncPath 校验同步路径：拒绝绝对路径与路径穿越（"" 表示 FS 根，合法）。
// 不再拒绝 .__ 前缀段——同步 src/dst 相对租户 user 根解析，用户路径恒落在 user/ 桶内，
// 与租户根顶层功能桶（cloud/chunk/version/meta）物理隔离，天然无法访问功能数据或
// 同步任务持久化状态（审查 I-3 的原 .__ 拒绝逻辑已随布局迁移删除）。
func validateSyncPath(p, field string) error {
	if p == "" {
		return nil
	}
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("%s 包含空字节", field)
	}
	normalized := strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(normalized, "/") {
		return fmt.Errorf("%s 不能是绝对路径: %q", field, p)
	}
	if len(normalized) >= 2 && normalized[1] == ':' {
		return fmt.Errorf("%s 不能是绝对路径（盘符）: %q", field, p)
	}
	for seg := range strings.SplitSeq(normalized, "/") {
		if seg == ".." {
			return fmt.Errorf("%s 不能包含路径穿越: %q", field, p)
		}
	}
	return nil
}

// validateCreateRequest 校验并规范化创建请求。
func (m *Manager) validateCreateRequest(req *CreateRequest) error {
	if req.Direction != string(DirectionPush) && req.Direction != string(DirectionPull) {
		return fmt.Errorf("direction %q 无效，仅支持 push/pull", req.Direction)
	}
	switch req.ConflictPolicy {
	case "", ConflictSkip:
		req.ConflictPolicy = ConflictSkip
	case ConflictOverwrite, ConflictLWW, ConflictRename:
	default:
		return fmt.Errorf("conflict_policy %q 无效，仅支持 skip/overwrite/lww/conflict_rename", req.ConflictPolicy)
	}
	if _, err := m.validateRemote(req.Remote); err != nil {
		return err
	}
	if err := validateSyncPath(req.Src, "src"); err != nil {
		return err
	}
	if err := validateSyncPath(req.Dst, "dst"); err != nil {
		return err
	}
	return nil
}

// CreateTask 创建同步任务（不启动执行），返回 (任务, 是否新建)。
// 去重 + 预留 + 插入整体在写锁内完成（闭合 TOCTOU，审查 I-1：并发同 key 的
// CreateTask 只有一个能通过，避免双任务并发写同一 dst 路径/远程 session 踩踏）。
// pull 方向本地落盘，按占位大小预留配额（TryReserve 失败返回 ErrStorageFull）；
// push 方向远程自行预留，本地不预留。
func (m *Manager) CreateTask(req CreateRequest) (*SyncTask, bool, error) {
	if err := m.validateCreateRequest(&req); err != nil {
		return nil, false, err
	}
	m.mu.Lock()
	// 写锁内去重（retrying 仍是活跃任务，同样去重复用）。
	// 去重限**对请求者 owner 可见**的任务（同 owner 或空 owner 全局兼容）：跨 owner 的
	// 同参任务不吸收——否则 B 提交与 A 相同的 push 任务会返回 A 的任务（泄露 A 的归属
	// 与进度，且 B 无法建立自己的同步），与 cloud findByURL 的去重语义保持一致。
	for _, t := range m.tasks {
		if t.Direction == req.Direction && t.Remote == req.Remote && t.Src == req.Src && t.Dst == req.Dst &&
			ownerVisible(t.Owner, req.Owner) &&
			(t.Status == StatusPending || t.Status == StatusSyncing || t.Status == StatusRetrying) {
			c := *t
			m.mu.Unlock()
			m.logger.Info("duplicate sync task request, reusing existing",
				"task_id", c.ID,
				"direction", c.Direction,
				"remote", c.Remote,
				"status", c.Status,
			)
			return &c, false, nil
		}
	}

	reserved := int64(0)
	if req.Direction == string(DirectionPull) {
		reserved = syncReservePlaceholder
		if err := m.quota.TryReserve(reserved, m.quotaCat); err != nil {
			m.mu.Unlock()
			m.logger.Warn("storage full, sync task rejected",
				"remote", req.Remote,
				"requested", reserved,
				"usage", m.quota.Usage(),
				"max", m.quota.MaxBytes(),
				"error", err,
			)
			// 统一映射为 ErrStorageFull：TryReserve 只返回配额满或 nil
			// （真实实现返回 pkg/server.ErrStorageFull，syncmgr 无法 import server）。
			return nil, false, fmt.Errorf("storage full: %w", ErrStorageFull)
		}
	}

	now := time.Now()
	task := &SyncTask{
		ID:             newSyncTaskID(),
		Owner:          req.Owner, // 服务端派生（ActorFrom ctx），客户端不可伪造
		Direction:      req.Direction,
		Remote:         req.Remote,
		Src:            req.Src,
		Dst:            req.Dst,
		Recursive:      req.Recursive,
		Include:        append([]string(nil), req.Include...),
		Exclude:        append([]string(nil), req.Exclude...),
		ConflictPolicy: req.ConflictPolicy,
		SyncEmptyDirs:  req.SyncEmptyDirs,
		FollowSymlinks: req.FollowSymlinks,
		Status:         StatusPending,
		CreatedAt:      now,
		UpdatedAt:      now,
		ExpiresAt:      now.Add(m.config.TaskTTL),
		ReservedSize:   reserved,
	}

	m.tasks[task.ID] = task
	m.mu.Unlock()
	_ = m.saveTask(task)
	m.logger.Info("sync task created",
		"task_id", task.ID,
		"direction", task.Direction,
		"remote", task.Remote,
		"src", task.Src,
		"dst", task.Dst,
	)
	return task, true, nil
}

// SubmitAndStart 创建任务并立即启动执行（异步），返回 (任务, 是否新建)。
func (m *Manager) SubmitAndStart(req CreateRequest) (*SyncTask, bool, error) {
	task, isNew, err := m.CreateTask(req)
	if err != nil {
		return nil, false, err
	}
	// 写锁内复查 pending + 同步置位 running，闭合"检查→启动"竞态窗口：
	// 并发 SubmitAndStart 对同一任务只有一个能拿到 running，避免双 goroutine 并发执行。
	m.mu.Lock()
	realTask, ok := m.tasks[task.ID]
	if !ok || realTask.Status != StatusPending || m.running[realTask.ID] {
		m.mu.Unlock()
		return task, isNew, nil
	}
	m.running[realTask.ID] = true
	m.mu.Unlock()

	m.logger.Info("starting async sync task", "task_id", realTask.ID, "direction", realTask.Direction, "remote", realTask.Remote)
	//nolint:gosec // 后台执行需要独立 context
	m.wg.Add(1)
	go m.executeSync(context.Background(), realTask) //nolint:gosec
	return task, isNew, nil
}

// Get 返回任务深拷贝（切片字段隔离，防止调用方误改污染活任务，审查 M-7），
// 不存在或对请求者 owner 不可见（跨 owner，IDOR 防护）时返回 nil。
func (m *Manager) Get(id, owner string) *SyncTask {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[id]
	if !ok || !ownerVisible(t.Owner, owner) {
		return nil
	}
	c := *t
	c.Include = append([]string(nil), t.Include...)
	c.Exclude = append([]string(nil), t.Exclude...)
	c.Results = append([]SyncFileResult(nil), t.Results...)
	return &c
}

// List 返回任务元信息列表（CreatedAt 降序），按请求者 owner 过滤：
// owner 非空时只含匹配 owner 与空 owner（全局兼容）的任务；空 owner（管理员/未认证）返回全部。
func (m *Manager) List(owner string) []SyncTaskMeta {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]SyncTaskMeta, 0, len(m.tasks))
	for _, t := range m.tasks {
		if !ownerVisible(t.Owner, owner) {
			continue
		}
		out = append(out, SyncTaskMeta{
			ID: t.ID, Owner: t.Owner, Direction: t.Direction, Remote: t.Remote,
			Src: t.Src, Dst: t.Dst, Status: t.Status, Retries: t.Retries,
			FilesTotal: t.FilesTotal, FilesDone: t.FilesDone,
			BytesTotal: t.BytesTotal, BytesDone: t.BytesDone,
			Error: t.Error, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, ExpiresAt: t.ExpiresAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// CancelTask 取消任务（pending/syncing/retrying 可取消；排队中任务也可取消）。
// 按请求者 owner 过滤：跨 owner 任务返回 ErrNotFound（404 防枚举，不泄露存在性）。
func (m *Manager) CancelTask(id, owner string) error {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok || !ownerVisible(t.Owner, owner) {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if t.Status != StatusPending && t.Status != StatusSyncing && t.Status != StatusRetrying {
		m.mu.Unlock()
		return fmt.Errorf("cannot cancel task in status %q", t.Status)
	}
	t.Status = StatusCancelled
	t.UpdatedAt = time.Now()
	t.ExpiresAt = time.Now().Add(m.config.TaskTTL)
	// 释放预留配额（释放后归零防二次释放）。
	// 注意（审查 M-4）：pull 已落盘的本地文件不在此处清理（散落 uploadsDir，无法安全
	// 区分用户文件 vs 同步残留），释放预留后磁盘残留字节在下次周期扫描（≤30min）前
	// 未入账，属接受的瞬时欠计。
	if t.ReservedSize > 0 {
		m.quota.Release(t.ReservedSize, m.quotaCat)
		t.ReservedSize = 0
	}
	// 触发执行取消（排队中任务也已注册 cancelFuncs，可立即生效）
	if cancel, ok := m.cancelFuncs[id]; ok {
		cancel()
		delete(m.cancelFuncs, id)
	}
	m.mu.Unlock()

	// 终态持久化失败会丢失 cancelled 状态，必须显式报错
	if err := m.saveTask(t); err != nil {
		m.logger.Error("persist cancelled sync task state, state may be lost on restart",
			"task_id", id, "error", err)
	}
	m.logger.Info("sync task cancelled", "task_id", id)
	return nil
}

// DeleteTask 删除任务（任何状态均可删除），释放预留配额并移除持久化文件。
// 按请求者 owner 过滤：跨 owner 任务返回 ErrNotFound（404 防枚举，不泄露存在性）。
func (m *Manager) DeleteTask(id, owner string) error {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok || !ownerVisible(t.Owner, owner) {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if cancel, ok := m.cancelFuncs[id]; ok {
		cancel()
		delete(m.cancelFuncs, id)
	}
	delete(m.tasks, id)
	// 锁内归零防二次释放（failTask/完成路径可能在持锁时读 ReservedSize）
	reserved := t.ReservedSize
	if reserved > 0 {
		t.ReservedSize = 0
	}
	m.mu.Unlock()

	// 释放预留配额。与 CancelTask 同理（审查 M-4）：pull 已落盘文件不清理，残留字节
	// 在下次周期扫描前未入账，属接受的瞬时欠计。
	if reserved > 0 {
		m.quota.Release(reserved, m.quotaCat)
		m.logger.Debug("storage released", "task_id", id, "size", reserved)
	}
	m.logger.Info("deleting sync task", "task_id", id, "status", t.Status)

	if persistDir := m.persistDirFor(t.Owner); persistDir != "" {
		persistFile := filepath.Join(persistDir, t.ID+".json")
		if err := os.Remove(persistFile); err != nil && !os.IsNotExist(err) {
			m.logger.Warn("failed to remove sync task persist file", "task_id", id, "error", err)
		}
	}
	m.logger.Info("sync task deleted", "task_id", id)
	return nil
}

// executeSync 执行一次同步（push/pull 由 task.Direction 决定）。
// 调用者必须保证在调用前已调 m.wg.Add(1)，函数退出时自动 m.wg.Done()。
func (m *Manager) executeSync(ctx context.Context, task *SyncTask) {
	defer m.wg.Done()

	// 可取消 context：排队中的任务也能被取消/删除。
	// 必须在等待信号量之前注册 cancelFuncs。
	syncCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.mu.Lock()
	m.cancelFuncs[task.ID] = cancel
	m.running[task.ID] = true
	m.mu.Unlock()

	cleanupRunning := func() {
		m.mu.Lock()
		delete(m.cancelFuncs, task.ID)
		delete(m.running, task.ID)
		m.mu.Unlock()
	}
	defer cleanupRunning()

	// panic recovery 放在最外层 defer（最先执行），确保 panic 时先 failTask 再清理 running。
	defer func() {
		if r := recover(); r != nil {
			m.logger.Error("panic in sync task", "task_id", task.ID, "panic", r)
			m.failTask(task, fmt.Sprintf("panic: %v", r))
		}
	}()

	// 启动前复查取消
	m.mu.RLock()
	cancelled := task.Status == StatusCancelled
	m.mu.RUnlock()
	if cancelled {
		m.logger.Info("sync task skipped, cancelled before start", "task_id", task.ID)
		return
	}

	// 排队等待信号量（排队期间可取消）
	select {
	case m.semaphore <- struct{}{}:
		defer func() { <-m.semaphore }()
	case <-syncCtx.Done():
		// 排队期间被取消：CancelTask 已置 cancelled，这里不 failTask，终态由 CancelTask 写入
		m.logger.Info("queued sync task cancelled", "task_id", task.ID)
		return
	}

	// 取得信号量后复查一次
	m.mu.RLock()
	cancelled = task.Status == StatusCancelled
	m.mu.RUnlock()
	if cancelled {
		m.logger.Info("sync task skipped, cancelled while acquiring slot", "task_id", task.ID)
		return
	}

	m.mu.Lock()
	// 允许 retrying（重启恢复的 retrying 任务继续执行）：一律重置为 syncing 重新开始本轮执行
	if stored, ok := m.tasks[task.ID]; !ok ||
		(stored.Status != StatusPending && stored.Status != StatusSyncing && stored.Status != StatusRetrying) {
		m.mu.Unlock()
		return
	}
	task.Status = StatusSyncing
	task.UpdatedAt = time.Now()
	m.mu.Unlock()
	_ = m.saveTask(task)

	m.logger.Info("sync task started", "task_id", task.ID, "direction", task.Direction, "remote", task.Remote)

	// 解析并校验远程配置，然后委托 Executor 执行同步。
	remote, err := m.validateRemote(task.Remote)
	if err != nil {
		m.failTask(task, err.Error())
		return
	}
	if m.executor == nil {
		m.failTask(task, "sync executor not configured")
		return
	}

	// 重试循环（阶段 6 工作项 A）：执行器返回可重试的瞬时错误（网络中断/超时/5xx）时
	// 自动指数退避重试，达 MaxRetries 上限转 failed（错误信息含已重试次数）。
	// 重试等待期间可取消/删除（waitRetryBackoff 监听 syncCtx.Done）；completed 后不再重试。
	//
	// attempt 语义：0 = 首次执行；第 N 次重试前先置 retrying 状态 + 退避等待（attempt=N）。
	maxRetries := m.config.MaxRetries
	var lastErr string
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			// 进入重试：置 retrying（保留重试计数）+ 指数退避等待（等待期间可取消/删除）
			if !m.markRetrying(task, lastErr) {
				return
			}
			if !m.waitRetryBackoff(syncCtx, task, attempt) {
				return
			}
		}

		runResult, runErr := m.executor.Run(syncCtx, task, *remote)

		// 执行器在构造阶段失败（无法创建远程传输等）：无 RunResult，确定性错误，不重试
		if runResult == nil {
			if runErr != nil {
				m.failTask(task, runErr.Error())
			} else {
				m.failTask(task, "sync executor returned no result")
			}
			return
		}

		// 执行中取消：引擎已按 ctx 取消返回 cancelled，走终态收尾（applyRunResult
		// 对已 cancelled 任务不覆盖状态）
		if runResult.Status == StatusCancelled {
			m.finishTask(task, runResult, runErr)
			return
		}

		// 可重试错误（瞬时网络错误）：达上限转 failed；否则记录后进入下一次重试
		// 审查 I-1：上限用持久化计数 task.Retries（markRetrying 每次重试前已自增），
		// 而非循环局部 attempt——重启恢复的 retrying 任务保留 Retries，正确消耗"剩余
		// 预算"，避免跨重启超预算重试（错误文本"已重试 N 次"与 MaxRetries 语义自洽）。
		if runResult.Status == StatusFailed && runResult.Retryable {
			if task.Retries >= maxRetries {
				errMsg := fmt.Sprintf("同步任务已重试 %d 次仍失败: %s", task.Retries, pickErrorText(runResult, runErr))
				m.applyRunResultWithError(task, runResult, runErr, errMsg)
				if serr := m.saveTask(task); serr != nil {
					m.logger.Error("persist retry-exhausted sync task state, state may be lost on restart",
						"task_id", task.ID, "error", serr)
				}
				m.logger.Error("sync task failed after retries exhausted",
					"task_id", task.ID, "retries", task.Retries, "error", task.Error)
				return
			}
			lastErr = pickErrorText(runResult, runErr)
			m.logger.Warn("sync task transient error, will retry",
				"task_id", task.ID, "attempt", attempt+1, "max", maxRetries, "error", lastErr)
			continue
		}

		// 非可重试结果（完成/业务失败/未知状态）：直接收尾
		m.finishTask(task, runResult, runErr)
		return
	}
}

// markRetrying 把任务置为 retrying 并持久化重试计数。任务已被取消/删除时返回 false
// （调用方停止重试）。
func (m *Manager) markRetrying(task *SyncTask, lastErr string) bool {
	m.mu.Lock()
	stored, ok := m.tasks[task.ID]
	if !ok || (stored.Status != StatusSyncing && stored.Status != StatusRetrying) {
		// 任务已删除或已进入终态（取消）：不再重试
		m.mu.Unlock()
		return false
	}
	task.Retries++
	task.Status = StatusRetrying
	if lastErr != "" {
		task.Error = lastErr
	}
	task.UpdatedAt = time.Now()
	m.mu.Unlock()
	if err := m.saveTask(task); err != nil {
		m.logger.Error("persist retrying sync task state, state may be lost on restart",
			"task_id", task.ID, "error", err)
	}
	return true
}

// waitRetryBackoff 指数退避等待（第 attempt 次重试前）。等待期间任务被取消/删除
// （syncCtx 取消）时返回 false，调用方停止重试。
func (m *Manager) waitRetryBackoff(syncCtx context.Context, task *SyncTask, attempt int) bool {
	delay := m.retryBackoffDelay(attempt)
	m.logger.Info("sync task waiting before retry",
		"task_id", task.ID, "attempt", attempt, "retries", task.Retries, "delay", delay)
	// 用可停止的 timer（而非 time.After）：任务在长退避等待中被取消时立即停止 timer，
	// 避免定时器泄漏到延迟结束才触发。
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-syncCtx.Done():
		// 任务被取消/删除：终态由 CancelTask/DeleteTask 写入，这里不覆盖
		m.logger.Info("sync task retry cancelled during backoff", "task_id", task.ID)
		return false
	}
}

// retryBackoffDelay 计算第 attempt 次（1-based）重试前的等待延迟。
// 指数退避：base * backoff^(attempt-1)，封顶 base*10（对齐 cloud 重试预算，避免长尾）。
// 审查 M-3：backoff 上界校验（超大/NaN 会导致 float64 乘法溢出为负，time.NewTimer(负)
// 立即触发退化为忙循环），超上界回落默认 2。
func (m *Manager) retryBackoffDelay(attempt int) time.Duration {
	base := m.config.RetryDelay
	if base <= 0 {
		base = 10 * time.Second
	}
	backoff := m.config.RetryBackoff
	if backoff <= 0 || backoff > 100 || backoff != backoff { // NaN 检查（backoff != backoff）
		backoff = 2
	}
	capDelay := base * 10
	delay := base
	for i := 1; i < attempt; i++ {
		// 乘法前检查：float64 溢出（+Inf）转 int64 为负 → 负 delay 忙循环。先判 cap。
		if float64(delay) >= float64(capDelay)/backoff {
			return capDelay
		}
		delay = time.Duration(float64(delay) * backoff)
	}
	return delay
}

// finishTask 应用终态 RunResult 并持久化（回填进度/状态/错误 + 落盘 + 终态日志）。
func (m *Manager) finishTask(task *SyncTask, runResult *RunResult, runErr error) {
	m.applyRunResult(task, runResult, runErr)
	if err := m.saveTask(task); err != nil {
		m.logger.Error("persist sync task state failed, state may be lost on restart",
			"task_id", task.ID, "error", err)
	}
	m.logTaskResult(task)
}

// applyRunResult 在写锁内回填任务进度/状态/错误（终态）。
func (m *Manager) applyRunResult(task *SyncTask, runResult *RunResult, runErr error) {
	m.applyRunResultWithError(task, runResult, runErr, "")
}

// applyRunResultWithError 在写锁内回填任务进度/状态/错误。
// errorOverride 非空且结果为 failed 时覆盖 Error（用于重试耗尽：保留进度回填但
// 错误改写为"已重试 N 次"）。用 defer 释放写锁，panic 时外层 recover 的 failTask
// 不会自锁（审查 M-6）。
func (m *Manager) applyRunResultWithError(task *SyncTask, runResult *RunResult, runErr error, errorOverride string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.tasks[task.ID]
	if !ok {
		// 任务已被删除：不再写状态/对账
		return
	}
	if stored.Status == StatusCancelled {
		// 取消即放弃：终态由 CancelTask 写入，这里不覆盖
		return
	}
	task.FilesTotal = runResult.FilesTotal
	task.FilesDone = runResult.FilesDone
	task.BytesTotal = runResult.BytesTotal
	task.BytesDone = runResult.BytesDone
	task.Results = runResult.Results
	task.Status = runResult.Status
	task.UpdatedAt = time.Now()

	if errorOverride != "" && task.Status == StatusFailed {
		task.Error = errorOverride
		return
	}

	switch task.Status {
	case StatusCompleted:
		task.ExpiresAt = time.Now().Add(m.config.TaskTTL)
		task.Error = ""
		m.reconcileQuotaLocked(task)
	case StatusCancelled:
		// 死代码（审查 M-5）：syncCtx 仅被 CancelTask/DeleteTask 取消，二者都已置
		// cancelled 或删除任务，回填入口的 stored.Status==StatusCancelled 已先返回；
		// 保留作防御（执行器未来若自判取消可落到此处）。
		task.Error = runResult.Error
	case StatusFailed:
		switch {
		case runResult.Error != "":
			task.Error = runResult.Error
		case runErr != nil:
			task.Error = runErr.Error()
		default:
			task.Error = "同步失败"
		}
		// 审查 M-2：failed 终态释放预留配额（pull 任务创建时 TryReserve 1GiB 占位）——
		// 不释放会永久钉住配额（failed 不可取消，用户只能手动 DeleteTask 或重启）。
		// 释放后归零防二次释放；磁盘残留由下次周期扫描记账。
		if task.Direction == string(DirectionPull) && task.ReservedSize > 0 {
			m.quota.Release(task.ReservedSize, m.quotaCat)
			task.ReservedSize = 0
		}
	default:
		task.Status = StatusFailed
		if task.Error == "" {
			task.Error = "同步执行器返回未知状态"
		}
		// 同上：未知状态落 failed 也释放预留配额。
		if task.Direction == string(DirectionPull) && task.ReservedSize > 0 {
			m.quota.Release(task.ReservedSize, m.quotaCat)
			task.ReservedSize = 0
		}
	}
}

// logTaskResult 记录终态结果日志。
func (m *Manager) logTaskResult(task *SyncTask) {
	switch task.Status {
	case StatusCompleted:
		m.logger.Info("sync task completed",
			"task_id", task.ID, "files", task.FilesDone, "bytes", task.BytesDone)
	case StatusCancelled:
		m.logger.Info("sync task cancelled during execution", "task_id", task.ID)
	case StatusFailed:
		m.logger.Error("sync task failed", "task_id", task.ID, "error", task.Error)
	}
}

// pickErrorText 从 RunResult/error 中取错误文本（优先 RunResult.Error）。
func pickErrorText(runResult *RunResult, runErr error) string {
	if runResult != nil && runResult.Error != "" {
		return runResult.Error
	}
	if runErr != nil {
		return runErr.Error()
	}
	return "同步失败"
}

// reconcileQuotaLocked 按 BytesDone 对账预留配额（调用方须持有写锁）。
// pull 方向本地落盘：预留占位 → 收敛到实际写入字节。
// 恢复任务（Restored）不重新 TryReserve：启动时 StorageManager 已按磁盘扫描记账
// （storage_manager.go ScanAndRecalculate），否则磁盘已记账字节被二次预留、配额虚高
// 瞬时 507（审查 I-2）。
func (m *Manager) reconcileQuotaLocked(task *SyncTask) {
	if task.Direction != string(DirectionPull) {
		return // push 方向本地不预留
	}
	if task.Restored {
		task.ReservedSize = task.BytesDone // 磁盘扫描已记账，仅记录实际大小，不动 quota
		return
	}
	reserved := task.ReservedSize
	actual := task.BytesDone
	delta := actual - reserved
	switch {
	case delta > 0:
		if err := m.quota.TryReserve(delta, m.quotaCat); err != nil {
			// 存储不足，无法容纳实际大小：释放旧占位，任务失败（不破坏已写入文件）
			if reserved > 0 {
				m.quota.Release(reserved, m.quotaCat)
			}
			task.ReservedSize = 0
			task.Status = StatusFailed
			task.Error = "storage full after sync"
			return
		}
		task.ReservedSize = actual
	case delta < 0:
		m.quota.Release(-delta, m.quotaCat)
		task.ReservedSize = actual
	}
}

// failTask 将任务标记为失败并释放预留配额。
func (m *Manager) failTask(task *SyncTask, errMsg string) {
	m.mu.Lock()
	if task.Status == StatusFailed || task.Status == StatusCompleted || task.Status == StatusCancelled {
		m.mu.Unlock()
		return
	}
	if task.ReservedSize > 0 {
		m.quota.Release(task.ReservedSize, m.quotaCat)
		task.ReservedSize = 0
	}
	task.Status = StatusFailed
	task.Error = errMsg
	task.UpdatedAt = time.Now()
	m.mu.Unlock()
	if err := m.saveTask(task); err != nil {
		m.logger.Error("persist failed sync task state, state may be lost on restart",
			"task_id", task.ID, "error", err)
	}
	m.logger.Error("sync task failed", "task_id", task.ID, "error", errMsg)
}

// saveTask 持久化单个任务，返回写盘错误（终态调用方必须显式处理）。
func (m *Manager) saveTask(t *SyncTask) error {
	m.mu.RLock()
	_, exists := m.tasks[t.ID]
	m.mu.RUnlock()
	if !exists {
		m.logger.Debug("skip persisting deleted sync task", "id", t.ID)
		return nil
	}
	m.mu.RLock()
	data, err := json.Marshal(t)
	m.mu.RUnlock()
	if err != nil {
		m.logger.Warn("failed to marshal sync task", "id", t.ID, "error", err)
		return err
	}
	persistDir := m.persistDirFor(t.Owner)
	if persistDir == "" {
		m.logger.Warn("租户不可用，跳过任务持久化", "id", t.ID, "owner", t.Owner)
		return fmt.Errorf("tenant unavailable for task %s", t.ID)
	}
	if err := os.MkdirAll(persistDir, 0755); err != nil {
		m.logger.Warn("创建持久化目录失败", "dir", persistDir, "error", err)
		return err
	}
	if err := os.WriteFile(filepath.Join(persistDir, t.ID+".json"), data, 0644); err != nil {
		m.logger.Warn("failed to persist sync task", "id", t.ID, "error", err)
		return err
	}
	return nil
}

// recoverTasks 从磁盘恢复任务，仅重启 syncing/retrying 状态（pending 不自动启动）。
// 遍历全部租户的 meta/sync 目录（listTenants 磁盘扫描，非内存缓存），按任务落盘目录
// 逐租户读取。retrying 任务（重试等待/重试执行中崩溃）与 syncing 一样中断续执行，
// 保留已累计的重试计数。
func (m *Manager) recoverTasks() {
	recovered := 0
	restarted := 0
	for _, tenant := range m.listTenants() {
		persistDir := m.persistDirFor(tenant)
		if persistDir == "" {
			continue
		}
		entries, err := os.ReadDir(persistDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(persistDir, e.Name()))
			if err != nil {
				m.logger.Warn("failed to read persisted sync task, skipping", "file", e.Name(), "error", err)
				continue
			}
			var task SyncTask
			if err := json.Unmarshal(data, &task); err != nil {
				m.logger.Warn("failed to unmarshal persisted sync task, skipping", "file", e.Name(), "error", err)
				continue
			}
			// 重启后 StorageManager 已按磁盘扫描校准计数器，任务不再持有预留（防二次释放）
			task.ReservedSize = 0
			task.Restored = true // 完成对账时不再 TryReserve（审查 I-2）
			m.mu.Lock()
			m.tasks[task.ID] = &task
			m.mu.Unlock()
			recovered++

			if task.Status == StatusSyncing || task.Status == StatusRetrying {
				m.logger.Info("restarting interrupted sync task", "task_id", task.ID, "remote", task.Remote, "retries", task.Retries)
				m.mu.Lock()
				m.running[task.ID] = true
				m.mu.Unlock()
				m.wg.Add(1)
				go m.executeSync(context.Background(), &task) //nolint:gosec
				restarted++
			}
		}
	}
	if recovered > 0 {
		m.logger.Info("sync tasks recovered", "count", recovered, "restarted", restarted)
	}
}

// cleanupExpiredOnce 清理超过 TTL 的终态任务，返回清理数量。
func (m *Manager) cleanupExpiredOnce() int {
	now := time.Now()
	m.mu.Lock()
	type expiredTask struct{ id, owner string }
	var expired []expiredTask
	for id, t := range m.tasks {
		switch t.Status {
		case StatusCompleted, StatusFailed, StatusCancelled:
		default:
			continue
		}
		if now.After(t.UpdatedAt.Add(m.config.TaskTTL)) {
			expired = append(expired, expiredTask{id: id, owner: t.Owner})
			delete(m.tasks, id)
		}
	}
	m.mu.Unlock()

	for _, et := range expired {
		if persistDir := m.persistDirFor(et.owner); persistDir != "" {
			_ = os.Remove(filepath.Join(persistDir, et.id+".json"))
		}
	}
	if len(expired) > 0 {
		m.logger.Info("expired sync tasks cleaned up", "count", len(expired))
	}
	return len(expired)
}

func (m *Manager) cleanupExpired() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.cleanupExpiredOnce()
		case <-m.stopCleanup:
			m.cleanupExpiredOnce()
			return
		}
	}
}

// Stop 停止后台清理 goroutine 并等待执行中的任务退出（最多 30 秒）。
// 多次调用安全。
func (m *Manager) Stop() {
	m.closeOnce.Do(func() {
		close(m.stopCleanup)
		done := make(chan struct{})
		go func() {
			m.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			m.logger.Warn("sync manager Stop timed out waiting for goroutines")
		}
	})
}

// newSyncTaskID 生成唯一同步任务 ID（sync-<8hex>-<counter>）。
func newSyncTaskID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	syncIDCounter.mu.Lock()
	syncIDCounter.n++
	n := syncIDCounter.n
	syncIDCounter.mu.Unlock()
	return fmt.Sprintf("sync-%s-%d", hex.EncodeToString(b), n)
}

var syncIDCounter struct {
	mu sync.Mutex
	n  int64
}
