// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// state_share.go 是分享链接的 StateStore 适配（statestore.md §5.1 P1）：
//   - stateBackedShareStore 实现 ShareStoreIface：逐 token key `share/<token>` 委托
//     StateStore.Get/Put/Delete；
//   - **Consume 计数走 CAS**（Get → 改 Downloads → CAS 冲突重试，有界）：一次性/限量
//     分享并发消费不超发（设计决策：副本节点 503 防超发，见 cluster-state-migration.md §3.4）；
//   - 双读单写（零回归铁律，statestore.md §5.1）：StateStore 未命中回退读旧
//     <meta>/share/<token>.json（legacyDir，迁移前存量零丢失）；首写恒写 StateStore
//     新路径（旧文件仅删除清理）。
//
// 序列化格式与既有 shareLinkPersist JSON 逐字一致（读旧格式兼容）。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/state"
)

// maxConsumeRetries 是 Consume CAS 冲突重试上限（有界循环，对齐 statestore.md §4）。
const maxConsumeRetries = 8

// ShareStoreIface 是分享存储的业务接口（*ShareStore 本地形态与 *stateBackedShareStore
// StateStore 形态共用；handlers 装配层持接口，测试可直接构造具体类型）。
type ShareStoreIface interface {
	Create(filename, tenantID, rel, owner string, ttl time.Duration, maxDownloads int, oneTime, readOnly bool, watermarkSeed string) (*ShareLink, error)
	Peek(token string) *ShareLink
	Consume(token string) *ShareLink
	List(owner string) []*ShareLink
	Revoke(token, owner string) error
	EnablePersist(dir string)
	cleanupExpired()
	Stop()
}

var _ ShareStoreIface = (*ShareStore)(nil)

// stateBackedShareStore 是 ShareStoreIface 的 StateStore 适配（装配层用）。
// legacyDir 为旧 <meta>/share 绝对路径（回退读；空 = 不回退）。
type stateBackedShareStore struct {
	st        state.StateStore
	legacyDir string // 旧 <meta>/share（回退读 + 清理删除）；空 = 不回退
	logger    *slog.Logger
}

// newStateBackedShareStore 构造 StateStore 后端分享存储。
func newStateBackedShareStore(st state.StateStore, legacyDir string, logger *slog.Logger) *stateBackedShareStore {
	return &stateBackedShareStore{st: st, legacyDir: legacyDir, logger: logger}
}

// shareKey 返回 token 对应的 StateStore key（statestore.md §2.3：share/<token> 两段式）。
func shareKey(token string) string { return sharePrefix + token }

// EnablePersist 装配层调用：把 legacyDir 置为旧 <meta>/share（回退读路径；惰性，无扫描）。
// 语义对齐 *ShareStore.EnablePersist 的装配时机（首次请求前，幂等）。
func (s *stateBackedShareStore) EnablePersist(dir string) {
	if dir == "" {
		return
	}
	s.legacyDir = dir
}

// legacyPath 返回 token 对应的旧落盘文件路径（legacyDir 为空时为空串）。
func (s *stateBackedShareStore) legacyPath(token string) string {
	if s.legacyDir == "" {
		return ""
	}
	return filepath.Join(s.legacyDir, token+".json")
}

