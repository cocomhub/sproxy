// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"fmt"
	"io"
	"sync"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// ExternalBackend 是外部卷（非本地文件系统卷）的运行时句柄。
//
// 与本地卷的 storage.Root 对应：本地卷由装配层 OpenRoot 建根句柄，外部卷由
// backend 构造器返回本接口。FS 是外部卷的同步视图（sync.FS），供 syncexec
// 工厂按 remote.volume 查询后直接驱动同步引擎；Close 释放后端资源（连接/临时目录）。
type ExternalBackend interface {
	// FS 返回外部卷的同步视图（sync.FS）。装配后只读（多次调用返回同一实例）。
	FS() syncpkg.FS
	// Close 释放后端资源（幂等：重复调用安全）。
	Close() error
}

// Presigner 是可选能力接口：后端支持预签名 URL（当前 s3 实现）。
// 装配/API 层用类型断言检测；不支持的后端调用方回 405。
type Presigner interface {
	// PresignedURL 生成对象级预签名 URL（PUT 直传 / GET 下载）。
	// relPath 是卷内相对路径；method 为 PUT/GET；expires 秒（≤0 = 后端默认）。
	PresignedURL(ctx context.Context, relPath, method string, expires int64) (string, error)
}

// URLResolver 是可选能力接口：后端支持用 URL 寻址内容（scheme://卷/路径）。
// 与 Presigner/VolumeStatsProvider 同模式——registry 通用层**不提及任何具体后端
// 类型**（secrets/secretdata/pikpak/s3 是各自后端的实现者，本接口仅定义能力）。
//
// 后端在注册期经 RegisterBackend 的 protocols 参数声明支持的 scheme（如 "secrets"），
// 同一 scheme 被多个类型声明 → 注册期 panic（协议冲突装配期 fail-fast）。因此
// ResolveURL 无需逐后端扫描 Supports，直接按 scheme 查表定位类型。
//
// 遵循 RFC 3986 语义：URL 为 `scheme://authority/path`。authority（卷名）与 path
// （secret 名）由实现方按各自 scheme 解析。

// IndexRefresher 是可选能力接口（S5，2026-10-27）：后端支持**显式重建内存索引**。
//
// 背景（docs/designs/2026-10-27-remote-derived-state-cache.md §6 裁定 B）：secretdata 的
// 内存索引只在挂载时扫描一次；卷被**本进程之外**修改（用户用网盘 App 直改 / 另一实例
// 写入）后幽灵条目/旧哈希永不收敛。本接口让装配/API 层**类型断言**探测——支持则暴露
// 显式 refresh 入口（`sclient volume refresh <卷>`），不支持的后端调用方明确拒绝。
//
// 与 Presigner 同模式：registry 通用层不提及具体后端类型，实现方（secretdata backend）
// 自证能力。
type IndexRefresher interface {
	// RefreshIndex 重扫底层并原子替换内存索引；失败保持旧索引不动（fail-closed，
	// 绝不让瞬时故障把卷变成空视图）。
	RefreshIndex(ctx context.Context) error
}

// 不支持/未知 URL → 实现方必须 fail-closed（返回错误，绝不静默返回 nil）。
type URLResolver interface {
	// OpenURL 解析 URL 并返回其内容（io.ReadCloser；调用方负责 Close）。
	OpenURL(ctx context.Context, url string) (io.ReadCloser, error)
}

// BackendFactory 按卷描述构造外部后端（从 v.Extra 读类型特有配置）。
// 返回的 ExternalBackend 由装配层持有（并入 registry.Set），随 Set.Close 统一关闭。
type BackendFactory func(ctx context.Context, v volume.Volume) (ExternalBackend, error)

// VolumeStats 是外部卷当前容量情况（C1 外部卷容量纳管）。
//
// 两层容量语义：
//   - TotalBytes：外部卷**总量**（backend 级查询：baidupcs 配额 / S3 bucket 用量；
//     0 = 后端不支持查询总量）。
//   - UsedBytes：外部卷**已用量**（同一 backend 查询；0 = 未知）。
//
// 本系统可用限额（UserVolume.Capacity）与记账不在此结构——那是卷级计数（C2）。
type VolumeStats struct {
	TotalBytes int64
	UsedBytes  int64
}

