// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package registry 是 pkg/volume 域下的**运行时卷集合**子包：持有装配完成后的卷视图
// （不可变卷描述 + 每卷已打开的根句柄 + 每卷容量池 + 卷上租户懒建缓存），并提供按卷名/
// 默认卷的查询与统一关闭。
//
// 边界（本包刻意不承担的部分）：
//   - **配置解析**不在本包：把服务端配置（*Config / *VolumeACLConfig）翻译成纯域类型的
//     resolveDefaultVolumeRoot / assembleVolumes / parseVolumeACL 留在 pkg/server——
//     搬进来要连配置类型一起搬，且会造成 registry → pkg/server 成环。
//   - **卷路由决策**不在本包：routeUpload / locateForRead / volumeTenant 等 (h *Handlers)
//     方法消费本包，但自身需要 Handlers 状态（配额 Scope、日志、配置热读），留在 pkg/server。
package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	neturl "net/url"
	"reflect"
	"strings"
	"sync"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// Set 是装配后的卷集合：配置元数据 + 每卷打开的根句柄 + 每卷容量池 + 外部卷句柄。
// 默认卷 = cfg.Volumes[0]（defaultName 记录）；globalRoot/globalPool 语义映射到默认卷，
// 保持既有 handler 不改（globalRoot 字段 = 默认卷根，见 RegisterRoutes 接线）。
type Set struct {
	// mu 守卫 volumes 切片与 external map 的**动态写**（Add/RemoveExternalVolume）与查询：
	// 装配后本应为只读，但用户卷（U1）需要在运行时注册/移除外部卷，故 volumes/external
	// 的读写经 mu 串行化（roots/pools/caches 不被动态写，沿用既有并发模型）。
	mu sync.RWMutex
	// volumes 是装配后不可变卷描述（声明序，默认卷在 [0]），可直接作为 pkg/volume
	// AllowedVolumes/OrderCandidates 的输入（T4 路由）。
	volumes []volume.Volume
	// roots 是 name → 打开的卷根句柄（含默认卷；Close 时统一关闭）。
	// 仅本地卷（Type==""/local）在此；外部卷在 external。
	roots map[string]*storage.Root
	// external 是 name → 外部卷句柄（V3 通用卷模型）：非本地后端（如 baidupcs）的
	// 运行时句柄，FS() 提供同步视图供 syncexec 工厂消费；Close 时统一关闭。
	external map[string]ExternalBackend
	// storageVolumes 是 name → 统一存储卷身份（本地 localBackend / 外部 capacity.Backend）。
	// **U1 统一存储卷模型**（docs/designs/2026-10-28-unified-volume-model.md）：装配期一次
	// 构造，`StorageVolume(name)` 是唯一取卷入口——本地卷不再只能经 `Root` 拿裸 root。
	// 与 roots/external 的关系：本地卷同时进 roots（服务默认卷 tenant/globalRoot 存量语义）
	// 与 storageVolumes（统一身份）；外部卷进 external 与 storageVolumes（同一实例）。
	storageVolumes map[string]StorageVolume
	// viewDecorator 是 VolumeView 的**装配期注入装饰器**（U2）：对裸卷 FS 按 use 套完整
	// 装饰链（封印 → trusted.Guard/Wrap → capacity → staging 由消费方）。registry 不 import
	// server（避免成环），由装配层（pkg/server）注入。nil = 返回裸 FS（测试/无装饰装配）。
	viewDecorator VolumeViewDecorator
	// pools 是 name → 卷容量池。Capacity<=0 仍建池（上限 0 = 不限量），便于统一入账与
	// T4 的 used(name) 活用量闭包（OrderCandidates spread）。外部卷同样建池（入账统一）。
	pools map[string]*quota.Pool
	// defaultName 是默认卷名（cfg.Volumes[0].Name）。恒等于 Default().Name（构造方由
	// assembleVolumes 保证：i==0 时取 volumes[0]）；跨包调用方一律走既有 Default().Name，
	// 故本字段不导出。
	defaultName string
	// firstExternal 是首个登记的**外部卷**卷名（AddExternalVolume 首次成功时记录，Remove
	// 移除后按 volumes 声明序重算下一位）：DefaultExternal 按它返回默认外部卷——装配序决定
	// 默认（早期问题 P5），不再依赖 Go map 的随机迭代序。与 external 同临界区（mu 守卫）。
	firstExternal string
	// caches 是「非默认卷名 → 该卷上的租户缓存」：每卷一个 storage.TenantCache，
	// 键为 owner（卷维度由 map 的键表达，避免跨卷句柄混用）。默认卷租户由装配层的
	// 默认卷缓存单一持有，不在此缓存（避免同路径双句柄）。
	//
	// 懒创建：第一次对某卷取租户时才建该卷的 cache；cacheMu 串行化 map 读写（cache
	// 自身的懒创建与失败关闭由 storage.TenantCache 内部加锁）。
	caches  map[string]*storage.TenantCache
	cacheMu sync.Mutex
}

