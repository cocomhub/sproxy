// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
)

// AnonymousOwner 是未认证请求的默认租户名。
//
// 租户名是**存储布局契约**（`<root>/<owner>/…`）：该值在装配层、各领域包与磁盘布局
// 之间必须一致，故由本包单源持有（此前 `pkg/server` 与 `pkg/files` 各持一份同值常量）。
const AnonymousOwner = "anonymous"

// NormalizeOwner 把空 owner 归一为 AnonymousOwner（未认证请求的默认租户）。
// 不做合法性校验——合法性由 OpenTenant（经 ValidSegmentName）fail-closed 判定。
func NormalizeOwner(owner string) string {
	if owner == "" {
		return AnonymousOwner
	}
	return owner
}

// ---- 租户创建骨架 ----

// tenantConfig 是 OpenTenant/TenantCache 的解析后选项。
type tenantConfig struct {
	metaBucket bool
	logger     *slog.Logger
	logAttrs   []any
}

// TenantOption 配置租户创建/缓存行为。
type TenantOption func(*tenantConfig)

// WithMetaBucket 让 OpenTenant 预建 `meta` 桶（per-tenant checksum / 凭据记录的落点）。
//
// **只有默认卷（权威布局）应当开启**：非默认卷不预建 meta 桶（既有语义，见
// pkg/volume/registry.Set.Tenant 的说明）——非默认卷上的租户不承载 meta 记录。
func WithMetaBucket() TenantOption {
	return func(c *tenantConfig) { c.metaBucket = true }
}

// WithLogger 设置创建过程中的告警 logger（nil 回落 slog.Default()）。
func WithLogger(l *slog.Logger) TenantOption {
	return func(c *tenantConfig) { c.logger = l }
}

// WithLogAttrs 追加固定日志字段（如 `"volume", volName`），供多卷装配区分来源。
func WithLogAttrs(attrs ...any) TenantOption {
	return func(c *tenantConfig) { c.logAttrs = append(c.logAttrs, attrs...) }
}

// resolve 归一化选项：logger 非 nil、logAttrs 拷贝一份（避免调用方后续改动影响已建对象）。
func (c tenantConfig) resolve() tenantConfig {
	if c.logger == nil {
		c.logger = slog.Default()
	}
	c.logAttrs = append([]any(nil), c.logAttrs...)
	return c
}

// warn 按配置的固定字段记录一条 Warn。
func (c tenantConfig) warn(msg string, attrs ...any) {
	c.logger.Warn(msg, append(append([]any(nil), c.logAttrs...), attrs...)...)
}

// OpenTenant 在 parent 下按 owner 打开（必要时创建）租户子根。
//
// 步骤（既有语义逐字保留）：
//
//	ValidSegmentName(owner) → parent.Abs(owner) → MkdirAll → OpenRoot → NewTenant
//
// 任一步失败即 **fail-closed**：返回 `(nil, err)`，并关闭该步之前已打开的句柄
// （绝不泄漏 *Root 句柄），**绝不回落 parent**。调用方按 400 处理。
//
// owner 的归一化由调用方负责（空 owner 先过 NormalizeOwner）。
func OpenTenant(parent *Root, owner string, opts ...TenantOption) (*Tenant, error) {
	cfg := tenantConfig{}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	return openTenant(parent, owner, cfg.resolve())
}

// openTenant 是 OpenTenant 的已解析选项版本（供 TenantCache 复用，避免重复解析选项）。
func openTenant(parent *Root, owner string, cfg tenantConfig) (*Tenant, error) {
	if parent == nil {
		return nil, errors.New("storage: OpenTenant: parent 不能为 nil")
	}

	if !ValidSegmentName(owner) {
		cfg.warn("非法租户名，拒绝创建（fail-closed）", "owner", owner)
		return nil, fmt.Errorf("storage: OpenTenant: 非法租户名 %q", owner)
	}
	abs, ok := parent.Abs(owner)
	if !ok {
		cfg.warn("租户路径越界，拒绝创建（fail-closed）", "owner", owner)
		return nil, fmt.Errorf("storage: OpenTenant: 租户路径越界 %q", owner)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		cfg.warn("创建租户根目录失败", "owner", owner, "error", err)
		return nil, fmt.Errorf("storage: OpenTenant: 创建租户根目录失败: %w", err)
	}
	tenantRoot, err := OpenRoot(abs)
	if err != nil {
		cfg.warn("打开租户子根失败（fail-closed）", "owner", owner, "error", err)
		return nil, fmt.Errorf("storage: OpenTenant: 打开租户子根失败: %w", err)
	}
	t, err := NewTenant(owner, tenantRoot)
	if err != nil {
		cfg.warn("创建租户失败（fail-closed）", "owner", owner, "error", err)
		_ = tenantRoot.Close()
		return nil, fmt.Errorf("storage: OpenTenant: 创建租户失败: %w", err)
	}
	if cfg.metaBucket {
		if err := tenantRoot.MkdirAll("meta", 0o755); err != nil {
			cfg.warn("创建租户 meta 目录失败", "owner", owner, "error", err)
			_ = tenantRoot.Close()
			return nil, fmt.Errorf("storage: OpenTenant: 创建租户 meta 目录失败: %w", err)
		}
	}
	return t, nil
}

