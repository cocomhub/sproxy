// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// manager_task.go 是**任务创建与执行**：CreateTask（含同 owner 去重）、SubmitAndStart（异步起点）、
// executeDownload（下载主循环，含限额/重试/配额结算）、failTask（终态收口）、refreshTaskGroup。//
// 拆分说明见 manager.go 顶部。

package cloud

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/quota"
)

// 自动去重：相同 URL 且**对请求者可见**（同 owner 或全局空 owner）的活跃任务返回已有任务。
func (m *CloudDownloadManager) CreateTask(method, url, filename string, totalSize int64, owner string, params TaskParams) (*CloudTask, error) {
	// 真空洞校验：不下载本地 + 不转存 + 不保留 cloud 桶 = 无任何产出 → 拒绝（fail-closed）。
	if !params.DownloadLocal && params.Transfer == nil && !params.Save {
		return nil, fmt.Errorf("cloud download: 语义空洞（download_local=false + 无 transfer + save=false 无任何产出）")
	}
	// 前置判断（避免浪费资源下载）：转存目标卷须在创建时已可解析（装配 + 协议声明）。
	if err := m.checkTransferVolumePreflight(params.Transfer); err != nil {
		return nil, err
	}
	// URL 去重：仅对请求者可见的任务去重（跨 owner 的同 URL 任务不吸收，各自独立下载）。
	// M3：去重命中仅当**语义一致**（transfer/save/download_local 三参与请求相同）时吸收，
	// 否则不吸收创建新任务——否则本次的转存/保留意图被静默吞掉（去重吸收改变语义）。
	if existing := m.findByURL(url, owner, params); existing != nil {
		m.logger.Info("duplicate cloud download request, reusing existing task",
			"url", url,
			"existing_id", existing.ID,
			"existing_status", existing.Status,
		)
		return existing, nil
	}

	// 预留存储空间（全局账本 storageMgr 保留，供 /api/stats 与既有测试；租户配额并行）。
	reserved := totalSize
	if reserved <= 0 {
		reserved = cloudReservePlaceholder // 1 GiB 保底
	}
	if err := m.storage.TryReserveCloud(reserved); err != nil {
		m.logger.Warn("storage full, cloud download rejected",
			"url", url,
			"requested_size", totalSize,
			"current_usage", m.storage.Usage(),
			"max_bytes", m.storage.MaxBytes(),
		)
		return nil, err
	}

	// P4/P5 租户配额（任务 7 迁移到下载流 QuotaWriter 边写边记）：
	//   - **已知大小**（totalSize>0）：创建期在**租户根 Scope**（上限=owner_quota）上做
	//     容量预检，不足即 507（保留"创建即拒绝"同步语义，与旧行为一致——旧实现创建期
	//     在 cloud 桶 Scope TryReserve 沿父链到租户根被拦截）；
	//   - **未知大小**（totalSize<=0）：不预检占位，延迟到下载流首次写盘（NewQuotaWriter
	//     占位 1 GiB 预留；失败→本次下载 failed）。小文件完成后 Scope 不再虚高 1 GiB 占位。
	// 预检用租户根 Scope 的 Available（父链向上汇总 committed+reserved）。Scope 装配在
	// quotaFor 注入的 cloud 桶 Scope；租户根经其父链不可直接取指针，改由装配时额外注入
	// 一个"租户根 Scope 解析器"不可得——故用 cloud 桶 Scope 上溯：Objective 简化为对
	// cloud 桶 Scope.TryReserve(totalSize) 立即 Release（副作用=沿父链校验租户根/全局上限）。
	if totalSize > 0 {
		if scope := m.quotaScope(owner); scope != nil {
			// 临时预留探测：TryReserve 沿父链校验（租户根 owner_quota + globalPool max），
			// 满足则立即 Release（探测不落地 committed）。满足=放行；不满足=507。
			probe, err := scope.TryReserve(totalSize)
			if err != nil {
				m.storage.ReleaseCloud(reserved)
				m.logger.Warn("storage full, cloud download rejected",
					"url", url,
					"requested_size", totalSize,
					"owner", owner,
				)
				return nil, quota.ErrStorageFull
			}
			probe.Release()
		}
	}

	task := &CloudTask{
		ID:           newTaskID(),
		Owner:        owner,
		URL:          url,
		Method:       method,
		Filename:     filename,
		Status:       "pending",
		TotalSize:    totalSize,
		ReservedSize: reserved,
		// 浅拷贝 Transfer：组内多子任务共享同一请求指针时，各任务独立持有副本——
		// 否则 transferDone 的 OwnerPrefix 原地写（共享卷）对并发组转存构成 write-write
		// 数据竞争（同值写入，-race 可报）。深度只拷贝顶层字段（Volume/Path/OwnerPrefix）。
		Transfer:       cloneTransferSpec(params.Transfer),
		DownloadLocal:  params.DownloadLocal,
		Save:           params.Save, // 客户端任务参数（默认 true 保留；false = 完成即服务端清理）
		ForceIntegrity: params.ForceIntegrity,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
		ExpiresAt:      time.Now().Add(m.config.TaskTTL),
	}

	m.mu.Lock()
	m.tasks[task.ID] = task
	m.mu.Unlock()

	_ = m.saveTask(task)
	m.logger.Info("cloud download task created",
		"task_id", task.ID,
		"url", url,
		"filename", filename,
		"reserved_size", totalSize,
	)
	m.metrics.TasksCreated.Add(1)
	return task, nil
}

// SubmitAndStart 创建任务并立即启动下载。
// 仅当调用方已知 totalSize > 0 且 < syncThreshold 且 syncCtx 非 nil 时才同步执行
// （在调用方 goroutine 内完成，便于小文件请求同步返回）；否则始终异步。
// 注意：服务端 handler 提交时大小未知（传 -1），因此实际请求恒异步；
// 同步路径主要供调用方在已知小文件大小时使用。
func (m *CloudDownloadManager) SubmitAndStart(method, url, filename string, totalSize int64, syncCtx context.Context, owner string, params TaskParams) (*CloudTask, error) {
	task, err := m.CreateTask(method, url, filename, totalSize, owner, params)
	if err != nil {
		return nil, err
	}

	// 启动必须使用 m.tasks 中的真实对象，而非 CreateTask 返回的对象：
	// CreateTask 去重命中时返回 findByURL 的快照副本，对副本启动 goroutine 会
	// 导致 executeDownload 全程只写副本、真实对象永远停在 pending（findByURL
	// 持续命中使同 URL 无法再下载、任务卡死，直到进程重启自愈）。
	// 在写锁内检查 Status 并同步置位 running，闭合"检查→启动"竞态窗口，避免
	// 对同一 URL 并发启动两个 goroutine 写同一 .partial（Critical 修复）。
	m.mu.Lock()
	realTask, ok := m.tasks[task.ID]
	if !ok || realTask.Status != "pending" || m.running[realTask.ID] {
		m.mu.Unlock()
		return task, nil
	}
	m.running[realTask.ID] = true
	m.mu.Unlock()

	useSync := syncCtx != nil && totalSize > 0 && totalSize < m.config.SyncThreshold

	if useSync {
		m.logger.Info("starting sync cloud download", "task_id", realTask.ID, "url", url, "size", totalSize)
		// 同步下载：直接在当前 goroutine 执行，wg.Add(1) 在 go 之前确保不竞态
		m.wg.Add(1)
		m.executeDownload(syncCtx, realTask)
		return task, nil
	}

	m.logger.Info("starting async cloud download", "task_id", realTask.ID, "url", url, "size", totalSize)
	// 异步下载：goroutine 执行，wg.Add(1) 在 go 之前确保不竞态
	//nolint:gosec
	m.wg.Add(1)
	go m.executeDownload(context.Background(), realTask) //nolint:gosec
	return task, nil
}

// removeTaskFile 是任务产物删除的**单次尝试** seam：默认委托 os.Remove。生产路径不替换。
//
// 存在的意义是确定性验证「先清理文件、后发布终态」的顺序不变量：观察者（API /
// SnapshotTask / 测试轮询）一旦读到 failed，就不应再看到该任务的文件残留。真实文件系统
// 时序跨平台不可确定（Windows 还受句柄共享冲突影响），故用 seam 让测试能在删除发生的
// 那一刻读取任务状态来钉住顺序（见 TestCloudDownloadManager_StorageFullAfterDownload_
// DeletesAndReleases），也能注入「首次失败、再次成功」的故障来验证有界重试。
var removeTaskFile = os.Remove

// removeRetries / removeRetryDelay 是任务产物清理的有界重试预算（见 removeWithRetry）。
const (
	removeRetries    = 3
	removeRetryDelay = 10 * time.Millisecond
)

