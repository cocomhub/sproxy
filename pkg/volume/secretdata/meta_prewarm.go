// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"context"
	"log/slog"
	"path"
	"time"
)

// defaultPrewarmInterval 是预热的最小间隔（每个文件之间）。**这是对远端的节流**：
// baidupcs/pikpak 等外部卷每次读 meta 都要起一次下载 CLI（秒级）⇒ 不节流会打爆远端/限流。
const defaultPrewarmInterval = 1 * time.Second

// StartMetaPrewarm 在后台**限速**预热精简 meta 缓存（L1a，**磁盘**）。
//
// # 为什么这样做（而不是别的）
//
//   - **只预热到磁盘**：写进 L1a（`putCompactMeta`），**不常驻内存** ⇒ 不触碰 L1b 的字节预算；
//   - **复用既有路径**：直接调 `readFileMetaFast` —— 它"未命中则读权威 blob 并回填 L1a"，
//     预热**不需要任何新机制**（也自动复用 `metaLoadGate` 的并发闸，不额外放大并发）；
//   - **跳过已缓存**：先 `getCompactMeta` 探一下，命中就跳过 ⇒ 只补缺失项，重启后几乎零成本；
//   - **限速**：默认每文件间隔 `defaultPrewarmInterval` —— 对外部卷这是**必需的节流**；
//   - **可取消**：ctx 取消即退出（进程关闭/卷卸载）。
//
// # 价值与边界（实测驱动，勿夸大）
//
// 实测（#1133）：**L1a 只省带宽、不省 CPU** —— 命中后仍要解密+解析 ≈584 ms。
// 故预热的真实价值是「把**远端下载**换成**本地解密**」⇒ 对**外部卷**（读 meta 起 CLI，秒级）有意义；
// **本地卷**收益很小（本地读本来就快）⇒ 装配层可只对外部卷开启。
//
// 契约：本函数**立即返回**（预热在后台 goroutine 内进行）；`disable_meta_prewarm` 为 true 时不启动。
func (s *SecretdataFS) StartMetaPrewarm(ctx context.Context) {
	if s.opts.DisableMetaPrewarm {
		return
	}
	// **本地卷直接跳过**：本地读 meta 本来就快（无远端下载），预热只会多付一份后台扫描开销 ⇒
	// 零收益。判断下沉到此处（装配层不必触碰 `FS()`）—— 见用户在 2026-11-13 明示的架构原则。
	if s.IsLocalVolume() {
		return
	}
	// 快照待预热清单（rel → blobKey），避免长期持锁。
	s.mu.RLock()
	jobs := make([]prewarmJob, 0, len(s.index))
	for rel, e := range s.index {
		jobs = append(jobs, prewarmJob{rel: rel, blobKey: path.Join(e.dirSeg, e.metaName)})
	}
	s.mu.RUnlock()
	if len(jobs) == 0 {
		return
	}
	go s.prewarmLoop(ctx, jobs)
}

// prewarmJob 是预热清单里的一项（rel 仅用于日志，blobKey 是权威 blob 相对路径）。
type prewarmJob struct{ rel, blobKey string }

// prewarmLoop 逐个（**限速**）预热；单个失败不中断整轮（缓存是加速器，失败不致命）。
// 抽成独立方法：把「取数 + 计数 + 限速 + 取消」的复杂度从 StartMetaPrewarm 里分离。
func (s *SecretdataFS) prewarmLoop(ctx context.Context, jobs []prewarmJob) {
	var done, skipped, failed int
	for _, j := range jobs {
		if ctx.Err() != nil {
			break
		}
		switch s.prewarmOne(ctx, j) {
		case prewarmSkipped:
			skipped++
		case prewarmFailed:
			failed++
		default:
			done++
		}
		if !sleepCtx(ctx, defaultPrewarmInterval) {
			break
		}
	}
	slog.Info("secretdata: meta 预热结束", "fetched", done, "skipped", skipped, "failed", failed)
}

// prewarmResult 表示单个文件预热的结果。
type prewarmResult int

const (
	prewarmFetched prewarmResult = iota
	prewarmSkipped
	prewarmFailed
)

// prewarmOne 预热单个文件：已缓存 ⇒ 跳过；否则读权威 blob（会自动回填 L1a）。
func (s *SecretdataFS) prewarmOne(ctx context.Context, j prewarmJob) prewarmResult {
	if _, _, ok := s.getCompactMeta(j.blobKey); ok {
		return prewarmSkipped // 只补缺失，避免无谓远端调用
	}
	if _, _, err := s.readFileMetaFast(ctx, j.blobKey); err != nil {
		return prewarmFailed
	}
	return prewarmFetched
}

// sleepCtx 可取消的等待（返回 false 表示 ctx 已取消，调用方应停止）。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// MetaPrewarmer 是 FS 的**可选能力**：启动「后台限速预热 L1a（磁盘）」。
//
// 抽成可选能力（而非塞进 `Options`/主接口）符合仓内约定：调用方（装配层）经
// `AssertMetaPrewarmer(v)` 探测，**未实现者不受影响**（零回归）；也不给测试替身增加负担。
type MetaPrewarmer interface { // NOSONAR: S8196 — 能力接口（非 -er 角色命名），表达能力语义
	StartMetaPrewarm(ctx context.Context)
}

// AssertMetaPrewarmer 查询 FS 是否支持后台预热（未实现 ⇒ ok=false）。
func AssertMetaPrewarmer(v any) (MetaPrewarmer, bool) {
	p, ok := v.(MetaPrewarmer)
	return p, ok
}

// 编译期断言：secretdata 必须满足该可选能力（否则装配层静默不预热）。
var _ MetaPrewarmer = (*SecretdataFS)(nil)
