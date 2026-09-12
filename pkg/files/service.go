// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package files 是文件服务领域根：承载文件服务的 HTTP 处理器（按能力族分文件），
// 以及它们唯一的依赖接缝 Deps。
//
// # 接缝形态（本工作后续各片一律遵守）
//
// 领域包只 import 下层能力包的**顶层包**（pkg/pathguard、pkg/checksum、pkg/storage、
// pkg/quota、pkg/volume）；**不得 import 子包**（如 pkg/volume/registry、
// pkg/storage/capacity）也**不得 import pkg/server**：
//
//   - 子包由门禁 R2（子包可见性）保护——只允许父域子树与装配层导入。跨域消费一律走
//     「消费者定义接口」（见 VolumeSet：领域包声明所需能力，装配层注入真实实现）；
//   - pkg/server 是装配层，反向依赖它既违反层级方向（门禁规则③：未登记包）也会成环。
//
// 因此凡是「只有装配层才知道」的东西（配置、日志器、租户/配额/校验和台账的懒建缓存、
// 装配后的卷集合、请求主体），一律经 Deps 以**窄函数/窄接口**取用——绝不把 pkg/server
// 的类型（*Config / *Metrics / *Handlers…）放进接缝。
//
// # 组织单位是「文件」，不是「包」（R34 / P1）
//
// 本包**不再往下切子包**：分块族（会话存储 + init/chunk/status/complete + 分块下载）与目录族
// 平铺在同一领域包内，按**文件**组织（chunked_store.go / chunked_upload.go /
// chunked_download.go / chunked_response.go / dirs.go）。
//
// 判据（P6）：子包**只应是** ① 可复用的扩展工具集合，或 ② 真正的子领域。
// 「某个功能的处理器 + 它的存储」**不属于任何一类**——强行拆包会把父域读/写面的能力
// （卷路由、读定位、版本备份…）成批推上接缝（实测：拆包形态下 19 个接缝字段里 12 个
// 是处理器需要、存储 0 个），既没有换来边界，也把「同族必然一起改」的代码拆到两个包。
//
// # 依赖方向与护栏
//
// 本包只 import 下层能力的**顶层包**（pkg/pathguard、pkg/checksum、pkg/storage、pkg/quota、
// pkg/volume）；**不得 import 子包**（pkg/volume/registry、pkg/storage/capacity 等——门禁
// R2 只允许其父域子树与装配层导入），也**不得 import pkg/server**（装配层，反向依赖既违反
// 层级方向、门禁规则③也会成环）。跨域消费一律走「消费者定义接口」（见 VolumeSet /
// StorageManager：领域包声明所需能力，装配层注入结构上满足的真实实现）。
//
// 因此凡是「只有装配层才知道」的东西（配置、日志器、租户/配额/校验和台账的懒建缓存、
// 装配后的卷集合、容量账本、锁池、卷路由与读定位、审计），一律经 Deps 以**窄函数/窄接口**
// 取用——绝不把 pkg/server 的类型（*Config / *Metrics / *Handlers…）放进接缝。
//
// # 接缝项的两种形状（判据）
//
//  1. **取用函数（getter）**——装配层会在**运行期替换**这同一个状态，快照会让领域包
//     读到旧值。形状：`func() T`。实例：`Logger`（PUT /api/config 会就地重建 `h.logger`）
//     与各配置项（`ChunkSize` / `VersioningEnabled` / …，配置可被改写）。
//  2. **快照值（snapshot）**——**构造后不再变更**的装配产物（`VolSet`、`StorageManager`）
//     或稳定绑定（方法值 `h.tenantFor` / 包级函数值 `ownerFromRequest`）。形状：字段直持。
//
// 方法值属快照值类，但要注意被快照的是**绑定**而非**数据**：`h.tenantFor` 的方法值
// 绑定的是 `h`，其函数体每次读 `h` 的实时字段（懒建缓存 map），故缓存内容的变化对
// 领域包可见。只有「装配层会把字段本身换成另一个值」时才需要形状 1。
//
// **窄接口字段的赋值必须先判 nil**（typed-nil）：把 nil 的具体指针赋给接口字段会得到
// **非 nil 接口**，使领域包的 `== nil`（未装配路径）判断失效。装配层写法固定为
// `if x != nil { deps.Field = x }`。
//
// # DTO 与响应写出
//
// 本包自带 HTTP 契约 DTO（`chunked_response.go`）与 `sendJSON`：`pkg/server` 侧另有同名外壳
// （通用 `UploadResponse` 被 cloud/auth/share 等 400+ 处使用，不属本域），两侧 JSON 形状
// 由 `pkg/server/response_drift_test.go` 与 `pkg/server/chunked_wire_drift_test.go` 逐字节守卫。
//
// # 跨族共享的纯函数
//
// `atomicRenameRoot`、`fileChecksumRoot`、`verifyFileWithChecksumRoot`、`checksumReader`、
// `drainAndVerifyBody`（五个都在本文件末尾，按此顺序）与 `formatContentDisposition`
// （在 `chunked_response.go`）在 `pkg/server` 侧另有多个消费者，
// 既不能随本族从那边删走、本包也无法 import `pkg/server`（规则③）。故本包持**语义等价的
// 本地实现**，逐条注明对应实现，并由 `pkg/server` 的源码级等价断言守卫 `atomicRenameRoot`
// （Windows 退避重试语义分叉不会被任何行为测试发现）。
//
// # 构造函数规则
//
// 构造期**全量校验**缺项并 fail-fast（见 `NewService`）：缺项只在对应族的请求路径上才炸
// （表现为 nil 解引用），越早暴露越好。**例外有四**：① 有缺省值的字段（`Logger` 回落
// `slog.Default()`）；② **nil 具有合法语义**的字段（`VolSet` = 未装配卷集合/单卷零回归；
// `StorageManager`/`Metrics` = 未装配该能力，跳过对应路径）。**注意**：把 nil 语义字段改成
// 取用函数 `func() VolumeSet` **修不好** typed-nil——`func() VolumeSet { return h.volSet }`
// 在 `h.volSet == nil` 时照样返回非 nil 接口，除非访问器内部自带 nil 判断。
package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// VolumeSet 是文件服务域需要的**运行时卷集合**能力（消费者定义接口）：枚举卷（含默认卷，
// 声明序）、按卷名取卷根（存在性探测）、按卷名取容量池（删除后释放）。
//
// 为什么是接口而不是直接 import `*registry.Set`：门禁 R2（子包可见性）规定
// pkg/volume/registry 只允许 pkg/volume 子树与装配层导入——pkg/files 是**另一个领域**，
// 不得直接依赖它（这也是 R2 存在的意义：跨域消费走能力接口，而非伸进对方内部）。
// 装配层的 `*registry.Set` 结构上满足本接口，无需任何适配代码。
//
// **typed-nil 陷阱**：nil 的具体指针装入本接口会得到非 nil 接口——装配层必须先判 nil
// 再赋值（约定第 4 条，见本文件包文档），否则 `deps.VolSet == nil`（单卷路径）判断失效。
type VolumeSet interface {
	All() []volume.Volume
	Root(name string) *storage.Root
	Pool(name string) *quota.Pool
}

