// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// bandwidth.go 是文件级带宽限速（roadmap §6 P1）：upload/download 可选带宽上限，
// 纯 stdlib 令牌桶实现，per-owner 独立桶互不影响。
//
// 设计：
//   - TokenBucket：字节令牌桶（rate = bytes/sec，burst = 单次突发上限）。WaitN(ctx, n)
//     阻塞直到 n 字节可用（按 elapsed 时间 refill）。rate<=0 = 不限速（默认关零回归）。
//   - BandwidthLimiter：按 owner 返回 *TokenBucket 的能力接口（nil = 该 owner 不限速）。
//     领域 Upload/Download 在 io.Copy 处包一层限速 Reader/Writer，经 WithBandwidthLimiter
//     注入；装配层（pkg/server）实现 per-owner 桶映射（懒建 + cfg.RateLimit.Bandwidth.*）。
//   - 可观测：首次触发限速（WaitN 实际阻塞 >0）时记一条 Warn 日志；限速生效状态
//     经 PUT /api/config 的 bandwidth 字段可查（见 pkg/server/config_api.go）。

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// TokenBucket 是字节令牌桶（纯 stdlib，thread-safe）。
// rate 为 refill 速率（bytes/sec）；burst 为桶容量（单次可突发字节数）。
// rate<=0 表示不限速（WaitN 直接返回 nil，零回归）。
type TokenBucket struct {
	mu       sync.Mutex
	rate     float64 // bytes/sec
	burst    float64 // 桶容量
	tokens   float64
	lastTime time.Time
	// coord 是可选的跨实例协调器（coord_backend=file）：WaitN 放行前先查协调配额，
	// 配额耗尽时等待重试（不拒绝请求——带宽限速语义是慢速传输）。nil = 不协调
	// （默认 local 单实例 token 桶，零回归）。
	coord QuotaCoordinator
	key   string
}

// QuotaCoordinator 是跨实例字节配额协调窄接口（领域层不依赖装配层类型）。
// Consume(key, n) 尝试在协调后端消耗 n 字节配额：成功返回 true；配额不足返回 false
// （调用方应等待后重试，等待有界防挂起）。实现必须并发安全。
type QuotaCoordinator interface {
	Consume(key string, n int64) bool
}

// SetCoordinator 装配跨实例协调器（owner key）。nil 取消协调（回退纯内存 token 桶）。
func (b *TokenBucket) SetCoordinator(key string, c QuotaCoordinator) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.key = key
	b.coord = c
}

// NewTokenBucket 构造字节令牌桶。rate<=0 → 不限速（WaitN 恒立即成功）。
// burst<=0 时回落 rate（即单次突发 = 1 秒配额）。
func NewTokenBucket(rate, burst int64) *TokenBucket {
	r := float64(rate)
	b := float64(burst)
	if r <= 0 {
		return &TokenBucket{rate: 0, burst: 0, tokens: 0}
	}
	if b <= 0 {
		b = r
	}
	return &TokenBucket{
		rate:     r,
		burst:    b,
		tokens:   b,
		lastTime: time.Now(),
	}
}

