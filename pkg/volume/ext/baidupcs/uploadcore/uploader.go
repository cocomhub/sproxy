// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package uploadcore 是 BaiduPCS 分片上传的**自研编排内核**：保留「我们自己发请求」的
// Precreate/TmpFile/CreateSuperFile 三个动作（见上级包 multiupload.go），**替换其外的
// 编排**（替换 fork 库 uploader.MultiUploader）。
//
// 为什么替换（行级依据，2026-11-08）：
//   - `requester/uploader/multiworker.go`：失败分片**零退避、无次数上限**重入队
//     （我们只能在 TmpFile 内用 `*uploader.MultiError{Terminated:true}` 兜底）；
//   - 库自建 client 超时不可注入：`pcsupload/upload.go:37 SetTimeout(200s)`、
//     `uploader/uploader.go:63 SetResponseHeaderTimeout(10s)`；
//   - 库退避是 `time.Sleep`（`taskframework/executor.go:135` + `pcsfunctions/common.go:8-13`），
//     **不听 ctx** ⇒ 表现为「卡 ~90s 且不可取消」。
//
// 本内核的设计目标与约束见 `docs/design-baidupcs-upload-core.md`：
// 可取消（分片级 ctx）、可观测（WAL 逐片记录 + 阶段）、可恢复（audit scope 回放）、
// 幂等（probe 短路 + 续传只补缺失片）、可验证（依赖全部注入 ⇒ httptest 可锁定）。
//
// 阶段流水线：probe →（小文件 single）→ rapid → slices → merge → verify。
// 已完成决策（2026-11-08 人工确认）：uploadid 沿用库 RapidUpload｜WAL 用 audit scope
// 逐片记录｜默认 auto（失败降级 legacy + 告警）｜≥4MiB 分片 / <4MiB 单请求。
package uploadcore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/pipe"
)

// Via 表示本次上传**走通哪条路径**（用于观测与降级判定）。
type Via string

const (
	ViaProbe  Via = "probe"  // 目标已存在且 md5 相同：幂等短路，零请求
	ViaRapid  Via = "rapid"  // 秒传命中：零传输
	ViaSlices Via = "slices" // 分片上传 + 合并
)

// Part 是一个分片（Seq 从 0 起；Size 为字节）。
type Part struct {
	Seq    int
	Offset int64
	Size   int64
}

// Request 是一次上传请求。
type Request struct {
	LocalPath  string
	TargetPath string
	Size       int64
	MD5        string // 整文件 md5（hex）；空 = 未预计算，probe 短路不适用
	Overwrite  bool
}

// Result 是一次上传的结论（I/O 与观测用）。
type Result struct {
	Done       bool
	Via        Via
	UploadID   string
	PartsTotal int
	PartsDone  int
}

// ScopeState 是**恢复态**（来自 audit WAL 回放；由调用方/装配层构造）。
type ScopeState struct {
	UploadID  string
	PartsDone map[int]bool
}

// ---- 注入接口（窄接口 → 依赖倒置：内核不认识 fork 库，L2 可注入 fake）----

// Prober 查询目标当前内容的 md5（"" = 不存在）。实现须用**精确列举**（实测 0.33–0.38s），
// 不得用会挂起的 Meta（见 docs/external-dependencies.md §7.8）。
type Prober interface {
	ExistingMD5(ctx context.Context, targetPath string) (string, error)
}

// RapidProvider 尝试秒传：done=true 表示已命中（零传输完成）；否则返回 uploadid 供分片使用。
// 决策：沿用 fork 库 `pcs.RapidUpload`（协议实现不重造）。
type RapidProvider interface {
	Rapid(ctx context.Context, req Request) (uploadID string, done bool, err error)
}

// Slicer 给出分片计划（分片大小由实现按 PCS 约束决定，内核不写死）。
type Slicer interface {
	Plan(ctx context.Context, req Request) ([]Part, error)
}

// PartUploader 上传单个分片（同一 Seq 重传须幂等覆盖）。
type PartUploader interface {
	UploadPart(ctx context.Context, uploadID string, p Part) error
}

// Merger 合并全部分片（createsuperfile）。
type Merger interface {
	Merge(ctx context.Context, uploadID string, req Request, parts []Part) error
}

// Verifier 上传后校验（数据操作必须 md5 确认）。
type Verifier interface {
	Verify(ctx context.Context, req Request) error
}