// StorageManager 是本域需要的**容量核算**能力（P5 回退预留路径：quota 未装配时按字节
// 预留/释放，超限拒绝上传）。
//
// 为什么是接口而不是直接 import `*capacity.StorageManager`：`pkg/storage/capacity` 是
// `pkg/storage` 的**子包**，门禁规则②不允许本包直接 import（跨域消费走能力接口）。
//
// **类别由装配层适配器固定**（`capacity.CategoryChunked`），故方法名带 Chunked 而非 category
// 形参——领域包不得重复该类别常量（重复即两处定义、可漂移）。
//
// **typed-nil 陷阱**：nil 的具体指针装入本接口会得到非 nil 接口，使 `deps.StorageManager != nil`
// （P5 回退路径闸门）判断失效。装配层必须先判 nil 再赋值。
type StorageManager interface {
	TryReserveChunked(bytes int64) error
	ReleaseChunked(bytes int64)
	Usage() int64
	MaxBytes() int64
}

// Metrics 是本域需要的计量能力（分块下载成功写出后记传输字节）。nil = 未装配计量。
//
// **typed-nil 陷阱**同 StorageManager：装配层须判 nil 后赋值。
type Metrics interface {
	RecordDownload(bytes int64)
}

// DownloadPath 是 `ResolveDownloadPath` 的解析结果：目标租户 + 租户根内相对路径 + 用户可见名。
// 所有下载 kind（普通 / cloud_task / cloud_archive）都被装配层解析为同一形状后交进来。
type DownloadPath struct {
	// Filename 是用户可见文件名（Content-Disposition / 日志用）。
	Filename string
	// Tenant 是文件所属租户（经 Tenant.Root() 打开，os.Root 防符号链接逃逸）。
	Tenant *storage.Tenant
	// Rel 是租户根内相对路径（如 user/dir/f.txt、cloud/<taskID>/<file>、archive/<name>）。
	Rel string
}