// VolumeStatsProvider 是 ExternalBackend 的**可选**扩展：提供外部卷当前容量情况。
//
// 为什么是可选接口：WebDAV 无标准用量查询 API（PROPFIND 遍历全卷成本高）→ 不实现，
// 查询 API 对该类卷仅展示「本系统限额」维度；baidupcs（配额 API）/ S3（bucket 用量）
// 实现本接口。断言失败（未实现）→ 查询方按「无总量信息」处理（不失败）。
type VolumeStatsProvider interface {
	// Stats 返回卷当前容量情况。nil Stats + nil err = 后端不支持/无数据（与未实现
	// 同语义）；错误 = 查询失败（调用方告警而非 fail-closed——容量展示非关键路径）。
	Stats(ctx context.Context) (*VolumeStats, error)
}

// UsageProvider 是 ExternalBackend 的**可选**扩展：提供本系统可用限额与已用字节
// （C2 卷级计数记账查询）。装配层包 CapacityFS 时实现（capacityBackend）；未实现
// → 查询方按「无限额/无计数」处理（兼容未装配计数的旧路径）。
type UsageProvider interface {
	// Usage 返回本系统已占用该卷的字节（写入累计 - 删除释放）。
	Usage() int64
	// Capacity 返回本系统可用限额（0 = 不限制）。
	Capacity() int64
}

// HealthProbe 是 ExternalBackend 的**可选**扩展：提供外部卷健康探测（roadmap 3.3 P1
// 后端健康探针：不可达 → 卷状态 degraded 可观测）。
//
// 为什么是可选接口：本地卷无远端可探（恒 healthy）；WebDAV 可经 PROPFIND 探但成本高
// （默认不实现 → 状态 unknown 不降级）；SFTP/baidupcs 等长连接后端实现（拨号/握手探测）。
// 未实现（断言失败）→ 查询方按「unknown」处理（不误报 degraded，也不假装 healthy）。
//
// 「Probe」是健康检查的标准角色名词（探针），强改 -er
// （Pinger/HealthPinger）并不更符合 Go 惯用；-er 约定面向动词型方法名，此处保留角色名词。
type HealthProbe interface { // NOSONAR: S8196 — 「Probe」是健康检查标准角色名词（探针），-er 约定面向动词型方法名
	// Ping 探测后端可用性。nil = 可用（healthy）；错误 = 不可达（degraded）。
	Ping(ctx context.Context) error
}