// NewSet 由装配层已解码的卷集合构造运行时卷视图。
// 参数与顺序刻意与 Set 的字段一一对应（互斥量与懒建缓存由构造处自建，不作为入参），
// 使调用方（pkg/server 的 assembleVolumes）只需把装配产物交给构造处，其余逻辑不受影响。
// NewSet 由装配层已解码的卷集合构造运行时卷视图。
// 外部卷句柄**同步进统一存储卷表**（U1）：`external[name]` 与 `storageVolumes[name]`
// 指向同一实例（`capacity.Backend` 内嵌 `ExternalBackend` 并实现 `StorageVolume` 四方法）。
func NewSet(
	volumes []volume.Volume,
	roots map[string]*storage.Root,
	external map[string]ExternalBackend,
	pools map[string]*quota.Pool,
	defaultName string,
) *Set {
	if external == nil {
		external = map[string]ExternalBackend{}
	}
	storageVolumes := make(map[string]StorageVolume, len(external))
	for name, be := range external {
		// 外部卷句柄可能是 capacity.Backend（已实现 StorageVolume）或裸 ExternalBackend
		// （缺 Kind）——统一用 storageVolumeOf 提升（含卷名）。名称已知（map 键）✓。
		storageVolumes[name] = storageVolumeOf{name: name, ExternalBackend: be}
	}
	// 默认外部卷按**声明序**确定（首个有 external 句柄的卷）——NewSet 生产构造路径
	// 不能留空 firstExternal，否则 DefaultExternal 回落到 map 随机迭代序，同一进程内
	// 多次调用结果不一致（违装配序确定性契约；P2 对抗评审）。
	firstExternal := ""
	for _, v := range volumes {
		if _, ok := external[v.Name]; ok {
			firstExternal = v.Name
			break
		}
	}
	return &Set{
		volumes:        volumes,
		roots:          roots,
		external:       external,
		pools:          pools,
		defaultName:    defaultName,
		firstExternal:  firstExternal,
		caches:         map[string]*storage.TenantCache{},
		storageVolumes: storageVolumes,
	}
}

// isNilStorageVolume 判定 StorageVolume 接口是否为 nil（含 typed-nil：持有 nil 具体指针）。
func isNilStorageVolume(sv StorageVolume) bool {
	if sv == nil {
		return true
	}
	v := reflect.ValueOf(sv)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}