// FileLocation 是 `LocateOwnerFile` 的定位结果：目标文件所在卷名（空 = 默认卷）与该卷租户。
type FileLocation struct {
	VolumeName string
	Tenant     *storage.Tenant
}

// UploadRoute 是 `RouteUpload` 的结果：目标卷租户 + owner 全局 Scope 预留 + 卷容量池预留
// （双账本，AD-7；未装配卷集合/配额时对应字段为 nil）。
type UploadRoute struct {
	VolumeName string
	Tenant     *storage.Tenant
	// ScopeRes 是 owner 全局/user 桶 Scope 预留（双账本之一）。
	ScopeRes *quota.Reservation
	// Pool/PoolRes 是卷容量池预留（双账本之二，未装配卷集合时为 nil）。
	Pool    *quota.Pool
	PoolRes *quota.Reservation
	// Release 双回滚（预留全额归还），供路由后失败路径调用。
	Release func()
}

// HTTPError 是「带 HTTP 状态码的失败原因」（用户可见文案 + 状态码），由装配层把 pkg/server
// 的对应错误类型映射而来：卷路由拒绝（403/409/507）与下载路径解析失败（400/404）。
// 本域按它原样回包；非本类型按各调用点的兜底状态码处理（卷路由 500 / 下载 400）。
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string { return e.Message }