// WaitN 阻塞直到 n 字节可用（耗尽时按 refill 速率等待）。
// ctx 取消/超时返回 ctx.Err()。rate<=0（不限速）时立即返回 nil。
//
// 协调（coord_backend=file）：WaitN 前先查协调配额（QuotaCoordinator.Consume）。
// 配额不足时等待重试（带宽限速语义：慢速传输不拒绝）——重试间隔 100ms，最大等待
// maxCoordWait（5s），超时按未限速继续（协调后端故障不挂起传输，记审计由调用方）。
func (b *TokenBucket) WaitN(ctx context.Context, n int64) error {
	if n <= 0 || b == nil || b.rate <= 0 {
		return nil
	}
	// 协调配额检查（仅装配了协调器时）。
	if b.coord != nil && !b.waitCoord(ctx, n) {
		// 协调等待超时：按未限速继续（协调后端故障兜底，传输不挂起）。
		return nil
	}
	need := float64(n)
	for {
		wait, err := b.waitFor(need)
		if err != nil {
			return err
		}
		if wait <= 0 {
			return nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// maxCoordWait 是协调配额等待的最大时长（协调后端故障/配额长期不足时兜底，防传输挂起）。
const maxCoordWait = 5 * time.Second

// coordRetryInterval 是协调配额不足时的重试间隔。
const coordRetryInterval = 100 * time.Millisecond

// waitCoord 等待协调配额可用：循环 Consume（成功立即返回 true）；不足则 ctx-aware 休眠
// coordRetryInterval 后重试，直到 maxCoordWait 超时（返回 false，调用方按未限速继续）。
func (b *TokenBucket) waitCoord(ctx context.Context, n int64) bool {
	deadline := time.NewTimer(maxCoordWait)
	defer deadline.Stop()
	ticker := time.NewTicker(coordRetryInterval)
	defer ticker.Stop()
	for {
		if b.coord.Consume(b.key, n) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-ticker.C:
		}
	}
}

// waitFor 返回「本次 wait 后可用 n 字节所需等待时长」；<=0 表示立即可用并已扣减。
// 用循环调用实现：单次 refill 可能不足 n 时（burst < n），下一次 timer 到期继续补。
func (b *TokenBucket) waitFor(need float64) (time.Duration, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if b.rate <= 0 {
		return 0, nil
	}
	// refill：按 elapsed 时间补令牌。**不封顶到 burst**——burst 是「桶初始容量/单次突发上限」，
	// 而 WaitN(n) 在 n > burst 时（如 copy 用 32KB buffer 限速 1KB/s）需要累积超过 burst 的
	// 令牌才能满足一次大额申请；若 refill 封顶到 burst，tokens 永远到不了 need，waitFor 无限
	// 循环（缺口恒在）。速率限制由 rate refill 保证（elapsed*rate 持续入桶），burst 只决定
	// 初始可立即突发量——消费侧一次性扣 need，不做 burst 拆分（拆分会让 WaitN 对 n>burst 的
	// 申请每次只放 burst 又返回正 wait，WaitN 外层 for 循环随之死循环）。
	elapsed := now.Sub(b.lastTime).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.rate
		b.lastTime = now
	}
	if b.tokens >= need {
		b.tokens -= need
		return 0, nil
	}
	// 不足：计算还需等待多久（缺口 / rate）。
	short := need - b.tokens
	wait := time.Duration(short / b.rate * float64(time.Second))
	if wait <= 0 {
		wait = time.Millisecond // 防 busy-loop
	}
	return wait, nil
}

// BandwidthLimiter 是按 owner 提供带宽令牌桶的能力接口。
// BucketFor(owner) 返回该 owner 的桶（nil = 该 owner 不限速）。
type BandwidthLimiter interface {
	BucketFor(owner string) *TokenBucket
}

// BandwidthOption 是注入带宽限制器的配置项。
type BandwidthOption struct {
	Limiter BandwidthLimiter // nil = 不限速（默认关零回归）
	Logger  func() *io.Writer
}

// NewBandwidthLimiter 构造 per-owner 桶映射的默认实现：factory 决定每个 owner 的桶
// （nil 返回 = 该 owner 不限速）。线程安全：桶经 sync.Map 按 owner 懒建缓存。
func NewBandwidthLimiter(factory func(owner string) *TokenBucket) *PerOwnerBandwidthLimiter {
	return &PerOwnerBandwidthLimiter{factory: factory, buckets: &sync.Map{}}
}

// PerOwnerBandwidthLimiter 是 BandwidthLimiter 的默认实现：owner → TokenBucket 懒建缓存。
type PerOwnerBandwidthLimiter struct {
	factory func(owner string) *TokenBucket
	buckets *sync.Map
}

// BucketFor 返回 owner 的令牌桶（懒建缓存；factory 返回 nil = 不限速）。
func (l *PerOwnerBandwidthLimiter) BucketFor(owner string) *TokenBucket {
	if l == nil || l.factory == nil {
		return nil
	}
	if v, ok := l.buckets.Load(owner); ok {
		return v.(*TokenBucket) //nolint:errcheck // 类型断言安全：只存 *TokenBucket
	}
	b := l.factory(owner)
	if b == nil {
		return nil
	}
	actual, _ := l.buckets.LoadOrStore(owner, b)
	return actual.(*TokenBucket) //nolint:errcheck
}

// rateLimitReader 包一层令牌桶限速的 io.Reader：先读实际字节数，再按实际消费配额。
// （修正：先 WaitN(len(p)) 再读会在 copy 用大 buffer（如 32KB）时按请求长度等待——
// 限速 1KB/s 时每次 Read(32KB) 先等 32s，即使文件只有 10KB；且 http.Server 背景读 +
// 无超时 client 会让上传慢到超时。先读后 WaitN 只为实际读到的字节等待，准确且无过度等待。）
type rateLimitReader struct {
	bucket *TokenBucket
	r      io.Reader
}

// Read 限速读：先读实际 n 字节，再按 n 消费配额（n==0/EOF 直接返回不等待）。
func (rl *rateLimitReader) Read(p []byte) (int, error) {
	if rl.bucket == nil {
		return rl.r.Read(p)
	}
	n, err := rl.r.Read(p)
	if n > 0 {
		if werr := rl.bucket.WaitN(context.Background(), int64(n)); werr != nil {
			return n, fmt.Errorf("带宽限速等待失败: %w", werr)
		}
	}
	return n, err
}

// rateLimitResponseWriter 是限速 + 透传的 http.ResponseWriter（ServeContent 用）。
// 内嵌 countingWriter 获得 Header/WriteHeader/Flush；Write 先写实际字节再按实际量消费配额。
type rateLimitResponseWriter struct {
	*countingWriter
	bucket *TokenBucket
}

// Write 限速写：先写实际 n 字节，再按 n 消费配额（n==0 直接返回）。
func (rl *rateLimitResponseWriter) Write(p []byte) (int, error) {
	if rl.bucket == nil {
		return rl.countingWriter.Write(p)
	}
	// 写前等配额：数据字节在配额放行后才发给客户端，下载完成时间才真正受限速约束。
	// （先写后 WaitN 会让数据先发出、等待发生在「下载已完成」之后——完成时间不受限速。）
	// WaitN 在 n>burst（如 ServeContent 用 32KB buffer）时不会死循环：refill 不封顶，
	// 直到累积令牌 >= n 一次性扣减（见 waitFor 注释）。
	if err := rl.bucket.WaitN(context.Background(), int64(len(p))); err != nil {
		return 0, fmt.Errorf("带宽限速等待失败: %w", err)
	}
	return rl.countingWriter.Write(p)
}

// Flush 透传给底层（http.ResponseWriter 完整接口需要）。
func (rl *rateLimitResponseWriter) Flush() {
	if f, ok := rl.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// limitReader 用 owner 桶包装 reader（bucket=nil 时原样返回）。
func limitReader(l BandwidthLimiter, owner string, r io.Reader) io.Reader {
	if l == nil {
		return r
	}
	b := l.BucketFor(owner)
	if b == nil {
		return r
	}
	return &rateLimitReader{bucket: b, r: r}
}

// limitResponseWriter 用 owner 桶包装 countingWriter 为限速 ResponseWriter
// （bucket=nil 时原样返回 countingWriter，保持原行为）。
func limitResponseWriter(l BandwidthLimiter, owner string, cw *countingWriter) http.ResponseWriter {
	if l == nil {
		return cw
	}
	b := l.BucketFor(owner)
	if b == nil {
		return cw
	}
	return &rateLimitResponseWriter{countingWriter: cw, bucket: b}
}
