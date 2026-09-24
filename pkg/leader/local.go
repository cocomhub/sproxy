// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package leader

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// LocalLeaderElector 是本地 flock 实现：<root>/state/leader.lock。
//
// 语义（与设计文档 §2.2 一致）：
//   - TryAcquire：O_CREATE|O_WRONLY 打开 + 非阻塞排他锁（Unix flock / Windows
//     LockFileEx）；成功 → 写 leaseID + 时间戳 → (true, nil)；占用中 → (false, nil)；
//   - Renew：非阻塞校验本进程仍持有（同一 f 上再次 LOCK_EX|LOCK_NB 恒成功）→ 更新时间戳；
//   - Release：解锁 + 关句柄（幂等）。
//
// 单节点恒主零回归：无竞争者时 TryAcquire 恒 true，与「无选主」行为一致。
// 崩溃恢复：进程崩溃后内核自动释放锁（无需清理）；文件内容仅诊断用途。
// 并发安全：mu 串行化多 goroutine 对同一实例的调用。

// errWouldBlock 是排他锁被他人持有的平台错误（Unix EWOULDBLOCK / Windows
// ERROR_LOCK_VIOLATION）。由各平台 lock 文件映射。
var errWouldBlock = errors.New("leader: lock would block")

type LocalLeaderElector struct {
	path string     // <root>/state/leader.lock
	f    *os.File   // 持有中的 flock 句柄（nil = 未持有）
	mu   sync.Mutex // 串行化 TryAcquire/Renew/Release
}

// NewLocalLeaderElector 创建本地选主实例（懒打开：TryAcquire 时才建文件）。
func NewLocalLeaderElector(root string) *LocalLeaderElector {
	return &LocalLeaderElector{path: filepath.Join(root, "state", "leader.lock")}
}