// RegisterStorageVolume 登记统一存储卷身份（U1）。
//
// 装配期调用：本地卷由装配层经 `NewLocalBackend` 构造后登记；外部卷在 NewSet /
// AddExternalVolume / AttachExternal 时同步登记（同一实例进 external 与 storageVolumes）。
// 重名登记 → 明确错误（卷名唯一，防同卷双持有）。
func (vs *Set) RegisterStorageVolume(sv StorageVolume) error {
	// typed-nil 防御（独立复审 Important-2，2026-10-28）：接口持有 nil 具体指针（如
	// `var lb *localBackend = nil; set.RegisterStorageVolume(lb)`）时 `sv == nil` 判不成立，
	// 此后 StorageVolume(name) 返回非 nil 接口、sv.FS() 在 nil 接收者上行为未定义。反射探测。
	if isNilStorageVolume(sv) {
		return fmt.Errorf("registry: 登记统一存储卷为 nil（含 typed-nil）")
	}
	if sv.Name() == "" {
		return fmt.Errorf("registry: 统一存储卷名不能为空")
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if _, dup := vs.storageVolumes[sv.Name()]; dup {
		return fmt.Errorf("registry: 统一存储卷 %q 重复登记", sv.Name())
	}
	vs.storageVolumes[sv.Name()] = sv
	return nil
}

// StorageVolume 返回指定卷名的统一存储卷身份（本地/外部同形）。
// 未知卷名 → (nil, false)（fail-closed，不返回伪成功）。
func (vs *Set) StorageVolume(name string) (StorageVolume, bool) {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	sv, ok := vs.storageVolumes[name]
	return sv, ok
}

// Default 返回默认卷描述（volumes[0]）。
func (vs *Set) Default() volume.Volume {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return volume.DefaultVolume(vs.volumes)
}

// All 返回全部装配卷的**副本**（声明序，默认卷在 [0]）。返回副本防调用方改写内部底层数组
// 造成别名污染（volume.Volume 是值类型，切片头复制即隔离；append 到副本不影响 vs.volumes）。
func (vs *Set) All() []volume.Volume {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return append([]volume.Volume(nil), vs.volumes...)
}

// ByName 按卷名查找装配卷描述。
func (vs *Set) ByName(name string) (volume.Volume, bool) {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	for _, v := range vs.volumes {
		if v.Name == name {
			return v, true
		}
	}
	return volume.Volume{}, false
}

// DefaultRoot 返回默认卷的根句柄（nil = 未装配/空集合）。
func (vs *Set) DefaultRoot() *storage.Root {
	return vs.roots[vs.defaultName]
}

// Root 返回指定卷名的根句柄（未知卷名返回 nil）。
// 外部卷（非本地）不在 roots——调用方应走 External(name)。
// Root 返回本地卷根（**仅本地卷**；外部/封装卷无本地根 ⇒ nil）。
//
// ⚠️ **装配层专用**（M3）：调用方应优先用 `HasLocalRoot`（探针）或统一存储卷视图
// （`StorageVolume`/`FS`）；直接用本函数拿裸 root 的点由 **R28 棘轮**登记（只降不升，
// 每条需写「为什么必需 root 而非 FS/接口」）✓。
func (vs *Set) Root(name string) *storage.Root {
	return vs.roots[name]
}

// LocalRoots 返回本地卷根的快照（卷名 → 根句柄）。
//
// 用途：装配层为本地卷接线**结构性变异观察者**（设计 2026-10-28-volume-state-maintenance
// §4.1）——观察点只认卷根，故装配层需要一次性遍历全部本地根。返回副本 ⇒ 调用方可自由迭代
// （运行期新增卷由创建点自行接线）。
func (vs *Set) LocalRoots() map[string]*storage.Root {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	out := make(map[string]*storage.Root, len(vs.roots))
	for name, root := range vs.roots {
		if root != nil {
			out[name] = root
		}
	}
	return out
}

// HasLocalRoot 报告该卷是否为**本地根形态**（有 `*storage.Root`；外部/封装卷为 false）。
//
// 用途（M3）：取代 `Root(name) != nil` / `== nil` 这类**间接探针**（把「是否本地卷」的语义
// 显式化 ✓，并减少裸 root 访问器面 ✓）。与 `KindOf` 的区别：`KindOf` 分「存储/封装」；
// 本函数分「**本地根 / 非本地根**」（外部卷也是 `KindStorage` 但无本地 root ✓）。
func (vs *Set) HasLocalRoot(name string) bool {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return vs.roots[name] != nil
}

// DefaultExternal 返回默认外部卷句柄（**配置声明序中首个 AddExternalVolume 登记的外部卷**；
// 无外部卷返回 nil）。用途：URL 解析的空/"default" authority 的协议默认寻址目标。
//
// **不存在「默认卷」的特化角色**（2026-10-15 per-owner 默认桶重构后）：`secrets://` 的空/
// `default`/`default-secrets` authority 在 `secretdata` 侧由 `secrets.IsDefaultVolumeRef`
// **前置改道** per-owner 桶（cmd/sproxy.secret_register.defaultSecretdataSecret），不流到此处
// ⇒ 本函数只服务「未指定卷名」的一般协议寻址，其默认值纯由声明序决定（原
// `ensureDefaultSecretsVolume` 「先注册即默认 secrets 卷」的模型已随该重构删除）。
// firstExternal 显式记录首个登记卷名，不再依赖 map 随机迭代序（早期问题 P5）；
// firstExternal 指向的卷被移除后按 volumes 声明序重算，保证多次调用恒返回同一（装配序首个
// 存活的）外部卷。
func (vs *Set) DefaultExternal() ExternalBackend {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	if vs.firstExternal != "" {
		if be := vs.external[vs.firstExternal]; be != nil {
			return be
		}
	}
	// 声明序重算（确定性；AddExternalVolume 会 append 到 volumes，故覆盖全部已登记卷）。
	for _, v := range vs.volumes {
		if be := vs.external[v.Name]; be != nil {
			return be
		}
	}
	return nil
}

// External 返回指定卷名的外部卷句柄（未知卷名/非外部卷返回 nil）。
func (vs *Set) External(name string) ExternalBackend {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return vs.external[name]
}

// ResolveURL 按 URL 寻址内容：`scheme://<卷名>/<路径>`。
//
// 解析步骤：
//  1. 按 scheme 查协议声明表（RegisterBackend 的 protocols 参数注册）→ 确认协议有效；
//     未知 scheme → 明确错误。同一 scheme 冲突在注册期已 panic（见 RegisterBackend）。
//  2. authority（u.Host）即**卷名** → Set.External(卷名) 取该卷实例。
//  3. 断言实例实现 URLResolver → OpenURL 返回内容。
//
// 通用层设计（不提及具体后端类型）：协议表由各后端经 RegisterBackend(protocols)
// 声明，本方法只做「scheme 校验 + 按卷名取实例 + 透传 URL」，不解析 scheme 语义。
// 新增卷后端只需声明协议 + 实现 URLResolver 即自动可被寻址。
//
// fail-closed：未知 scheme / 卷未装配 / 卷实例未实现 URLResolver → 明确错误
// （绝不静默返回 nil）。
func (vs *Set) ResolveURL(ctx context.Context, url string) (io.ReadCloser, error) {
	u, err := neturl.Parse(url)
	if err != nil {
		return nil, fmt.Errorf("registry: ResolveURL 解析 %q 失败: %w", url, err)
	}
	scheme := strings.ToLower(u.Scheme)
	backendMu.RLock()
	if _, ok := schemeBackends[scheme]; !ok {
		backendMu.RUnlock()
		return nil, fmt.Errorf("registry: 无后端声明协议 %q（需 RegisterBackend(type, factory, %q)）", scheme, scheme)
	}
	backendMu.RUnlock()

	vol := u.Host
	if vol == "" && u.Opaque != "" {
		if before, _, ok := strings.Cut(u.Opaque, "/"); ok {
			vol = before
		}
	}
	// U4：`local://<卷>/<rel>` 本地卷协议寻址（统一卷模型）——不经 URLResolver（本地后端
	// 只实现 FS），直接经统一访问器 `VolumeView(name, ViewUser)` 读。路径安全与 owner
	// 前缀由 `volume.ResolveUserLocation` 唯一入口承担（与读路径同键空间）。
	if scheme == "local" {
		return vs.openLocalURL(ctx, vol, u.Path)
	}
	// 空 authority 或 "default" → 默认外部卷（协议默认寻址目标；卷名是装配细节）。
	be := vs.External(vol)
	if (vol == "" || vol == "default") && be == nil {
		be = vs.DefaultExternal()
	}
	if be == nil {
		return nil, fmt.Errorf("registry: ResolveURL %q 的卷 %q 未装配", url, vol)
	}
	ur, ok := be.(URLResolver)
	if !ok {
		return nil, fmt.Errorf("registry: ResolveURL %q 的卷 %q 未实现 URLResolver（地址 %q 不可寻址）", url, vol, scheme)
	}
	rc, err := ur.OpenURL(ctx, url)
	if err != nil {
		if errors.Is(err, syncpkg.ErrUnsupported) {
			return nil, fmt.Errorf("registry: ResolveURL %q 的卷 %q 未实现 URLResolver（地址 %q 不可寻址）", url, vol, scheme)
		}
		return nil, fmt.Errorf("registry: ResolveURL %q 失败: %w", url, err)
	}
	return rc, nil
}

// openLocalURL 解析 `local://<卷>/<rel>`（U4）：经 `VolumeView(name, ViewUser)` 读本地卷。
//
// 路径语义：rel 是**卷内 user 桶相对路径**（`<rel>`，owner 前缀由卷层统一承担）——与转存
// 目标键空间一致（`volume.ResolveUserLocation`）。路径穿越/非法段 → fail-closed 明确错误。
func (vs *Set) openLocalURL(ctx context.Context, volName, path string) (io.ReadCloser, error) {
	fs, ok := vs.VolumeView(volName, ViewUser)
	if !ok || fs == nil {
		return nil, fmt.Errorf("registry: local:// 卷 %q 未装配或不可读", volName)
	}
	rel := strings.TrimPrefix(path, "/")
	if rel == "" {
		return nil, fmt.Errorf("registry: local:// 缺少路径（%s://%s/）", "local", volName)
	}
	// path 是**卷内 user 桶相对路径**（与转存 URL 的桶内形态一致：转存产物键
	// `<owner>/user/<path>` 的 `<path>` 部分；URL 里**不含** owner/user 前缀段）。
	// 读键 = FSPath(ResolveUserLocation("", path)) —— 与转存产物键空间对齐
	// （Ruling，2026-10-28：openLocalURL 首版直接读 rel 导致键空间双向不一致）。
	// 路径安全由 ResolveUserLocation 唯一入口承担（../ 逃逸/绝对路径/非法段 fail-closed）。
	v, vok := vs.ByName(volName)
	if !vok {
		return nil, fmt.Errorf("registry: local:// 卷 %q 元信息不可解析", volName)
	}
	loc, lerr := v.ResolveUserLocation("", volume.Path(rel))
	if lerr != nil {
		return nil, fmt.Errorf("registry: local:// 路径非法 %q: %w", rel, lerr)
	}
	key := loc.FSPath()
	rc, err := fs.OpenRead(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("registry: local:// 读 %s://%s/%s: %w", "local", volName, key, err)
	}
	return rc, nil
}

// SetPool 登记/替换某卷的容量池（运行期新增封装卷「配额继承源卷」用）：装配层以
// `quota.NewPoolWithParent(有效源卷池, max_bytes)` 建池后经本方法登记，使 `Pool(name)`
// 恒返回该继承池（提交进自身池即沿父链聚合到源卷池，无需任何委托分支）。
//
// nil 池合法：清空该卷的池登记（删封装卷时回收）。
func (vs *Set) SetPool(name string, p *quota.Pool) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if p == nil {
		delete(vs.pools, name)
		return
	}
	vs.pools[name] = p
}