// removeWithRetry 对任务产物清理做有界重试（removeRetries 次、间隔 removeRetryDelay）。
//
// 为什么必须重试：Windows 上 os.Remove/os.RemoveAll 会在句柄刚释放、杀毒/索引器短暂
// 持有、共享句柄尚未关闭时以 ERROR_SHARING_VIOLATION 瞬时失败，而同一路径随后即可删除。
// 不重试就会留下「任务已 failed 而文件仍在盘上」的可观测不一致（CI run 34958930590 的
// `stat err=<nil>` 即此）。写盘句柄在下载器内部已先关闭再 rename（见
// pkg/downloader/http_downloader.go 的 f.Close() 在 finalizeDownload 之前），故此处
// 无需额外的句柄同步。
//
// 预算固定且极小：永久性错误（权限/路径非法）最多多花
// removeRetryDelay*(removeRetries-1)=20ms 后原样返回，由调用方并入任务错误对用户可观测。
// 目标不存在视为成功（已删除/并发清理）。
func removeWithRetry(remove func() error) error {
	var err error
	for attempt := range removeRetries {
		if attempt > 0 {
			time.Sleep(removeRetryDelay)
		}
		if err = remove(); err == nil || os.IsNotExist(err) {
			return nil
		}
	}
	return err
}

// removeDiscardedTaskFiles 删除 force 续传要丢弃的产物（结果文件 + .partial + .partial.etag），
// 返回**确实已从磁盘消失**的常规文件字节数，供调用方按实际移除量回拨配额占用。
//
// 为什么不是「删除前量到的字节」：删除可能失败（Windows 句柄占用/杀软短暂持有，与
// removeWithRetry 文档里的同一类瞬时错误；重试耗尽后仍可能失败）。失败时字节**仍占磁盘**，
// 照样回拨会让账本低于磁盘（fail-open：租户短时可越过 max_storage_bytes），而偏高只由
// ≤30 min 周期扫描收敛（fail-closed，与回收路径的取舍一致）。
//
// 判据取「删除后再看一次」这一**可观测事实**，而不是删除调用的返回值：本次删除成功、或字节
// 已被并发清理删掉，两种情况下这些字节都确已不在磁盘，回拨都是对的；仍在盘上（含状态无法
// 确认，例如权限错误让 Lstat 也失败）则保守不回拨。
//
// remove 以参数注入（生产传 removeTaskFile）：os.Remove 的失败无法在测试里跨平台确定性制造。
func removeDiscardedTaskFiles(destPath string, remove func(string) error) int64 {
	var removed int64
	for _, path := range []string{destPath, destPath + ".partial", destPath + ".partial.etag"} {
		if n, gone := removeDiscardedFile(path, remove); gone {
			removed += n
		}
	}
	return removed
}

// removeDiscardedFile 删除单个产物路径，返回 (已消失的字节数, 是否可视为已从磁盘消失)。
//
// 只统计常规文件（用 Lstat：符号链接不计其目标大小——链接被删不等于目标字节消失）；目录等
// 非普通文件不贡献字节（无可计量的内容）。路径本就不存在即视为已消失（无可回拨字节，也不算
// 失败）。
func removeDiscardedFile(path string, remove func(string) error) (int64, bool) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, os.IsNotExist(err)
	}
	_ = removeWithRetry(func() error { return remove(path) })
	if _, serr := os.Lstat(path); serr == nil || !os.IsNotExist(serr) {
		return 0, false
	}
	if !fi.Mode().IsRegular() {
		return 0, true
	}
	return fi.Size(), true
}

// cleanupIncompleteError 在清理失败时把「清理未完成」并入任务错误文本（对用户可观测）：
// 任务已发布终态（failed）但文件仍残留时，用户必须能从 task.Error 看出磁盘未被释放。
//
// 只写**错误类别**、不写原始错误：task.Error 会经 GET /api/cloud/tasks/{id} 原样返回给
// 客户端，而 *os.PathError 的 `%v` 含 Op 与服务端绝对路径 ⇒ 信息披露。原始错误（含路径）
// 由调用方记入日志。
func cleanupIncompleteError(reason string, removeErr error) string {
	if removeErr == nil {
		return reason
	}
	return fmt.Sprintf("%s (cleanup incomplete: %s)", reason, cleanupFailureClass(removeErr))
}

// errSharingViolation 是 Windows ERROR_SHARING_VIOLATION：路径仍被句柄/杀软/索引器占用。
const errSharingViolation syscall.Errno = 32

// cleanupFailureClass 把清理失败归类为对用户安全的简短原因（不含路径，见 cleanupIncompleteError）。
// 分类必须覆盖 Windows 共享违规：它与 syscall.EBUSY **不等价**——Windows 上 EBUSY 属
// APPLICATION_ERROR 人造值域（1<<29+），匹配不到任何真实 errno ⇒ 需按数值判定，且仅在
// Windows 上成立（其它平台 32 是别的 errno，不能误判）。
func cleanupFailureClass(err error) string {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, syscall.EBUSY), runtime.GOOS == "windows" && errors.Is(err, errSharingViolation):
		return "resource busy"
	default:
		return "unknown error"
	}
}

// errIntegrityFail 是完整性判定「重下」哨兵：语义校验失败但尚未到两次一致（可能为瞬时
// 异常/传输损坏），返回后由 runRetryLoop 继续 retry 循环（shouldRetryDownload 认可）。
var errIntegrityFail = errors.New("integrity: 语义校验失败，重下重试")

// errIntegrityPermanent 是完整性判定「永久」哨兵：两次本地 checksum 一致仍语义异常
// （源文件损坏或类型误判）。按任务处置：默认放行标记 damaged / ForceIntegrity 阻断。
var errIntegrityPermanent = errors.New("integrity: 两次校验一致仍异常，源损坏或类型误判")

// maxIntegritySames 是「本地 checksum 一致但语义校验仍失败」的收敛阈值：2 次后不再重下
// （Review Focus 3：避免对损坏源无限重下）。
const maxIntegritySames = 2

// checkDownloadIntegrity 是 runRetryLoop 成功分支的完整性判定嵌入点（步骤 4）：
// 依据下载器声明的完整性归属（IntegrityProvider 断言）与语义校验管道决定重下/放行/阻断。
//
// 判定逻辑（对齐设计规格 §3）：
//   - ① 权威匹配（ModeAuthority，下载器已在内部完成权威校验）→ 置 verified，跳过语义校验 → nil；
//   - ② 其余模式（Unknown/LocalOnly/SelfVerified）→ integrity.Lookup(ext)：
//   - nil（未知类型/无校验器装配）→ 视为通过（不误报 damaged）→ 返回 nil（不设状态）；
//   - Check 通过 → 返回 nil；
//   - Check 失败 → 语义异常：累计「本地 checksum 一致仍异常」次数（task.integritySames，
//     跨 attempt 保留）；2 次 → errIntegrityPermanent；1 次 → errIntegrityFail（重下）。
//
// 返回 nil 表示校验通过/跳过（继续完成路径）；errIntegrityFail/errIntegrityPermanent 由调用方
// 按错误处置段分派。destPath 供 Check 校验落盘文件；result.Checksum 作本地一致性累计依据。
func (m *CloudDownloadManager) checkDownloadIntegrity(ctx context.Context, task *CloudTask, destPath string, result *downloader.Result) error {
	// ① 权威匹配（下载器已权威确认，如 pikpak GCID 复算命中官方 hash）：跳过语义校验，
	// 直接 verified。判据用 **result.Integrity（本次下载产物级）**——pikpak finalizeDownload
	// 在 GCID 复算命中时置 ModeAuthority + AuthorityHash；IntegrityProvider 接口仅表达
	// 下载器类型声明（HTTP=self_verified / pikpak=local_only），不表达单次产物命中（R1-C1）。
	if result.Integrity == downloader.ModeAuthority && result.AuthorityHash != "" {
		m.setTaskIntegrityStatus(task, "verified")
		return nil
	}
	// ② 语义校验（本地自洽）：Lookup 未命中（未知类型/无校验器装配）视为通过——
	// 不误报 damaged（Review Focus 1）。
	if m.integrityLookup == nil {
		return nil
	}
	checker := m.integrityLookup(filepath.Ext(destPath))
	if checker == nil {
		// R2-P2：未知类型（无校验器）默认放行；force=true 时记录审计日志（显式强制
		// 校验对未覆盖类型名不副实——不阻断，但可审计覆盖缺口）。
		if task.ForceIntegrity {
			m.logger.Warn("force-integrity: 类型无校验器，跳过语义校验（覆盖范围外）",
				"task_id", task.ID, "ext", filepath.Ext(destPath))
		}
		return nil
	}
	rpt, err := checker.Check(ctx, destPath, result.Size)
	if err != nil {
		// 校验执行本身出错（文件打开失败等）——不是语义异常，放行（不重下不误报）。
		m.logger.Warn("integrity check execution failed, treating as passed", "task_id", task.ID, "error", err)
		return nil
	}
	if rpt.OK {
		m.setTaskIntegrityStatus(task, "verified")
		return nil
	}
	// 语义异常：仅当本次 checksum 与上次校验失败 attempt 一致才累计（R1-C2——「两次
	// 本地 checksum 一致仍异常」= 源损坏/类型误判 → 永久；不同 = 瞬态损坏可恢复 → 重置
	// 重试，不误判 damaged）。持锁防 data race（跨 attempt 保留）。
	m.mu.Lock()
	if result.Checksum != "" && result.Checksum == task.integrityLastChecksum {
		task.integritySames++
	} else {
		task.integritySames = 1
	}
	task.integrityLastChecksum = result.Checksum
	permanent := task.integritySames >= maxIntegritySames
	m.mu.Unlock()
	if permanent {
		return errIntegrityPermanent
	}
	return errIntegrityFail
}