// Deps 是文件服务领域根的依赖接缝：**只放必须由装配层（pkg/server）注入的装配项**。
// 下层能力（路径校验、存储根/租户、配额池、卷集合、校验和台账的类型）直接 import
// 使用，不经接缝——接缝越小，包边界越清楚。
//
// 每项的形状（取用函数 / 快照值）见各字段注释首行标注；判据见包文档「接缝项的两种形状」。
type Deps struct {
	// Logger 【形状 1：取用函数】返回当前生效的业务日志器。必须是取用函数：pkg/server
	// 在日志配置热更新（PUT /api/config）时就地替换其 logger 字段，快照会让领域包一直
	// 写旧 handler（级别/格式不再生效）。缺省（nil）回落 slog.Default()。
	Logger func() *slog.Logger

	// ActorFromRequest 【形状 2：快照值（包级函数值）】返回请求的操作主体（未认证返回 ""）。
	// 必须注入：主体由 pkg/server 的认证中间件写入请求 ctx，其 ctx key 是 pkg/server 的
	// 内部实现，领域包无从读取。
	ActorFromRequest func(*http.Request) string

	// VolSet 【形状 2：快照值】是装配后的运行时卷集合。必须注入：配置解析与卷根打开都
	// 留在 pkg/server，装配产物才交进来。**nil 是合法语义**（未装配卷功能的旧装配路径，
	// 单卷零回归），故不参与构造期缺项校验；装配层仍需按约定第 4 条判 nil 后赋值
	// （typed-nil）。快照语义的边界：`Handlers.Close()` 会把 `h.volSet` 置 nil，此后
	// 领域包仍看得到旧集合——只影响「已关闭后仍来请求」的不可服务区间。
	VolSet VolumeSet

	// TenantFor 【形状 2：快照值（方法值）】返回 owner 在默认卷上的租户（懒创建缓存，
	// 空 owner → anonymous）。必须注入：租户句柄的懒建与缓存是装配层状态（pkg/server 的
	// tenantRoots），领域包只消费，不持有第二份句柄。
	TenantFor func(owner string) *storage.Tenant

	// VolumeTenant 【形状 2：快照值（方法值）】返回指定卷上 owner 的租户（默认卷委托
	// TenantFor）。必须注入：非默认卷的租户懒建缓存由卷集合持有（registry.Set.Tenant），
	// 领域包不自建缓存（否则同路径双句柄）。
	VolumeTenant func(volName, owner string) *storage.Tenant

	// QuotaScopeFor 【形状 2：快照值（方法值）】按文件相对路径（含功能桶前缀，如
	// "user/dir/f.txt"）解析最长前缀配额子 Scope；首段不是已配置功能桶时返回 nil。
	// 必须注入：Scope 树按 owner 懒建并缓存在装配层（pkg/server 的 quotaBuckets），
	// 其层级/上限语义是装配期配置。
	//
	// 可复用性：`version` 等**固定桶**取 Scope 无需新字段，直接 `QuotaScopeFor(owner, "version")`
	// 即可——pkg/server 的未导出方法 `quotaBucketFor`（handlers.go）与之逻辑等价
	// （实测唯一差异是形参名与一句注释）。
	QuotaScopeFor func(owner, rel string) *quota.Scope

	// ChecksumStoreFor 【形状 2：快照值（方法值）】返回 owner 的 per-tenant 校验和台账
	// （懒创建）。必须注入：台账按 owner 懒建并缓存在装配层（pkg/server 的 checksumStores）。
	ChecksumStoreFor func(owner string) *checksum.ChecksumStore

	// ChunkSize 【形状 1：取用函数】返回配置的分块大小（cfg.ChunkSize；<=0 时由本域回落
	// 默认分块大小）。配置可被改写，故取用而不快照。
	ChunkSize func() int64

	// VersioningEnabled 【形状 1：取用函数】返回是否启用文件版本管理（cfg.Versioning.Enabled）。
	// 它决定分块 init 遇到同名但 checksum 不同的文件时是"视为覆盖"还是 409。
	VersioningEnabled func() bool

	// UploadStoreFor 【形状 2：快照值（方法值）】返回 owner 的 per-tenant 分块上传存储
	// （懒创建并缓存在装配层）。必须注入：store 由装配层的租户句柄、卷根映射、容量回退预留
	// 目标与 session TTL 共同构造，且装配层另有两处生命周期消费（启动预建 anonymous store、
	// Close 时逐个 Stop）——领域包自建第二份缓存会产生同租户双 store（双写会话目录）。
	UploadStoreFor func(owner string) *UploadStore

	// StorageManager 【形状 2：快照值（装配产物）】**nil 合法**（未装配容量管理时无 P5 回退
	// 预留路径）。必须注入：容量核算是跨族共享账本（cloud/sync/stats 同样记账），
	// 领域包不得持第二份。
	StorageManager StorageManager

	// Uploading 【形状 2：快照值】是装配层的文件级互斥锁池（键 `<归一 owner>` + NUL + `<rel>`）。
	// 必须注入：它是**跨族共享**的非阻塞锁池——单次上传、跨卷 move、delete/版本 restore 与
	// 分块 init/complete 共用同一键空间，领域包持第二份会让互斥失效（move 与 complete 并发
	// 落双份）。本域直接读写（init 以 upload_id 为值，complete 经 AcquireFileLock 取 txn 标记）。
	Uploading *sync.Map

	// Metrics 【形状 2：快照值】计量器。**nil 合法**（未装配计量时跳过记录）。
	Metrics Metrics

	// ResolveDownloadPath 【形状 2：快照值（方法值）】把分块下载请求解析为
	// (租户, 根内相对路径, 用户可见名)。必须注入：解析要覆盖 kind 白名单（普通 / cloud_task /
	// cloud_archive），其中 cloud_task 分支需要云任务管理器的归属校验与任务状态——那是装配层
	// 持有的跨族能力，领域包无从触及。
	ResolveDownloadPath func(r *http.Request) (DownloadPath, error)

	// LocateOwnerFile 【形状 2：快照值（方法值）】在 owner 的卷视图内定位 rel 所在卷
	// （读定位，不创建目录）。必须注入：这是**多族共享的单一实现**（分块状态查询、下载、
	// 列表、stat 都用它），且依赖卷 ACL 收紧语义；领域包重写第二份会与读面产生定位分歧。
	LocateOwnerFile func(owner, rel string) (FileLocation, bool)

	// RouteUpload 【形状 2：快照值（方法值）】为写路径定卷并预留双账本
	// （owner 全局 Scope + 卷容量池）。必须注入：这是**写面共享的单一实现**（单次上传与
	// 分块上传共用），含卷 ACL/唯一性/配额语义；领域包重写第二份会让两条写路径的路由规则分叉。
	RouteUpload func(owner, rel, explicitVol string, size int64, forceHomeVol string) (UploadRoute, error)

	// SaveVersion 【形状 2：快照值（方法值）】把目标卷上的现有文件备份进版本桶，返回新版本字节数。
	// 必须注入：版本存储属**另一个能力族**（与单次上传覆盖写共用同一实现），其存储布局与
	// 本域强绑定，不宜在此重复实现。
	SaveVersion func(userRel string, tnt *storage.Tenant, owner string) (int64, error)

	// AcquireFileLock 【形状 2：快照值（方法值）】为 owner 的 rel 取文件级排他锁（非阻塞），
	// 返回释放函数与是否取到。必须注入：它与单次上传 / 跨卷 move 共用锁池（`Uploading`），
	// 是 complete 与 move 互斥的唯一手段（否则 move 删源后 complete 仍可把文件落回源卷，
	// 与目标卷副本并存）。
	AcquireFileLock func(owner, rel string) (release func(), ok bool)

	// RecordOverwriteAudit 【形状 2：快照值（方法值）】记录一次「覆盖写」审计（装配层固定
	// Action=overwrite / ObjectType=file / Result=success 与详情文案，与单次上传覆盖写写法一致）。
	// 必须注入：审计 logger 与环形缓冲由装配层持有，且审计事件的 actor/mesh 取自 pkg/server
	// 写入 ctx 的认证信息（ctx key 是该包内部实现），领域包无从构造。
	RecordOverwriteAudit func(ctx context.Context, filename string)
}