// backendFactories 是后端类型 → 构造器注册表（可插拔）。
// 由各后端包（或装配层）经 RegisterBackend 注册；NewBackend 按 v.Type 分派。
// backendMu 串行化读写（注册发生在装配期，查询在执行期，跨 goroutine；RWMutex 保并发安全）。
var (
	backendMu        sync.RWMutex
	backendFactories = map[string]BackendFactory{}
	// schemeBackends 是协议（scheme）→ 后端类型的声明表：由 RegisterBackend 的 protocols
	// 参数注册。同一 scheme 被多个类型声明 → panic（协议冲突装配期 fail-fast）。
	// ResolveURL 按此表直接定位类型，无需逐后端扫描 Supports。
	schemeBackends = map[string]string{}
	// deferredBackends 是「推迟装配」的外部卷类型（Imp-2 时序修复）：此类后端构造依赖
	// 已装配卷集（如 secretdata 经 secret_url 读 secrets 卷密钥），须在卷集合就绪后由
	// 装配层统一补装。assembleVolumes 遇此类类型跳过 registry.NewBackend（不再因
	// secretDataSet 未就绪而装配失败），卷集返回后由 cmd/sproxy attachSecretdataVolumes
	// 逐个补装（含 config 声明的 secretdata/secrets 卷；分「先行/后置」两轮以解除声明顺序限制）。
	deferredBackends = map[string]bool{}
	// staticSchemas 是后端类型的**静态**创建表单 schema 表（RegisterBackendSchema 登记；
	// backendMu 同临界区守卫）。静态表不依赖构造后端实例——生产 wrapper 类型
	// （secretdata/secrets/egress）的构造依赖已装配卷集/密钥（空 Extra 直接失败），
	// 无法经「构造读 SchemaProvider」得到 schema，故装配期登记静态表（BackendSchemas /
	// BackendSchema 优先读静态表，未登记才回落构造读 SchemaProvider）。
	staticSchemas = map[string][]FieldSchema{}
	// wrapperTypes 是**动态登记**的 wrapper category 类型集合（MarkWrapperType）：封装处理层
	// 注册表（enclosure.RegisterWrapper）在注册时单向登记——「哪些类型是 wrapper」的事实源
	// 是处理层注册表本身，而 registry 不能反向 import enclosure（成环），故用登记入口解耦。
	// 与 backendCategory 的静态基线表（secrets/secretdata/egress）合并使用：静态表覆盖
	// 「尚未/不会注册处理层」的既有类型（如 egress），登记表覆盖新注册的处理层。
	wrapperTypes = map[string]bool{}
)

// MarkDeferredType 把后端类型标记为「推迟装配」：assembleVolumes 对 config 声明该类型
// 的卷跳过 registry.NewBackend（构造器依赖已装配卷集，此时 secretDataSet 尚未就绪），
// 由装配层在卷集合就绪后统一 AddExternalVolume 补装。重复标记幂等。仅生产 secret
// 加密卷装配使用（cmd/sproxy secret_register.go）。
func MarkDeferredType(typ string) {
	if typ == "" {
		panic("registry: 空类型不可标记推迟装配")
	}
	backendMu.Lock()
	defer backendMu.Unlock()
	deferredBackends[typ] = true
}

// IsDeferredType 报告类型是否标记为推迟装配（assembleVolumes 据此跳过）。
func IsDeferredType(typ string) bool {
	backendMu.RLock()
	defer backendMu.RUnlock()
	return deferredBackends[typ]
}

// UnmarkDeferredTypeForTest 移除测试注册的推迟装配标记（测试辅助：跨包测试注册 fake
// deferred 类型后清理，防污染共享标记表；镜像 UnregisterBackendForTest 语义）。
// 生产代码不得调用。
func UnmarkDeferredTypeForTest(typ string) {
	backendMu.Lock()
	defer backendMu.Unlock()
	delete(deferredBackends, typ)
}

// RegisterBackend 注册卷后端类型构造器（可插拔扩展）。
//
// protocols 是可选的后端支持的 URL scheme 列表（如 "secrets" → secrets:// 可寻址）。
// 声明后：ResolveURL 能按 scheme 定位本类型；同一 scheme 被多个类型声明 → panic
// （协议冲突应在装配期暴露，而非运行时静默选择）。缺省不传 protocols → 后端不可被
// URL 寻址（不影响非 URL 用法）。
//
// 重复注册同一类型 → panic（编程错误，仿标准库 Register 语义——重复注册意味着
// 两个包声明了同一类型的所有权，装配期应 fail-fast 暴露而非静默覆盖）。
// 空类型名 → panic（type 空串请先归一为 `volume.TypeLocal`）。
// **`volume.TypeLocal` 允许注册**（统一存储卷模型 M1b）：本地卷成为**普通后端**，装配层特例
// 分支（`Type == "" || TypeLocal`）随后删除。用户可创建面（建卷 API / 创建表单 / 预签名）
// 另由 `IsUserCreatableBackendType` 排除本地 ✓（注册 ≠ 用户可创建）。
func RegisterBackend(typ string, f BackendFactory, protocols ...string) {
	if typ == "" {
		panic(fmt.Sprintf("registry: 非法后端类型 %q（空串请归一为 %q）", typ, volume.TypeLocal))
	}
	if f == nil {
		panic(fmt.Sprintf("registry: 后端类型 %q 的构造器为 nil", typ))
	}
	backendMu.Lock()
	defer backendMu.Unlock()
	if _, dup := backendFactories[typ]; dup {
		panic(fmt.Sprintf("registry: 后端类型 %q 重复注册", typ))
	}
	backendFactories[typ] = f
	for _, sc := range protocols {
		if sc == "" {
			panic(fmt.Sprintf("registry: 后端类型 %q 声明空协议", typ))
		}
		if prev, dup := schemeBackends[sc]; dup {
			panic(fmt.Sprintf("registry: 协议 %q 被后端类型 %q 与 %q 同时声明（协议冲突，装配期 fail-fast）", sc, prev, typ))
		}
		schemeBackends[sc] = typ
	}
}