// resolveIntegrityTaskErr 把完整性判定的哨兵错误解析为 retry 循环需要的 downloadErr。
//   - errIntegrityFail（1 次异常）→ 返回哨兵（继续循环，下方 shouldRetryDownload 重下）；
//   - errIntegrityPermanent（两次一致仍异常）：
//   - ForceIntegrity → 返回哨兵（错误上行 handleDownloadDone → failTask，阻断）；
//   - 默认 → 置 IntegrityStatus="damaged" 并返回 nil（放行，按成功完成）。
func (m *CloudDownloadManager) resolveIntegrityTaskErr(task *CloudTask, ierr error) error {
	if !errors.Is(ierr, errIntegrityPermanent) {
		return ierr
	}
	if task.ForceIntegrity {
		return ierr
	}
	m.setTaskIntegrityStatus(task, "damaged")
	return nil
}

// setTaskIntegrityStatus 持锁写 task.IntegrityStatus（与 SnapshotTask/saveTask 的
// RLock 读对称，防 data race——审查 P1 修复）。
func (m *CloudDownloadManager) setTaskIntegrityStatus(task *CloudTask, status string) {
	m.mu.Lock()
	task.IntegrityStatus = status
	m.mu.Unlock()
}

// postDownloadCheck 是 runRetryLoop 成功分支的收尾判定：记录 ETag + 完整性校验。
// 返回 (是否已到终态完成, 最终 downloadErr)：
//   - 本次尝试失败（downloadErr != nil）→ (false, 原错误)（继续循环或终止由调用方判定）；
//   - 成功但完整性校验失败 → resolveIntegrityTaskErr 分派（fail→重下哨兵 / force→阻断哨兵 /
//     默认 permanent→damaged 放行 nil），返回 (false, 哨兵)（仍需循环判定）；
//   - 成功且校验通过/放行 → (true, nil)：调用方 break 按完成处理。
func (m *CloudDownloadManager) postDownloadCheck(ctx context.Context, task *CloudTask, destPath string, result *downloader.Result, downloadErr error) (bool, error) {
	if downloadErr != nil {
		return false, downloadErr
	}
	// 立即记录 ETag 到 task：续传重试时可通过 task.ETag 做二次校验，
	// 完成后客户端也可通过 API 读取 ETag 确认版本。
	m.recordTaskETag(task, result)
	// 完整性判定嵌入点：成功下载后校验内容语义可用性（重下 / 放行 damaged /
	// Force 阻断三向分派在 resolveIntegrityTaskErr）。
	if ierr := m.checkDownloadIntegrity(ctx, task, destPath, result); ierr != nil {
		return false, m.resolveIntegrityTaskErr(task, ierr)
	}
	return true, nil
}

// executeDownload 执行实际下载逻辑。
// 注意：调用者必须保证在调用前已调 m.wg.Add(1)，函数退出时自动 m.wg.Done()。
func (m *CloudDownloadManager) executeDownload(ctx context.Context, task *CloudTask) {
	defer m.wg.Done()
	// handedOff 标记"同步下载断连后已把下载转交给新 goroutine"：
	// 转交时 running 标记由新 goroutine 接管，旧 goroutine 的 defer 不得清除，
	// 否则新 goroutine 会短暂丢失 running，导致 ResumeTask 并发启动。
	var handedOff bool
	// 创建可取消的 context（从 Background 派生，使客户端断连后下载可继续异步重试）。
	// 必须在等待信号量之前注册 cancelFuncs：排队中的任务也能被取消/删除。
	dlCtx, cancel := context.WithCancel(context.Background()) // NOSONAR: S8239 — 刻意从 Background 派生：客户端断连后下载任务继续异步重试（见上注释）
	defer cancel()

	// cleanupRunning 清理 running/cancelFuncs 标记。
	// 拆为独立函数，让 panic recovery 可先调用 failTask 再清理。
	// 同时在此释放「已放弃」任务的租户配额（取消/删除）：释放必须发生在**最后一次
	// commit 之后**，goroutine 退出是唯一能保证这一点的时点。顺序：**先清 running、
	// 后释放**（releaseAbandonedTaskScope 判据以 running 为唯一依据，先清使释放时
	// running 已清 ⇒ 配额归零；若先释放则判据见 running 为真跳过，释放点错过——
	// T3 泄漏窗口根治关键）。不变量结论：release 与 delete(running) 同一 m.mu 临界区，
	// waitTaskStopped 观察者（running 已清）必在释放完成后醒来。
	//
	// 注册时机：必须在写入任何运行时标记（cancelFuncs/running）**之前**注册。标记一旦
	// 写入就必须有人负责清除；若在写入之后、defer 注册之前发生 panic，会留下永久 running
	// 标记——CancelTask 会把租户配额释放推迟到「goroutine 退出路径」，而该 goroutine 已经
	// 退出 ⇒ 释放永不发生，同时 ResumeTask 也会因 running 为真而永久拒绝恢复。
	cleanupRunning := func() {
		if handedOff {
			return
		}
		m.mu.Lock()
		// T3：先清 running 再释放——releaseAbandonedTaskScope 判据以 running 为唯一依据，
		// 先清使释放时 running 已清（goroutine 已退 ⇒ 配额归零）；若先释放则判据见
		// running 为真跳过，释放点错过（本次泄漏窗口 CI run 35419872637 根治关键）。
		delete(m.running, task.ID)
		delete(m.cancelFuncs, task.ID)
		m.releaseAbandonedTaskScope(task)
		m.mu.Unlock()
	}
	defer cleanupRunning()

	// 标记运行中：ResumeTask/Cancel 竞争保护依赖该标记区分"goroutine 仍存活"
	// 与"已退出"。必须在任何可能的写盘操作前设置。
	m.mu.Lock()
	m.cancelFuncs[task.ID] = cancel
	m.running[task.ID] = true
	m.mu.Unlock()

	// 任务进入终态后刷新所属组状态，保证持久化的组状态不滞后于子任务实际进展
	defer m.refreshTaskGroup(task)

	// panic recovery 放在最后（最外层 defer，最先执行），
	// 确保 panic 时先调用 failTask 再清理 running（避免 ResumeTask 在 failTask
	// 完成前误判 goroutine 已退出并发起重写 .partial）。
	defer func() {
		if r := recover(); r != nil {
			m.logger.Error("panic in download", "task_id", task.ID, "panic", r)
			m.failTask(task, fmt.Sprintf("panic: %v", r))
			// failTask 之后刷新组状态（panic 时 refreshTaskGroup 已先执行，但那时状态是
			// downloading 非终态，这里再调一次确保用 failed 状态更新组）。
			m.refreshTaskGroup(task)
		}
	}()

	// 竞态守卫：goroutine 启动前/排队期间任务可能已被取消（CancelTask 已置
	// cancelled 并释放存储）。此时直接退出，不启动下载、不覆盖终态。
	m.mu.RLock()
	cancelled := task.Status == "cancelled"
	m.mu.RUnlock()
	if cancelled {
		m.logger.Info("download skipped, task was cancelled before start", "task_id", task.ID)
		return
	}

	// 排队等待信号量（排队期间可取消）。NM5：槽下载完成后释放（转存不占槽），
	// 故 defer 条件释放（acquiredSlot 标记），转存前手动 release 后不再双释。
	var acquiredSlot bool
	defer func() {
		if acquiredSlot {
			<-m.semaphore
		}
	}()
	select {
	case m.semaphore <- struct{}{}:
		acquiredSlot = true
	case <-dlCtx.Done():
		// 排队期间被取消：CancelTask 已（或即将）置 cancelled 并释放存储。
		// 这里不 failTask——把"已取消"标成 failed 会与 CancelTask 的终态打架，
		// 让用户看到错误的失败状态；直接退出，终态由 CancelTask 写入。
		m.logger.Info("queued download cancelled", "task_id", task.ID)
		return
	}

	// 取得信号量后复查一次：CancelTask 可能恰在 acquire 与这里之间执行
	m.mu.RLock()
	cancelled = task.Status == "cancelled"
	m.mu.RUnlock()
	if cancelled {
		m.logger.Info("download skipped, task cancelled while acquiring slot", "task_id", task.ID)
		return
	}

	m.metrics.ActiveDownloads.Add(1)
	defer m.metrics.ActiveDownloads.Add(-1)

	// 复查任务状态与存在性并置 downloading：CancelTask/DeleteTask 可能在信号量获取与
	// 此处之间执行（详见 markDownloading）。状态已非 pending/downloading → 放弃下载。
	if !m.markDownloading(task) {
		return
	}
	_ = m.saveTask(task)

	m.logger.Info("download started", "task_id", task.ID, "url", task.URL, "filename", task.Filename)

	// 构建目标文件路径（按任务 owner 落租户 cloud 桶）
	taskDir := m.TaskDirFor(task.Owner, task.ID)
	if taskDir == "" {
		m.logger.Warn("租户不可用，无法创建任务目录", "task_id", task.ID, "owner", task.Owner)
		m.failTask(task, "tenant unavailable")
		return
	}
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		m.logger.Warn("创建任务目录失败", "task_id", task.ID, "dir", taskDir, "error", err)
		m.failTask(task, fmt.Sprintf("create task dir: %v", err))
		return
	}
	destPath := filepath.Join(taskDir, task.Filename)

	// 执行下载（带重试）
	result, downloadErr := m.runRetryLoop(ctx, dlCtx, task, destPath)
	// downloadDone 段：任务删除竞态守卫 + 失败路径分派（异步继续 / 取消 / failTask）。
	// 返回 handled=true 表示已由本函数处理终态（调用方直接返回）；异步转交分支经
	// handedOff 指针置位（旧 goroutine 的 defer 不清除 running/cancelFuncs）。
	if m.handleDownloadDone(ctx, dlCtx, task, destPath, result, downloadErr, &handedOff) {
		return
	}
	// NM5：下载完成释放下载并发槽——转存写卷（远程卷上传慢）不应继续占 MaxConcurrent
	// 槽阻塞后续下载；转存并发由独立 transferSem 限流（防风暴）。置 acquiredSlot=false
	// 防 defer 双释（channel 空取 panic）。
	if acquiredSlot {
		<-m.semaphore
		acquiredSlot = false
	}
	// 转存（客户端任务参数 Transfer 非 nil）：下载产物 → 目标卷（secretdata 自动加密/
	// 普通卷纯上传），流程层不感知加密。失败分类：目标卷异常 3 次指数重试；文件异常
	// 删本地重下载（两次校验和一致仍失败 → 任务失败）。KeepLocal=false 转存成功后删本地。
	var handled bool
	if handled, result = m.transferAfterDownload(ctx, dlCtx, task, destPath, result); handled {
		return
	}
	// 成功路径终态提交：锁内复查存在/未取消 → 全局账本对账 → 置 completed。
	// F1：result 已是转存重下后的最终值（finalize 收口一致）。
	m.finalizeCompleted(task, result, destPath)
	// b1/M4：Save=false 且客户端不下载本地 → 服务端自动删 cloud 桶文件并记录清理状态
	// （审计可查，客户端异常也不残留）。统一在 finalize 后执行（此前只在
	// transferAfterDownload 内——纯下载 Transfer==nil 亦可，只要满足同门）。
	// 客户端要下载本地（DownloadLocal=true）→ 保留（链式 archive/下载拉取后客户端删）。
	// save=true 由客户端链式 keep-files/显式 delete 控制。
	m.cleanupCloudIfNotNeeded(task, destPath)
}