// TryAcquire 尝试获得排他锁。leaseID 空 / ttl <= 0 → 参数校验错误（fail-fast）。
// 已被他人持有 → (false, nil)（非错误）。
func (e *LocalLeaderElector) TryAcquire(ctx context.Context, leaseID string, ttl time.Duration) (bool, error) {
	if leaseID == "" {
		return false, fmt.Errorf("leader: leaseID 不能为空")
	}
	if ttl <= 0 {
		return false, fmt.Errorf("leader: ttl 必须 > 0（显式给租约时长，local 实现仅校验语义）")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.f != nil {
		// 本实例已持有：重复 TryAcquire 视为成功（与 Renew 同语义——同进程内
		// flock 可重入，先校验持有再更新时间戳）。
		if err := e.touchLocked(ctx, leaseID); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(e.path), 0o755); err != nil {
		return false, fmt.Errorf("leader: 创建 state 目录失败: %w", err)
	}
	f, err := os.OpenFile(e.path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, fmt.Errorf("leader: 打开锁文件失败: %w", err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		if errors.Is(err, errWouldBlock) {
			return false, nil // 已被他人持有，非错误
		}
		return false, fmt.Errorf("leader: 加锁失败: %w", err)
	}
	e.f = f
	if err := e.touchLocked(ctx, leaseID); err != nil {
		_ = e.releaseLocked()
		return false, err
	}
	return true, nil
}

// Renew 续租：校验本进程仍持有锁（同句柄非阻塞加锁恒成功——若已被内核回收则失败）
// 并更新时间戳。未持有 → ErrLeaseLost（fail-closed：调用方降级只读）。
func (e *LocalLeaderElector) Renew(ctx context.Context, leaseID string) error {
	if leaseID == "" {
		return fmt.Errorf("leader: leaseID 不能为空")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.f == nil {
		return ErrLeaseLost
	}
	return e.touchLocked(ctx, leaseID)
}

// Release 释放排他锁并关闭句柄（幂等：未持有 no-op 成功）。
func (e *LocalLeaderElector) Release(ctx context.Context, leaseID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.releaseLocked()
	return nil
}

// touchLocked 更新锁文件内容（leaseID + 时间戳，诊断用途）。持 e.mu 时调用。
func (e *LocalLeaderElector) touchLocked(ctx context.Context, leaseID string) error {
	_ = ctx // 本地实现无网络操作；保留签名对齐接口
	stamp := fmt.Sprintf("%s\n%d\n", leaseID, time.Now().UnixNano())
	if err := e.f.Truncate(0); err != nil {
		return fmt.Errorf("leader: 截断锁文件失败: %w", err)
	}
	if _, err := e.f.WriteAt([]byte(stamp), 0); err != nil {
		return fmt.Errorf("leader: 写锁文件失败: %w", err)
	}
	return nil
}

// releaseLocked 释放并关闭句柄。持 e.mu 时调用。
func (e *LocalLeaderElector) releaseLocked() error {
	if e.f == nil {
		return nil
	}
	_ = unlockFile(e.f)
	_ = e.f.Close()
	e.f = nil
	return nil
}

// WriteGuard 是写面门：主节点放行、从节点拒绝。
//
// isLeader 由装配层经 RenewLoop 持续维护（atomic.Bool）。Authorize 在业务逻辑**之前**
// 调用：非主节点直接拒绝，保证从节点绝不执行写面副作用（门后漏写由装配回归测试防）。
type WriteGuard struct {
	elector  LeaderElector
	leaseID  string
	logger   *slog.Logger
	isLeader atomic.Bool
	// tryAcquireFn 是续租循环的 TryAcquire 注入点（测试注入 fake / 退避参数）。
	tryAcquireFn func(ctx context.Context, leaseID string, ttl time.Duration) (bool, error)
}

// NewWriteGuard 构造写面门。elector 为 nil → 未装配（旧行为零回归：Authorize 恒 nil）。
func NewWriteGuard(elector LeaderElector, leaseID string, logger *slog.Logger) *WriteGuard {
	if logger == nil {
		logger = slog.Default()
	}
	g := &WriteGuard{
		elector: elector,
		leaseID: leaseID,
		logger:  logger,
	}
	g.tryAcquireFn = func(ctx context.Context, leaseID string, ttl time.Duration) (bool, error) {
		if g.elector == nil {
			return true, nil // 未装配 = 恒主（零回归）
		}
		return g.elector.TryAcquire(ctx, leaseID, ttl)
	}
	// 未装配（elector nil）= 写面门不装配：恒主放行（旧行为零回归，设计 §2.5）。
	if elector == nil {
		g.isLeader.Store(true)
	}
	return g
}

// Authorize 校验本节点是否为写面主节点。非主 → ErrNotLeader（装配层映射 503）。
func (g *WriteGuard) Authorize() error {
	if !g.isLeader.Load() {
		return ErrNotLeader
	}
	return nil
}

// IsLeader 返回当前主节点状态（测试/观测用）。
func (g *WriteGuard) IsLeader() bool { return g.isLeader.Load() }

// SetLeader 显式设置主节点状态（测试注入 / 初始装配用）。
func (g *WriteGuard) SetLeader(v bool) { g.isLeader.Store(v) }

// RenewLoop 是续租守护循环（装配层调用，goroutine）：
//   - 每 renewInterval 调 elector.Renew；ErrLeaseLost → isLeader=false + 日志 +
//     按退避（1s→2s→4s→封顶 10s）重试 TryAcquire；成功 → isLeader=true；
//   - elector 为 nil（未装配）→ 恒主循环（零回归，仅随 ctx 退出）；
//   - ctx 取消退出。
//
// 退避间隔可经 testBackoff 注入（默认 1s 起步 2 倍封顶 10s）。
func (g *WriteGuard) RenewLoop(ctx context.Context, leaseID string, renewInterval, ttl time.Duration) {
	if g.elector == nil {
		// 未装配：恒主，仅随 ctx 退出（零回归——不启动任何续租/抢占逻辑）。
		g.isLeader.Store(true)
		<-ctx.Done()
		return
	}
	if renewInterval <= 0 {
		renewInterval = ttl / 3
	}
	if renewInterval <= 0 {
		renewInterval = time.Second
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	backoff := time.Second
	const backoffMax = 10 * time.Second
	ticker := time.NewTicker(renewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := g.elector.Renew(ctx, leaseID); err != nil {
			if errors.Is(err, ErrLeaseLost) {
				if g.isLeader.Swap(false) {
					g.logger.Warn("leader: 租约丢失，降级为只读（fail-closed）", "lease_id", leaseID)
				}
				// 退避重试抢占（1s→2s→4s→封顶 10s）；被他人持有（ok=false）时
				// 继续退避；获取成功 → isLeader=true 回到外层续租循环。
				retryTicker := time.NewTicker(backoff)
			acquireLoop:
				for {
					select {
					case <-ctx.Done():
						retryTicker.Stop()
						return
					case <-retryTicker.C:
						ok, aerr := g.tryAcquireFn(ctx, leaseID, ttl)
						if aerr != nil {
							g.logger.Warn("leader: 抢占失败（重试）", "lease_id", leaseID, "error", aerr)
							continue
						}
						if ok {
							retryTicker.Stop()
							backoff = time.Second
							g.isLeader.Store(true)
							g.logger.Info("leader: 重新成为主节点", "lease_id", leaseID)
							break acquireLoop
						}
						// 仍被他人持有：指数退避（封顶 10s）。
						if backoff < backoffMax {
							backoff *= 2
							if backoff > backoffMax {
								backoff = backoffMax
							}
						}
					}
				}
				continue
			}
			g.logger.Warn("leader: 续租失败（重试）", "lease_id", leaseID, "error", err)
		}
	}
}