// NewBackend 按 v.Type 分派到已注册的后端构造器构造外部后端。
//
// 未注册的 Type → 明确错误（fail-closed，不回落 local——回落会静默把外部卷当本地
// 目录打开，产生错误数据位置）。v.Type 空串/"local" 应由装配层走本地卷路径，不应
// 调用本函数（防御：直接报错）。
// SchemeOf 反查卷类型的协议 scheme（首个声明）：转存 URL 生成用（scheme://卷/路径）。
// 卷未注册 / 未声明任何 protocol → 返回 ""（调用方 fail-closed）。
func SchemeOf(typ string) string {
	backendMu.RLock()
	defer backendMu.RUnlock()
	for sc, t := range schemeBackends {
		if t == typ {
			return sc
		}
	}
	return ""
}

func NewBackend(ctx context.Context, v volume.Volume) (ExternalBackend, error) {
	// 空串归一为 TypeLocal（与装配层「`Type == ""` 等义 local」一致）⇒ **统一分派**。
	// M1b 前这里硬拒 local（「不应经后端构造器分派」）；现在本地卷也是普通后端 ✓。
	if v.Type == "" {
		v.Type = volume.TypeLocal
	}
	typ := v.Type
	backendMu.RLock()
	f, ok := backendFactories[typ]
	backendMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("registry: 卷 %q 类型 %q 未注册后端（需 RegisterBackend 注册）", v.Name, typ)
	}
	be, err := f(ctx, v)
	if err != nil {
		return nil, fmt.Errorf("registry: 卷 %q 类型 %q 后端构造失败: %w", v.Name, typ, err)
	}
	if be == nil {
		return nil, fmt.Errorf("registry: 卷 %q 类型 %q 后端构造器返回 nil（装配错误）", v.Name, typ)
	}
	return be, nil
}

// BackendTypes 返回已注册后端类型列表（V4：backend 列表 API 数据源；Web/CLI 动态感知）。
//
// 顺序不承诺稳定（map 遍历）；调用方（列表 API/UI 下拉）不得依赖顺序。
// 返回副本（防调用方改写内部 map 键）；空注册表 → 空切片（非 nil，JSON 序列化为 []）。
func BackendTypes() []string {
	backendMu.RLock()
	defer backendMu.RUnlock()
	if len(backendFactories) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(backendFactories))
	for typ := range backendFactories {
		out = append(out, typ)
	}
	return out
}

// FieldSchema 声明后端创建表单的一个字段（前端 schema 驱动渲染）。
type FieldSchema struct {
	Key          string   `json:"key"`
	Label        string   `json:"label"`
	Type         string   `json:"type"` // text|volume-select|enum|bool|number
	Required     bool     `json:"required"`
	Options      []string `json:"options,omitempty"`
	AllowWrapper bool     `json:"allow_wrapper,omitempty"` // volume-select 是否可选封装卷底层
}