// cleanupCloudIfNotNeeded Save=false 且客户端不下载本地时服务端删 cloud 桶文件（save=false
// 语义的单一收口；executeDownload 成功尾部调用）。客户端拉取本地/保留副本时不删。
func (m *CloudDownloadManager) cleanupCloudIfNotNeeded(task *CloudTask, destPath string) {
	if task.Save || task.DownloadLocal {
		return
	}
	m.cleanupTaskCloud(task, destPath)
}

// runRetryLoop 执行带重试的下载主循环：每次尝试独立超时（超时可重试续传，用户取消
// 不可重试）。外层 ctx（同步下载的请求 ctx）已取消而内层未取消 = 客户端断连：立即
// 交还，由 downloadDone 处转入异步继续，避免阻塞 handler 至重试耗尽。
func (m *CloudDownloadManager) runRetryLoop(ctx context.Context, dlCtx context.Context, task *CloudTask, destPath string) (*downloader.Result, error) {
	maxRetries := m.config.MaxRetries
	var result *downloader.Result
	var downloadErr error
	var timedOut bool
retryLoop:
	for attempt := range maxRetries {
		// 外层 ctx（同步下载的请求 ctx）已取消而内层未取消 = 客户端断连：
		// 立即交还，由 downloadDone 处转入异步继续，避免阻塞 handler 至重试耗尽。
		if outerCtxCancelled(ctx, dlCtx) {
			downloadErr = ctx.Err()
			break retryLoop
		}
		if stopped, waitErr := m.retryAttemptWait(ctx, dlCtx, attempt, task, maxRetries); stopped {
			downloadErr = waitErr
			break retryLoop
		}

		result, downloadErr, timedOut = m.runDownloadAttempt(ctx, dlCtx, task, destPath)

		// 成功分支收尾：记录 ETag + 完整性判定（fail→重下哨兵继续循环；force→阻断哨兵；
		// 默认 permanent→damaged 放行 nil → done=true 直接 break 按完成处理）。
		if done, dErr := m.postDownloadCheck(ctx, task, destPath, result, downloadErr); done {
			break
		} else {
			downloadErr = dErr
		}

		// 用户取消/任务删除：停止重试
		if dlCtx.Err() != nil {
			downloadErr = dlCtx.Err()
			break
		}
		// 仅重试可重试错误（网络/5xx）或本次尝试超时；最后一次尝试失败不再重试。
		if !shouldRetryDownload(downloadErr, timedOut, attempt, maxRetries) {
			break
		}
	}
	// R2-P2：完整性校验失败重试耗尽（errIntegrityFail 已不可重试）且任务非 force →
	// 按规格「默认放行标记 damaged」处理（而非 failTask——动态源每次内容不同时，
	// checksum 不一致永不触 permanent，耗尽后应放行标记而非失败）。
	if errors.Is(downloadErr, errIntegrityFail) && !task.ForceIntegrity {
		m.setTaskIntegrityStatus(task, "damaged")
		return result, nil
	}
	return result, downloadErr
}

// outerCtxCancelled 外层 ctx 已取消而内层未取消 = 客户端断连（立即取消继续重试）。
func outerCtxCancelled(ctx, dlCtx context.Context) bool {
	return ctx.Err() != nil && dlCtx.Err() == nil
}

// retryAttemptWait 等待重试（仅 attempt>0 时触发）：等待期间用户取消/客户端断线则
// 立即停止并返回停止原因；等待结束输出重试日志。返回 stopped=true 表示应终止重试。
func (m *CloudDownloadManager) retryAttemptWait(ctx, dlCtx context.Context, attempt int, task *CloudTask, maxRetries int) (bool, error) {
	if attempt <= 0 {
		return false, nil
	}
	stopped, waitErr := m.retryWait(ctx, dlCtx)
	if stopped {
		return true, waitErr
	}
	m.logger.Info("retrying download", "task_id", task.ID, "url", task.URL, "attempt", attempt+1, "max", maxRetries)
	return false, nil
}

// recordTaskETag 把下载结果的 ETag 记录到任务（续传重试二次校验与客户端 API 读取用）。
func (m *CloudDownloadManager) recordTaskETag(task *CloudTask, result *downloader.Result) {
	if result.ETag != "" {
		m.mu.Lock()
		task.ETag = result.ETag
		m.mu.Unlock()
	}
}

// shouldRetryDownload 判定下载错误是否可重试：仅重试可重试错误（网络/5xx）或本次尝试
// 超时；最后一次尝试失败不再重试。用户取消/任务删除由调用方以 dlCtx.Err() 先行判定。
func shouldRetryDownload(downloadErr error, timedOut bool, attempt, maxRetries int) bool {
	// 完整性判定「重下」哨兵（errIntegrityFail，1 次异常）可重试（重下）：
	// 复用退避避免损坏/瞬时异常文件直接放行，溢出前重试。
	if errors.Is(downloadErr, errIntegrityFail) {
		return attempt < maxRetries-1
	}
	// 完整性判定「永久」哨兵（errIntegrityPermanent，两次一致仍异常）不在此重试：
	// 由调用方按任务处置（默认 damaged 放行 / Force 阻断），立刻出循环。
	if errors.Is(downloadErr, errIntegrityPermanent) {
		return false
	}
	var retryable *downloader.RetryableError
	if !errors.As(downloadErr, &retryable) && !timedOut {
		return false // 非可重试错误：停止
	}
	return attempt < maxRetries-1
}

// retryWait 重试等待（等待期间用户取消/客户端断连则立即停止）。返回 stopped=true
// 表示等待被打断（err 为停止原因）。
func (m *CloudDownloadManager) retryWait(ctx, dlCtx context.Context) (stopped bool, err error) {
	m.metrics.TasksRetried.Add(1)
	select {
	case <-time.After(m.config.RetryDelay):
		return false, nil
	case <-dlCtx.Done():
		return true, dlCtx.Err()
	case <-ctx.Done():
		return true, ctx.Err()
	}
}