// ReplaceExternal 替换已登记卷的外部后端句柄（**不** Close 旧句柄——由调用方按语义决定：
// 封印装饰变化时旧句柄只是被包了一层，其底层资源仍由新句柄持有）。未登记的卷名 → 明确错误。
//
// 用途：运行期新增/删除封装声明时，源卷的封印装饰必须随之更新（`enclosure.ApplyBackendSeals`
// 产出新句柄）；外部/远端源卷没有 storage.Root 可就地改封印，只能换句柄。
func (vs *Set) ReplaceExternal(name string, be ExternalBackend) error {
	if be == nil {
		return fmt.Errorf("registry: 替换外部卷 %q 的句柄为 nil", name)
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if _, ok := vs.external[name]; !ok {
		return fmt.Errorf("registry: 外部卷 %q 不存在（无法替换句柄）", name)
	}
	vs.external[name] = be
	// Minor-2（独立复审）：替换句柄须同步统一存储卷表——否则 VolumeView/StorageVolume
	// 仍取旧实例（「VolumeView 读旧、External 读新」分叉窗口）。
	vs.storageVolumes[name] = storageVolumeOf{name: name, ExternalBackend: be}
	return nil
}

// AddExternalVolume 在运行时注册外部卷（用户卷，U1）：追加卷元数据 + 外部句柄。
//
// 重名（volumes 中已有同名卷——本地或已注册外部卷）→ 明确错误（防卷名歧义：
// ByName/External 按名查表，同名双持有会破坏唯一性）。
//
// 不触碰 roots/pools（容量 Pool 由装配层/调用方建；动态卷池入账见 U3）。
// mu 串行化 volumes/external 的写与 All/ByName/External 的读（并发安全）。
func (vs *Set) AddExternalVolume(v volume.Volume, be ExternalBackend) error {
	if v.Name == "" {
		return fmt.Errorf("registry: 外部卷名不能为空")
	}
	if be == nil {
		return fmt.Errorf("registry: 外部卷 %q 句柄为 nil", v.Name)
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	for _, vv := range vs.volumes {
		if vv.Name == v.Name {
			return fmt.Errorf("registry: 外部卷 %q 已存在（卷名必须唯一）", v.Name)
		}
	}
	vs.volumes = append(vs.volumes, v)
	vs.external[v.Name] = be
	vs.storageVolumes[v.Name] = storageVolumeOf{name: v.Name, ExternalBackend: be} // U1：外部卷同实例进统一存储卷表
	if vs.firstExternal == "" {
		// 首个登记的外部卷即默认外部卷（装配序决定默认，早期问题 P5）；
		// 首次记录后不覆盖（后续注册不得改写默认）。
		vs.firstExternal = v.Name
	}
	return nil
}

// RemoveExternalVolume 移除运行时注册的外部卷（用户卷，U1）：Close 后端句柄 + 删 external
// 条目 + 删 volumes 元数据。
//
// 移除不存在的卷 → 明确错误（fail-closed：静默 no-op 会掩盖调用方的卷名笔误）。
// 容量 Pool 保留（不再使用，Ruling-U1：Remove 不触碰 pools）。
func (vs *Set) RemoveExternalVolume(name string) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	be, ok := vs.external[name]
	if !ok {
		return fmt.Errorf("registry: 外部卷 %q 不存在（无法移除）", name)
	}
	if be != nil {
		_ = be.Close()
	}
	delete(vs.external, name)
	delete(vs.storageVolumes, name) // U1：移除外部卷同步清理统一存储卷表
	for i, v := range vs.volumes {
		if v.Name == name {
			vs.volumes = append(vs.volumes[:i], vs.volumes[i+1:]...)
			break
		}
	}
	// 移除的是当前默认外部卷 → 按 volumes 声明序重算下一位（保持 DefaultExternal 装配序
	// 确定性，防止回落 map 随机迭代；无存活外部卷则清空）。
	if name == vs.firstExternal {
		vs.firstExternal = ""
		for _, v := range vs.volumes {
			if _, ok := vs.external[v.Name]; ok {
				vs.firstExternal = v.Name
				break
			}
		}
	}
	return nil
}

// AttachExternal 在卷已登记（NewSet/AddExternalVolume 已含卷元数据与容量池）时补装外部
// 后端句柄（Imp-2 装配时序修复专用）：推迟装配的类型（如 secretdata）在 assembleVolumes
// 中只登记卷描述 + pool、不调 registry.NewBackend（构造依赖已装配卷集），set 就绪后由
// 装配层用本方法把构造好的后端挂到已存在的卷名下。不做重名校验（卷名必已存在——否则
// external 会指向无卷描述的后端）；重复补装同一卷 → 明确错误（fail-closed）。
func (vs *Set) AttachExternal(name string, be ExternalBackend) error {
	if name == "" {
		return fmt.Errorf("registry: 补装外部卷名不能为空")
	}
	if be == nil {
		return fmt.Errorf("registry: 补装外部卷 %q 句柄为 nil", name)
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if vs.external[name] != nil {
		return fmt.Errorf("registry: 外部卷 %q 已装配（重复补装）", name)
	}
	vs.external[name] = be
	vs.storageVolumes[name] = storageVolumeOf{name: name, ExternalBackend: be} // U1：补装外部卷同实例进统一存储卷表
	if vs.firstExternal == "" {
		// 首个补装的外部卷即默认外部卷（与 AddExternalVolume 语义一致：装配序决定默认）。
		vs.firstExternal = name
	}
	return nil
}

// Pool 返回指定卷名的容量池（未知卷名返回 nil）。
//
// 封装卷（enclosure）的池由装配层以 `quota.NewPoolWithParent(有效源卷池, max_bytes)` 建好后
// 经 `SetPool` 登记（阶段 2 迁移：取代旧的「底层池占用子目录 Scope」委托形态）——消费方
// （reserveVolume/路由排序/metrics/reconcile 等）经本方法统一拿池，TryReserve/Adjust/释放自动
// 沿父链聚合到源卷池，整体不超源卷配额。
//
// 持读锁读取（`SetPool` 允许运行期登记继承池，与高频读并发）。
func (vs *Set) Pool(name string) *quota.Pool {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return vs.pools[name]
}

// Close 关闭全部卷上租户子根、卷根句柄与外部卷句柄（幂等：重复调用安全，nil/已关跳过）。
// 顺序：先关各卷的租户子根（缓存持有），再关卷根与外部卷。
func (vs *Set) Close() error {
	vs.cacheMu.Lock()
	for name, c := range vs.caches {
		_ = c.Close()
		delete(vs.caches, name)
	}
	vs.cacheMu.Unlock()
	for name, rt := range vs.roots {
		if rt != nil {
			_ = rt.Close()
		}
		delete(vs.roots, name)
	}
	vs.mu.Lock()
	for name, be := range vs.external {
		if be != nil {
			_ = be.Close()
		}
		delete(vs.external, name)
	}
	vs.mu.Unlock()
	return nil
}

// Tenant 返回指定卷上 owner 的租户（懒建缓存）。未知卷名/非法 owner/根不可用返回 nil
// （fail-closed）。物理位置 = <卷根>/<owner>/（与默认卷 tenantFor 布局同构；meta 桶仅在默认
// 卷权威，非默认卷不预建 meta）。默认卷（vs.defaultName）不在此缓存——调用方应走
// server.tenantFor(owner)（默认卷缓存单一持有），避免同路径双句柄。
//
// 创建骨架与失败关闭语义单源在 pkg/storage（OpenTenant/TenantCache）：本方法只负责
// 「按卷取/建 cache」。log 仅在**首次为某卷创建 cache 时**生效（与既有「首次创建才记日志」
// 一致）；卷名以 `volume` 字段记入每条告警。
func (vs *Set) Tenant(volName, owner string, log *slog.Logger) *storage.Tenant {
	if !vs.HasLocalRoot(volName) {
		return nil
	}
	vs.cacheMu.Lock()
	c, ok := vs.caches[volName]
	if !ok {
		c = storage.NewTenantCache(vs.roots[volName],
			storage.WithLogger(log), storage.WithLogAttrs("volume", volName))
		vs.caches[volName] = c
	}
	vs.cacheMu.Unlock()
	return c.TenantFor(owner)
}