// SchemaProvider 是 ExternalBackend 的**可选**扩展：声明本后端的创建表单 schema。
//
// 与 VolumeStatsProvider/HealthProbe 同模式——后端实现它则以本 schema 驱动前端建卷表单
// （字段渲染；NewBackend 侧可用 schema 校验 Extra）。断言失败（未实现）不失败——调用方
// 按「无字段」处理，前端仅展示 {type, category}，表单只填 name。
type SchemaProvider interface {
	Schema() []FieldSchema
}

// BackendSchemaInfo 是 backend 列表 API 单个后端的 schema 条目（type → category + fields）。
type BackendSchemaInfo struct {
	Type     string        `json:"type"`
	Category string        `json:"category"`
	Label    string        `json:"label,omitempty"`
	Fields   []FieldSchema `json:"fields"`
}

// MarkWrapperType 把后端类型登记为 **wrapper category**（封装处理层语义）：由
// `enclosure.RegisterWrapper` 在注册处理层时调用——「哪些类型是 wrapper」的事实源是处理层
// 注册表，registry 只持有其投影（不能反向 import enclosure，见 wrapperTypes 注释）。
//
// 重复登记幂等；空类型名/本地类型 panic（与 RegisterBackend 同纪律：编程错误应装配期 fail-fast）。
func MarkWrapperType(typ string) {
	if typ == "" || typ == volume.TypeLocal {
		panic(fmt.Sprintf("registry: 非法 wrapper 类型 %q（空串与 %q 保留给本地卷）", typ, volume.TypeLocal))
	}
	backendMu.Lock()
	defer backendMu.Unlock()
	wrapperTypes[typ] = true
}

// UnmarkWrapperTypeForTest 移除测试登记的 wrapper category 标记（测试辅助：跨包测试经
// MarkWrapperType 登记 fake 处理层后清理，防全局永久登记污染共享投影表；镜像
// UnregisterBackendForTest / UnmarkDeferredTypeForTest 语义）。生产代码不得调用。
//
// 注册表内不会出现的保留值（空串/本地类型）无标记可清 → no-op（不 panic：清理路径不应反过来
// 制造故障）。
func UnmarkWrapperTypeForTest(typ string) {
	backendMu.Lock()
	defer backendMu.Unlock()
	delete(wrapperTypes, typ)
}

// backendCategory 由后端类型推导 schema category：
//   - "secrets"/"secretdata"/"egress"（基线封装/密钥卷）或**已登记的处理层类型**
//     （MarkWrapperType，见 wrapperTypes）→ "wrapper"；
//   - 本地卷 → "mt-local"（`local` 现已可注册（M1b）⇒ 本分支不再是死分支；但 `BackendSchemas`
//     排除本地 ⇒ 建卷表单清单仍不含它 ✓）；
//   - 其余外部类型 → "linked"。
func backendCategory(typ string) string {
	backendMu.RLock()
	isWrapper := isWrapperCategoryLocked(typ)
	backendMu.RUnlock()
	if isWrapper {
		return "wrapper"
	}
	switch typ {
	case "", volume.TypeLocal:
		return "mt-local"
	default:
		return "linked"
	}
}

// isWrapperCategoryLocked 判定 wrapper category（调用方须持 backendMu）。
func isWrapperCategoryLocked(typ string) bool {
	switch typ {
	case "secrets", "secretdata", "egress":
		return true // 静态基线：既有封装/密钥卷类型（egress 无处理层注册，恒走本分支）
	default:
		return wrapperTypes[typ]
	}
}

// backendSchema 尝试构造后端实例读取创建表单 schema（可选 SchemaProvider 断言）。
// 构造失败 / 后端 nil / 未实现 → 返回 nil（调用方回退空 fields）。构造仅用于读取静态
// schema（与 backendPresignHandler 同一「构造实例断言能力」模式）；读毕立即 Close 释放。
// 未注册类型经 NewBackend 返回错误 → 空 fields（不 fail-closed——schema 非关键路径）。
func backendSchema(ctx context.Context, typ string) []FieldSchema {
	be, err := NewBackend(ctx, volume.Volume{Type: typ})
	if err != nil || be == nil {
		return nil
	}
	defer be.Close()
	sp, ok := be.(SchemaProvider)
	if !ok {
		return nil
	}
	return sp.Schema()
}

