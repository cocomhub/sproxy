// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Ring 是 AK→多 SK 条目的原子集合（凭据单一事实源）。
//
// 并发安全：所有公开方法均由互斥锁保护，物理层（auth / hub / 派生）可任意并发
// 查询而无需外部再同步。查询类方法按"当前时间"过滤存活条目（alive），写入类方法
// 校验 AK 存在 / SK 长度 / ID 唯一。
type Ring struct {
	mu  sync.RWMutex
	m   map[string]*Key
	now func() time.Time
}

// NewRing 创建空 Ring。可注入自定义时钟 now 用于测试（未传默认 time.Now）。
func NewRing(now ...func() time.Time) *Ring {
	n := time.Now
	if len(now) > 0 && now[0] != nil {
		n = now[0]
	}
	return &Ring{
		m:   make(map[string]*Key),
		now: n,
	}
}

// aliveLocked 判定条目是否存活：状态非 disabled，且（ExpiresAt 为零值=永久 或 未到过期时间）。
func aliveLocked(e SKEntry, now time.Time) bool {
	if e.Status == StatusDisabled {
		return false
	}
	if e.ExpiresAt.IsZero() {
		return true
	}
	return now.Before(e.ExpiresAt)
}

// UpsertAK 登记一个 AK（存在则更新 Owner，不重置其条目）。AK 为空返回 ErrInvalidAK。
func (r *Ring) UpsertAK(ak, owner string) error {
	if ak == "" {
		return ErrInvalidAK
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if k, ok := r.m[ak]; ok {
		k.Owner = owner
		return nil
	}
	r.m[ak] = &Key{AK: ak, Owner: owner}
	return nil
}

// EntryOption 是 AddKey 的可选入参（ID / 生命周期 / 形态 / 元信息）。
type EntryOption func(*SKEntry)

// WithID 显式指定条目 ID（默认留空由 AddKey 自动生成）。
func WithID(id string) EntryOption {
	return func(e *SKEntry) { e.ID = id }
}

// WithKind 指定条目形态（默认 KindPlain）。
func WithKind(k Kind) EntryOption {
	return func(e *SKEntry) { e.Kind = k }
}

// WithWrapKeyID 指定包裹该 SK 的信封 (wrap) 密钥的 AK 标识。
func WithWrapKeyID(wrapAK string) EntryOption {
	return func(e *SKEntry) { e.WrapKeyID = wrapAK }
}

// WithExpiresAt 指定条目过期时间（零值=永久有效）。
func WithExpiresAt(t time.Time) EntryOption {
	return func(e *SKEntry) { e.ExpiresAt = t }
}

// WithMeta 指定条目元信息（类型 / 来源 IP）。
func WithMeta(m Meta) EntryOption {
	return func(e *SKEntry) { e.Meta = m }
}

// AddKey 为已存在的 AK 追加一条 SK 条目，返回生成的条目 ID。
//
//   - SK 必须为 32 字节（AES-256 密钥长度），否则 ErrInvalidSecret。
//   - ID 为空时自动生成（newEntryID）；显式指定则须唯一，重复返回 ErrDuplicate。
//   - ExpiresAt 默认零值=永久有效；可通过 WithExpiresAt 覆盖。
func (r *Ring) AddKey(ak string, sk []byte, opts ...EntryOption) (string, error) {
	return r.addKey(ak, sk, opts)
}

// addKey 是 AddKey 的内部实现（加锁 + 校验 + 追加），供公共同名方法调用。
func (r *Ring) addKey(ak string, sk []byte, opts []EntryOption) (string, error) {
	if len(sk) != 32 {
		return "", ErrInvalidSecret
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key, ok := r.m[ak]
	if !ok {
		return "", ErrNotFound
	}
	e := SKEntry{
		// 复制入参切片，避免调用方在 AddKey 后改写缓冲区污染 ring 内部凭据。
		SK:        append([]byte(nil), sk...),
		Kind:      KindPlain,
		Status:    StatusActive,
		CreatedAt: r.now(),
	}
	for _, o := range opts {
		o(&e)
	}
	if e.ID == "" {
		id, err := newEntryID()
		if err != nil {
			// crypto/rand 故障属于不可重试的系统性失败，向上抛出（不复用其他哨兵）。
			return "", fmt.Errorf("accesskey: add key: %w", err)
		}
		e.ID = id
	}
	for i := range key.Entries {
		if key.Entries[i].ID == e.ID {
			return "", ErrDuplicate
		}
	}
	key.Entries = append(key.Entries, e)
	return e.ID, nil
}

// Lookup 返回 AK 名下全部存活（alive）SK 条目的深拷贝及存在性。
//
// ok=false 表示该 AK 未登记（或全部条目已过期/禁用，此时返回空切片 + ok=true）。
func (r *Ring) Lookup(ak string) ([]SKEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key, ok := r.m[ak]
	if !ok {
		return nil, false
	}
	now := r.now()
	var out []SKEntry
	for _, e := range key.Entries {
		if aliveLocked(e, now) {
			out = append(out, cloneEntry(e))
		}
	}
	return out, true
}

// CoreEntry 返回该 AK 存活条目中"主条目"：alive 且 CreatedAt 最新（多个同 CreatedAt
// 取切片最后加入者，即最晚加入的）。nil 表示无存活条目。物理层（验签 / 派生）以此
// 作为默认 SK 来源。
func (r *Ring) CoreEntry(ak string) *SKEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key, ok := r.m[ak]
	if !ok {
		return nil
	}
	now := r.now()
	var best *SKEntry
	for i := range key.Entries {
		e := &key.Entries[i]
		if !aliveLocked(*e, now) {
			continue
		}
		// 取 CreatedAt 最新；同 CreatedAt 时取切片靠后的（i 递增，后加者在后）。
		if best == nil || !e.CreatedAt.Before(best.CreatedAt) {
			best = e
		}
	}
	// 返回深拷贝，避免调用方持有内部指针。
	if best == nil {
		return nil
	}
	cp := cloneEntry(*best)
	return &cp
}

// GetEntry 返回 AK 名下指定 ID 条目及存活标记。
//
// 语义：条目不存在 → ErrNotFound；条目存在但非存活（过期/禁用）→ ErrExpired。
// 调用方（4B / 验证 / 管理端点）据此精确区分"未找到"与"已过期"。
func (r *Ring) GetEntry(ak, id string) (SKEntry, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key, ok := r.m[ak]
	if !ok {
		return SKEntry{}, false, ErrNotFound
	}
	for i := range key.Entries {
		if key.Entries[i].ID == id {
			e := key.Entries[i]
			alive := aliveLocked(e, r.now())
			if !alive {
				return SKEntry{}, false, ErrExpired
			}
			return cloneEntry(e), true, nil
		}
	}
	return SKEntry{}, false, ErrNotFound
}

// ExpireKey 设置某条 SK 的生效截止时间（直到 until）。until 传零值表示清除过期时间
// （恢复永久有效）。设置后同步刷新 Status，避免状态滞留误导展示/持久化：
//   - until 零值（恢复永久）→ Status=active
//   - until 非零且尚未到达 → Status=active
//   - until 非零且已过去 → Status=expired
//
// 条目或 AK 不存在返回 ErrNotFound。aliveLocked 判定独立于 Status（仅 disabled 与
// ExpiresAt 参与），Status 的刷新为使持久化/审计视图一致。
func (r *Ring) ExpireKey(ak, id string, until time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key, ok := r.m[ak]
	if !ok {
		return ErrNotFound
	}
	for i := range key.Entries {
		if key.Entries[i].ID == id {
			e := &key.Entries[i]
			e.ExpiresAt = until
			switch {
			case until.IsZero():
				// 恢复永久有效。
				e.Status = StatusActive
			case r.now().After(until):
				e.Status = StatusExpired
			default:
				e.Status = StatusActive
			}
			return nil
		}
	}
	return ErrNotFound
}

// DeleteKey 删除某条 SK。条目不存在返回 ErrNotFound（404 语义）。
func (r *Ring) DeleteKey(ak, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key, ok := r.m[ak]
	if !ok {
		return ErrNotFound
	}
	for i := range key.Entries {
		if key.Entries[i].ID == id {
			key.Entries = append(key.Entries[:i], key.Entries[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

// DeleteAK 删除整个 AK。AK 不存在返回 ErrNotFound。
func (r *Ring) DeleteAK(ak string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[ak]; !ok {
		return ErrNotFound
	}
	delete(r.m, ak)
	return nil
}

// Snapshot 返回全部 Key 的深拷贝，按 AK 字符串排序。调用方修改返回内容不影响 ring。
func (r *Ring) Snapshot() []Key {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Key, 0, len(r.m))
	for _, key := range r.m {
		out = append(out, cloneKey(key))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AK < out[j].AK })
	return out
}

// Replace 用给定 Key 列表原子全量替换 ring 内容（用于 store 装载 / 快照还原）。
// 每个 Key 的 AK 必须非空，否则返回 ErrInvalidAK 且整个替换不生效。
// 入参被深拷贝，调用方随后修改不影响 ring。
func (r *Ring) Replace(keys []Key) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range keys {
		if k.AK == "" {
			return ErrInvalidAK
		}
	}
	m := make(map[string]*Key, len(keys))
	for _, k := range keys {
		cp := cloneKey(&k)
		m[k.AK] = &cp
	}
	r.m = m
	return nil
}

// Len 返回已登记 AK 数量（ring 判空用，如 authMiddleware 无凭据兜底）。
func (r *Ring) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.m)
}

// cloneKey 深拷贝 Key（Entries 的 SK 底层字节一并复制）。
func cloneKey(k *Key) Key {
	cp := Key{AK: k.AK, Owner: k.Owner, Entries: make([]SKEntry, 0, len(k.Entries))}
	for _, e := range k.Entries {
		cp.Entries = append(cp.Entries, cloneEntry(e))
	}
	return cp
}

// cloneEntry 深拷贝 SKEntry（复制 SK 底层字节，避免共享切片）。
func cloneEntry(e SKEntry) SKEntry {
	if e.SK != nil {
		sk := make([]byte, len(e.SK))
		copy(sk, e.SK)
		e.SK = sk
	}
	return e
}
