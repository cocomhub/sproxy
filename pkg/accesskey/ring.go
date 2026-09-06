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
// 旧 credentials.json（4A 无 role 字段）载入时 Role 为空串——这里归一为 RoleUser
// （R3-M4），显式 role 字段保留。
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
		if cp.Role == "" {
			cp.Role = RoleUser
		}
		m[k.AK] = &cp
	}
	r.m = m
	return nil
}

// GetKey 返回 AK 对应完整 Key 的深拷贝（含 Role / TOTPSecret）；不存在 → (nil, false)。
//
// I1：getRole 读 Key.Role 与登录 handler 取 TOTPSecret 的唯一访问路径（Lookup/GetEntry/
// Snapshot 均拿不到含 Role 的整 Key）。调用方修改返回值不影响 ring。
func (r *Ring) GetKey(ak string) (*Key, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key, ok := r.m[ak]
	if !ok {
		return nil, false
	}
	cp := cloneKey(key)
	return &cp, true
}

// AddRegistration 注册一个新账号（4B 简单模式 / TOTP 模式共用，DEC-A/DEC-B，I2）：
//
//   - sk 非 nil（简单模式）→ 写 Key.Role + 内联追加一条 plain SK 条目（ExpiresAt =
//     now+ttl，ttl 由调用方注入——pkg/accesskey 不读服务端配置，R4-I1）；
//   - totpSecret 非 nil（TOTP 模式）→ 写 Key.TOTPSecret，不追加 SK 条目（ttl 忽略），
//     返回 id 为空串；
//   - sk 与 totpSecret 必须恰有一个非 nil（双 nil → ErrRegistrationRequiresSecret，R3-M2）；
//   - sk 非 nil 且非 32 字节 → ErrInvalidSecret。
//
// admin 授予由方法内原子判定覆盖（D2/I2/M5）：写锁内扫描全 ring 无 Key.Role=="admin"
// → 本次授 RoleAdmin（首注册恒 admin，无论入参）；已有 admin → 本次 RoleUser（非首注册
// 传 RoleAdmin 降级为 RoleUser；RoleNode/RoleUser 入参按原值）。已为 admin 的账号重复
// 注册保持 admin。granted = 本次是否授予 admin；id = 简单模式新建 SK 条目 ID。
//
// owner 空值默认 = AK 字符串（R2-N4）。
//
// 注意：方法持有写锁期间内联完成 Key 建立 + 条目追加，不得重入 UpsertAK/AddKey
// （sync.RWMutex 不可重入，持写锁再调会死锁，M5）。
func (r *Ring) AddRegistration(ak, owner string, sk, totpSecret []byte, role Role, ttl time.Duration) (granted bool, id string, err error) {
	// sk 与 totpSecret 恰有一个非 nil（双 nil → error，R3-M2）。
	if (sk == nil) == (totpSecret == nil) {
		return false, "", ErrRegistrationRequiresSecret
	}
	if ak == "" {
		return false, "", ErrInvalidAK
	}
	if owner == "" {
		owner = ak // R2-N4
	}
	if sk != nil && len(sk) != 32 {
		return false, "", ErrInvalidSecret
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// 扫描全 ring：是否已有 admin（决定本次授予角色）。
	hasAdmin := false
	for _, k := range r.m {
		if k.Role == RoleAdmin {
			hasAdmin = true
			break
		}
	}

	now := r.now()
	key, exists := r.m[ak]
	if !exists {
		key = &Key{AK: ak, Owner: owner}
		r.m[ak] = key
	} else {
		key.Owner = owner
	}

	// 角色决定（granted = 是否授予 admin）：
	//   1. 该 AK 已是 admin（重复注册）→ 保持 admin；
	//   2. 全 ring 无 admin → 首注册恒授 admin（无论入参）；
	//   3. 入参请求 admin → 降级 user（不可经注册产生第二个 admin）；
	//   4. 其余按入参（user/node），空值归一 user。
	switch {
	case key.Role == RoleAdmin:
		granted = true
	case !hasAdmin:
		key.Role = RoleAdmin
		granted = true
	case role == RoleAdmin:
		key.Role = RoleUser
		granted = false
	default:
		if role == "" {
			role = RoleUser
		}
		key.Role = role
		granted = false
	}

	if totpSecret != nil {
		// TOTP 模式：只写账号级 TOTPSecret，无 SK 条目，ttl 忽略。
		key.TOTPSecret = cloneBytes(totpSecret)
		return granted, "", nil
	}

	// 简单模式：内联追加 plain SK 条目（ExpiresAt = now + ttl，R4-I1）。
	e := SKEntry{
		SK:        cloneBytes(sk),
		Kind:      KindPlain,
		Status:    StatusActive,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	e.ID, err = newEntryID()
	if err != nil {
		return false, "", fmt.Errorf("accesskey: add registration: %w", err)
	}
	key.Entries = append(key.Entries, e)
	return granted, e.ID, nil
}

// Len 返回已登记 AK 数量（ring 判空用，如 authMiddleware 无凭据兜底）。
func (r *Ring) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.m)
}

// cloneKey 深拷贝 Key（Role/TOTPSecret 与 Entries 的 SK 底层字节一并复制）。
func cloneKey(k *Key) Key {
	cp := Key{
		AK:         k.AK,
		Owner:      k.Owner,
		Role:       k.Role,
		TOTPSecret: cloneBytes(k.TOTPSecret),
		Entries:    make([]SKEntry, 0, len(k.Entries)),
	}
	for _, e := range k.Entries {
		cp.Entries = append(cp.Entries, cloneEntry(e))
	}
	return cp
}

// cloneBytes 深拷贝字节切片（nil 保持 nil，避免空切片与 nil 语义漂移）。
func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
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