// RegisterBackendSchema 登记后端类型的**静态**创建表单 schema（装配期；不依赖构造后端
// 实例）。生产 wrapper 类型（secretdata/secrets/egress）构造依赖已装配卷集/密钥，空 Extra
// 无法构造 → 无法经 SchemaProvider 读 schema，故在装配处登记静态表（防环校验依赖它）。
//
// 重复登记同一类型 → panic（仿 RegisterBackend：重复登记 = 两个装配点声明类型 schema
// 所有权，装配期应 fail-fast 而非静默覆盖）。空类型名 → panic。空字段列表 → panic
// （登记空 schema 无意义——该类型应按不登记处理，由调用方走构造回落或无字段）。
func RegisterBackendSchema(typ string, fields []FieldSchema) {
	if typ == "" || typ == volume.TypeLocal {
		panic(fmt.Sprintf("registry: 非法后端 schema 类型 %q（空串与 %q 保留给本地卷）", typ, volume.TypeLocal))
	}
	if len(fields) == 0 {
		panic(fmt.Sprintf("registry: 后端类型 %q 的静态 schema 为空（无意义登记）", typ))
	}
	backendMu.Lock()
	defer backendMu.Unlock()
	if _, dup := staticSchemas[typ]; dup {
		panic(fmt.Sprintf("registry: 后端类型 %q 的静态 schema 重复登记", typ))
	}
	staticSchemas[typ] = append([]FieldSchema(nil), fields...)
}

// UnregisterBackendSchemaForTest 移除测试登记的静态 schema（测试辅助：清理污染共享注册
// 表；镜像 UnregisterBackendForTest 语义）。生产代码不得调用。
func UnregisterBackendSchemaForTest(typ string) {
	backendMu.Lock()
	defer backendMu.Unlock()
	delete(staticSchemas, typ)
}

// CategoryOf 返回后端类型的 schema category（backendCategory：wrapper|linked|mt-local）。
// 导出供建卷校验兜底使用（wrapper 类型空 schema 时强制 target 校验）。
func CategoryOf(typ string) string {
	return backendCategory(typ)
}

// KindOf 返回卷的**结构性类型**（统一存储卷模型 M0/M1，设计 §11）。
//
// 判据**单源**在本注册表的 wrapper 事实源（`isWrapperCategoryLocked`：静态基线
// secrets/secretdata/egress + 处理层动态登记 `MarkWrapperType`），**不重复**任何
// 「哪些 type 是 wrapper」的清单 ✗：
//   - wrapper 类型 ⇒ `volume.KindWrapper`（视图层，**不注册配额层**）；
//   - 其余（含 `""`/`local`、baidupcs/s3/ftp/sftp/federated/cluster 等）⇒ `volume.KindStorage`
//     （承载真实字节，**需要配额层**）。
//
// 注意：本函数**不**额外校验 `Extra["source"]` —— 封装声明（wrapper 类型 **且** source 非空）
// 由 `enclosure.Parse` 在装配期校验（非法组合装配期即拒），故运行期类型分类只需按类型。
func KindOf(v volume.Volume) volume.Kind {
	backendMu.RLock()
	isWrapper := isWrapperCategoryLocked(v.Type)
	backendMu.RUnlock()
	if isWrapper {
		return volume.KindWrapper
	}
	return volume.KindStorage
}

// BackendSchema 返回单后端类型的创建表单 schema（副本；nil = 无字段）。
//
// 读取顺序：先查静态表（RegisterBackendSchema——构造无关、装配期登记，生产 wrapper 类型
// 走此路径）；未登记静态表 → 回落构造后端实例读 SchemaProvider（注册期即可读的类型，
// 如测试 fake）。构造失败/未实现 → nil。**不构造全注册表**（高频建卷校验调用此单类型
// 访问器，避免 BackendSchemas 的全量构造开销）。
func BackendSchema(typ string) []FieldSchema {
	backendMu.RLock()
	if flds, ok := staticSchemas[typ]; ok {
		backendMu.RUnlock()
		return append([]FieldSchema(nil), flds...)
	}
	backendMu.RUnlock()
	return backendSchema(context.Background(), typ)
}