// loadFromState 从 StateStore 读单条（未命中 → nil，不报错）。
func (s *stateBackedShareStore) loadFromState(ctx context.Context, token string) (*ShareLink, error) {
	data, err := s.st.Get(ctx, shareKey(token))
	if err != nil {
		if errors.Is(err, state.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("分享 StateStore 读取失败: %w", err)
	}
	var pl shareLinkPersist
	if err := json.Unmarshal(data, &pl); err != nil {
		return nil, fmt.Errorf("分享 StateStore 值解析失败: %w", err)
	}
	return pl.toLink(), nil
}

// loadLegacy 读旧 <meta>/share/<token>.json（未命中/损坏 → nil）。
func (s *stateBackedShareStore) loadLegacy(token string) *ShareLink {
	p := s.legacyPath(token)
	if p == "" {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var pl shareLinkPersist
	if err := json.Unmarshal(data, &pl); err != nil {
		s.logger.Warn("旧分享文件解析失败，跳过", "file", p, "error", err)
		return nil
	}
	return pl.toLink()
}

// removeLegacy 删除旧落盘文件（未启用 legacyDir 时 no-op；文件不存在静默）。
func (s *stateBackedShareStore) removeLegacy(token string) {
	p := s.legacyPath(token)
	if p == "" {
		return
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		s.logger.Warn("旧分享文件删除失败", "token", token, "error", err)
	}
}

// Create 生成新的分享链接并写入 StateStore（逐 token key）。
func (s *stateBackedShareStore) Create(filename, tenantID, rel, owner string, ttl time.Duration, maxDownloads int, oneTime, readOnly bool, watermarkSeed string) (*ShareLink, error) {
	ctx := context.Background()
	// 容量上限：先清理过期条目再计数（与 *ShareStore.Create 同语义）。
	s.cleanupExpired()

	const maxTokenRetries = 10
	var token string
	var link *ShareLink
	for range maxTokenRetries {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return nil, fmt.Errorf("生成 token 失败: %w", err)
		}
		token = hex.EncodeToString(b)
		if _, err := s.st.Get(ctx, shareKey(token)); errors.Is(err, state.ErrKeyNotFound) {
			break
		}
	}
	if link == nil {
		// token 冲突重试耗尽（或全部被占用）→ 报错（对齐 *ShareStore 语义）。
		if _, err := s.st.Get(ctx, shareKey(token)); err == nil {
			return nil, fmt.Errorf("无法生成唯一的分享 token（重试 %d 次后仍冲突）", maxTokenRetries)
		}
	}

	now := time.Now()
	link = &ShareLink{
		Token: token, Filename: filename, TenantID: tenantID, Rel: rel, Owner: owner,
		CreatedAt: now, ExpiresAt: now.Add(ttl), MaxDownloads: maxDownloads,
		OneTime: oneTime, ReadOnly: readOnly, WatermarkSeed: watermarkSeed,
	}
	// 容量淘汰（与 *ShareStore 同语义：仍满按创建时间淘汰最旧 10%）。
	keys, err := s.st.List(ctx, sharePrefix)
	if err == nil && len(keys) >= maxShareEntries {
		if evictErr := s.evictLocked(ctx, keys); evictErr != nil {
			s.logger.Warn("分享容量淘汰失败", "error", evictErr)
		}
	}
	data, err := json.Marshal(link.toPersist())
	if err != nil {
		return nil, fmt.Errorf("分享序列化失败: %w", err)
	}
	if err := s.st.Put(ctx, shareKey(token), data); err != nil {
		return nil, fmt.Errorf("分享 StateStore 写入失败: %w", err)
	}
	s.removeLegacy(token) // 首写迁移：旧同名文件清理（防双写残留）
	return link, nil
}

// evictLocked 容量淘汰：清理过期条目；仍满按创建时间淘汰最旧 10%（对齐 *ShareStore.Create）。
func (s *stateBackedShareStore) evictLocked(ctx context.Context, keys []string) error {
	now := time.Now()
	var active []*ShareLink
	for _, k := range keys {
		link, err := s.loadFromState(ctx, strings.TrimPrefix(k, sharePrefix))
		if err != nil || link == nil {
			continue
		}
		if now.After(link.ExpiresAt) {
			token := strings.TrimPrefix(k, sharePrefix)
			_ = s.st.Delete(ctx, k)
			s.removeLegacy(token)
			continue
		}
		active = append(active, link)
	}
	if len(active) < maxShareEntries {
		return nil
	}
	sort.Slice(active, func(i, j int) bool { return active[i].CreatedAt.Before(active[j].CreatedAt) })
	evictCount := maxShareEntries / 10
	for i := 0; i < evictCount && i < len(active); i++ {
		_ = s.st.Delete(ctx, shareKey(active[i].Token))
		s.removeLegacy(active[i].Token)
		s.logger.Warn("分享容量满，淘汰最旧分享（活跃链接将失效）",
			"token", active[i].Token, "created_at", active[i].CreatedAt.Format(time.RFC3339))
	}
	return nil
}

// Peek 返回指定 token 的分享链接副本，不修改状态。
func (s *stateBackedShareStore) Peek(token string) *ShareLink {
	ctx := context.Background()
	link, err := s.loadFromState(ctx, token)
	if err != nil {
		s.logger.Warn("分享 Peek 读取失败", "token", token, "error", err)
		return nil
	}
	if link == nil {
		// 双读：StateStore 未命中回退旧 meta 文件。
		link = s.loadLegacy(token)
	}
	if link == nil {
		return nil
	}
	c := *link
	return &c
}

// Consume 原子性地检查并消费一个分享链接（Downloads 计数走 CAS，冲突重试有界）。
// 一次性分享：CAS(old=cur, new=nil) 删除；限量分享：CAS 递增 Downloads。
// 返回链接信息供后续使用，如果链接无效则返回 nil。
func (s *stateBackedShareStore) Consume(token string) *ShareLink {
	ctx := context.Background()
	key := shareKey(token)
	for range maxConsumeRetries {
		if result, retry := s.consumeOnce(ctx, key, token); !retry {
			return result
		}
	}
	s.logger.Warn("分享 Consume CAS 重试耗尽", "token", token)
	return nil
}

// consumeOnce 单次消费尝试：返回最终结果与是否需重试（retry=true 表示 CAS 冲突，
// 调用方按有界次数重试；retry=false 时 result 即最终消费结果）。
func (s *stateBackedShareStore) consumeOnce(ctx context.Context, key, token string) (res *ShareLink, retry bool) {
	cur, err := s.st.Get(ctx, key)
	if err != nil {
		if errors.Is(err, state.ErrKeyNotFound) {
			// 双读：回退旧 meta 文件（一次性删除 + 计数型尽力而为更新）。
			return s.consumeLegacy(token), false
		}
		s.logger.Warn("分享 Consume 读取失败", "token", token, "error", err)
		return nil, false
	}
	var pl shareLinkPersist
	if err := json.Unmarshal(cur, &pl); err != nil {
		s.logger.Warn("分享 StateStore 值解析失败，视为已消费", "token", token, "error", err)
		return nil, false
	}
	link := pl.toLink()
	// 「过期」比较用 !Before（对齐 *ShareStore.Consume 的 Windows 单调时钟语义）。
	if now := time.Now(); !now.Before(link.ExpiresAt) {
		_ = s.st.Delete(ctx, key) // 过期即删除（CAS 不必要：过期由时间推进保证）
		s.removeLegacy(token)
		return nil, false
	}
	if link.MaxDownloads > 0 && link.Downloads >= link.MaxDownloads {
		_ = s.st.Delete(ctx, key)
		s.removeLegacy(token)
		return nil, false
	}
	if link.OneTime {
		// 一次性：CAS(old=cur, new=nil)——防并发双消费。
		if result, done := s.consumeOneTimeCAS(ctx, key, token, cur, link); done {
			return result, false
		}
		return nil, true // 冲突重试（他人已消费/已改）
	}
	// 计数递增：CAS(old=cur, new=bumped)。
	if result, done := s.consumeBumpCountCAS(ctx, key, token, cur, pl, link); done {
		return result, false
	}
	return nil, true // 冲突重试（并发消费，读取最新再改）
}

// consumeOneTimeCAS 一次性分享消费：CAS(old=cur, new=nil) 删除，防并发双消费。
// done=false 表示 CAS 冲突（他人已消费/已改），由调用方继续重试。
func (s *stateBackedShareStore) consumeOneTimeCAS(ctx context.Context, key, token string, cur []byte, link *ShareLink) (result *ShareLink, done bool) {
	if cerr := s.st.CAS(ctx, key, cur, nil); cerr == nil {
		s.removeLegacy(token)
		return link, true
	} else if errors.Is(cerr, state.ErrCASMismatch) {
		return nil, false
	} else {
		s.logger.Warn("分享一次性删除 CAS 失败", "token", token, "error", cerr)
		return nil, true
	}
}

// consumeBumpCountCAS 限量分享消费：CAS 递增 Downloads 计数（old=cur, new=bumped）。
// done=false 表示 CAS 冲突（并发消费，读取最新再改），由调用方继续重试。
func (s *stateBackedShareStore) consumeBumpCountCAS(ctx context.Context, key, token string, cur []byte, pl shareLinkPersist, link *ShareLink) (result *ShareLink, done bool) {
	newPL := pl
	newPL.Downloads++
	data, merr := json.Marshal(newPL)
	if merr != nil {
		s.logger.Warn("分享计数序列化失败", "token", token, "error", merr)
		return nil, true
	}
	if cerr := s.st.CAS(ctx, key, cur, data); cerr == nil {
		link.Downloads = newPL.Downloads
		return link, true
	} else if errors.Is(cerr, state.ErrCASMismatch) {
		return nil, false
	} else {
		s.logger.Warn("分享计数 CAS 失败", "token", token, "error", cerr)
		return nil, true
	}
}

// consumeLegacy 回退消费旧 <meta>/share/<token>.json（迁移前存量；尽力而为）。
// 一次性：删除旧文件；计数型：更新旧文件计数。StateStore 迁移后该路径不再被写。
func (s *stateBackedShareStore) consumeLegacy(token string) *ShareLink {
	link := s.loadLegacy(token)
	if link == nil {
		return nil
	}
	if now := time.Now(); !now.Before(link.ExpiresAt) {
		s.removeLegacy(token)
		return nil
	}
	if link.MaxDownloads > 0 && link.Downloads >= link.MaxDownloads {
		s.removeLegacy(token)
		return nil
	}
	if link.OneTime {
		s.removeLegacy(token)
		return link
	}
	link.Downloads++
	if p := s.legacyPath(token); p != "" {
		if data, err := json.Marshal(link.toPersist()); err == nil {
			_ = os.WriteFile(p, data, 0o600)
		}
	}
	return link
}

// List 返回所有分享链接的副本（StateStore 全量 + 旧 meta 补列）。
func (s *stateBackedShareStore) List(owner string) []*ShareLink {
	ctx := context.Background()
	seen := map[string]bool{}
	result := make([]*ShareLink, 0)
	result = s.listFromState(ctx, owner, seen, result)
	result = s.listFromLegacy(owner, seen, result)
	return result
}

// listFromState 从 StateStore 列出 owner 可见的分享链接（追加到 result，seen 表去重）。
func (s *stateBackedShareStore) listFromState(ctx context.Context, owner string, seen map[string]bool, result []*ShareLink) []*ShareLink {
	keys, err := s.st.List(ctx, sharePrefix)
	if err == nil {
		for _, k := range keys {
			token := strings.TrimPrefix(k, sharePrefix)
			link, lerr := s.loadFromState(ctx, token)
			if lerr != nil || link == nil {
				continue
			}
			if owner != "" && link.Owner != owner {
				continue
			}
			seen[token] = true
			cp := *link
			result = append(result, &cp)
		}
	}
	return result
}

// listFromLegacy 从旧 meta 目录补列迁移前存量分享（StateStore 未命中且未在 seen 表中的）。
func (s *stateBackedShareStore) listFromLegacy(owner string, seen map[string]bool, result []*ShareLink) []*ShareLink {
	// 旧 meta 补列（迁移前存量在 StateStore 尚未写入时的可见性）。
	if s.legacyDir != "" {
		entries, rerr := os.ReadDir(s.legacyDir)
		if rerr == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				token := strings.TrimSuffix(e.Name(), ".json")
				result = s.listLegacyEntry(owner, token, seen, result)
			}
		}
	}
	return result
}

