// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/internal/shortid"
	"github.com/cocomhub/sproxy/pkg/quota"
)

// ChunkedUploadSession 表示一个分块上传会话。
type ChunkedUploadSession struct {
	UploadID       string    `json:"upload_id"`
	Filename       string    `json:"filename"`
	TotalSize      int64     `json:"total_size"`
	ChunkSize      int64     `json:"chunk_size"`
	TotalChunks    int       `json:"total_chunks"`
	ReceivedChunks []bool    `json:"received_chunks"`
	ChunkChecksums []string  `json:"chunk_checksums"`
	FileChecksum   string    `json:"file_checksum"`
	FileModTime    int64     `json:"file_mod_time"` // UnixNano, 0 = unknown
	CreatedAt      time.Time `json:"created_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	Completed      bool      `json:"completed"`

	// Completing 是「complete 正在进行」的独占标记（不持久化）：置位后拒绝新分块，
	// 使「全文件校验 → rename」期间临时文件不再被改写（否则落在 rename 之后到达的分块会把
	// 落盘内容改成 ≠ 刚校验通过的内容）。重启后不恢复：进程中断的结束不成立，会话应可重试 complete。
	Completing bool `json:"-"`

	// TempPath 是任务 4 分块在途整文件的存储根相对路径（user 桶下，如
	// user/.inflight-<hash16>-<upload_id>.part）。init 创建并截断（Truncate(TotalSize)），
	// chunk 经 seek+BoundWriter 直写，complete 校验后 rename 为正式名，会话删除/过期删除。
	// 持久化（重启后可恢复续传；恢复时按内容重新校验分片）。TempPath 相对**目标卷**租户根
	// （Volume 决定在哪个卷上解析）。
	TempPath string `json:"temp_path,omitempty"`

	// Volume 是 init 经 routeUpload 定卷的目标卷名（空 = 默认卷 / 单卷旧会话，多卷装配前
	// 全部为空 → 解析为默认租户，零回归）。version/chunk 桶与目标 user 文件同卷（AD-5）。
	// 持久化：重启恢复时按 Volume 在对应卷租户根解析 TempPath。
	Volume string `json:"volume,omitempty"`

	// Reservation 是分块上传的租户配额预留句柄（P4）。init 预留、complete Commit、
	// 会话删除/过期 Release。不持久化（json:"-"），重启后内存预留丢失，由上游对账补齐。
	Reservation *quota.Reservation `json:"-"`

	// Pool/PoolRes 是 routeUpload 双账本的卷容量池预留句柄（T4/AD-7，volSet 装配时启用）。
	// init 预留、complete Commit/Adjust、会话删除/过期 Release。不持久化（json:"-"）。
	Pool    *quota.Pool        `json:"-"`
	PoolRes *quota.Reservation `json:"-"`
	// StorageMgrReserved 是 storageMgr 回退预留的字节数（P5，quota 未装配时启用）。
	// 与 Reservation 二选一：scope 预留走 Reservation，storageMgr 回退走本字段。
	// **未完成**会话删除/过期时按此释放；已完成会话**不释放**（字节已成正式文件，释放会让
	// 容量账少算 TotalSize，见 DeleteSession）。不持久化（json:"-"），重启后由对账补齐。
	StorageMgrReserved int64 `json:"-"`
}

// UploadStoreIface 定义 UploadStore 的业务接口，方便测试替身。
type UploadStoreIface interface {
	Health() error
	Stop()
	CreateSession(uploadID, filename string, totalSize, chunkSize int64, totalChunks int, fileChecksum string, fileModTime int64) (*ChunkedUploadSession, error)
	GetSession(uploadID string) *ChunkedUploadSession
	GetSessionByFilename(filename string) *ChunkedUploadSession
	MarkChunkReceived(uploadID string, chunkIndex int, checksum string) error
	AllChunksReceived(uploadID string) bool
	CompleteSession(uploadID string) error
	ChunkFilePath(uploadID string, chunkIndex int) string
	SessionDir(uploadID string) string
	DeleteSession(uploadID string)
	CleanupSessionAfter(uploadID string, delay time.Duration)
	GetOrCreateSession(uploadID, filename string, totalSize, chunkSize int64, totalChunks int, fileChecksum string, fileModTime int64) (*ChunkedUploadSession, bool, error)
	ListSessions() []ChunkedUploadSessionMeta
	LockChunkIO(uploadID string) func()
	LockChunkMerge(uploadID string) func()
	// 幂等/持久化现状（原 TODO 审计结论，2026-09-14）：会话元数据由 writeSessionJSON 原子写入
	// （CreateTemp + Rename）且由 writeMu 串行化（防 Windows rename 竞争，见本文件 writeMu 注释）；
	// 分块重传以同 index 覆盖写实现幂等。不扩 SyncPersistSession/RollbackChunkReceived：
	// 它们会要求调用方持有跨请求事务语义，收益不抵复杂度。
}

// ChunkFileLocker 管理分块文件的并发读写锁。
// 提取为独立导出类型，使 UploadStore 和 MockUploadStore 共享同一份真实锁定逻辑。
type ChunkFileLocker struct {
	fileLocks   map[string]*sync.RWMutex
	fileLocksMu sync.Mutex
}

// NewChunkFileLocker 创建一个新的 ChunkFileLocker。
func NewChunkFileLocker() *ChunkFileLocker {
	return &ChunkFileLocker{fileLocks: make(map[string]*sync.RWMutex)}
}

// LockChunkIO 获取 chunk 文件写入锁（读锁）。
// uploadChunk 在写入 chunk 文件前调用，允许多个 uploadChunk 并发写入不同 chunk。
func (l *ChunkFileLocker) LockChunkIO(uploadID string) func() {
	l.fileLocksMu.Lock()
	f, ok := l.fileLocks[uploadID]
	if !ok {
		f = new(sync.RWMutex)
		l.fileLocks[uploadID] = f
	}
	l.fileLocksMu.Unlock()
	f.RLock()
	return f.RUnlock
}

// LockChunkMerge 获取 chunk 文件合并锁（写锁）。
// mergeOneChunk 在读取 chunk 文件前调用，排他地等待所有正在写入的 chunk 完成后才允许读取，
// 同时阻塞新的 chunk 写入，避免读到不完整的 chunk。
func (l *ChunkFileLocker) LockChunkMerge(uploadID string) func() {
	l.fileLocksMu.Lock()
	f, ok := l.fileLocks[uploadID]
	if !ok {
		f = new(sync.RWMutex)
		l.fileLocks[uploadID] = f
	}
	l.fileLocksMu.Unlock()
	f.Lock()
	return f.Unlock
}

// DeleteLock 删除指定 uploadID 的锁条目，防止内存泄漏。
func (l *ChunkFileLocker) DeleteLock(uploadID string) {
	l.fileLocksMu.Lock()
	delete(l.fileLocks, uploadID)
	l.fileLocksMu.Unlock()
}

// UploadStore 管理分块上传会话的持久化与并发安全。
type UploadStore struct {
	mu         sync.RWMutex
	writeMu    sync.Mutex // 串行化 writeSessionJSON 与产物删除（防 Windows rename/删除竞争）
	baseDir    string     // 默认卷租户 chunk 桶绝对路径（<默认卷根>/<owner>/chunk/），会话目录直接位于其下
	sessions   map[string]*ChunkedUploadSession
	locker     *ChunkFileLocker // chunk 文件并发锁
	persistCh  chan string      // uploadID → 异步持久化
	stopCh     chan struct{}    // 关闭后台 goroutine
	stopOnce   sync.Once        // 保证 Stop 幂等
	wg         sync.WaitGroup
	sessionTTL time.Duration // 未完成上传会话的保留时间
	logger     *slog.Logger
	// storageMgr 是 storageMgr 回退预留的释放目标（P5，quota 未装配时由 uploadStoreFor
	// 经 SetStorageMgr 注入；nil = 无回退预留需释放）。
	storageMgr StorageManager
	// volTenantRoots 是卷名 → 该卷 owner 租户根绝对路径的映射（非默认卷；默认卷 = Dir(baseDir)）。
	// 会话可跨卷定卷（session.Volume），TempPath 需按目标卷租户根解析（DeleteSession/
	// cleanupExpired/verifyTempChunks/findMismatchChunks 共用）。mu 保护。
	volTenantRoots map[string]string
	// artifactsProbe 仅供测试注入（生产恒 nil）：在**开始删除会话产物**时调用，用于确定性
	// 钉住「身份闸门路径（cleanupSessionIfCurrent / cleanupExpiredArtifacts）的产物删除发生在
	// us.mu 临界区内」（RV9-CHUNK-FINAL F-1 回归门禁用 us.mu.TryLock 判定）。
	// 注入函数**不得**再取 us.mu（该路径持写锁时调用，会自死锁）。按实例设置，故并行用例互不干扰。
	artifactsProbe func(uploadID string)
}

// SetStorageMgr 注入 storageMgr 回退预留的释放目标（P5）。
// quota 未装配（globalPool nil）时 uploadStoreFor 调用；已装配 quota 时无需注入。
func (us *UploadStore) SetStorageMgr(sm StorageManager) {
	us.mu.Lock()
	us.storageMgr = sm
	us.mu.Unlock()
}

// SetVolumeTenantRoot 注册非默认卷 → owner 租户根绝对路径（供会话 TempPath 按目标卷解析）。
// 默认卷（""）始终回落 Dir(baseDir)，无需注册。幂等：重复注册同卷覆盖（路径不变）。
func (us *UploadStore) SetVolumeTenantRoot(volume, tenantRootAbs string) {
	if volume == "" || tenantRootAbs == "" {
		return
	}
	us.mu.Lock()
	if us.volTenantRoots == nil {
		us.volTenantRoots = make(map[string]string)
	}
	us.volTenantRoots[volume] = tenantRootAbs
	us.mu.Unlock()
}

// tenantRootFor 返回 session.Volume 对应的租户根绝对路径。默认卷（Volume 空/未注册）=
// 本 store 归属租户根（baseDir 的父目录，即 <默认卷根>/<owner>）。
func (us *UploadStore) tenantRootFor(volume string) string {
	if volume != "" {
		us.mu.RLock()
		r := us.volTenantRoots[volume]
		us.mu.RUnlock()
		if r != "" {
			return r
		}
	}
	return filepath.Dir(us.baseDir)
}

// InflightPrefix 是任务 4 分块在途整文件临时名前缀（user 桶目标同目录）：
// `.inflight-<sha256(正式名)前16hex>-<upload_id>.part`。不以 .__ 开头（避免被
// ValidSegmentName 拒绝），扫描层对不以 .inflight 开头的普通文件按 user 桶配额计入，
// 本前缀命中的在途文件随会话清理/过期删除。
const InflightPrefix = ".inflight-"

// InflightTempName 生成分块在途整文件临时名。name 为存储根相对正式路径（user/...），
// uploadID 为会话 ID；hash 取 sha256(name) 前 8 字节（16 hex）缩短，uploadID 保证
// 同目标多会话唯一。返回的临时名可作为单个路径段（inflightToken + uploadID 均为
// 安全段），散列相同目标不同会话的文件名可区分。
func InflightTempName(name, uploadID string) string {
	h := sha256.Sum256([]byte(name))
	return fmt.Sprintf("%s%s-%s.part", InflightPrefix, hex.EncodeToString(h[:8]), uploadID)
}

// inflightTempID 解析分块在途临时名，返回内嵌的 upload_id 段（不含 .part 后缀）；ok=false 表示
// 形态不合规。形态定义只出现在 InflightTempName（生成）与本函数（解析）两处，形态判据与归属判据
// 都经本函数派生，避免解析口径分叉。
func inflightTempID(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, InflightPrefix)
	if !ok {
		return "", false
	}
	// 形态：<16hex>-<uploadID>.part（uploadID 为合法段名，可为多段拼接前的裸名）。
	// Cut 取**第一个** '-'，故 uploadID 自身含 '-' 时仍整体保留在 idPart 内。
	token, idPart, hasPart := strings.Cut(rest, "-")
	if !hasPart || len(token) != 16 || !strings.HasSuffix(idPart, ".part") {
		return "", false
	}
	for _, c := range token {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return strings.TrimSuffix(idPart, ".part"), true
}

// IsInflightTempName 判断 name 是否为分块在途临时文件
// （.inflight-<hash16>-<upload_id>.part，InflightTempName 命中的完整形态）。
// 列表/搜索按整临时名过滤（服务端内部在途文件，对外不可见），避免与用户可创建的同名前缀
// 普通文件（如 <id>.part 形式的用户文件）误拦——严格校验整个临时名形态而非仅前缀。
func IsInflightTempName(name string) bool {
	_, ok := inflightTempID(name)
	return ok
}

// IsInflightTempNameFor 判断 name 是否为**属于 uploadID** 的分块在途临时文件名：形态合法
// （IsInflightTempName）且内嵌的 upload_id 段与本会话 id **完整相等**。
//
// 删除路径用它而**不是**只校验形态的 IsInflightTempName：临时名只依赖 (rel, uploadID)，一份陈旧
// 或被篡改的会话记录把 TempPath 指向**另一个会话**的在途临时名时，仅校验形态会删掉别人的在途
// 文件（RV9-CHUNK-FINAL F-3；同样把该上传拖入审计 C-2 的「temp 丢失不可修复」）。
// 列表/搜索等按形态过滤的场景继续用 IsInflightTempName（它们不看归属）。
//
// 归属判据必须比较**整段 id**（RV9-CHUNK-FINAL 建议②）：uploadID 允许含 '-'，若用
// HasSuffix("-"+uploadID+".part") 判断，本会话 id="bar" 会误认 id="foo-bar" 的在途名（后缀相同）
// ⇒ 同样会删掉别的会话的在途文件。
func IsInflightTempNameFor(name, uploadID string) bool {
	if uploadID == "" {
		return false
	}
	id, ok := inflightTempID(name)
	return ok && id == uploadID
}

// NewUploadStore 创建并启动 UploadStore，同时从磁盘恢复已有 session。
// baseDir 是租户 chunk 桶的绝对路径（<root>/<owner>/chunk/，经 Tenant.Root().Abs("chunk")
// 派生）；会话目录直接位于 baseDir 下（<baseDir>/<uploadID>/）。不再拼接魔法目录。
// sessionTTL 指定未完成上传会话的过期时间，默认 24h。
// volumeRoots（可选，变参）是卷名 → 该卷 owner 租户根绝对路径映射，**必须在 recoverSessions
// 之前装配**——recover 按 session.Volume 经 tempAbsPath 解析在途 temp 文件所在卷（AD-5 换卷
// 路径核心）；非默认卷会话若未预注册会把 temp 解析到默认卷 → 打开失败清空 bitmap → 续传
// 退化为整文件重传（T6a 修复轮发现-1）。
func NewUploadStore(baseDir string, sessionTTL time.Duration, logger *slog.Logger, volumeRoots ...map[string]string) (*UploadStore, error) {
	if logger == nil {
		logger = slog.Default()
	}
	log := logger
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("创建分块上传目录失败: %w", err)
	}

	if sessionTTL < 0 {
		// 负 TTL：保留原值，用于测试"已过期"场景。
		// ExpiresAt = now.Add(negative) 保证为过去时间，cleanupExpired 可立即清理。
	} else if sessionTTL == 0 {
		sessionTTL = 24 * time.Hour
	}

	us := &UploadStore{
		baseDir:        baseDir,
		sessions:       make(map[string]*ChunkedUploadSession),
		locker:         NewChunkFileLocker(),
		persistCh:      make(chan string, 64),
		stopCh:         make(chan struct{}),
		sessionTTL:     sessionTTL,
		logger:         log,
		volTenantRoots: make(map[string]string),
	}
	if len(volumeRoots) > 0 && volumeRoots[0] != nil {
		for v, root := range volumeRoots[0] {
			us.SetVolumeTenantRoot(v, root)
		}
	}
	us.recoverSessions()

	// 启动持久化 goroutine
	us.wg.Add(1)
	go us.persistLoop()

	// 启动过期清理 goroutine（每 5 分钟）
	us.wg.Add(1)
	go us.cleanupLoop()

	return us, nil
}

// Health 返回 UploadStore 的健康状态。
// 检查后台 goroutine 是否仍在运行。
func (us *UploadStore) Health() error {
	select {
	case <-us.stopCh:
		return fmt.Errorf("UploadStore 已停止")
	default:
	}
	return nil
}

// Stop 停止后台 goroutine 并等待结束。
//
// 优雅停止流程（draining）：
//  1. 关闭 stopCh 通知 cleanupLoop 和 fallback goroutine 退出，同时阻止新的持久化请求
//  2. 关闭 persistCh（不再接受新请求）
//  3. 排空 persistCh：处理所有已入列的持久化请求
//  4. 等待 wg 完成
//
// 多次调用是安全的（幂等）。
func (us *UploadStore) Stop() {
	us.stopOnce.Do(func() {
		// 1. 先关闭 stopCh，通知所有后台 goroutine 退出
		//    同时确保 MarkChunkReceived / CompleteSession 不会再向 persistCh 发送新请求
		close(us.stopCh)

		// 2. 关闭 persistCh，persistLoop 将在消费完当前请求后退出
		close(us.persistCh)

		// 3. 排空 persistCh：处理所有已入列的持久化请求（受 wg 追踪）
		us.wg.Go(func() {
			for uploadID := range us.persistCh {
				us.persistSession(uploadID)
			}
		})

		// 4. 等待所有 goroutine 完成
		us.wg.Wait()
	})
}

// newSession 创建 ChunkedUploadSession 对象（不持久化）。
func newSession(uploadID, filename string, totalSize, chunkSize int64, totalChunks int, fileChecksum string, fileModTime int64, sessionTTL time.Duration) *ChunkedUploadSession {
	now := time.Now()
	return &ChunkedUploadSession{
		UploadID:       uploadID,
		Filename:       filename,
		TotalSize:      totalSize,
		ChunkSize:      chunkSize,
		TotalChunks:    totalChunks,
		ReceivedChunks: make([]bool, totalChunks),
		ChunkChecksums: make([]string, totalChunks),
		FileChecksum:   fileChecksum,
		FileModTime:    fileModTime,
		CreatedAt:      now,
		ExpiresAt:      now.Add(sessionTTL),
	}
}

// saveNewSession 创建会话目录、持久化 session.json，并将 session 注册到内存 map。
// 调用方需保证不在持锁状态（writeSessionJSON 内部会持 writeMu 做 I/O）。
func (us *UploadStore) saveNewSession(session *ChunkedUploadSession) error {
	sessionDir := filepath.Join(us.baseDir, session.UploadID)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return fmt.Errorf("创建会话目录失败: %w", err)
	}
	if err := us.writeSessionJSON(session); err != nil {
		os.RemoveAll(sessionDir)
		return err
	}
	us.mu.Lock()
	us.sessions[session.UploadID] = session
	us.mu.Unlock()
	return nil
}

// CreateSession 创建一个新的分块上传会话，使用客户端提供的 uploadID。
//
// 返回值是**store 持有的内部对象**（与 GetOrCreateSession 的新建路径一致；其续传路径则返回
// copySession 副本）。该对象一经发布进 us.sessions 就会被并发请求经 GetSession / PersistNow
// 整结构深拷贝 ⇒ 之后修改任何字段都必须走锁内 setter（SetSessionRoute /
// SetSessionStorageMgrReserved / SetSessionTempPath），直接改字段与并发读者构成数据竞争
// （审计 C-8：string 头/指针可能出现 torn read）。不要图省事把它改成返回副本：既有调用点
// 按字段写入返回值，改副本会让那些写入静默失效（其中断言“未发生释放”的用例会因此退化为
// 真空假绿，必须靠读回 store 对象才能发现）。
func (us *UploadStore) CreateSession(uploadID, filename string, totalSize, chunkSize int64, totalChunks int, fileChecksum string, fileModTime int64) (*ChunkedUploadSession, error) {
	if uploadID == "" {
		return nil, fmt.Errorf("upload_id 不能为空")
	}

	session := newSession(uploadID, filename, totalSize, chunkSize, totalChunks, fileChecksum, fileModTime, us.sessionTTL)

	us.logger.Info("创建上传会话", "upload_id", uploadID, "file_name", filename,
		"total_size", totalSize, "chunk_size", chunkSize, "total_chunks", totalChunks)

	if err := us.saveNewSession(session); err != nil {
		return nil, err
	}

	return session, nil
}

// GetSession 返回指定 upload_id 的会话副本。
func (us *UploadStore) GetSession(uploadID string) *ChunkedUploadSession {
	us.mu.RLock()
	defer us.mu.RUnlock()
	s, ok := us.sessions[uploadID]
	if !ok {
		return nil
	}
	return copySession(s)
}

// ChunkedUploadSessionMeta 是分块上传会话的紧凑元信息（不含 bitmap），用于列表展示。
type ChunkedUploadSessionMeta struct {
	UploadID      string `json:"upload_id"`
	Filename      string `json:"filename"`
	TotalSize     int64  `json:"total_size"`
	ChunkSize     int64  `json:"chunk_size"`
	TotalChunks   int    `json:"total_chunks"`
	ReceivedCount int    `json:"received_count"`
	MissingCount  int    `json:"missing_count"`
	FileChecksum  string `json:"file_checksum"`
	FileModTime   int64  `json:"file_mod_time"` // UnixNano, 0 = unknown
	ExpiresAt     int64  `json:"expires_at"`
	Completed     bool   `json:"completed"`
}

// ListSessions 返回所有（含已完成）会话的元信息快照，并按上传 ID 升序排列（输出确定）。
// 持锁期间从 sessions map 拷贝元信息（不含 bitmap），并在锁内完成计数，
// 避免读取时被 MarkChunkReceived 改写造成数据竞争。
// 注：已完成会话在 complete 后 CleanupSessionAfter 删除前仍在 sessions map 中，列表交由调用方过滤。
func (us *UploadStore) ListSessions() []ChunkedUploadSessionMeta {
	us.mu.RLock()
	meta := make([]ChunkedUploadSessionMeta, 0, len(us.sessions))
	for _, s := range us.sessions {
		m := ChunkedUploadSessionMeta{
			UploadID:      s.UploadID,
			Filename:      s.Filename,
			TotalSize:     s.TotalSize,
			ChunkSize:     s.ChunkSize,
			TotalChunks:   s.TotalChunks,
			ReceivedCount: countReceived(s.ReceivedChunks),
			MissingCount:  s.TotalChunks - countReceived(s.ReceivedChunks),
			FileChecksum:  s.FileChecksum,
			FileModTime:   s.FileModTime,
			ExpiresAt:     s.ExpiresAt.UnixNano(),
			Completed:     s.Completed,
		}
		meta = append(meta, m)
	}
	us.mu.RUnlock()
	slices.SortFunc(meta, func(a, b ChunkedUploadSessionMeta) int { return strings.Compare(a.UploadID, b.UploadID) })
	return meta
}

// GetSessionByFilename 按文件名查找未完成的 session。
// per-tenant UploadStore 实例天然只含本租户会话，无需 owner 过滤。
func (us *UploadStore) GetSessionByFilename(filename string) *ChunkedUploadSession {
	us.mu.RLock()
	defer us.mu.RUnlock()
	for _, s := range us.sessions {
		if s.Filename == filename && !s.Completed {
			return copySession(s)
		}
	}
	return nil
}

// MarkChunkReceived 标记指定分块为已接收并持久化。
func (us *UploadStore) MarkChunkReceived(uploadID string, chunkIndex int, checksum string) error {
	us.mu.Lock()

	s, ok := us.sessions[uploadID]
	if !ok {
		us.mu.Unlock()
		return fmt.Errorf("upload_id 不存在: %s", uploadID)
	}
	if chunkIndex < 0 || chunkIndex >= s.TotalChunks {
		us.mu.Unlock()
		return fmt.Errorf("chunk_index %d 超出范围 [0, %d)", chunkIndex, s.TotalChunks)
	}

	s.ReceivedChunks[chunkIndex] = true
	s.ChunkChecksums[chunkIndex] = checksum

	received := countReceived(s.ReceivedChunks)
	total := s.TotalChunks
	us.mu.Unlock()

	us.logger.Debug("chunk 已接收", "upload_id", uploadID, "chunk_index", chunkIndex,
		"checksum", shortid.ShortHash(checksum), "received", received, "total", total)

	// 异步持久化（检查 UploadStore 是否已停止）
	select {
	case <-us.stopCh:
		// 已停止，丢弃持久化请求
	default:
		select {
		case us.persistCh <- uploadID:
		default:
			// 通道满时异步持久化，受 wg 追踪
			us.wg.Go(func() {
				us.persistSession(uploadID)
			})
		}
	}
	return nil
}

// ClearChunksReceived 把指定分块索引置为未接收（bitmap 清位 + checksum 记录清空），
// 并持久化。任务 5 用：全文件校验失败后按 mismatch 逐分片清位——精确恢复客户端
// 需重传的接收态（坏分片需重传 seek 覆盖，skipped 分片不受影响）。
// index 越界/会话不存在时返回错误（不 panic）。
func (us *UploadStore) ClearChunksReceived(uploadID string, indices []int) error {
	ss := slices.Clone(indices)
	slices.Sort(ss)
	us.mu.Lock()
	s, ok := us.sessions[uploadID]
	if !ok {
		us.mu.Unlock()
		return fmt.Errorf("upload_id 不存在: %s", uploadID)
	}
	for _, i := range ss {
		if i < 0 || i >= s.TotalChunks {
			us.mu.Unlock()
			return fmt.Errorf("chunk_index %d 超出范围 [0, %d)", i, s.TotalChunks)
		}
		s.ReceivedChunks[i] = false
		s.ChunkChecksums[i] = ""
	}
	us.mu.Unlock()

	select {
	case <-us.stopCh:
	default:
		select {
		case us.persistCh <- uploadID:
		default:
			us.wg.Go(func() { us.persistSession(uploadID) })
		}
	}
	return nil
}

// AllChunksReceived 检查是否所有分块都已接收。
func (us *UploadStore) AllChunksReceived(uploadID string) bool {
	us.mu.RLock()
	defer us.mu.RUnlock()
	s, ok := us.sessions[uploadID]
	if !ok {
		return false
	}
	if s.Completed {
		return true
	}
	for _, received := range s.ReceivedChunks {
		if !received {
			return false
		}
	}
	return true
}

// BeginComplete 置位「合并中」标记并返回是否成功（会话不存在/已完成/已在合并中 ⇒ false）。
// 与 chunk 写路径靠同一把 us.mu 互斥：置位后到达的分块会被 UploadChunk 立刻拒绝（C-3 屏障）。
func (us *UploadStore) BeginComplete(uploadID string) bool {
	us.mu.Lock()
	defer us.mu.Unlock()
	s, ok := us.sessions[uploadID]
	if !ok || s.Completed || s.Completing {
		return false
	}
	s.Completing = true
	return true
}

// EndComplete 清除「合并中」标记（complete 失败/中断路径；成功路径由 CompleteSession 终结会话）。
func (us *UploadStore) EndComplete(uploadID string) {
	us.mu.Lock()
	defer us.mu.Unlock()
	if s, ok := us.sessions[uploadID]; ok {
		s.Completing = false
	}
}

// CompleteSession 标记会话为已完成。
func (us *UploadStore) CompleteSession(uploadID string) error {
	us.mu.Lock()
	defer us.mu.Unlock()

	s, ok := us.sessions[uploadID]
	if !ok {
		return fmt.Errorf("upload_id 不存在: %s", uploadID)
	}
	if s.Completed {
		return fmt.Errorf("upload_id %s 已完成", uploadID)
	}

	s.Completed = true
	us.logger.Info("上传会话已完成", "upload_id", uploadID, "file_name", s.Filename,
		"received", countReceived(s.ReceivedChunks), "total", s.TotalChunks)
	select {
	case <-us.stopCh:
		// 已停止，不触发持久化
	default:
		select {
		case us.persistCh <- uploadID:
		default:
			us.wg.Go(func() {
				us.persistSession(uploadID)
			})
		}
	}
	return nil
}

// ChunkFilePath 曾是任务 4 前独立 .chunk 文件的路径推导；改造后分块直写整临时文件，
// 不再存在 per-chunk 文件，方法已删除（ChunkFileLocker 仍保留供遗留锁域使用）。

// SessionDir 返回会话目录路径。
func (us *UploadStore) SessionDir(uploadID string) string {
	return filepath.Join(us.baseDir, uploadID)
}

// DeleteSession 删除会话目录并清理在途临时文件、释放预留，最后清理 fileLocks 条目防内存泄漏。
func (us *UploadStore) DeleteSession(uploadID string) {
	us.mu.Lock()
	s := us.sessions[uploadID]
	delete(us.sessions, uploadID)
	us.mu.Unlock()

	us.releaseSessionReservations(s)
	us.deleteSessionArtifacts(uploadID, s)
}

// releaseSessionReservations 归还会话自己的配额/容量预留：P4 Scope（Reservation）、P5 storageMgr
// 回退（StorageMgrReserved）、AD-7 卷容量池（PoolRes）。
//
// 只操作**该会话对象自己的句柄**，与 upload_id 当前归属无关 ⇒ 可在释放 us.mu 之后调用（刻意不
// 放进身份闸门临界区：CleanupExpired 亦在锁外归还，避免持锁调用外部组件）。
func (us *UploadStore) releaseSessionReservations(s *ChunkedUploadSession) {
	if s == nil {
		return
	}
	// P4 配额：清理会话时释放未落地的预留。已完成会话的预留已被 complete Commit
	// 消费（Commit 原子生效一次），此 Release 为空操作；未完成会话则归还 reserved。
	// P5 回退预留（quota 未装配时）与此**不同**：complete 没有对应的 Commit，字节已成
	// 正式文件 ⇒ 只有**未完成**会话才释放（审计 C-4：无条件释放会让 capacity 的 totalUsage
	// 少算 TotalSize，直到下一次全量扫描才校正，期间放宽 max_storage_bytes 门禁）。
	// 卷容量池双账本预留（routeUpload，volSet 装配时）随会话删除 Release（AD-7）。
	if s.Reservation != nil {
		s.Reservation.Release()
	} else if s.StorageMgrReserved > 0 && !s.Completed {
		if us.storageMgr != nil {
			us.storageMgr.ReleaseChunked(s.StorageMgrReserved)
		}
		s.StorageMgrReserved = 0
	}
	if s.PoolRes != nil {
		s.PoolRes.Release()
		s.PoolRes = nil
	}
}

// deleteSessionArtifacts 删除会话的磁盘产物（在途临时文件 + 会话目录 + per-uploadID 文件锁），
// **不依赖 us.sessions**：恢复期发现的过期/损坏会话从未进入 map，走 DeleteSession 永不触达
// ⇒ 临时名与会话目录永久孤儿（审计 C-5：磁盘泄漏 + 列表里看不到的幽灵占用）。
// session 为 nil（session.json 缺失/损坏 ⇒ TempPath 未知）时只删会话目录。
//
// 本函数按 upload_id 定位，**不校验该 id 是否已被新会话接管**——需要该保护的调用点走
// cleanupExpiredArtifacts；本函数供「该 id 的所有权已确定」的路径使用：DeleteSession（语义就是
// 清掉这个 id）、恢复期就地回收（构造期单 goroutine，map 尚未对外发布）。
//
// 锁序：本函数只取 writeMu（调用方均不持 us.mu 调用它），与 GetOrCreateSession 的
// us.mu → writeMu 同向，不成环。
func (us *UploadStore) deleteSessionArtifacts(uploadID string, session *ChunkedUploadSession) {
	tempAbs, hasTemp := us.tempAbsPath(session)
	us.deleteSessionArtifactsAt(uploadID, tempAbs, hasTemp)
}

// deleteSessionArtifactsAt 是产物删除的实际执行体。tempAbs/hasTemp 由调用方**预先解析**：
// tempAbsPath → tenantRootFor 内部取 us.mu.RLock，持 us.mu 写锁时调用会自死锁
// （cleanupExpiredArtifacts 与 cleanupSessionIfCurrent 都需要在持 us.mu 临界区内完成删除）。
//
// 调用方分两类，产物删除的**原子性口径**不同：
//   - 持 us.mu 调用（cleanupExpiredArtifacts / cleanupSessionIfCurrent）：与登记路径互斥 ⇒
//     可作为「身份判定 + 删除」的原子单元；
//   - 不持 us.mu 调用（DeleteSession / 恢复期就地回收）：语义上该 id 的所有权已确定，
//     不需要与登记互斥。
//
// 整个删除过程持 writeMu，与 session.json 的在途写入（writeSessionJSON 的 tmp+rename）互斥：
// Windows 上 os.RemoveAll 撞到被持久化打开着的 session.json.tmp.* 会以「目录非空」失败，
// 留下无 session.json 的空目录（实测：重命名 Access is denied + unlinkat 目录非空并存）。
func (us *UploadStore) deleteSessionArtifactsAt(uploadID, tempAbs string, hasTemp bool) {
	if probe := us.artifactsProbe; probe != nil {
		probe(uploadID)
	}
	us.writeMu.Lock()
	defer us.writeMu.Unlock()

	// 任务 4：删除 in-flight 临时文件（在 user 桶，独立于 session 目录；tempAbs 来自
	// session.TempPath，由 init/恢复写入）。多卷会话按 session.Volume 在目标卷租户根解析
	// （AD-5 temp 与 user 文件同卷）。
	//
	// 纵深防御：只删**我们自己的在途临时名形态**，且内嵌 upload_id 必须属于本会话
	// （IsInflightTempNameFor：形态 + 归属，见 F-3）。tempAbsPath 只校验「user/ 前缀 +
	// 租户根容器」，因此一份陈旧/被篡改的 session.json 把 TempPath 写成 user/important.txt
	// 时，删除路径会删掉一个**正式用户文件**（实测复现）；不校验内嵌 id 则会删掉**另一个
	// 会话**的在途文件。本片新增的「恢复期就地回收」（会话从未入 map 也要删临时名）把该逻辑
	// 集中到这里，正是加闸门的位置。读路径不加此闸门：verifyTempChunks/findMismatchChunks
	// 仍须按记录解析，形态不符时按「不可读」保守处理（重传）而非拒绝读取。
	switch {
	case !hasTemp:
	case IsInflightTempNameFor(filepath.Base(tempAbs), uploadID):
		if err := os.Remove(tempAbs); err != nil && !os.IsNotExist(err) {
			us.logger.Warn("删除在途临时文件失败", "upload_id", uploadID, "error", err)
		}
	default:
		us.logger.Warn("会话记录的在途临时名形态或归属非法，拒绝按该路径删除",
			"upload_id", uploadID, "temp_path", tempAbs)
	}

	us.locker.DeleteLock(uploadID)

	dir := filepath.Join(us.baseDir, uploadID)
	if err := os.RemoveAll(dir); err != nil {
		us.logger.Warn("删除会话目录失败", "upload_id", uploadID, "error", err)
	}
}

// cleanupExpiredArtifacts 删除一个**已从 map 摘除**的过期会话的磁盘产物，并保证不误删
// 「同 upload_id 已被新会话接管」时的产物（审计 C-7 同族；RV8-CHUNK must-fix）。
//
// 为什么需要闸门：CleanupExpired 的收集阶段已把过期项移出 map，**解锁后**才逐项做删除 I/O
// （每项都受 writeMu 阻塞 ⇒ 后面项的窗口可被拉长）。该窗口内客户端可用同一 upload_id 重新
// init（SDK 的 upload_id 由 filename|size|mtime|checksum 派生 ⇒ 同文件重试即同 id），新会话的
// 会话目录与在途临时名与旧会话同路径；若旧清理此时按 id 删除，会删掉**新会话**的 session.json
// 与在途临时文件（complete 随后 fail-closed，但重启后该上传不可恢复）。
//
// 判据是「该 id 当前是否被**另一个**会话对象占用」。**不能**用「表内是否仍是本对象」：收集
// 阶段已把本项摘除，正常情形 sessions[id] 就是 nil，用后者会把**所有**正常清理挡掉（即 C-5/C-6
// 刚修好的产物回收反向失效）。
//
// 检查与删除在**同一个 us.mu 临界区**内完成：登记路径（GetOrCreateSession）全程持 us.mu，
// 建目录 / 写 session.json / 写 map 都在其中 ⇒ 与本次检查互斥，不存在「查完再删」的残留窗口。
// 代价是删除 I/O 期间持 us.mu（短暂阻塞 store 的其它操作），这是为原子性付的必要代价；
// 锁序 us.mu → writeMu 与 GetOrCreateSession 同向（全包不存在 writeMu → us.mu 的反向路径）。
//
// 接管时**整项跳过**（宁可让旧临时名成为孤儿，也不删新会话的在途文件）：SDK 派生 id 下同 id
// 意味着同元数据 ⇒ 新旧临时名相同，本就不存在额外孤儿；元数据不同而复用同 id 的调用（如
// CreateSession）目前仅测试使用。
//
// 预留的释放不在此处：Reservation/PoolRes/StorageMgrReserved 操作的是**旧对象自己的句柄**，
// 与该 id 是否换主无关，仍由 CleanupExpired 无条件执行。
func (us *UploadStore) cleanupExpiredArtifacts(uploadID string, session *ChunkedUploadSession) bool {
	// 必须在取 us.mu 前解析（tempAbsPath → tenantRootFor 取 us.mu.RLock，不可重入）。
	tempAbs, hasTemp := us.tempAbsPath(session)

	us.mu.Lock()
	defer us.mu.Unlock()
	if cur := us.sessions[uploadID]; cur != nil && cur != session {
		us.logger.Info("过期会话的产物已被新会话接管，跳过删除", "upload_id", uploadID)
		return false
	}
	us.deleteSessionArtifactsAt(uploadID, tempAbs, hasTemp)
	return true
}

// LockChunkIO 获取 chunk IO 读锁（任务 4：并发分段 seek 直写整临时文件，锁域按 uploadID
// 划分，避免同会话 bitmap 更新/MarkChunkReceived 与 complete 读全文件间的竞态；读锁允许多
// 分片并发写，complete 用 LockChunkMerge 写锁排他读整文件）。
func (us *UploadStore) LockChunkIO(uploadID string) func() {
	return us.locker.LockChunkIO(uploadID)
}

// LockChunkMerge 获取 chunk 合并写锁（排他）。
// complete 在读取整临时文件前调用，等待所有正在写入的分片完成后才允许读取，
// 同时阻塞新的分片写入，避免读到不完整的临时文件。
func (us *UploadStore) LockChunkMerge(uploadID string) func() {
	return us.locker.LockChunkMerge(uploadID)
}

// persistLoop 异步持久化 goroutine。
//
// 使用 for-range 从 persistCh 消费，当 persistCh 被关闭时自动退出。
func (us *UploadStore) persistLoop() {
	defer us.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			us.logger.Error("persistLoop panic", "panic", r)
		}
	}()
	for uploadID := range us.persistCh {
		us.persistSession(uploadID)
	}
}

// persistSession 将指定 session 持久化到磁盘。
// 在持锁状态下深拷贝 session（含 ReceivedChunks / ChunkChecksums 两个 slice），
// 然后在释放锁后再做 JSON marshal / 写文件，避免 marshal 期间被 MarkChunkReceived 改写 slice 造成 data race。
func (us *UploadStore) persistSession(uploadID string) {
	us.mu.RLock()
	s, ok := us.sessions[uploadID]
	if !ok {
		us.mu.RUnlock()
		return
	}
	snapshot := copySession(s)
	us.mu.RUnlock()

	if err := us.writeSessionJSON(snapshot); err != nil {
		us.logger.Error("持久化 session 失败", "upload_id", uploadID, "error", err)
	}
}

// CopySession 返回 session 的深拷贝（含两个 slice），调用方需保证持锁或持有稳定副本。
func copySession(s *ChunkedUploadSession) *ChunkedUploadSession {
	cp := *s
	cp.ReceivedChunks = make([]bool, len(s.ReceivedChunks))
	copy(cp.ReceivedChunks, s.ReceivedChunks)
	cp.ChunkChecksums = make([]string, len(s.ChunkChecksums))
	copy(cp.ChunkChecksums, s.ChunkChecksums)
	return &cp
}

// PersistNow 同步持久化指定 session（如 init 写入 tempPath 后立即落盘，供重启恢复）。
// 复用 copySession 快照 + writeSessionJSON；与异步 persistSession 语义一致。
func (us *UploadStore) PersistNow(uploadID string) error {
	us.mu.RLock()
	s, ok := us.sessions[uploadID]
	if !ok {
		us.mu.RUnlock()
		return fmt.Errorf("upload_id 不存在: %s", uploadID)
	}
	snapshot := copySession(s)
	us.mu.RUnlock()
	return us.writeSessionJSON(snapshot)
}

// writeSessionJSON 原子写入 session.json。
// 使用 writeMu 串行化写入，防止 Windows 上 os.CreateTemp + os.Rename 并发竞争。
func (us *UploadStore) writeSessionJSON(s *ChunkedUploadSession) error {
	us.writeMu.Lock()
	defer us.writeMu.Unlock()

	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("序列化 session 失败: %w", err)
	}
	dir := filepath.Join(us.baseDir, s.UploadID)
	// **不**在此处 MkdirAll：会话目录只应由会话创建路径建立（saveNewSession /
	// GetOrCreateSession 各自 MkdirAll 后调用本函数）。若在此创建，一次「快照早于删除」的异步
	// 持久化（persistSession/PersistNow）会把**已删除**的会话目录重建并写回陈旧 session.json
	// ⇒ 重启后该会话被复活，C-6 的 TTL 回收与会话删除都被抵消（实测：删除后目录仍存在）。
	// 目录不存在时 CreateTemp 直接失败（返回错误、由调用方记日志），即期望的 fail-closed 行为。
	finalPath := filepath.Join(dir, "session.json")
	tmpFile, err := os.CreateTemp(dir, "session.json.tmp.*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		// Windows cannot rename over an existing file；先删除已存在的目标文件再重试。
		// 若重试仍失败，回退到 os.WriteFile 覆盖写入，避免"目标文件已被删除但新文件未写入"
		// 造成数据丢失（此时目标文件已不存在）。
		removeErr := os.Remove(finalPath)
		if removeErr == nil || os.IsNotExist(removeErr) {
			if os.Rename(tmpPath, finalPath) == nil {
				return nil
			}
			// 回退：直接覆盖写入目标路径（writeMu 已串行化，不会并发竞争同一文件）。
			if writeErr := os.WriteFile(finalPath, data, 0644); writeErr == nil {
				os.Remove(tmpPath)
				return nil
			}
		}
		os.Remove(tmpPath)
		return fmt.Errorf("重命名失败: %w", err)
	}
	return nil
}

// cleanupLoop 周期性清理过期 session。
func (us *UploadStore) cleanupLoop() {
	defer us.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			us.logger.Error("cleanupLoop panic", "panic", r)
		}
	}()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-us.stopCh:
			return
		case <-ticker.C:
			us.CleanupExpired()
		}
	}
}

// CleanupExpired 清理过期会话：未完成的归还预留，已完成的只回收磁盘与会话目录。
//
// 一并处理 Completed 会话（审计 C-6）：complete 后的 5 s 延迟清理若未执行（停机/异常），
// 该会话会被恢复路径长期留在 map 与磁盘上（「永不回收」）。TTL（ExpiresAt）给出有界上界。
// 先持锁收集过期 ID，释放锁后再逐一删除临时文件 / os.RemoveAll，避免长时间持锁执行 I/O；
// 但**产物删除本身**走 cleanupExpiredArtifacts——它在 us.mu 内完成「身份检查 + 删除」，
// 避免解锁后按 id 误删窗口内接管同 id 的新会话（审计 C-7 同族）。
func (us *UploadStore) CleanupExpired() {
	type expiredItem struct {
		id                 string
		session            *ChunkedUploadSession // 供 tempAbsPath 按 Volume 解析（保留引用）
		reservation        *quota.Reservation
		poolRes            *quota.Reservation
		storageMgrReserved int64
	}
	var expired []expiredItem

	us.mu.Lock()
	now := time.Now()
	for id, s := range us.sessions {
		if now.After(s.ExpiresAt) {
			us.logger.Info("清理过期上传会话", "upload_id", id, "file_name", s.Filename,
				"expires_at", s.ExpiresAt, "completed", s.Completed)
			delete(us.sessions, id)
			item := expiredItem{id: id, session: s, reservation: s.Reservation, poolRes: s.PoolRes}
			// P5 回退预留只对**未完成**会话归还（已完成会话的字节已 rename 成正式文件，
			// 释放会让容量账少算 TotalSize，见 DeleteSession）。
			if !s.Completed {
				item.storageMgrReserved = s.StorageMgrReserved
			}
			expired = append(expired, item)
		}
	}
	us.mu.Unlock()

	for _, item := range expired {
		// P4 配额：过期会话（从未完成）归还预留，避免 chunk 字节长期挂账；已完成会话的预留
		// 已被 complete Commit 消费（此 Release 为空操作）。
		// P5：storageMgr 回退预留（quota 未装配时）同样释放（与 Reservation 二选一）。
		// 卷容量池双账本预留（routeUpload，volSet 装配时）随过期 Release（AD-7）。
		if item.reservation != nil {
			item.reservation.Release()
		} else if item.storageMgrReserved > 0 && us.storageMgr != nil {
			us.storageMgr.ReleaseChunked(item.storageMgrReserved)
		}
		if item.poolRes != nil {
			item.poolRes.Release()
		}
		// 在途临时文件 + 会话目录 + per-uploadID 文件锁：复用与 DeleteSession 同源的实现
		// （避免两处口径分叉），但必须过**身份闸门**：收集阶段已把本项移出 map，而解锁后的
		// 删除 I/O 期间同 upload_id 可能已被新会话接管（审计 C-7 同族）。已完成会话的 temp
		// 已被 rename 成正式名，os.Remove 命中 IsNotExist 被忽略。
		us.cleanupExpiredArtifacts(item.id, item.session)
	}
}

// CleanupSessionAfter 在指定延迟后清理 session 目录。
// 受 UploadStore.wg 追踪，支持通过 stopCh 提前中止：
//   - 已在停机中（stopCh 已关）⇒ 不再登记新清理（与 MarkChunkReceived 同形的防御：避免
//     WaitGroup 的 Add 与 Wait 交错）；
//   - 延迟窗口内停机 ⇒ **仍执行一次清理**，否则该会话目录会滞留到 TTL / 下次启动；
//   - 只清理「登记时对应的那个会话对象」：同 upload_id 在窗口内被新 init 复用（审计 C-7；
//     SDK 的 upload_id 由 filename|size|mtime|checksum 派生 ⇒ 同 id 会被复用）时放弃清理，
//     目录与临时名此时归属新会话。
func (us *UploadStore) CleanupSessionAfter(uploadID string, delay time.Duration) {
	select {
	case <-us.stopCh:
		return
	default:
	}
	us.mu.RLock()
	expect := us.sessions[uploadID]
	us.mu.RUnlock()

	us.wg.Go(func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-us.stopCh:
		}
		us.cleanupSessionIfCurrent(uploadID, expect)
	})
}

// cleanupSessionIfCurrent 仅在 map 中 uploadID 仍指向 expect 时删除该会话（身份闸门）。
// expect 为 nil（登记时会话已不存在）或已被替换（同 id 新会话）时放弃，避免误删新会话的
// 目录与临时名（审计 C-7）。
//
// 「身份判定」与「产物删除」必须在**同一个 us.mu 临界区**内完成（RV9-CHUNK-FINAL F-1）：此前是
// 「RLock 读 → RUnlock → 比较 → DeleteSession(id)」，而 DeleteSession 自己加锁后**按 id 重新取值**，
// 因此两次加锁之间存在窗口——期间若 expect 被并发删除（cancel / 过期清理）且同 id 被新 init 接管，
// DeleteSession 会删掉**新会话**的 map 条目、预留与产物，正是本函数要避免的事。登记路径
// GetOrCreateSession 全程持 us.mu（建目录 / 写 session.json / 写 map 都在其中）⇒ 判定与删除同
// 临界区即与登记互斥，窗口消失。代价是产物删除 I/O 期间持 us.mu（短暂），与
// cleanupExpiredArtifacts 同形；锁序 us.mu → writeMu 与 GetOrCreateSession 同向。
//
// 预留归还**不在此临界区内**（见 releaseSessionReservations：只操作 expect 自己的句柄，与 id
// 归属无关）。
//
// 调用点：complete 后的延迟清理（CleanupSessionAfter，审计 C-7）与 init 的**错误路径**
// （清掉「本次刚创建、且仍归本请求」的会话；按 id 直删会在并发接管时误删新会话，
// RV9-CHUNK-FINAL F-2 同族）。
func (us *UploadStore) cleanupSessionIfCurrent(uploadID string, expect *ChunkedUploadSession) {
	if expect == nil {
		return
	}
	// 必须在取 us.mu 写锁前解析（tempAbsPath → tenantRootFor 取 us.mu.RLock，不可重入）。
	tempAbs, hasTemp := us.tempAbsPath(expect)

	us.mu.Lock()
	if us.sessions[uploadID] != expect {
		us.mu.Unlock()
		return
	}
	delete(us.sessions, uploadID)
	us.deleteSessionArtifactsAt(uploadID, tempAbs, hasTemp)
	us.mu.Unlock()

	us.releaseSessionReservations(expect)
}

// recoverSessions 从磁盘恢复未完成的 session。
// per-tenant baseDir（租户 chunk 桶）下直接是会话目录，单层恢复即可。
func (us *UploadStore) recoverSessions() {
	entries, err := os.ReadDir(us.baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		us.logger.Warn("读取分块上传目录失败", "error", err)
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		uploadID := entry.Name()
		sessionDir := filepath.Join(us.baseDir, uploadID)
		sessionPath := filepath.Join(sessionDir, "session.json")

		data, err := os.ReadFile(sessionPath)
		if err != nil {
			// 会话目录存在但 session.json 读不到/缺失（init 期间进程被杀，含 writeSessionJSON
			// 的 Windows 回退窗口）⇒ 该会话无法恢复，回收其会话目录（审计 C-5：此前只 continue
			// ⇒ 目录永久孤儿）。在途临时名无法从损坏记录推导，故不在本分支回收（已知窄口）。
			us.logger.Warn("读取 session.json 失败，回收会话目录", "upload_id", uploadID, "error", err)
			us.deleteSessionArtifacts(uploadID, nil)
			continue
		}

		us.restoreSession(uploadID, sessionDir, data)
	}
}

// restoreSession 从磁盘恢复单个会话（解析 + 过期/完成判断 + 临时文件分片校验）。
func (us *UploadStore) restoreSession(uploadID, sessionDir string, data []byte) {
	var session ChunkedUploadSession
	if err := json.Unmarshal(data, &session); err != nil {
		// 解析失败 ⇒ 无法恢复该会话，回收其会话目录（审计 C-5；同理无法定位在途临时名）。
		us.logger.Warn("解析 session.json 失败，回收会话目录", "upload_id", uploadID, "error", err)
		us.deleteSessionArtifacts(uploadID, nil)
		return
	}
	// 已过期：不恢复，并**就地回收**磁盘产物（在途临时文件 + 会话目录）。审计 C-5：原注释称
	// 「后续由 cleanupExpired 清理」，但 CleanupExpired 只遍历内存 map，而该会话从未进入
	// map ⇒ temp 名（init 已 Truncate(TotalSize)）与会话目录永久孤儿。
	// 预留句柄（Reservation/PoolRes/StorageMgrReserved）均为 json:"-" ⇒ 重启后为 nil/0，
	// 无预留需归还（上游扫描对账另行补齐）。
	if time.Now().After(session.ExpiresAt) {
		us.logger.Info("回收已过期上传会话产物", "upload_id", uploadID, "file_name", session.Filename,
			"expires_at", session.ExpiresAt)
		us.deleteSessionArtifacts(uploadID, &session)
		return
	}
	// 已完成的跳过（保留供 complete 查询）
	if session.Completed {
		us.sessions[uploadID] = &session
		return
	}
	// 任务 4：按临时名（user 桶在途整文件）逐分片重算校验，校准 bitmap——
	// 内容与 checksum 表匹配的分片保留，不匹配/缺失的分片需重传（bitmap 置 false）。
	if session.TempPath != "" {
		us.verifyTempChunks(&session)
	}
	us.sessions[uploadID] = &session
	us.logger.Info("恢复上传会话", "upload_id", uploadID, "file_name", session.Filename,
		"received", countReceived(session.ReceivedChunks), "total", session.TotalChunks)
}

// tempAbsPath 把在途临时文件的存储根相对路径（user/...，相对**目标卷**租户根）派生为绝对路径。
// baseDir 恒为 <默认卷根>/<owner>/chunk（per-tenant store 只归属一个租户），默认卷租户根 =
// 其父目录；多卷会话（session.Volume 非空）按 volTenantRoots 解析目标卷租户根（AD-5：
// temp 与 user 文件同卷）。再拼 user/ 桶相对段。非 user/ 桶（被篡改/异常）返回 ok=false。
func (us *UploadStore) tempAbsPath(session *ChunkedUploadSession) (string, bool) {
	if session == nil || !strings.HasPrefix(session.TempPath, "user/") {
		return "", false
	}
	tenantRoot := us.tenantRootFor(session.Volume)
	abs := filepath.Join(tenantRoot, filepath.FromSlash(session.TempPath))
	// 纵深防御：clean 后必须仍在该租户根内（防 session.json 篡改逃逸）。
	clean := filepath.Clean(abs)
	if clean != tenantRoot && !strings.HasPrefix(clean, tenantRoot+string(filepath.Separator)) {
		return "", false
	}
	return clean, true
}

// verifyTempChunks 打开在途临时文件，逐分片按 checksum 表重算校验：
// 匹配保留 bitmap，不匹配清除（需重传）。临时文件不存在/打不开时全部清除。
func (us *UploadStore) verifyTempChunks(session *ChunkedUploadSession) {
	abs, ok := us.tempAbsPath(session)
	if !ok {
		us.logger.Warn("恢复会话临时文件路径非法，分片全部重传", "upload_id", session.UploadID)
		clear(session.ReceivedChunks)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		us.logger.Warn("恢复会话临时文件不可读，分片全部重传", "upload_id", session.UploadID, "error", err)
		clear(session.ReceivedChunks)
		return
	}
	defer f.Close()

	chunkSize := session.ChunkSize
	for i := 0; i < session.TotalChunks; i++ {
		offset := int64(i) * chunkSize
		want := session.ChunkChecksums[i]
		if want == "" {
			// 无 checksum 记录：无法校验，视为需重传（旧会话/异常状态）。
			session.ReceivedChunks[i] = false
			continue
		}
		// 该分片的实际长度（末片短于 chunk_size），与写侧 chunkLenAt 一致。
		length := chunkSize
		if remaining := session.TotalSize - offset; remaining < chunkSize {
			length = remaining
		}
		ok, err := us.verifyChunkChecksum(f, offset, length, want)
		if err != nil {
			us.logger.Warn("临时文件分片校验失败，需重传", "upload_id", session.UploadID, "chunk_index", i, "error", err)
			session.ReceivedChunks[i] = false
			continue
		}
		if !ok {
			session.ReceivedChunks[i] = false
		}
	}
}

// verifyChunkChecksum 从 f 的 offset 起计算 length 字节的 SHA-256 并与 want 比较。
// 与 pkg/testutil.ChecksumAt 语义一致（分片限定校验工具层次：testutil 为通用无状态版，
// 此处为 server 内部带 logger 版？——实际 ChecksumAt 已够用，但保留本地实现避免与
// testutil 的强耦合，且返回错误经其中间态语义一致）。
func (us *UploadStore) verifyChunkChecksum(f *os.File, offset, length int64, want string) (bool, error) {
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false, err
	}
	lim := io.LimitReader(f, length)
	h := sha256.New()
	if _, err := io.Copy(h, lim); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == want, nil
}

// findMismatchChunks 对临时文件逐分片 seek 重算校验，返回与保存 checksum 表不一致的
// 分片索引（升序）。与恢复期 verifyTempChunks 相同的带长度语义（offset=i*ChunkSize、
// length=chunkLenAt）：临时文件缺失/不可读/读取错误时全部标为 mismatch（客户端整文件重传）。
// 仅对已接收分片（ChunkChecksums[i] 非空）做比对；未接收分片不支持（complete 前置
// AllChunksReceived 已保证全接收，此处防御性跳过空 checksum 分片）。
// 任务 5 I-2：重叠/越界写坏单个分片 → 该单片被精确识别为 mismatch（而非泛化 400）。
func (us *UploadStore) findMismatchChunks(session *ChunkedUploadSession) []int {
	abs, ok := us.tempAbsPath(session)
	if !ok {
		us.logger.Warn("complete 校验临时文件路径非法，全部分片视为 mismatch", "upload_id", session.UploadID)
		return allMismatchIndices(session)
	}
	f, err := os.Open(abs)
	if err != nil {
		us.logger.Warn("complete 校验临时文件不可读，全部分片视为 mismatch", "upload_id", session.UploadID, "error", err)
		return allMismatchIndices(session)
	}
	defer f.Close()

	chunkSize := session.ChunkSize
	var mismatch []int
	for i := 0; i < session.TotalChunks; i++ {
		want := session.ChunkChecksums[i]
		if want == "" {
			continue // 未接收分片：complete 前置已保证全接收，此处防御性跳过
		}
		ok, err := us.verifyChunkChecksum(f, int64(i)*chunkSize, chunkLenAt(session, i), want)
		if err != nil {
			us.logger.Warn("complete 校验临时文件分片失败，全部分片视为 mismatch",
				"upload_id", session.UploadID, "chunk_index", i, "error", err)
			return allMismatchIndices(session)
		}
		if !ok {
			us.logger.Warn("complete 校验分片不匹配（需重传）",
				"upload_id", session.UploadID, "chunk_index", i)
			mismatch = append(mismatch, i)
		}
	}
	return mismatch
}

// allMismatchIndices 返回全部分片索引（0..TotalChunks-1，升序）。
func allMismatchIndices(session *ChunkedUploadSession) []int {
	out := make([]int, session.TotalChunks)
	for i := range out {
		out[i] = i
	}
	return out
}

// countReceived 返回 bitmap 中已置位的数量。
func countReceived(bitmap []bool) int {
	count := 0
	for _, b := range bitmap {
		if b {
			count++
		}
	}
	return count
}

// GetOrCreateSession 根据 uploadID 或文件名查找已有未完成的 session，或创建新 session。
//
// 返回的会话**可能与 store 内部对象是同一对象**（新建路径；复用路径返回副本）。该对象会被
// 并发请求经 GetSession / GetSessionByFilename / PersistNow 整结构深拷贝 ⇒ 对它的写入必须
// 经 SetSessionRoute / SetSessionStorageMgrReserved / SetSessionTempPath 这类锁内 setter，
// **不得直接改返回值上的字段**（否则与读者构成数据竞争，且 string 头/指针可能出现 torn read）。
func (us *UploadStore) GetOrCreateSession(uploadID, filename string, totalSize, chunkSize int64, totalChunks int, fileChecksum string, fileModTime int64) (*ChunkedUploadSession, bool, error) {
	us.mu.Lock()
	defer us.mu.Unlock()

	// 按 uploadID 查找
	if uploadID != "" {
		if s, ok := us.sessions[uploadID]; ok && !s.Completed {
			// 审查 F4：按 key 复用前强制校验 filename/checksum/大小一致——否则攻击者可
			// 预置同 key 会话（伪造 owner 前缀或碰撞）劫持本次续传，篡改目标文件名。
			if s.Filename != filename || s.FileChecksum != fileChecksum || s.TotalSize != totalSize {
				us.logger.Warn("upload_id 冲突且元数据不符，拒绝复用旧会话",
					"upload_id", uploadID, "old_file", s.Filename, "new_file", filename)
				return nil, false, fmt.Errorf("upload_id 已存在但文件元数据不一致")
			}
			us.logger.Info("找到可续传的 session", "upload_id", s.UploadID, "file_name", s.Filename)
			return copySession(s), true, nil
		}
	}

	// 按文件名查找（兼容旧版本 / 无 upload_id 场景）
	for _, s := range us.sessions {
		if s.Filename == filename && !s.Completed && s.FileChecksum == fileChecksum && s.TotalSize == totalSize {
			us.logger.Info("找到可续传的 session（按文件名匹配）", "upload_id", s.UploadID, "file_name", filename)
			return copySession(s), true, nil
		}
	}

	// 创建新 session
	if uploadID == "" {
		return nil, false, fmt.Errorf("upload_id 不能为空")
	}
	session := newSession(uploadID, filename, totalSize, chunkSize, totalChunks, fileChecksum, fileModTime, us.sessionTTL)

	us.logger.Info("创建上传会话", "upload_id", uploadID, "file_name", filename,
		"total_size", totalSize, "chunk_size", chunkSize, "total_chunks", totalChunks)

	// 创建会话目录
	sessionDir := filepath.Join(us.baseDir, uploadID)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return nil, false, fmt.Errorf("创建会话目录失败: %w", err)
	}
	if err := us.writeSessionJSON(session); err != nil {
		os.RemoveAll(sessionDir)
		return nil, false, err
	}

	us.sessions[uploadID] = session
	return session, false, nil
}

// SetSessionRoute 在 store 锁内回写 init 定卷与容器预留结果（AD-5 卷路由 / AD-7 卷容量池 /
// P4 owner Scope）。会话对象会被并发请求整结构深拷贝（见 GetOrCreateSession 注释），故必须锁内写。
//
// 返回 false 表示会话已被并发删除（cancel / 过期清理）。**无接管时**「返回 false ⇔ 会话已被并发
// 删除」成立，调用方据此 fail-closed 回滚：UploadInit 会删掉刚创建的在途临时文件（若其未被新会话
// 认领）、归还本次 routeUpload 的预留与（未曾登记进会话的）P5 回退预留，并回 **409
// Success:false**（见 abortInitOrphanRollback）。
//
// **接管场景下该 iff 不成立**（已登记为独立发现，待后续片修）：三处 setter 都按 uploadID 查表，
// 不校验「表内对象是否仍是本次创建的会话」⇒ 同 id 已被新会话接管时仍返回 true，并把本次 init 的
// route/P5/temp 状态**发布到接管会话对象上**。后果：接管方自己的 route/pool 句柄被覆盖而未释放
// （预留泄漏）、其 TempPath 可能指向本次请求的在途临时名（并随 PersistNow 落盘，跨重启持久），
// 最坏是「接管方的上传卡到 TTL/清理 + 账本泄漏 + 对本次客户端谎报 200」；**不会静默产出损坏或
// 错长文件**（合并前对整临时文件做全量哈希比对 FileChecksum，污染必被检出）。
// 修法方向：用世代 token（或身份门控的原子发布）替代按 id 查表。
func (us *UploadStore) SetSessionRoute(uploadID, volume string, res *quota.Reservation, pool *quota.Pool, poolRes *quota.Reservation) bool {
	us.mu.Lock()
	defer us.mu.Unlock()
	s, ok := us.sessions[uploadID]
	if !ok {
		return false
	}
	s.Volume = volume
	s.Reservation = res
	s.Pool = pool
	s.PoolRes = poolRes
	return true
}

// SetSessionStorageMgrReserved 在 store 锁内登记 P5（storageMgr 回退）预留字节数。
// 返回 false 的语义（含接管场景的例外）见 SetSessionRoute 注释。
func (us *UploadStore) SetSessionStorageMgrReserved(uploadID string, bytes int64) bool {
	us.mu.Lock()
	defer us.mu.Unlock()
	s, ok := us.sessions[uploadID]
	if !ok {
		return false
	}
	s.StorageMgrReserved = bytes
	return true
}

// SetSessionTempPath 在 store 锁内回写在途整临时文件（目标卷 user 桶）相对路径。
// 返回 false 的语义（含接管场景的例外）见 SetSessionRoute 注释。
func (us *UploadStore) SetSessionTempPath(uploadID, tempRel string) bool {
	us.mu.Lock()
	defer us.mu.Unlock()
	s, ok := us.sessions[uploadID]
	if !ok {
		return false
	}
	s.TempPath = tempRel
	return true
}

// RemoveUnclaimedTemp 在一次 us.mu.RLock 临界区内完成「身份判定 + 删除」，供 init 回滚删除本次
// 遗留的在途临时文件：仅当当前会话记录**未**把 tempRel 记为在途临时名（该文件尚无人认领）时才
// 调用 remove。
//
// 判定与删除必须同临界区（RV9-CHUNK-FINAL 建议①）：若先 GetSession 取副本、放锁后再删，两步之间
// 新会话可能恰好发布同一 TempPath（新会话可能已建同名文件但尚未发布）⇒ 会删掉它的在途文件。
// 判定通过时 remove 在本临界区内执行 ⇒ 与新会话「发布 TempPath」（持写锁）互斥。
// 注意：临界区内**不得**调用会二次取锁的函数（GetSession / tempAbsPath —— RWMutex 在有等待写者
// 时递归 RLock 会死锁），故路径解析由调用方在外层完成并以闭包传入。
//
// 返回 claimed=true 表示该 id 的会话已认领同一临时名（调用方应跳过删除并告警）；claimed=false
// 表示已执行 remove（其错误原样返回）。
func (us *UploadStore) RemoveUnclaimedTemp(uploadID, tempRel string, remove func() error) (claimed bool, err error) {
	us.mu.RLock()
	defer us.mu.RUnlock()
	if s, ok := us.sessions[uploadID]; ok && s.TempPath == tempRel {
		return true, nil
	}
	return false, remove()
}

// MissingChunks 返回缺失的分块索引列表。
func MissingChunks(session *ChunkedUploadSession) []int {
	var missing []int
	for i, received := range session.ReceivedChunks {
		if !received {
			missing = append(missing, i)
		}
	}
	return missing
}
