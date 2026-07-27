// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

const (
	replayWindowSize = 5 * 60 // 5 分钟窗口（秒）
	replayMaxJTILen  = 32     // jti hex 最大长度（16 字节）
)

// ReplayProtector 用于检测和拒绝重放的隧道帧。
type ReplayProtector struct {
	mu       sync.Mutex
	seen     map[string]int64 // jti -> iat
	cleanupN int              // 计数器，达到阈值时触发清理
}

// NewReplayProtector 创建重放保护器。
func NewReplayProtector() *ReplayProtector {
	return &ReplayProtector{
		seen: make(map[string]int64),
	}
}

// Validate 验证 jti 和 iat 是否有效。
// 返回 nil 表示通过验证，拒绝重放。
func (rp *ReplayProtector) Validate(jti string, iat int64) error {
	now := time.Now().Unix()

	// 时间戳必须在 ±5 分钟窗口内
	if iat < now-replayWindowSize || iat > now+replayWindowSize {
		return fmt.Errorf("replay: iat out of window")
	}

	// jti 必须是有效的 hex 字符串
	if len(jti) == 0 || len(jti) > replayMaxJTILen {
		return fmt.Errorf("replay: invalid jti length")
	}
	if _, err := hex.DecodeString(jti); err != nil {
		return fmt.Errorf("replay: invalid jti hex: %w", err)
	}

	rp.mu.Lock()
	defer rp.mu.Unlock()

	// 检查是否已存在
	if _, exists := rp.seen[jti]; exists {
		return fmt.Errorf("replay: duplicate jti")
	}

	rp.seen[jti] = iat

	// 周期性清理过期条目（每 1000 次插入触发一次）
	rp.cleanupN++
	if rp.cleanupN >= 1000 {
		rp.cleanup()
		rp.cleanupN = 0
	}

	return nil
}

// cleanup 清理窗口外的条目。
func (rp *ReplayProtector) cleanup() {
	now := time.Now().Unix()
	for jti, iat := range rp.seen {
		if iat < now-replayWindowSize || iat > now+replayWindowSize {
			delete(rp.seen, jti)
		}
	}
}

// Len 返回当前保护的条目数（用于测试）。
func (rp *ReplayProtector) Len() int {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	return len(rp.seen)
}

// generateJTI 生成一个随机的 16 字节 hex 字符串作为请求唯一 ID。
func generateJTI() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// 注意：crypto/rand.Read 失败时，Encrypt 中的 nonce 生成也会失败，
		// 请求在加密阶段就已终止，此 fallback 路径实际不可达。
		// 保留 fallback 仅用于防御性编程，避免 panic 导致全服务中断。
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