// runDownloadAttempt 执行单次下载尝试：每次尝试独立超时（超时可重试续传，用户取消
// 不可重试——由 dlCtx 传播）、进度回调（锁外 markDirty 防 ABBA 死锁）、QuotaWriter
// sink 分派（任务 7 边写边记 + 自动补留）；及时释放本次尝试的定时器。返回结果、
// 错误与「本次尝试是否因独立超时到期」（timedOut，供重试判定）。
func (m *CloudDownloadManager) runDownloadAttempt(ctx context.Context, dlCtx context.Context, task *CloudTask, destPath string) (*downloader.Result, error, bool) {
	attemptCtx := dlCtx
	var attemptCancel context.CancelFunc
	if m.config.DownloadTimeout > 0 {
		attemptCtx, attemptCancel = context.WithTimeout(dlCtx, m.config.DownloadTimeout)
	}

	progressFn := func(downloaded, total int64) {
		m.mu.Lock()
		task.Downloaded = downloaded
		if total > 0 {
			task.TotalSize = total
		}
		id := task.ID
		m.mu.Unlock()
		// 在 m.mu 外调用 markDirty，避免与 flushDirty 的 dirtyMu → m.mu 形成 ABBA 死锁。
		// flushDirty 顺序：dirtyMu.Lock → saveTask(内部 m.mu.RLock)；
		// progress 回调顺序：m.mu.Lock → markDirty(dirtyMu.Lock)。
		// 将 markDirty 移出 m.mu 范围后锁序不再反转。
		m.markDirty(id)
	}

	// 主写盘路径：下载器支持 WriterDownloader 则注入 QuotaWriter sink（任务 7）
	// 边写边记 + 自动补留；否则退回普通 Download（仅全局账本）。
	// 下载器选择：按 URL 自动发现（downloader.Find，注册了 pikpak 后 mypikpak 分享 URL
	// 自动命中；http 等未注册 URL 回落配置默认下载器 m.dl——不改默认下载器语义）。
	dl := m.downloaderFor(task.URL)
	sinkFactory := m.downloadSinkFactory(task)
	var result *downloader.Result
	var downloadErr error
	if wd, ok := dl.(downloader.WriterDownloader); ok && sinkFactory != nil {
		result, downloadErr = wd.DownloadWithWriter(attemptCtx, task.URL, destPath, progressFn, sinkFactory)
	} else {
		result, downloadErr = dl.Download(attemptCtx, task.URL, destPath, progressFn)
	}

	// 及时释放本次尝试的定时器，避免累积到函数退出
	if attemptCancel != nil {
		attemptCancel()
	}
	return result, downloadErr, attemptCtx.Err() == context.DeadlineExceeded
}

// handleDownloadDone 处理下载结束的失败路径（downloadDone 段）：
// 任务删除竞态守卫（删除后不再触碰存储与状态）+ 失败路径分派——客户端断开转异步
// 继续（running/cancelFuncs 由新 goroutine 接管并标记 handedOff）、用户取消兜底清理、
// 其余 failTask。返回 handled=true 表示已由本函数处理（调用方直接返回），以及异步
// 转交标记 handedOff（true = 旧 goroutine 的 defer 不得清除运行标记）。
func (m *CloudDownloadManager) handleDownloadDone(ctx context.Context, dlCtx context.Context, task *CloudTask, destPath string, result *downloader.Result, downloadErr error, handedOff *bool) bool {
	// 任务删除竞态守卫：删除后完成/失败的下载不再触碰存储与状态。
	// 注意：真正的终态提交在下方锁内统一复查，这里仅避免无谓的 failTask/对账。
	m.mu.RLock()
	_, exists := m.tasks[task.ID]
	m.mu.RUnlock()
	if !exists {
		m.logger.Info("download finished after task deletion, skipping completion", "task_id", task.ID)
		m.removeTaskDir(task.Owner, task.ID)
		return true
	}
	if downloadErr == nil {
		return false
	}
	// 客户端断开（只有外层 ctx 取消，内层 dlCtx 未取消），转为异步继续。
	// running/cancelFuncs 由新 goroutine 接管：同步置位并标记 handedOff，使旧
	// goroutine 的 defer 不清除二者，避免新 goroutine 短暂丢失运行标记与取消句柄。
	if ctx.Err() != nil && dlCtx.Err() == nil {
		m.logger.Info("sync download client disconnected, switching to async",
			"task_id", task.ID, "url", task.URL)
		//nolint:gosec // G118: 断线后异步继续需要独立 context
		m.mu.Lock()
		m.running[task.ID] = true
		*handedOff = true
		m.mu.Unlock()
		m.wg.Add(1)
		go m.executeDownload(context.Background(), task) //nolint:gosec
		return true
	}
	// 用户取消：CancelTask 已更新状态并释放存储，这里不重复处理，仅兜底清理
	// 可能残留的任务文件（CancelTask 的 RemoveAll 在 Windows 下可能因文件被占用失败）。
	if dlCtx.Err() == context.Canceled {
		m.logger.Info("download cancelled", "task_id", task.ID)
		m.removeTaskDir(task.Owner, task.ID)
		return true
	}
	m.failTask(task, downloadErr.Error())
	m.logger.Error("download failed", "task_id", task.ID, "url", task.URL, "error", downloadErr)
	return true
}

// finalizeCompleted 成功路径终态提交：在 m.mu 写锁内原子完成"复查存在/未取消 →
// 账本对账 → 置 completed"，与 CancelTask/DeleteTask 的写锁互斥，消除完成路径 TOCTOU：
//   - 任务恰在此前被取消/删除时不会覆盖终态（取消后任务仍会残留文件的场景被移除）；
//   - 不再出现对已删除任务写状态或对 m.tasks[id]（可能为 nil）解引用 panic；
//   - 账本对账与置位同临界区，避免"CancelTask 释放预留后完成路径又补预留"的二次记账。
//
// 锁外完成 mtime 恢复与终态持久化。
func (m *CloudDownloadManager) finalizeCompleted(task *CloudTask, result *downloader.Result, destPath string) {
	m.mu.Lock()
	stored, ok := m.tasks[task.ID]
	if !ok {
		m.mu.Unlock()
		m.logger.Info("download finished after task deletion, skipping completion", "task_id", task.ID)
		m.removeTaskDir(task.Owner, task.ID)
		return
	}
	if stored.Status == "cancelled" {
		m.mu.Unlock()
		m.logger.Info("download finished after cancel, discarding result", "task_id", task.ID)
		// 取消即放弃：连同 .partial/.partial.etag 一并清理（CancelTask 已释放存储
		// 并尝试删除，这里兜底），保持磁盘与已归零账本一致。
		m.removeTaskDir(task.Owner, task.ID)
		return
	}

	// 恢复原始文件 mtime（先记录，锁外执行 Chtimes）
	fileMTime := captureExistingFileMTime(stored, result)

	// 全局 storageMgr 账本补偿：以 ReservedSize 为基准对齐到实际大小（CategoryCloud，
	// /api/stats 依赖）。TryReserve/Release 均为内存计数，锁内调用与 failTask/CancelTask
	// 的锁内存储操作保持一致锁序。
	// 租户 Scope 侧（任务 7）：QuotaWriter 已在写盘时边写边记（commitUp 实时落地 committed、
	// 写超预留自动补留、下载器 Finish(success,oldSize) 释放未用 reserve），此处不再做
	// Commit/Adjust 收尾——只需把 task.qw.Committed()（若 Scope 装配且 QW 建成）记入
	// storageMgr 全局账本仍须收敛到实际。
	if !m.reconcileCompletedStorage(stored, result, destPath) {
		return
	}

	// 租户 Scope：记录已确认占用字节，供取消/删除/过期对账（ReleaseUsage）。Scope 未装配
	// 时恒 0。QW 已完成（下载器 Finish(true) 释放未用 reserve 并清零 written），此处用
	// result.Size（QW 边写边记写完整个文件必然 committed==result.Size）。QW 句柄置 nil：
	// 完成后不再写盘，续传/删除不再复用。
	//
	// 判据不同源（审计 F2，2026-09-16 探针取证）：本行判据只看「Scope 是否装配」，而主写盘
	// 分支的判据是「Scope 装配 ∧ 下载器实现 downloader.WriterDownloader」⇒ 若配置到不实现
	// WriterDownloader 的下载器（插件形态；树内不可达，见 quota_sink_criteria_test.go 的前提
	// 门禁），字节直写不入租户 Scope，此处仍记 result.Size（**幻影账本**）。实测后果已很有限：
	// #302 之后 releaseCommittedUp 只传播本层实际扣减量，删除该任务时释放被本层钳制，
	// **不会**连带扣减祖先或兄弟桶；残留影响仅为 cloud 桶在下次扫描前欠计——而这正是直写
	// 分支的既有设计语义（分派处注释：退回普通 Download = 仅全局账本）。若要彻底同源，最小
	// 修法是只在真正走过 sink 时记账（先捕获分派标志，再 `if usedSink { ... }`）；因树内不可达、
	// 且现有副作用已被 #302 消解，本次**未改行为**，留待与插件下载器一并决策。
	// account 实时记账已覆盖 sink 路径（committed==result.Size）。**保留 account 不置 nil**：
	// 完成任务的 committed 即磁盘真实占用，DeleteTask/过期清理的 releaseTaskScope 按
	// account.Release 回拨（置 nil 会让删除路径释放悬空，如 quota_write_path_test.go 的
	// TestQuota_CloudDownloadCommitAndDelete 删除后 Usage 残留）。直写路径 scope 未装配恒 0
	// （releaseTaskScope 对 account nil 空操作）——与既有「直写仅全局账本」语义一致。
	m.reconcileCompletedQuota(task, stored, result)

	// 写入 ChecksumStore。迁移后云任务文件落 <tenant>/cloud/<taskID>/<file>，key 用
	// per-tenant store + 相对租户根的协议正斜杠 rel（cloud/<taskID>/<file>，无 owner 前缀），
	// 与读端 resolveDownloadPath(kind=cloud_task) 返回的 dp.rel 完全一致。
	// 审查 F2：filepath.Join 在 Windows 下产出反斜杠（ID\file），读端 key 恒正斜杠 →
	// 用 ToSlash 归一为协议正斜杠，保证缓存命中。
	relKey := filepath.ToSlash(filepath.Join("cloud", stored.ID, stored.Filename))
	if cs := m.checksumStoreFor(stored.Owner); cs != nil {
		cs.Set(relKey, result.Checksum)
	}

	// 更新任务状态
	stored.Status = "completed"
	stored.TotalSize = result.Size
	stored.Downloaded = result.Size
	stored.Checksum = result.Checksum
	stored.ETag = result.ETag
	stored.UpdatedAt = time.Now()
	stored.ExpiresAt = time.Now().Add(m.config.TaskTTL)
	m.mu.Unlock()

	// 锁外 I/O：恢复文件 mtime 与终态持久化
	m.persistCompleted(stored, task, result, destPath, fileMTime)
}