// BackendSchemas 返回已注册后端的 type→(category, fields) 映射（V4 backend 列表 schema
// 驱动建卷表单）。category 由协议推导（backendCategory）；fields 优先读静态表
// （RegisterBackendSchema，构造无关），未登记的类型回落构造读 SchemaProvider——未实现者
// 为空数组（json:"fields" 无 omitempty，前端依赖该键恒为 []）。
// 顺序不承诺稳定（map 遍历）；调用方不得依赖顺序。空注册表 → 空切片（非 nil）。
func BackendSchemas() []BackendSchemaInfo {
	backendMu.RLock()
	types := make([]string, 0, len(backendFactories))
	for typ := range backendFactories {
		// **本地卷不出现在用户可创建类型清单**（M1b 零行为变更）：`local` 已注册为普通后端，
		// 但建卷表单/类型下拉只列**用户可创建**的外部/封装类型 ✓（见 IsUserCreatableBackendType）。
		if typ == volume.TypeLocal {
			continue
		}
		types = append(types, typ)
	}
	backendMu.RUnlock()

	out := make([]BackendSchemaInfo, 0, len(types))
	for _, typ := range types {
		info := BackendSchemaInfo{
			Type:     typ,
			Category: backendCategory(typ),
		}
		if flds := BackendSchema(typ); flds != nil {
			info.Fields = flds
		} else {
			info.Fields = []FieldSchema{}
		}
		out = append(out, info)
	}
	return out
}

// UnregisterBackendForTest 移除测试注册的后端（测试辅助：跨包测试（如 pkg/server）注册
// fake backend 后清理，防污染共享注册表）。生产代码不得调用。
func UnregisterBackendForTest(typ string) {
	backendMu.Lock()
	defer backendMu.Unlock()
	delete(backendFactories, typ)
	delete(staticSchemas, typ) // 同删静态 schema（防 stale 掩盖重注册类型的 schema）
	// 同删该类型声明的协议（schemeBackends：scheme→typ 反向清理），
	// 否则 -count=2/并发测试重注册同 scheme 触发协议冲突 panic（CI 复现）。
	for sc, t := range schemeBackends {
		if t == typ {
			delete(schemeBackends, sc)
		}
	}
}

// IsBackendRegistered 报告类型是否已注册后端工厂（供 API 区分「未注册」与「构造失败」——
// 此前一律映射 404「后端类型未注册」，缺配置的真实类型也误报未注册）。
//
// 语义是**字面真实**的「已注册」✓（`local` 注册后为 true ✓）。用户可创建面请用
// `IsUserCreatableBackendType`（本地卷不在其列 ✓）。
func IsBackendRegistered(typ string) bool {
	backendMu.RLock()
	defer backendMu.RUnlock()
	_, ok := backendFactories[typ]
	return ok
}

// IsUserCreatableBackendType 报告该类型是否为**用户可创建**的后端类型（建卷 API / 创建表单 /
// 预签名面）。
//
// **`local` 不是**：本地卷由**配置声明**（`volumes[]`，含 `Root`/加密 key）⇒ 不允许运行期
// 创建，否则调用方可指定任意 `RootDir` ⇒ **路径/权限面** ✗。空串同样不是（归一为 local）。
//
// 与 `IsBackendRegistered` 的差别是**语义**而非实现：前者问「能否由用户建卷」，后者问
// 「工厂是否已注册」（M1b 后 local 已注册 ⇒ 后者为 true ✓）。
func IsUserCreatableBackendType(typ string) bool {
	if typ == "" || typ == volume.TypeLocal {
		return false
	}
	return IsBackendRegistered(typ)
}