// ListOwners 扫描 parent 下的直接子目录，返回**合法租户名**（按名排序）。
//
// 供重启后的恢复扫描使用：内存缓存只有已访问的租户，仅靠缓存会漏掉已落盘但尚未访问
// 的租户。磁盘扫描以租户根目录为准，跳过：非目录、非法段名目录、遗留服务端内部目录
// （`.__` 魔法前缀与 `__` 遗留前缀——它们不是租户根）。
//
// parent 为 nil 或不可读返回 nil（调用方按「无租户」处理，与既有语义一致）。
func ListOwners(parent *Root) []string {
	if parent == nil {
		return nil
	}
	base, ok := parent.Abs("")
	if !ok {
		return nil
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !ValidSegmentName(name) {
			continue
		}
		// 跳过遗留服务端内部目录（.__ 魔法目录 / __ 遗留前缀等）——它们不是租户根。
		if strings.HasPrefix(name, ".__") || strings.HasPrefix(name, "__") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ---- 租户缓存 ----

// TenantCache 是「单父根 + 按 owner 懒创建缓存」的并发安全租户缓存。
//
// 它把此前分散在装配层与卷装配里的同构实现（`Handlers` 的默认卷租户缓存 +
// `registry.Set` 的按卷租户缓存）收敛为一份：加锁 → 查缓存 → 未命中则经 OpenTenant 创建 → 入缓存。
// 零值不可用，必须经 NewTenantCache 构造。
//
// **方法集与消费方声明的窄接口一致**：`TenantFor(owner) *Tenant` 使
// `*TenantCache` 结构上直接满足 `pkg/files.TenantResolver`，装配层无需适配类型。
type TenantCache struct {
	parent *Root
	cfg    tenantConfig
	mu     sync.Mutex
	cache  map[string]*Tenant
}

// NewTenantCache 构造绑定到 parent 的租户缓存。parent 为 nil 时缓存恒返回 nil
// （未装配存储根的旧路径，fail-closed）。options 见 OpenTenant。
func NewTenantCache(parent *Root, opts ...TenantOption) *TenantCache {
	cfg := tenantConfig{}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	return &TenantCache{parent: parent, cfg: cfg.resolve(), cache: map[string]*Tenant{}}
}

// TenantFor 返回 owner 的租户（懒创建并缓存）。
//
// **owner 必须已归一化**（空 owner 由调用方先过 NormalizeOwner）：本类型不做归一，
// 因为「空即 anonymous」是**调用方的策略**而非缓存的策略——pkg/volume/registry 的
// `Set.Tenant` 就把空 owner 视为非法（fail-closed，其钉住用例禁用归一）。
//
// 未命中时经 OpenTenant 创建；parent 未装配、owner 非法或创建失败一律返回 nil
// （调用方按 400 fail-closed，**绝不回落父根**）。
func (c *TenantCache) TenantFor(owner string) *Tenant {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.cache[owner]; ok {
		return t
	}
	t, err := openTenant(c.parent, owner, c.cfg)
	if err != nil {
		return nil
	}
	c.cache[owner] = t
	return t
}

// Close 关闭**本缓存拥有的**租户子根（幂等）。
//
// **不关闭 parent**——parent 由创建者负责（装配层的卷集合/全局根有自己的 Close）。
// 关闭后缓存清空：再次 TenantFor 会重新创建（与既有「Close 后进程退出」的用法一致，
// 不影响正常生命周期）。
func (c *TenantCache) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for owner, t := range c.cache {
		if t != nil && t.Root() != nil {
			_ = t.Root().Close()
		}
		delete(c.cache, owner)
	}
	return nil
}