// listLegacyEntry 处理单个旧 meta 分享文件：有效且 owner 匹配的追加到 result（seen 表去重）。
func (s *stateBackedShareStore) listLegacyEntry(owner, token string, seen map[string]bool, result []*ShareLink) []*ShareLink {
	if seen[token] {
		return result
	}
	link := s.loadLegacy(token)
	if link == nil {
		return result
	}
	if owner != "" && link.Owner != owner {
		return result
	}
	seen[token] = true
	cp := *link
	result = append(result, &cp)
	return result
}

// Revoke 删除指定 token 的分享链接（StateStore + 旧 meta 双删）。
// 多租户：owner 非空时必须与链接创建者一致，否则拒绝。
func (s *stateBackedShareStore) Revoke(token, owner string) error {
	ctx := context.Background()
	link, err := s.loadFromState(ctx, token)
	if err != nil {
		return fmt.Errorf("分享链接不存在: %s", token)
	}
	if link == nil {
		link = s.loadLegacy(token)
	}
	if link == nil {
		return fmt.Errorf("分享链接不存在: %s", token)
	}
	if owner != "" && link.Owner != owner {
		return fmt.Errorf("分享链接不存在: %s", token) // 跨租户视为不存在（防枚举）
	}
	if derr := s.st.Delete(ctx, shareKey(token)); derr != nil {
		return fmt.Errorf("分享 StateStore 删除失败: %w", derr)
	}
	s.removeLegacy(token)
	return nil
}