// Service 是文件服务领域实例：持有接缝 Deps，承载各能力族的 HTTP 处理器。
//
// 并发安全**来自下层能力**（装配层的 tenantMu 串行化懒建、checksum 台账自带互斥、
// 配额池自带锁），不来自本类型；本类型自身不做共享可变状态（deps 构造后只读），
// 故同一实例可被并发请求使用。
type Service struct {
	deps Deps
}

// NewService 构造文件服务领域实例。
//
// **全量校验**（见包文档「构造函数规则」）：缺项即 panic 并列出缺失字段名。Deps 各项都是
// 领域运行的必要能力，缺项只在对应族的请求路径上才炸（nil 解引用），故在构造期 fail-fast。
// **四个例外**（不参与校验）：`Logger` 有缺省值（回落 slog.Default()）；`VolSet` /
// `StorageManager` / `Metrics` 的 **nil 是合法语义**（未装配该能力 = 跳过对应路径）。
func NewService(deps Deps) *Service {
	if deps.Logger == nil {
		deps.Logger = slog.Default
	}
	missing := make([]string, 0, len(requiredDeps))
	for _, item := range requiredDeps {
		if item.check(&deps) {
			continue
		}
		missing = append(missing, item.name)
	}
	if len(missing) > 0 {
		panic("files.NewService: Deps 缺少必须注入的装配项: " + strings.Join(missing, ", "))
	}
	return &Service{deps: deps}
}