// captureExistingFileMTime 记录并回填任务原始文件 mtime（供锁外 Chtimes 恢复用；锁内调用）。
func captureExistingFileMTime(stored *CloudTask, result *downloader.Result) int64 {
	if result.ModTime.IsZero() {
		return 0
	}
	mtime := result.ModTime.UnixNano()
	stored.FileMTime = mtime
	return mtime
}

// reconcileCompletedStorage 全局 storageMgr 账本补偿：以 ReservedSize 为基准对齐到实际大小。
// 返回 false 表示 storage-full 分支已由 completeStorageFull 处理（发布 failed 终态并释放锁），
// 调用方直接返回。
func (m *CloudDownloadManager) reconcileCompletedStorage(stored *CloudTask, result *downloader.Result, destPath string) bool {
	reserved := stored.ReservedSize
	if result.Size > reserved {
		if err := m.storage.TryReserveCloud(result.Size - reserved); err != nil {
			// 全局账本不足：无法容纳实际大小，删文件 + 失败（锁内删文件、锁外持久化）。
			m.completeStorageFull(stored, destPath, result, reserved)
			return false
		}
	} else if result.Size < reserved {
		m.storage.ReleaseCloud(reserved - result.Size)
	}
	stored.ReservedSize = result.Size
	return true
}

// reconcileCompletedQuota 直写路径（非 sink 下载器）没有 account：以磁盘真值 reconcile 建账
// （与 failTask 手动落盘路径同款 Reconcile 构造），使删除路径统一释放。scope 未装配恒 0
// （releaseTaskScope 对 account nil 空操作）——与既有「直写仅全局账本」语义一致。
func (m *CloudDownloadManager) reconcileCompletedQuota(stored *CloudTask, task *CloudTask, result *downloader.Result) {
	if task.account != nil {
		// account 实时记账已覆盖 sink 路径（committed==result.Size），完成任务的 committed
		// 即磁盘真实占用，DeleteTask/过期清理的 releaseTaskScope 按 account.Release 回拨。
		return
	}
	scope := m.quotaScope(stored.Owner)
	if scope == nil || result.Size <= 0 {
		return
	}
	task.account = quota.NewTaskAccountReconcile(scope)
	task.account.AdjustCommitted(result.Size)
}

// persistCompleted 锁外收尾：恢复文件 mtime、终态持久化并更新完成指标。
func (m *CloudDownloadManager) persistCompleted(stored *CloudTask, task *CloudTask, result *downloader.Result, destPath string, fileMTime int64) {
	if fileMTime != 0 {
		modTime := result.ModTime
		if err := os.Chtimes(destPath, modTime, modTime); err != nil {
			m.logger.Warn("设置文件修改时间失败", "task_id", task.ID, "error", err)
		}
	}
	// 终态持久化失败会丢失"已完成"状态（重启后任务回到 downloading 被重启下载），必须显式报错
	if err := m.saveTask(stored); err != nil {
		m.logger.Error("persist completed task failed, state may be lost on restart",
			"task_id", task.ID, "error", err)
	}
	m.logger.Info("download completed",
		"task_id", task.ID,
		"url", task.URL,
		"size", result.Size,
		"checksum", result.Checksum[:16]+"...",
	)
	m.metrics.TasksCompleted.Add(1)
	m.metrics.BytesDownloaded.Add(result.Size)
}

// completeStorageFull 处理全局账本不足（storage-full-after-download）分支：删文件 +
// 发布 failed 终态（调用方持 m.mu 写锁；本函数在锁内删文件、Unlock 后锁外持久化）。
//
// 顺序不变量（先删文件、后发布终态）：终态 status 是本路径对外的唯一完成信号，观察者
// 一旦看到 failed 就不得再看到文件残留。删除失败不再只是日志：瞬时占用（Windows 共享
// 违规）先做有界重试，重试耗尽仍失败时把「清理未完成」并入 task.Error，使「文件残留」
// 对用户可观测（否则只剩日志）。Scope 侧由 QW 边写边记已落账（committed + 未用
// reserve），releaseTaskScope 统一回拨防泄漏（含下载中字节）；account 实时记账已覆盖
// sink 路径（committed==result.Size）；releaseTaskScope 按 account.Release 回拨。
func (m *CloudDownloadManager) completeStorageFull(stored *CloudTask, destPath string, result *downloader.Result, reserved int64) {
	m.releaseTaskScope(stored)
	m.storage.ReleaseCloud(reserved) // 全局账本：删整文件归还创建期占位（与 ReservedSize 归零一致）
	stored.ReservedSize = 0
	removeErr := removeWithRetry(func() error { return removeTaskFile(destPath) })
	if removeErr != nil {
		m.logger.Error("storage full after download, remove file failed",
			"task_id", stored.ID, "path", destPath, "error", removeErr)
	}
	stored.Status = "failed"
	stored.Error = cleanupIncompleteError("storage full after download", removeErr)
	stored.UpdatedAt = time.Now()
	stored.ExpiresAt = time.Now().Add(m.config.FailedTaskTTL)
	m.mu.Unlock()
	m.logger.Error("storage full after download, cannot fit actual size",
		"task_id", stored.ID, "actual_size", result.Size, "reserved", reserved)
	_ = m.saveTask(stored)
	m.metrics.TasksFailed.Add(1)
}

// markDownloading 复查任务状态与存在性（CancelTask/DeleteTask 可能在信号量获取与
// 此处之间执行；若状态已非 pending 或任务已从 map 中删除，则放弃下载）并置
// downloading。返回 false 表示任务已不可下载（跳过日志已输出），调用方直接返回。
func (m *CloudDownloadManager) markDownloading(task *CloudTask) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if stored, ok := m.tasks[task.ID]; !ok || (stored.Status != "pending" && stored.Status != "downloading") {
		status := ""
		if ok {
			// 锁内捕获 status：Unlock 后读取会与 ResumeTask/Cancel 的持锁写构成数据竞争。
			status = stored.Status
		}
		if ok {
			m.logger.Info("download skipped, task status changed while acquiring slot",
				"task_id", task.ID, "status", status)
		} else {
			m.logger.Info("download skipped, task deleted while acquiring slot", "task_id", task.ID)
		}
		return false
	}
	task.Status = "downloading"
	task.UpdatedAt = time.Now()
	return true
}

// failTask 将任务标记为失败，释放存储并保留 .partial 文件供续传。
// 已处于 failed/completed/cancelled 的任务直接返回（防止二次释放与状态回滚）。
func (m *CloudDownloadManager) failTask(task *CloudTask, errMsg string, keepFiles ...bool) {
	m.mu.Lock()
	if task.Status == "failed" || task.Status == "completed" || task.Status == "cancelled" {
		m.mu.Unlock()
		return
	}
	// 释放存储：以磁盘实际占用为基准，只释放占位与实际大小的差额。
	// .partial 保留供续传，账本与实际磁盘保持一致，避免配额窗口被累积突破。
	// 租户 Scope 侧（任务 7）：QuotaWriter 已边写边记（committed=已落盘字节），失败路径
	// 保留 .partial 时这些字节继续占账（供续传），无未用 reserve（下载器已在写盘结束
	// Finish(false) 释放）；此处不再做 Commit/Adjust 收尾，仅记录 QuotaCommitted 供
	// 取消/删除对账与续传增量补预留。全局 storageMgr 账本仍按磁盘实际收敛。
	oldReserved := task.ReservedSize
	actual := m.diskUsageOfTask(task.Owner, task.ID)
	if actual < oldReserved {
		m.storage.ReleaseCloud(oldReserved - actual)
	} else if actual > oldReserved {
		// partial 超过占位（如大文件中途失败）：尝试补齐预留；失败则删文件防欠计
		if err := m.storage.TryReserveCloud(actual - oldReserved); err != nil {
			m.logger.Warn("storage full, cannot keep partial for resume, removing task files",
				"task_id", task.ID, "actual", actual, "reserved", oldReserved, "error", err)
			m.failTaskOnStorageFull(task, errMsg)
			return
		}
	}
	// 租户 Scope：以 QW 或磁盘为基准 reconcile 到 committed（审查 I3「二选一」模型——
	// 不得双计）。两条路径互斥：
	//   1）task.qw 存活（本次下载边写边记进行中）：Write 已实时 commitUp，committed 即
	//      QW 已写量 qw.Committed()，**不得再 Adjust**（否则与 commitUp 叠加双计）。
	//      下载器 Finish(false) 已 ReleaseReserve（未用 reserve 归还），此处兜底清残余
	//      reserve（幂等），QW 结算后置 nil，QuotaCommitted 累加本轮 qw.Committed()（续传
	//      保留首轮 partial 占用，防覆盖漏计）供取消/删除对账；
	//   2）task.qw 已 nil（未建 QW / QW 已结算 / 磁盘真值）：以磁盘实际占用为基准 reconcile 到
	//      committed——增量（手动落盘/旧语义测试）Adjust 补入，减量（force 清 partial）
	//      Adjust 释放。scope 未装配恒 0。
	m.reconcileFailureQuota(task, actual)
	task.ReservedSize = actual
	task.Status = "failed"
	task.Error = errMsg
	task.UpdatedAt = time.Now()
	task.ExpiresAt = time.Now().Add(m.config.FailedTaskTTL)
	m.mu.Unlock()
	// 终态持久化失败会丢失 failed 状态（重启后可能被当作 downloading 重启），必须显式报错
	if err := m.saveTask(task); err != nil {
		m.logger.Error("persist failed task state, state may be lost on restart",
			"task_id", task.ID, "error", err)
	}
	m.metrics.TasksFailed.Add(1)

	// 保留 .partial 供 ResumeTask 续传，仅清理临时文件与空目录。
	// keepFiles=true（转存失败场景）：已下载完整产物保留（Save 可取用/重试），不清理。
	if len(keepFiles) == 0 || !keepFiles[0] {
		m.cleanupTaskDirOnFail(task)
	}
}