// cleanupExpired 清理所有已过期的分享链接（StateStore 全量 + 旧 meta 目录扫描）。
func (s *stateBackedShareStore) cleanupExpired() {
	ctx := context.Background()
	now := time.Now()
	s.cleanupExpiredFromState(ctx, now)
	s.cleanupExpiredFromLegacy(now)
}

// cleanupExpiredFromState 清理 StateStore 中已过期/消费完的分享条目。
func (s *stateBackedShareStore) cleanupExpiredFromState(ctx context.Context, now time.Time) {
	keys, err := s.st.List(ctx, sharePrefix)
	if err == nil {
		for _, k := range keys {
			token := strings.TrimPrefix(k, sharePrefix)
			link, lerr := s.loadFromState(ctx, token)
			if lerr != nil || link == nil {
				continue
			}
			if now.After(link.ExpiresAt) || (link.MaxDownloads > 0 && link.Downloads >= link.MaxDownloads) {
				_ = s.st.Delete(ctx, k)
				s.removeLegacy(token)
			}
		}
	}
}

// cleanupExpiredFromLegacy 清理旧 meta 目录中已过期/消费完的分享文件。
func (s *stateBackedShareStore) cleanupExpiredFromLegacy(now time.Time) {
	if s.legacyDir == "" {
		return
	}
	entries, rerr := os.ReadDir(s.legacyDir)
	if rerr != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		link := s.loadLegacy(strings.TrimSuffix(e.Name(), ".json"))
		if link == nil {
			continue
		}
		if now.After(link.ExpiresAt) || (link.MaxDownloads > 0 && link.Downloads >= link.MaxDownloads) {
			s.removeLegacy(link.Token)
		}
	}
}

// Stop 无后台 goroutine（与 *ShareStore 兼容签名）。
func (s *stateBackedShareStore) Stop() { /* 无后台 goroutine：状态后端无需清理 */ }