// Repairer 是**可选**能力：校验失败时尝试自愈（如 baidupcs 的 rapidupload 秒传刷新，零传输），
// 成功后内核会**再复验一次**。nil ⇒ 无自愈能力（校验失败即报错，不静默放过）。
type Repairer interface {
	Repair(ctx context.Context, req Request) error
}

// Recorder 是 WAL 记录的**窄接口**（S2 由 pkg/audit scope 适配实现）。
type Recorder interface {
	Stage(ctx context.Context, stage string, kv map[string]any) error
	PartDone(ctx context.Context, seq int, md5 string) error
	MarkDone(ctx context.Context) error
}

// Options 是内核运行参数（全部可注入 ⇒ 单测/契约测试无网可跑）。
type Options struct {
	Probe  Prober
	Rapid  RapidProvider
	Slicer Slicer
	Parts  PartUploader
	Merge  Merger
	Verify Verifier
	// Repair 为可选自愈能力（nil ⇒ 校验失败直接报错）。
	Repair Repairer
	WAL    Recorder
	// Sleep 是退避等待实现（nil ⇒ 真实定时器）。测试注入零等待，避免用例被真实退避拖慢。
	Sleep  func(ctx context.Context, d time.Duration) error
	Resume *ScopeState // 有序恢复态（nil = 全新上传）
	Logger *slog.Logger
	Clock  func() time.Time
	// Env 提供可注入睡眠（复用 pkg/pipe；nil ⇒ 默认可取消等待，测试可零等待）。
	Env pipe.Env

	BlockSize       int64         // < 该值走单请求（默认 4MiB，PCS 约束）
	Concurrency     int           // 分片并发（默认 4，上限 16）
	MaxPartAttempts int           // 每片最大尝试次数（默认 4）
	PartTimeout     time.Duration // 单片超时（默认 60s；0 = 不设）
	Budget          time.Duration // 整体预算（默认 30m；0 = 不设）
}

const (
	defaultBlockSize    = 4 << 20
	defaultConcurrency  = 4
	maxConcurrency      = 16
	defaultPartAttempts = 4
	defaultPartTimeout  = 60 * time.Second
	defaultBudget       = 30 * time.Minute
	partBackoffBase     = 1 * time.Second
	partBackoffCap      = 8 * time.Second
)

// Uploader 是内核（并发安全：Upload 可被多 goroutine 调用）。
type Uploader struct {
	o Options
}