// failTaskOnStorageFull 处理失败路径中「磁盘占用超占位且 TryReserve 失败」的分支：
// 整目录删除防欠计（先释放旧占位 + 租户 Scope 回拨），并发布 failed 终态。
// 调用方持 m.mu 写锁；删除失败并入 task.Error 保持可观测。
func (m *CloudDownloadManager) failTaskOnStorageFull(task *CloudTask, errMsg string) {
	oldReserved := task.ReservedSize
	// 文件将被整体删除：先释放旧占位，避免磁盘清空后账本仍虚高
	// （TryReserve 已失败、未增加任何预留）。
	if oldReserved > 0 {
		m.storage.ReleaseCloud(oldReserved)
	}
	m.releaseTaskScope(task) // 整目录删除：QW committed + reserve 与 QuotaCommitted 一并回拨
	task.ReservedSize = 0
	// 顺序不变量同 storage-full-after-download 分支：先删任务目录、后发布 failed，
	// 否则观察者可能读到 failed 时 .partial / 最终文件仍在盘上（同一类可观测窗口）。
	// 删除失败同样做有界重试（见 removeWithRetry），耗尽后并入 task.Error 保持可观测。
	removeErr := m.removeTaskDirWithRetry(task.Owner, task.ID)
	if removeErr != nil {
		m.logger.Error("storage full after download, remove task dir failed",
			"task_id", task.ID, "owner", task.Owner, "error", removeErr)
	}
	task.Status = "failed"
	task.Error = cleanupIncompleteError(errMsg, removeErr)
	task.UpdatedAt = time.Now()
	task.ExpiresAt = time.Now().Add(m.config.FailedTaskTTL)
	m.mu.Unlock()
	if saveErr := m.saveTask(task); saveErr != nil {
		m.logger.Error("persist failed task after storage-full cleanup", "task_id", task.ID, "error", saveErr)
	}
	m.metrics.TasksFailed.Add(1)
}

// reconcileFailureQuota 失败路径的租户 Scope 对账（调用方持 m.mu）：
// task.account 存活 → 仅归还未用 reserve（committed 已实时记账，不得再 Adjust 双计）；
// account nil（直写/手动落盘路径）→ 以磁盘实际占用为基准 reconcile 到 committed
// 并记入新建 account（使 releaseTaskScope 统一释放）。scope 未装配恒 0。
func (m *CloudDownloadManager) reconcileFailureQuota(task *CloudTask, actual int64) {
	if task.account != nil {
		// 续传场景 account 保留首轮 partial 占用（committed 已实时记账），仅归还未用 reserve。
		task.account.ReleaseReserve()
	} else if scope := m.quotaScope(task.Owner); scope != nil {
		// account nil（直写/手动落盘路径）：以磁盘实际占用为基准 reconcile 到 committed，
		// 并把占用记入新建 account——使 releaseTaskScope 统一释放（否则 DeleteTask 释放
		// 只看 account，手动落盘用例会悬空）。scope 未装配恒 0（既有语义）。
		if actual != 0 {
			if task.account == nil {
				task.account = quota.NewTaskAccountReconcile(scope)
			}
			task.account.AdjustCommitted(actual)
		}
	}
}

// cleanupTaskDirOnFail 保留 .partial 供 ResumeTask 续传，仅清理临时文件与空目录。
func (m *CloudDownloadManager) cleanupTaskDirOnFail(task *CloudTask) {
	taskDir := m.TaskDirFor(task.Owner, task.ID)
	if taskDir == "" {
		return
	}
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return
	}
	hasPartial := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".partial") {
			hasPartial = true
			continue
		}
		if strings.Contains(e.Name(), ".tmp.") {
			_ = os.Remove(filepath.Join(taskDir, e.Name()))
		}
	}
	if !hasPartial {
		if err := os.RemoveAll(taskDir); err != nil {
			m.logger.Warn("failed to clean up task dir on fail", "task_id", task.ID, "error", err)
		}
	}
}

// refreshTaskGroup 任务进入终态（completed/failed/cancelled）后刷新所属组状态。
// 无组或状态非终态时为空操作。仅读取内存字段，不持有调用方锁。
func (m *CloudDownloadManager) refreshTaskGroup(task *CloudTask) {
	m.mu.RLock()
	status := task.Status
	groupID := task.GroupID
	m.mu.RUnlock()
	if groupID == "" {
		return
	}
	switch status {
	case "completed", "failed", "cancelled":
		m.UpdateGroupStatus(groupID)
	}
}

// findByURL 查找相同 URL 且对请求者 owner 可见的活跃任务（去重）。
// 仅匹配 pending/downloading 状态（排除 completed/failed/cancelled）。
// owner 非空时只匹配同 owner 或空 owner（全局）任务——跨 owner 的同 URL 任务不吸收，
// 避免把 A 的任务泄露给 B（IDOR）或让 B 的请求复用 A 的下载。
// **语义一致判定**（M3 深化，2026-10-05 rebase 复核）：map 遍历序不稳——同 URL 下
// 可能先命中「参数不匹配」的任务就返回 nil（错过参数匹配者 → 重复创建）。必须先扫
// 全部同 URL 任务，找到第一个与 params 语义一致者；无匹配者才返回 nil。调用方
// CreateTask 仅在「既有语义一致」时吸收。
// 复杂度注记：n = 单次请求的 URL 条目数，未构成实测瓶颈；若将来单批支持到数百级再引入 url→ID 索引。
func (m *CloudDownloadManager) findByURL(url, owner string, params TaskParams) *CloudTask {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, t := range m.tasks {
		if t.URL == url && (t.Status == "pending" || t.Status == "downloading") && ownerVisible(t.Owner, owner) &&
			sameTaskParams(t, params) {
			c := *t
			c.account = nil // 快照不暴露运行时配额句柄
			return &c
		}
	}
	return nil
}

// GetTask 返回任务的快照（副本），按请求者 owner 过滤（跨 owner 视为不存在）。