// requiredDeps 列出必须注入（缺省即 panic）的接缝项及其存在性判定。
// 四个例外（Logger 有缺省、VolSet/StorageManager/Metrics 的 nil 是语义）不在此表内。
var requiredDeps = []struct {
	name  string
	check func(*Deps) bool
}{
	{"ActorFromRequest", func(d *Deps) bool { return d.ActorFromRequest != nil }},
	{"TenantFor", func(d *Deps) bool { return d.TenantFor != nil }},
	{"VolumeTenant", func(d *Deps) bool { return d.VolumeTenant != nil }},
	{"QuotaScopeFor", func(d *Deps) bool { return d.QuotaScopeFor != nil }},
	{"ChecksumStoreFor", func(d *Deps) bool { return d.ChecksumStoreFor != nil }},
	{"ChunkSize", func(d *Deps) bool { return d.ChunkSize != nil }},
	{"VersioningEnabled", func(d *Deps) bool { return d.VersioningEnabled != nil }},
	{"UploadStoreFor", func(d *Deps) bool { return d.UploadStoreFor != nil }},
	{"Uploading", func(d *Deps) bool { return d.Uploading != nil }},
	{"ResolveDownloadPath", func(d *Deps) bool { return d.ResolveDownloadPath != nil }},
	{"LocateOwnerFile", func(d *Deps) bool { return d.LocateOwnerFile != nil }},
	{"RouteUpload", func(d *Deps) bool { return d.RouteUpload != nil }},
	{"SaveVersion", func(d *Deps) bool { return d.SaveVersion != nil }},
	{"AcquireFileLock", func(d *Deps) bool { return d.AcquireFileLock != nil }},
	{"RecordOverwriteAudit", func(d *Deps) bool { return d.RecordOverwriteAudit != nil }},
}

// anonymousOwner 是未认证请求的默认租户名（结构与其他租户完全同构）。
// 与 pkg/server 的 anonymousOwner 同值：租户名是存储布局契约（<root>/<owner>/…）。
// 两侧一致性由 `pkg/server/response_drift_test.go` 与 `pkg/files` 侧的同名断言双向守卫。
const anonymousOwner = "anonymous"

// normalizeOwner 把空 owner 归一为 anonymous 租户名（未认证请求的默认租户）。
func normalizeOwner(owner string) string {
	if owner == "" {
		return anonymousOwner
	}
	return owner
}

// UploadResponse 是文件服务的通用 JSON 响应外壳。
//
// JSON 形状与 pkg/server.UploadResponse 逐字一致（字段名/顺序/omitempty 相同）：迁移期
// 文件面响应体不得变化，pkg/server 侧的其余端点仍用自己的同名类型。**两份定义会被
// `pkg/server/response_drift_test.go` 逐字节守卫**（反射比对 tag + 三样本 Marshal 比对），
// 任一侧改动都会立即变红。
type UploadResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Checksum string `json:"file_checksum,omitempty"`
}

// sendJSON 写出 JSON 响应：Content-Type、状态码与「序列化失败」兜底（500 +
// {"error":"internal server error"} + Warn 日志）与 pkg/server.sendJSONResponse 一致，
// 只是日志器改从接缝取当前生效实例。
func (s *Service) sendJSON(w http.ResponseWriter, response any, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	buf, err := json.Marshal(response)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		s.deps.Logger().Warn("Encode JSON response failed", "error", err)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "internal server error"})
		return
	}
	w.WriteHeader(statusCode)
	_, _ = w.Write(buf)
}

// 本文件是**跨族共享纯函数**在本包内的等价实现。它们在 pkg/server 侧各自另有消费者
// （读面/写面/版本族），既不能随本族迁走、本包也无法 import pkg/server（规则③），
// 按接缝判据（纯计算不进接缝）只能下沉为本地实现。逐条注明对应实现与等价依据。

