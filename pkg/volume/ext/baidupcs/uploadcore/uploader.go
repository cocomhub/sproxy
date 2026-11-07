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
	"sync"
	"time"
)

// Via 表示本次上传**走通哪条路径**（用于观测与降级判定）。
type Via string

const (
	ViaProbe  Via = "probe"  // 目标已存在且 md5 相同：幂等短路，零请求
	ViaRapid  Via = "rapid"  // 秒传命中：零传输
	ViaSingle Via = "single" // 小文件单请求（< BlockSize）
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

// Verifier 上传后校验（数据操作必须 md5 确认；失败由上层走秒传刷新自愈）。
type Verifier interface {
	Verify(ctx context.Context, req Request) error
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
	WAL    Recorder
	Resume *ScopeState // 有序恢复态（nil = 全新上传）
	Logger *slog.Logger
	Clock  func() time.Time
	Sleep  func(ctx context.Context, d time.Duration) error // 可注入退避（测试不真等）

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
	backoffBase         = 1 * time.Second
	backoffCap          = 8 * time.Second
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
	if o.Sleep == nil {
		o.Sleep = sleepCtx
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	return &Uploader{o: o}, nil
}

// sleepCtx 是可取消的等待（**不用 time.Sleep**：那是库 90s 不可取消的根因之一）。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
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

// backoff 是指数退避 + 封顶（1s/2s/4s/8s…→8s）。
func backoff(attempt int) time.Duration {
	d := backoffBase << (attempt - 1)
	if d > backoffCap || d <= 0 {
		d = backoffCap
	}
	return d
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
	// ② 小文件：单请求（省掉 rapid/merge + 逐片 WAL 开销）。
	if req.Size < u.o.BlockSize {
		res.Via, res.PartsTotal, res.PartsDone = ViaSingle, 1, 1
		return res, u.uploadSingle(ctx, req, start)
	}
	// ③ rapid：拿 uploadid（或直接秒传命中）。
	uploadID, done, err := u.o.Rapid.Rapid(ctx, req)
	if err != nil {
		return res, fmt.Errorf("uploadcore rapid: %w", err)
	}
	res.UploadID = uploadID
	if done {
		return res, u.finishRapid(ctx, req, &res)
	}
	// ④ 分片（含续传态覆盖 uploadid）。
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

// uploadSingle 小文件单请求 + 校验 + 收口。
func (u *Uploader) uploadSingle(ctx context.Context, req Request, start time.Time) error {
	if err := u.stage(ctx, "single", start, nil); err != nil {
		return err
	}
	if err := u.partWithRetry(ctx, "", Part{Seq: 0, Offset: 0, Size: req.Size}); err != nil {
		return err
	}
	if err := u.o.Verify.Verify(ctx, req); err != nil {
		return fmt.Errorf("uploadcore verify(single): %w", err)
	}
	return u.o.WAL.MarkDone(ctx)
}

// finishRapid 秒传命中的校验与收口。
func (u *Uploader) finishRapid(ctx context.Context, req Request, res *Result) error {
	if err := u.o.Verify.Verify(ctx, req); err != nil {
		return fmt.Errorf("uploadcore verify(rapid): %w", err)
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
		for k, v := range u.o.Resume.PartsDone {
			completed[k] = v
		}
	}
	plan, err := u.o.Slicer.Plan(ctx, req)
	if err != nil {
		return res, fmt.Errorf("uploadcore plan: %w", err)
	}
	res.Via, res.PartsTotal = ViaSlices, len(plan)
	if err := u.runParts(ctx, uploadID, plan, completed); err != nil {
		res.PartsDone = len(completed)
		return res, err
	}
	res.PartsDone = len(plan)
	if err := u.o.Merge.Merge(ctx, uploadID, req, plan); err != nil {
		return res, fmt.Errorf("uploadcore merge: %w", err)
	}
	if err := u.o.Verify.Verify(ctx, req); err != nil {
		return res, fmt.Errorf("uploadcore verify(slices): %w", err)
	}
	if err := u.finish(ctx, &res, ViaSlices); err != nil {
		return res, err
	}
	u.o.Logger.Info("uploadcore 分片上传完成", "target", req.TargetPath,
		"parts", len(plan), "elapsed_ms", u.o.Clock().Sub(start).Milliseconds())
	return res, nil
}

// runParts 用受限并发执行分片（首个致命错误取消其余；已完成片跳过 ⇒ 续传）。
func (u *Uploader) runParts(ctx context.Context, uploadID string, plan []Part, completed map[int]bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := make(chan struct{}, u.o.Concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for i := range plan {
		p := plan[i]
		if completed[p.Seq] {
			continue
		}
		mu.Lock()
		stop := firstErr != nil
		mu.Unlock()
		if stop || ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := u.partWithRetry(ctx, uploadID, p); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel() // 首个致命错误：尽快中止其余分片
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			completed[p.Seq] = true
			mu.Unlock()
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// partWithRetry 上传单片：次数上限 + 指数退避（退避**可被 ctx 打断**）+ 成功后写 WAL。
func (u *Uploader) partWithRetry(ctx context.Context, uploadID string, p Part) error {
	var last error
	for attempt := 1; attempt <= u.o.MaxPartAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := u.attemptPart(ctx, uploadID, p)
		if err == nil {
			if werr := u.o.WAL.PartDone(ctx, p.Seq, ""); werr != nil {
				return fmt.Errorf("uploadcore wal part %d: %w", p.Seq, werr)
			}
			return nil
		}
		last = err
		if attempt == u.o.MaxPartAttempts {
			break
		}
		// 退避可被 ctx 打断：取消/预算到期 ⇒ 立即返回，不再重试（不热循环）。
		if serr := u.o.Sleep(ctx, backoff(attempt)); serr != nil {
			return serr
		}
	}
	return fmt.Errorf("uploadcore 分片 %d 失败 %d 次: %w", p.Seq, u.o.MaxPartAttempts, last)
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