// New 构造内核（注入项必填性在启动期校验，避免运行期 nil panic）。
func New(o Options) (*Uploader, error) {
	if o.Probe == nil || o.Rapid == nil || o.Slicer == nil || o.Parts == nil ||
		o.Merge == nil || o.Verify == nil || o.WAL == nil {
		return nil, errors.New("uploadcore: Probe/Rapid/Slicer/Parts/Merge/Verify/WAL 均为必填")
	}
	if o.BlockSize <= 0 {
		o.BlockSize = defaultBlockSize
	}
	if o.Concurrency <= 0 {
		o.Concurrency = defaultConcurrency
	}
	if o.Concurrency > maxConcurrency {
		o.Concurrency = maxConcurrency
	}
	if o.MaxPartAttempts <= 0 {
		o.MaxPartAttempts = defaultPartAttempts
	}
	if o.PartTimeout <= 0 {
		o.PartTimeout = defaultPartTimeout
	}
	if o.Budget <= 0 {
		o.Budget = defaultBudget
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	return &Uploader{o: o}, nil
}

// Upload 执行一次上传（幂等：同目标同内容重复调用 ⇒ probe 短路）。
// 复杂度控制：编排只做路由，各阶段分别由 helper 承担（gocognit ≤ 15）。
func (u *Uploader) Upload(ctx context.Context, req Request) (Result, error) {
	if u == nil {
		return Result{}, errors.New("uploadcore: nil uploader")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if u.o.Budget > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, u.o.Budget)
		defer stop()
	}
	start := u.o.Clock()
	res := Result{}

	// ① probe：目标已存在且 md5 相同 ⇒ 幂等短路（零请求）。
	if handled, err := u.tryProbe(ctx, req, start); handled || err != nil {
		if handled {
			res.Via, res.Done = ViaProbe, true
		}
		return res, err
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	// ② **建会话必须先于大小分流**（2026-11-08 真实网盘实测修正）：
	// 百度 v2 的**单请求上传同样需要 uploadid**（`superfile2/upload?uploadid=`）——
	// 早期实现把小文件分流放在建会话之前 ⇒ uploadid 为空 ⇒ HTTP 400。
	// 因此统一先走 rapid：命中即完成；未命中则（由实现）建立会话并返回 uploadid。
	uploadID, done, err := u.o.Rapid.Rapid(ctx, req)
	if err != nil {
		return res, fmt.Errorf("uploadcore rapid: %w", err)
	}
	res.UploadID = uploadID
	if err := u.stage(ctx, "upload", start, map[string]any{"upload_id": uploadID}); err != nil {
		return res, err
	}
	if done {
		return res, u.finishRapid(ctx, req, &res)
	}
	// ③ 分片（含续传态覆盖 uploadid）。
	//
	// **为什么没有"小文件单请求"捷径**（2026-11-08 真实网盘实测修正）：百度 v2 的
	// `superfile2/upload`（`pcs.UploadTmpFile`）只是把**分片**写入会话临时区，
	// **必须再调 `createsuperfile` 才会成为正式文件** —— 早期实现给小文件走了
	// "只上传一片、不合并"的捷径 ⇒ 内核报成功但远端**从未出现该文件**
	// （实测：`putCheckAfterUpload` 复核 3 轮均为空 ETag ⇒ ErrTransient）。
	// 因此小文件与分片走**同一条**流程（1 片 + 合并），与库 CLI（`MultiUploader`）一致。
	return u.uploadSlices(ctx, req, res, uploadID, start)
}

// tryProbe 目标已存在且 md5 相同 ⇒ 幂等短路（handled=true 时已完成 MarkDone）。
func (u *Uploader) tryProbe(ctx context.Context, req Request, start time.Time) (bool, error) {
	if req.MD5 == "" {
		return false, nil
	}
	m, err := u.o.Probe.ExistingMD5(ctx, req.TargetPath)
	if err != nil || m != req.MD5 {
		return false, nil
	}
	if err := u.stage(ctx, "probe", start, map[string]any{"hit": true}); err != nil {
		return false, err
	}
	u.o.Logger.Debug("uploadcore 幂等短路", "target", req.TargetPath, "via", ViaProbe)
	return true, u.o.WAL.MarkDone(ctx)
}

// finishRapid 秒传命中的校验与收口。
func (u *Uploader) finishRapid(ctx context.Context, req Request, res *Result) error {
	if err := u.verifyWithRepair(ctx, req, "rapid"); err != nil {
		return err
	}
	u.o.Logger.Info("uploadcore 秒传命中", "target", req.TargetPath, "upload_id", res.UploadID)
	return u.finish(ctx, res, ViaRapid)
}

// uploadSlices 分片上传：恢复态覆盖 → 计划 → 并发上传（只补缺失片）→ merge → verify → 收口。
func (u *Uploader) uploadSlices(ctx context.Context, req Request, res Result, uploadID string, start time.Time) (Result, error) {
	completed := map[int]bool{}
	if u.o.Resume != nil {
		if u.o.Resume.UploadID != "" {
			uploadID = u.o.Resume.UploadID
			res.UploadID = uploadID
		}
		maps.Copy(completed, u.o.Resume.PartsDone)
	}
	plan, err := u.o.Slicer.Plan(ctx, req)
	if err != nil {
		return res, fmt.Errorf("uploadcore plan: %w", err)
	}
	res.Via, res.PartsTotal = ViaSlices, len(plan)
	if err := u.runPartsWithRebuild(ctx, req, &uploadID, plan, completed); err != nil {
		if errors.Is(err, errRapidHitDuringRebuild) {
			res.UploadID, res.Via = "", ViaRapid
			u.o.Logger.Info("重建会话时秒传命中（目标已在盘）", "target", req.TargetPath)
			return res, nil
		}
		res.PartsDone = len(completed)
		return res, err
	}
	res.UploadID = uploadID // 可能因重建而更新（供 Merge/Repair 使用会话 id）
	res.PartsDone = len(plan)
	if err := u.o.Merge.Merge(ctx, uploadID, req, plan); err != nil {
		return res, fmt.Errorf("uploadcore merge: %w", err)
	}
	if err := u.verifyWithRepair(ctx, req, "slices"); err != nil {
		return res, err
	}
	if err := u.finish(ctx, &res, ViaSlices); err != nil {
		return res, err
	}
	u.o.Logger.Info("uploadcore 分片上传完成", "target", req.TargetPath,
		"parts", len(plan), "elapsed_ms", u.o.Clock().Sub(start).Milliseconds())
	return res, nil
}

// runParts 用受限并发执行分片（**复用 pkg/pipe.RunParts** —— 机制只有一份实现）。
// 已完成片跳过 ⇒ 续传；首错取消兄弟 ⇒ 快速失败。
func (u *Uploader) runParts(ctx context.Context, uploadID string, plan []Part, completed map[int]bool) error {
	todo := make([]Part, 0, len(plan))
	for i := range plan {
		if !completed[plan[i].Seq] {
			todo = append(todo, plan[i])
		}
	}
	var mu sync.Mutex
	if err := pipe.RunParts(ctx, todo, u.o.Concurrency, func(ctx context.Context, p Part) error {
		if err := u.partWithRetry(ctx, uploadID, p); err != nil {
			return err
		}
		mu.Lock()
		completed[p.Seq] = true
		mu.Unlock()
		return nil
	}); err != nil {
		return err
	}
	return ctx.Err()
}

// runPartsWithRebuild 上传全部分片；遇 `ErrSessionExpired` ⇒ 重建会话后**全量重传**。
//
// 为什么必须全量重传：新 `uploadid` 的临时区是**空的**（分片属于会话），已完成分片作废。
// 有界：最多重建 `maxSessionRebuilds` 次（防死循环）；超出 ⇒ 返回原错误（fail-closed）。
func (u *Uploader) runPartsWithRebuild(ctx context.Context, req Request, uploadID *string,
	plan []Part, completed map[int]bool) error {
	for rebuild := 0; ; rebuild++ {
		err := u.runParts(ctx, *uploadID, plan, completed)
		if err == nil || !errors.Is(err, ErrSessionExpired) || rebuild >= maxSessionRebuilds {
			return err
		}
		newID, hit, rErr := u.o.Rapid.Rapid(ctx, req)
		if rErr != nil {
			return fmt.Errorf("uploadcore 重建会话: %w", rErr)
		}
		if hit {
			return errRapidHitDuringRebuild
		}
		if newID == "" {
			return errors.New("uploadcore: 重建会话未返回 upload_id")
		}
		u.o.Logger.Warn("上传会话失效，已重建会话并全量重传分片",
			"old_upload_id", *uploadID, "new_upload_id", newID, "rebuild", rebuild+1)
		*uploadID = newID
		clear(completed) // 新会话临时区为空 ⇒ 已完成分片作废
	}
}

// errWALPart 标记 WAL 写失败（**不可重试**：重试只会重复写同一条记录）。
var errWALPart = errors.New("uploadcore: WAL 分片记录失败")

// ErrSessionExpired 表示上传会话（`uploadid`）已失效：百度在分片上传时返回 **31066
// （file does not exist）** ⇒ 会话临时区已不存在 ⇒ 必须**重建会话**（重走 rapid 拿新 uploadid）
// 并**全量重传**（新会话的临时区为空，已完成分片作废）。
// `PartUploader` 实现负责把底层错误映射到本哨兵（见 baidupcs 的 ucAdapter.UploadPart）。
var ErrSessionExpired = errors.New("uploadcore: 上传会话已失效（需重建）")

// errRapidHitDuringRebuild 表示"因会话失效而重走 rapid 时秒传命中"（目标已在盘）⇒ 视为已完成。
var errRapidHitDuringRebuild = errors.New("uploadcore: 重建会话时秒传命中")

// maxSessionRebuilds 是会话失效后**重建会话**的次数上限（防"重建-失效"死循环）。
const maxSessionRebuilds = 2

// partWithRetry 上传单片（**复用 pkg/pipe.Retry**：次数上限 + 可中断指数退避 + 成功后写 WAL）。
func (u *Uploader) partWithRetry(ctx context.Context, uploadID string, p Part) error {
	pol := pipe.Policy{
		Attempts: u.o.MaxPartAttempts,
		Base:     partBackoffBase,
		Cap:      partBackoffCap,
		Factor:   2,
		// 仅"分片上传失败"可重试：
		//   - WAL 写失败属本地状态错误（重试只会重复写同一条记录）；
		//   - **会话失效（31066）重试无意义**（同一 uploadid 内所有请求都会失败）⇒ 交给
		//     `runPartsWithRebuild` 重建会话后全量重传（避免白等 3 次退避）。
		Retryable: func(err error) bool {
			return !errors.Is(err, errWALPart) && !errors.Is(err, ErrSessionExpired)
		},
	}
	return pipe.Retry(ctx, u.o.Env, pol, func(attemptCtx context.Context, _ int) error {
		if err := u.attemptPart(attemptCtx, uploadID, p); err != nil {
			return err
		}
		if werr := u.o.WAL.PartDone(ctx, p.Seq, ""); werr != nil {
			return fmt.Errorf("%w: part %d: %w", errWALPart, p.Seq, werr)
		}
		return nil
	})
}

// attemptPart 发出单次分片请求（可选单片超时；与库 200s 不可注入超时形成对照）。
func (u *Uploader) attemptPart(ctx context.Context, uploadID string, p Part) error {
	if u.o.PartTimeout <= 0 {
		return u.o.Parts.UploadPart(ctx, uploadID, p)
	}
	actx, stop := context.WithTimeout(ctx, u.o.PartTimeout)
	defer stop()
	return u.o.Parts.UploadPart(actx, uploadID, p)
}

// sleep 等待退避：复用既有的可注入 `Env.Sleep`（测试零等待；见 Options.Env），否则真实定时器。
func (u *Uploader) sleep(ctx context.Context, d time.Duration) error {
	if u.o.Env.Sleep != nil {
		return u.o.Env.Sleep(ctx, d)
	}
	return sleepBackoff(ctx, d)
}

// sleepBackoff 等待退避（ctx 可取消）。本包自己实现：不用库的 time.Sleep（不可取消）。
func sleepBackoff(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// verifyWithRepair 校验；失败时自愈（秒传刷新）并**复验**——**有界重试 + 退避**。
//
// 为什么要重试：远端「块 md5 索引」的可见性有延迟（实测同一实现/同一文件：有时刷新命中、
// 有时 miss ⇒ md5 保持派生值）。单位只做 1 次 repair + 1 次复验命中率不足；
// 退避重试可收敛（实测 20MiB/5 块在 3 次内收敛）。
//
// 语义不变：重试耗尽仍不一致 ⇒ **一律报错**（绝不静默放过数据正确性问题）。
func (u *Uploader) verifyWithRepair(ctx context.Context, req Request, stage string) error {
	const attempts = 6 // 块数越多，远端块索引就绪越慢（120MiB/30 块实测 4 次不够）
	var vErr, lastRepairErr error
	for i := 1; i <= attempts; i++ {
		vErr = u.o.Verify.Verify(ctx, req)
		if vErr == nil {
			if i > 1 {
				u.o.Logger.Warn("uploadcore 校验失败后自愈成功", "target", req.TargetPath, "stage", stage, "attempt", i)
			}
			return nil
		}
		if u.o.Repair == nil {
			return fmt.Errorf("uploadcore verify(%s): %w", stage, vErr)
		}
		if i == attempts {
			break // 最后一次不再自愈（已无复验机会）
		}
		if rErr := u.o.Repair.Repair(ctx, req); rErr != nil {
			lastRepairErr = rErr
			u.o.Logger.Debug("uploadcore 自愈失败（将重试）",
				"target", req.TargetPath, "stage", stage, "attempt", i, "err", rErr)
		}
		// 退避：等待远端块索引就绪（2s/4s/8s，总上限 ~14s）。
		if err := u.sleep(ctx, time.Duration(1<<uint(i))*time.Second); err != nil {
			return fmt.Errorf("uploadcore verify(%s) 重试被中断: %w（原始校验错误: %v）", stage, err, vErr)
		}
	}
	if lastRepairErr != nil {
		return fmt.Errorf("uploadcore verify(%s) 自愈失败（%d 次重试后仍失败）: %w（原始校验错误: %v）",
			stage, attempts, lastRepairErr, vErr)
	}
	return fmt.Errorf("uploadcore verify(%s) 自愈重试 %d 次后仍失败: %w", stage, attempts, vErr)
}

// stage 记录阶段开始（WAL + 结构化日志，供"卡在哪"定位）。
func (u *Uploader) stage(ctx context.Context, name string, start time.Time, kv map[string]any) error {
	if kv == nil {
		kv = map[string]any{}
	}
	kv["elapsed_ms"] = u.o.Clock().Sub(start).Milliseconds()
	if err := u.o.WAL.Stage(ctx, name, kv); err != nil {
		return fmt.Errorf("uploadcore wal stage %s: %w", name, err)
	}
	return nil
}

// finish 收口：MarkDone（WAL 作用域完成，不再参与恢复）。
func (u *Uploader) finish(ctx context.Context, res *Result, via Via) error {
	res.Via, res.Done = via, true
	if err := u.o.WAL.MarkDone(ctx); err != nil {
		return fmt.Errorf("uploadcore wal markdone: %w", err)
	}
	return nil
}