// checksumReader 计算 src 的 SHA-256 十六进制摘要（小写）。
// 对应 pkg/server.Checksum：同为 sha256 + hex.EncodeToString；缓冲区大小只影响拷贝次数、
// 不影响摘要值。注意本函数会完全消耗 src，调用方负责关闭实现 io.Closer 的入参。
func checksumReader(src io.Reader) (string, error) {
	dst := sha256.New()
	if _, err := io.CopyBuffer(dst, src, make([]byte, 256*1024)); err != nil {
		return "", err
	}
	return hex.EncodeToString(dst.Sum(nil)), nil
}

// fileChecksumRoot 计算 storage.Root 内相对路径文件的 SHA-256 十六进制摘要。
// 全程 root 内打开，防符号链接逃逸。对应 pkg/server.FileChecksumRoot（语义等价：
// 同经 root.Open 打开后哈希整文件）。
func fileChecksumRoot(root *storage.Root, rel string) (string, error) {
	f, err := root.Open(rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return checksumReader(f)
}

// verifyFileWithChecksumRoot 验证 storage.Root 内相对路径文件的 SHA-256 checksum。
// 对应 pkg/server.verifyFileWithChecksumRoot：**先 root.Open**（打开失败即不匹配，即使
// expected 为空），打开成功后才做 `expected == ""` 空值短路（与 pkg/server.verifyChecksum
// 的分支顺序逐字一致）。顺序刻意对齐——两处实现的行为必须可逐句对照。
func verifyFileWithChecksumRoot(root *storage.Root, rel, expectedChecksum string) bool {
	f, err := root.Open(rel)
	if err != nil {
		return false
	}
	defer f.Close()
	if expectedChecksum == "" {
		return true
	}
	actual, err := checksumReader(f)
	if err != nil {
		return false
	}
	return actual == expectedChecksum
}

// atomicRenameRoot 在 storage.Root 内原子重命名 srcRel → dstRel。
// 对应 pkg/server.atomicRenameRoot（语义等价）：快速路径直接 Rename，失败（Windows 并发
// 场景）先删除目标再重命名，并使用短退避重试以应对 Windows 句柄释放延迟。
//
// 该函数在 pkg/server 侧另有 3 个消费者（单次上传 / rename / 跨卷 move），故两侧各留一份；
// **两份实现的 Windows 退避语义必须保持一致**——由 `pkg/server/helper_impl_drift_test.go`
// 的源码级等价断言守卫（重试次数 / 退避基数 / 调用次序；行为测试走不到慢速路径）。
func atomicRenameRoot(root *storage.Root, srcRel, dstRel string) error {
	// 快速路径：直接重命名
	if err := root.Rename(srcRel, dstRel); err == nil {
		return nil
	}
	// 慢速路径：删除目标文件，然后重命名临时文件
	// 使用短退避重试，解决 Windows 上并发 Rename 导致的"Access is denied"
	const maxAttempts = 5
	const baseDelay = 2 * time.Millisecond
	for i := range maxAttempts {
		_ = root.Remove(dstRel)
		if err := root.Rename(srcRel, dstRel); err == nil {
			return nil
		} else if i == maxAttempts-1 {
			return fmt.Errorf("重命名失败（已达最大重试次数 %d）: %w", maxAttempts, err)
		}
		time.Sleep(baseDelay << i)
	}
	return nil
}

// drainAndVerifyBody 强制消费请求体剩余部分，触发 SproxySig bodyValidator 的 EOF 哈希比对。
// json.Decoder / ParseMultipartForm 读到自身需要的数据后即返回、不读到 EOF，导致 bodyValidator
// 的哈希比对永不触发；此处兜底读完整个 body——body 被篡改（哈希不匹配）时返回错误，
// 调用方应在响应前拒绝（400）。
//
// 对应 pkg/server.drainAndVerifyBody（该函数在 pkg/server 侧另有 14 个消费者）：
// 实现完全相同（io.Copy 到 io.Discard，返回其错误）。
func drainAndVerifyBody(r *http.Request) error {
	_, err := io.Copy(io.Discard, r.Body)
	return err
}