// transferAfterDownload 执行下载后转存（Transfer 非 nil 时）。返回 true = 已处理终态
// （转存失败任务转 failed，调用方直接返回）；false = 转存成功/无转存，继续 finalize。
func (m *CloudDownloadManager) transferAfterDownload(ctx, dlCtx context.Context, task *CloudTask, destPath string, result *downloader.Result) (bool, *downloader.Result) {
	if task.Transfer == nil {
		return false, result
	}
	// NM5：转存并发限流（独立 transferSem，不占下载槽）。获取失败（ctx 取消）→ 中止。
	select {
	case m.transferSem <- struct{}{}:
		defer func() { <-m.transferSem }()
	case <-ctx.Done():
		m.logger.Info("transfer skipped: context cancelled before slot", "task_id", task.ID)
		return true, result
	}
	tr, newResult, terr := m.transferDone(ctx, task, destPath, result, func(c context.Context) (*downloader.Result, error) {
		// 文件异常重下载：清空后按 runRetryLoop 语义重下（续传禁用——文件已损坏）。
		// NM2：删旧文件前回拨其已计字节（account committed / 全局账本），否则重下再次
		// commit → 账本双倍（Scope 虚高）。回拨后磁盘=0、账本=0，重下从零计。
		// 释放按**磁盘真值**（stat destPath）而非外层 result.Size：动态源重下后尺寸可能
		// 与首次不同（NM2 Minor：用旧 size 释放会让 committed 与磁盘偏离，finalize 只
		// Adjust 向上取 max 不抹平虚高）。
		m.logger.Warn("transfer 文件异常，重新下载", "task_id", task.ID, "filename", task.Filename)
		diskSize := result.Size
		if st, serr := os.Stat(destPath); serr == nil {
			diskSize = st.Size()
		}
		m.releaseDownloadedBytes(task, diskSize)
		_ = os.Remove(destPath)
		return m.runRetryLoop(ctx, dlCtx, task, destPath)
	})
	if terr != nil {
		// M2：任务取消/删除导致的中止——不 failTask（任务已删除/取消，无终态可发布），
		// 也不记 TransferErr；直接返回 handled（调用方不再 finalize 已完成/已删任务）。
		if errors.Is(terr, errTransferAborted) {
			m.logger.Info("transfer aborted, skipping completion", "task_id", task.ID)
			return true, result
		}
		m.logger.Error("transfer failed", "task_id", task.ID, "volume", task.Transfer.Volume, "error", terr)
		m.failTaskWithTransfer(task, destPath, terr)
		return true, result
	}
	// F1：重下可能产生新 result——回传给调用方（finalizeCompleted 用最终 result 收口
	// checksumStore/Checksum/TotalSize/账本，不用旧 result 记已删文件数据）。
	if newResult != nil {
		result = newResult
	}
	if tr != nil && tr.URL != "" {
		m.mu.Lock()
		if stored, ok := m.tasks[task.ID]; ok {
			stored.TransferURL = tr.URL
			stored.UpdatedAt = time.Now()
		}
		m.mu.Unlock()
		// NM4：TransferURL 落盘失败（磁盘 I/O）不静默——记 Error（重启丢 URL 可观测）。
		// 崩溃窗口 = 内存写 URL 后、save 落盘前进程崩溃；此处立即 save 缩短窗口，
		// 失败可见（不再 _ = 丢弃）。
		if serr := m.saveTask(task); serr != nil {
			m.logger.Error("transfer done, persist TransferURL failed", "task_id", task.ID, "error", serr)
		} else {
			m.logger.Info("transfer done", "task_id", task.ID, "url", tr.URL)
		}
	}
	return false, result
}

// cleanupTaskCloud 服务端清理 cloud 桶任务文件（Save=false 时任务完成即删）。
// 记录清理状态供审计：cleaned（已删）/ failed（删失败，CleanupErr 供告警接入）。
func (m *CloudDownloadManager) cleanupTaskCloud(task *CloudTask, destPath string) {
	m.mu.Lock()
	stored, ok := m.tasks[task.ID]
	if !ok {
		m.mu.Unlock()
		return
	}
	stored.CleanupStatus = "cleaned" // 先置 cleaned，失败改 failed
	stored.CleanupAt = time.Now()
	m.mu.Unlock()
	if err := os.Remove(destPath); err != nil {
		m.mu.Lock()
		if s, ok := m.tasks[task.ID]; ok {
			s.CleanupStatus = "failed"
			s.CleanupErr = err.Error()
			s.UpdatedAt = time.Now()
		}
		m.mu.Unlock()
		m.logger.Warn("cloud task auto-cleanup failed", "task_id", task.ID, "error", err)
		return
	}
	// NM1：清理删文件后账本与磁盘一致——全局账本 ReservedSize 归零（ReleaseCloud），
	// 租户 Scope 回拨（releaseTaskScope 按 committed 释放；完成后 committed==result.Size）。
	// 否则 Scope 侧同向虚高（b3），24h 内误判 507。
	// 锁纪律：releaseTaskScope 会写 s.account（=nil），与 ListTasks/SnapshotTask 的 RLock
	// 读取构成数据竞争——必须与其他调用点一致在持锁下完成（NM1 曾 Unlock 后调用）。
	m.mu.Lock()
	if s, ok := m.tasks[task.ID]; ok {
		if s.ReservedSize > 0 {
			reserved := s.ReservedSize
			s.ReservedSize = 0
			m.storage.ReleaseCloud(reserved)
			m.releaseTaskScope(s)
		}
	}
	m.mu.Unlock()
	_ = m.saveTask(task)
	m.logger.Info("cloud task auto-cleaned (save=false)", "task_id", task.ID)
}

// checkTransferVolumePreflight 转存目标卷前置校验（创建即拒，避免白下载）：
// 卷须已装配（transferFS 可解析）且协议已声明（能生成 ResolveURL 可解析的 URL）。
func (m *CloudDownloadManager) checkTransferVolumePreflight(transfer *TransferSpec) error {
	if transfer == nil {
		return nil
	}
	tfs, scheme, _ := m.transferFS(transfer.Volume)
	if tfs == nil {
		return fmt.Errorf("cloud download: 转存目标卷 %q 未装配（创建即拒，避免浪费下载）", transfer.Volume)
	}
	if scheme == "" {
		return fmt.Errorf("cloud download: 转存目标卷 %q 协议未声明（无法生成可解析 URL，创建即拒）", transfer.Volume)
	}
	// NM6：secrets 卷是密钥管理卷（secret 名禁路径分隔符），非通用转存目标——
	// 转存 rel 恒含 /，secrets:// 无法解析且写路径绕过 Secrets Manager 校验 → 拒绝。
	if scheme == "secrets" {
		return fmt.Errorf("cloud download: 转存目标卷 %q 是 secrets 密钥卷（非转存目标，请用 secretdata/普通卷）", transfer.Volume)
	}
	return nil
}

// failTaskWithTransfer 转存失败时把任务置失败并记录原因（TransferErr 供后续告警接入）。
func (m *CloudDownloadManager) failTaskWithTransfer(task *CloudTask, destPath string, terr error) {
	// 转存失败原因记入 task.TransferErr（R2：须在锁内写，与并发 GetTask 读防 data race）。
	// 复用标准 failTask（锁外 saveTask + 账本 reconcile + FailedTTL + 目录清理）：
	// 不自行持锁调 saveTask（C2：RWMutex 重入自锁，转存失败必挂死）。
	m.mu.Lock()
	if stored, ok := m.tasks[task.ID]; ok {
		stored.TransferErr = terr.Error()
		stored.UpdatedAt = time.Now()
	}
	m.mu.Unlock()
	// H1：转存失败保留已下载完整产物（keepFiles=true，不调 cleanupTaskDirOnFail 删完整文件）。
	m.failTask(task, "transfer: "+terr.Error(), true)
	// W5：save=false 一律删 cloud 桶（用户裁定——save 控制 cloud 副本存在性，与成败无关）。
	// 注意：清理须是最后一步（删除后不再有桶文件操作）。用户裁定还要求：若要归档，
	// 必须先归档再删除——此处为失败路径无归档，harvest 直接删。
	m.cleanupCloudIfNotNeeded(task, destPath)
}

// cloneTransferSpec 浅拷贝 TransferSpec（供 CreateTask 存储副本；顶层三字段无指针）。
func cloneTransferSpec(t *TransferSpec) *TransferSpec {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

// sameTaskParams 判断既有任务与本次请求的四参语义一致（transfer/save/download_local/
// force_integrity）。去重吸收仅限语义一致者（M3：URL 相同但转存/保留/阻断意图不同 →
// 各自独立任务，不吞参数；任务5 审查补 ForceIntegrity）。
func sameTaskParams(existing *CloudTask, params TaskParams) bool {
	if (existing.Transfer == nil) != (params.Transfer == nil) {
		return false
	}
	if existing.Transfer != nil {
		if existing.Transfer.Volume != params.Transfer.Volume ||
			existing.Transfer.Path != params.Transfer.Path {
			return false
		}
	}
	if existing.Save != params.Save || existing.DownloadLocal != params.DownloadLocal {
		return false
	}
	// 任务5 审查 Important：ForceIntegrity 语义一致才吸收（force=true 请求不得被
	// force=false 任务吸收而丢失阻断语义——损坏源文件会被静默放行标记 damaged）。
	if existing.ForceIntegrity != params.ForceIntegrity {
		return false
	}
	return true
}

// releaseDownloadedBytes 回拨任务已下载字节的账本占用（NM2：transfer 重下删旧文件前调用）：
//   - 全局账本：ReservedSize 已 commit（finalize 前 = 占位），删文件归还占位差额。
//   - 租户 Scope：account 已 commitUp 该字节 → 按磁盘真值 reconcile（减量 Adjust）。
//
// 磁盘侧由调用方删文件。account 保留（重下继续复用同一 account，增量 commit）。
func (m *CloudDownloadManager) releaseDownloadedBytes(task *CloudTask, size int64) {
	if size <= 0 {
		return
	}
	m.mu.Lock()
	if task.ReservedSize > 0 {
		// 占位归还：以磁盘删除的 size 为准（占位可能大于实际）。
		rel := min(task.ReservedSize, size)
		task.ReservedSize -= rel
		m.mu.Unlock()
		m.storage.ReleaseCloud(rel)
		m.mu.Lock()
	}
	m.mu.Unlock()
	// 租户 Scope：磁盘已删，回拨已 commit 字节（ReleaseCommitted 减量；重下复用同
	// account 增量 commit，不双倍）。
	if task.account != nil {
		task.account.ReleaseCommitted(size)
	}
}
