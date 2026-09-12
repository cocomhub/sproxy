// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package files 是文件服务领域根：承载文件服务的 HTTP 处理器（按能力族分文件），
// 以及它们的能力接缝。
//
// # 构造入口
//
// 推荐 `New(tenants, opts...)`（见 options.go）：唯一必需项是租户解析，其余能力由 Option
// 注入接口，未注入的回落内建「最小可用」默认（单卷、无配额、无台账、无版本、无审计、
// 无计量、内建锁池）。领域代码只经 runtime 的 nil 安全访问器取用能力（见 runtime.go）。
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
// 装配后的卷集合、请求主体），一律经能力接口以**窄函数/窄接口**取用——绝不把 pkg/server
// 的类型（*Config / *Metrics / *Handlers…）放进接缝。
//
// # 组织单位是「文件」，不是「包」（R34 / P1）
//
// 本包**不再往下切子包**：只读面（列表 / 搜索 / 下载 / stat）、写面（单次上传 / 重命名 /
// 删除 / 批量）、分块族（会话存储 + init/chunk/status/complete + 分块下载）、目录族与版本族
// 的**存储侧**平铺在同一领域包内，按**文件**组织（read.go / write.go / rename.go /
// delete.go / chunked_store.go / chunked_upload.go / chunked_download.go /
// chunked_response.go / dirs.go / version_store.go）。
//
// 版本族的分工：`/api/versions` 的 HTTP 处理器（list/restore/delete）属**附属 API 面**，
// 留在装配层 pkg/server；本包承载其存储侧（version_store.go）。
//
// 只读面的分工：`/download` 与 `/api/files/stat` 的**路径解析**（kind 白名单 / 跨卷读定位 /
// 云任务归属校验）留在装配层，经能力 `DownloadPaths.Resolve` 交进来；处理器本身
// （ListFiles / SearchFiles / Download / Stat）在本包（read.go）。
//
// 写面的分工：`/upload`（含路径校验、并发互斥、重复检测/版本化覆盖、卷路由双账本）、
// `/rename`、`/delete` 与两个 `/api/batch/*` 的处理器全部在本包（write.go / rename.go /
// delete.go），装配层只留一行薄适配；**卷路由（VolumeRouter.Route）与文件级锁池的实现在装配层**，
// 经接缝交进来（领域包不重写第二份）。
//
// 判据（P6）：子包**只应是** ① 可复用的扩展工具集合，或 ② 真正的子领域。
// 「某个功能的处理器 + 它的存储」**不属于任何一类**——强行拆包会把父域读/写面的能力
// （卷路由、读定位、版本备份…）成批推上接缝（实测：拆包形态下 19 个接缝字段里 12 个
// 是处理器需要、存储 0 个；该实测取于写面迁入前），既没有换来边界，也把「同族必然一起改」
// 的代码拆到两个包。
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
// 装配后的卷集合、容量账本、锁池、卷路由与读定位、审计），一律经能力接口以**窄函数/窄接口**
// 取用——绝不把 pkg/server 的类型（*Config / *Metrics / *Handlers…）放进接缝。
//
// # 能力接口与 Option 构造（判据）
//
//  1. **唯一必需项放在编译期**：`New(tenants, opts...)` 的第一个参数（`TenantResolver`）
//     是「没有它文件服务无法工作」的能力（决定请求落到哪个存储根）；其余全部是 Option。
//  2. **默认即最小可用**：未注入的 Option 回落内建默认（单卷 `singleVolume`、无配额、
//     无台账、无版本、无审计、无计量、内建锁池、下载仅普通文件），零 Option 即可完成
//     单卷的列表/上传/下载/删除/改名/目录/批量。
//  3. **能力是接口**：`TenantResolver` / `ActorResolver` / `VolumeRouter` / `QuotaScopes` /
//     `ChecksumLedgers` / `DownloadPaths` / `FileLocks` / `ChunkedUploads` / `Versioning` /
//     `Auditor` / `Metrics`——方法每次调用读实时状态，故**不存在「取用函数 vs 快照值」的
//     形状歧义**，配置热更新天然可见（例：`Logger` 是 `func() *slog.Logger` 取用函数）。
//  4. **nil 语义由实现表达**：未装配的卷集合/容量/台账/配额在 runtime 访问器（runtime.go）
//     或装配层适配器（pkg/server 的 filesRuntime.Volumes/Capacity）内部判 nil 后返回
//     nil 接口，调用点按既有语义跳过；装配层**不需要**「nil 具体指针装入接口会变成非 nil
//     接口」的 typed-nil 守卫。
//  5. **配置原子化（C1+C2）**：配置跟随它所配置的能力——`ChunkedUploads` 自带容量回退
//     预留、`Versioning` 自带启停与保留上限；独立可调的纯配置项（`ChunkSize`）走
//     `WithChunkSize` 单独覆盖。
//
// # DTO 与响应写出
//
// HTTP 契约 DTO 按**族**分文件，定义在写出它的处理器所在处，响应统一经本包的 `sendJSON`
// （在 `service.go`）写出。三种形态（判据：`pkg/server` 侧是否存在同一契约的第二份定义）：
//
//  1. **单一事实源**——只读面列表契约 `FileInfo` / `ListResponse`（`read.go`）与写面的批量
//     契约 `BatchOperationResult` / `BatchResponse`（`service.go`）及批量请求体
//     （`BatchDeleteRequest` / `BatchDeleteFile` 在 `delete.go`，`BatchRenameRequest` /
//     `BatchRenameOp` 在 `rename.go`）：处理器已在本包，`pkg/server` 侧**没有**同名外壳
//     （其测试直接引用 `files.*`），故**不存在**跨侧漂移面，无需守卫。
//  2. **两份定义 + 逐字节守卫**——通用外壳 `UploadResponse`（`service.go`）：`pkg/server` 侧的
//     同名类型被 cloud/auth/share 等 400+ 处使用（不属本域，无法随本域删走），两侧 JSON 形状
//     由 `pkg/server/response_drift_test.go` 逐字节守卫（字段名 + tag + 序列化字节）。
//  3. **冻结表 + 三方守卫**——分块族 DTO（`chunked_response.go`）：与
//     `pkg/server/chunked_wire_drift_test.go` 的冻结表、SDK（`pkg/client`）及 Web UI 构造
//     三方对齐，防「服务端解析 ↔ 客户端构造」分叉。
//
// 注：批量族 DTO 与 SDK（`pkg/client` 的同名类型）构成**客户端侧第二份定义**，两侧由
// 各自的读写路径行为测试覆盖（本包无跨侧断言，与迁移前的 `pkg/server` ↔ SDK 关系相同）。
//
// # 跨族共享的纯函数
//
// `fileChecksumRoot`、`atomicRenameRoot`、`drainAndVerifyBody`、
// `defaultVolumeAllows`、`locateForRead`（五个都在本文件末尾，按此顺序）
// 与 `formatContentDisposition`
// （在 `chunked_response.go`）、`volumePoolForTenant`（在 `version_store.go`）、
// `volumeFileExists`（在 `read.go`）、`copyWithContext`（在 `write.go`）
// 在 `pkg/server` 侧另有消费者，
// 既不能随本族从那边删走、本包也无法 import `pkg/server`（规则③）。故本包持**语义等价的
// 本地实现**，逐条注明对应实现，并由 `pkg/server` 的源码级等价断言守卫
// `atomicRenameRoot`、`volumePoolForTenant`、`volumeFileExists`、`copyWithContext`、
// `defaultVolumeAllows`、`locateForRead`、`drainAndVerifyBody`、`formatContentDisposition`、
// `normalizeOwner` 与两个 `*ChecksumRoot` 包装。
//
// `checksumReader` **不属**此类：它已改为委托 L0 顶层包 `checksum.Reader`（单一事实源），
// 不再与 `pkg/server.Checksum` 各持一份 sha256 实现；守卫改判为「两侧都必须委托
// `checksum.Reader`」（重新内联本地实现即红）。
//
// `verifyFileWithChecksumRoot`（同为本文件末尾）**不属于**此类：写面迁入后其消费者全部在
// 本包，pkg/server 侧的同名实现已随之删除 ⇒ 单一事实源。
//
// # 构造函数规则
//
// `New` 只强制**唯一必需项**（`TenantResolver`）；其余能力未注入即回落内建默认，不再有
// 「缺一即 panic」的全量表。默认与降级分三类：① 有缺省值（`Logger` 回落 `slog.Default()`、
// 分块大小回落 internal/size 默认）；② 有单卷默认实现（`VolumeRouter` 由 `TenantResolver`
// 派生）；③ 未装配即 nil（配额/台账/审计/计量/分块），调用点按既有语义跳过。
package files

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// VolumeSet 是文件服务域需要的**运行时卷集合**能力（消费者定义接口）：枚举卷（含默认卷，
// 声明序）、取默认卷（ACL 回落护栏与默认卷名解析）、按卷名取卷（`?volume=` 列表过滤用，
// `ByName` 返回的 Volume 与 `All()` 同源）、按卷名取卷根（存在性探测）、按卷名取容量池
// （删除后释放）。
//
// 为什么是接口而不是直接 import `*registry.Set`：门禁 R2（子包可见性）规定
// pkg/volume/registry 只允许 pkg/volume 子树与装配层导入——pkg/files 是**另一个领域**，
// 不得直接依赖它（这也是 R2 存在的意义：跨域消费走能力接口，而非伸进对方内部）。
// 装配层的 `*registry.Set` 结构上满足本接口，无需任何适配代码。
//
// **接口宽度只随域内实际消费增长**（不是「装配层有什么就搬什么」）：`ByName` 由只读面的
// `?volume=` 过滤引入（ListFiles），`Default` 由写面的默认卷 ACL 护栏引入
// （defaultVolumeAllows / locateForRead，rename 与 delete 用）；两者取的都是**同一个已注入
// 对象**上的既有方法，不新增接缝字段、也无需任何适配代码。
//
// **nil 语义**：未装配卷集合时 runtime 访问器返回 nil（见 runtime.go），装配层适配器只需
// 在方法内判 nil 后返回 nil 接口（`filesRuntime.Volumes()`），无需逐字段 typed-nil 守卫。
type VolumeSet interface {
	Default() volume.Volume
	All() []volume.Volume
	ByName(name string) (volume.Volume, bool)
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

// Metrics 是本域需要的计量能力（分块下载成功写出后记传输字节；写面记上传字节/文件数与
// 删除数）。nil = 未装配计量。
//
// 方法集只随**域内实际消费**增长：`RecordUpload` / `RecordDelete` 由单次上传与删除族引入
// （`RecordDownload` 由分块下载引入）——三者都是装配层 `*Metrics` 上的既有方法，加宽接口
// 不新增接缝字段、无需适配代码。
//
// **nil 语义**同 StorageManager：未装配即 nil（runtime 访问器已判 nil）。
type Metrics interface {
	RecordUpload(bytes int64)
	RecordDownload(bytes int64)
	RecordDelete()
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
	// Scope 是预留所依据的 owner 全局/user 桶 Scope（与 ScopeRes 同源；覆盖写用它的
	// Adjust 做差分结算）。必须由 RouteUpload 一并交回：装配层按同一 (owner, rel) 解析出的
	// 就是本预留使用的那个 Scope，领域侧重解析（QuotaScopeFor）在配置热更新下不保证同对象。
	Scope *quota.Scope
	// ScopeRes 是 owner 全局/user 桶 Scope 预留（双账本之一）。
	ScopeRes *quota.Reservation
	// Pool/PoolRes 是卷容量池预留（双账本之二，未装配卷集合时为 nil）。
	Pool    *quota.Pool
	PoolRes *quota.Reservation
	// Release 双回滚（预留全额归还），供路由后失败路径调用。
	Release func()
}

// Commit 结算双账本预留：覆盖写（prev > 0）按 (prev, written) 差分收敛后释放预留，
// 新文件（prev == 0）把预留 Commit 成 written。
//
// 本方法是该结算规则的**单一事实源**：装配层 routeUpload 的 *volumeRoute 已不再持有
// commit 方法（唯一调用方随写面迁入本包），故两侧的账单收敛结果结构性不可分叉。
// Scope 字段即为此处覆盖写差分所需（见其字段注释）。
func (r *UploadRoute) Commit(prev, written int64) {
	if r.ScopeRes != nil {
		if prev > 0 {
			r.Scope.Adjust(prev, written)
			r.ScopeRes.Release()
		} else {
			r.ScopeRes.Commit(written)
		}
	}
	if r.PoolRes != nil {
		if prev > 0 {
			r.Pool.Adjust(prev, written)
			r.PoolRes.Release()
		} else {
			r.PoolRes.Commit(written)
		}
	}
}

// HTTPError 是「带 HTTP 状态码的失败原因」（用户可见文案 + 状态码），由装配层把 pkg/server
// 的对应错误类型映射而来：卷路由拒绝（403/409/507）与下载路径解析失败（400/404）。
// 本域按它原样回包；非本类型按各调用点的兜底状态码处理（卷路由 500 / 下载 400）。
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string { return e.Message }

// Service 是文件服务领域实例：持有已解析的能力运行时，承载各能力族的 HTTP 处理器。
//
// 并发安全**来自下层能力**（装配层的 tenantMu 串行化懒建、checksum 台账自带互斥、
// 配额池自带锁），不来自本类型；本类型自身不做共享可变状态（runtime 构造后只读），
// 故同一实例可被并发请求使用。
//
// 能力入口统一经 `s.rt.<accessor>()`（nil 安全）；构造入口是 `New(tenants, opts...)`。
type Service struct {
	rt runtime
}

// anonymousOwner 是未认证请求的默认租户名（结构与其他租户完全同构）。
// 单源在 pkg/storage（租户名是存储布局契约：<root>/<owner>/…），装配层与领域包同值。
const anonymousOwner = storage.AnonymousOwner

// normalizeOwner 把空 owner 归一为 anonymous 租户名（未认证请求的默认租户）。
// 判定单源在 pkg/storage.NormalizeOwner（委托守卫见
// pkg/server/helper_impl_drift_test.go 的 TestNormalizeOwner_DelegatesToStorage）；
// 本函数保留仅为包内调用点稳定。
func normalizeOwner(owner string) string {
	return storage.NormalizeOwner(owner)
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

// BatchOperationResult 批量操作单条结果（批量删除与批量重命名共用同一响应形状）。
type BatchOperationResult struct {
	Filename string `json:"filename"`
	Success  bool   `json:"success"`
	Message  string `json:"message"`
}

// BatchResponse 批量操作的响应体（批量删除与批量重命名共用）。
type BatchResponse struct {
	Results []BatchOperationResult `json:"results"`
}

// 审计结果取值：与 pkg/server 的 AuditResult* 同值——审计行的 result 字段是**跨层 JSON
// 契约**（`/api/audit` 直接序列化给 Web UI）。领域侧写面族的审计统一经 Auditor 能力
// 交装配层落盘（actor/mesh/TS 由装配层补齐）。
const (
	auditResultSuccess = "success"
	auditResultDenied  = "denied"
	auditResultError   = "error"
)

// sendJSON 写出 JSON 响应：Content-Type、状态码与「序列化失败」兜底（500 +
// {"error":"internal server error"} + Warn 日志）与 pkg/server.sendJSONResponse 一致，
// 只是日志器改从接缝取当前生效实例。
func (s *Service) sendJSON(w http.ResponseWriter, response any, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	buf, err := json.Marshal(response)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		s.rt.logger().Warn("Encode JSON response failed", "error", err)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "internal server error"})
		return
	}
	w.WriteHeader(statusCode)
	_, _ = w.Write(buf)
}

// 本文件末尾是**跨族共享纯函数**在本包内的等价实现（checksumReader / fileChecksumRoot /
// atomicRenameRoot / drainAndVerifyBody / defaultVolumeAllows / locateForRead）与
// 本包**独有**的 verifyFileWithChecksumRoot（其 pkg/server 副本已随写面删除，见各函数注释）。
// 前六个在 pkg/server 侧各自另有消费者（读面/写面/版本族），既不能随本族迁走、本包也无法
// import pkg/server（规则③），按接缝判据（纯计算/纯策略不进接缝）只能下沉为本地实现。
// 逐条注明对应实现与等价依据。

// checksumReader 计算 src 的 SHA-256 十六进制摘要（小写）。会完全消耗 src，调用方负责
// 关闭实现 io.Closer 的入参。
//
// **委托单一事实源**：SHA-256 的算法实现（sha256 + hex + 256 KiB CopyBuffer）下沉到本包
// 依赖的 L0 顶层包 `checksum.Reader`，本函数只保留域内名称。抽取期此处与 `pkg/server.Checksum`
// 各持一份逐字相同的实现（本包无法反向 import pkg/server，不能收敛），故改由两侧共同依赖的
// 下层包收口。等价性由 `pkg/server/helper_impl_drift_test.go` 的源码级断言守卫。
func checksumReader(src io.Reader) (string, error) {
	return checksum.Reader(src)
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
// 写面迁入后本包是本函数的**唯一定义**（pkg/server 侧的同名实现已随写面删除——其消费者
// 全部在 upload/rename/delete）。分支顺序与 pkg/server.verifyChecksum 逐字一致：
// **先 root.Open**（打开失败即不匹配，即使 expected 为空），打开成功后才做 `expected == ""`
// 空值短路。顺序刻意对齐——两处实现的行为必须可逐句对照。
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
// 对应 pkg/server.drainAndVerifyBody（该函数在 pkg/server 侧另有 **19 个消费者**——口径：
// `grep -rn 'drainAndVerifyBody(' pkg/server/*.go` 去掉测试文件、注释行与函数声明行；
// 写面迁入前为 **22** 个，迁走的 **3** 个已改为调用本包实现）：
// 实现完全相同（io.Copy 到 io.Discard，返回其错误）。
func drainAndVerifyBody(r *http.Request) error {
	_, err := io.Copy(io.Discard, r.Body)
	return err
}

// defaultVolumeAllows 判断 owner 是否被默认卷 ACL 放行（读/删/改名「未命中回落默认租户」前
// 的护栏——默认卷不在 owner 视图时不得回落默认租户 Open，防经回落读到默认卷自身遗留文件）。
// VolSet 未装配（旧装配路径，无卷 ACL）→ 恒 true（唯一根即默认，零回归）。
//
// 本函数自 pkg/server/volumes.go **原样下沉**（函数体逐字未改，仅接缝项 h.volSet →
// s.rt.volSet()）：它只用**已注入**的卷集合做纯计算，不含 Handlers 私有状态——属**纯策略**，
// 按接缝判据（纯策略不进接缝）下沉为领域内方法。pkg/server 侧同名实现另有 3 个消费者
// （download/archive/version），等价性由 `pkg/server/helper_impl_drift_test.go` 的源码级断言守卫。
func (s *Service) defaultVolumeAllows(owner string) bool {
	owner = normalizeOwner(owner)
	if s.rt.volSet() == nil {
		return true
	}
	v, ok := s.rt.volSet().ByName(s.rt.volSet().Default().Name)
	return ok && v.Authorize(owner)
}

// locateForRead 是读/删/改名路径的卷定位统一入口（带可选显式 volume 过滤）：
//   - explicitVol 非空 → 只在指定卷定位；未知卷名或 owner 不在该卷视图（ACL）→ 未命中
//     （fail-closed，调用方按 404，不泄卷存在性）；
//   - explicitVol 空 → 全视图定位（VolumeRouter.Locate）。
//
// 本函数自 pkg/server/volumes.go **原样下沉**（语义逐字未改，仅接缝项改为 s.rt.*，返回
// 类型由 *fileLocation 改为本包的值类型 FileLocation——旧装配路径的 `return nil, false`
// 对应新类型的零值）：它只用**已注入**的卷集合、本包的 volumeFileExists 纯函数与接缝的
// LocateOwnerFile/VolumeTenant 组合出结果，不含 Handlers 私有状态——属**纯策略**。
// pkg/server 侧同名实现另有 1 个消费者（下载路径解析），等价性由
// `pkg/server/helper_impl_drift_test.go` 的源码级断言守卫。
func (s *Service) locateForRead(owner, rel, explicitVol string) (FileLocation, bool) {
	owner = normalizeOwner(owner)
	if explicitVol != "" {
		if s.rt.volSet() == nil {
			return FileLocation{}, false
		}
		v, ok := s.rt.volSet().ByName(explicitVol)
		if !ok || !v.Authorize(owner) {
			return FileLocation{}, false
		}
		exists, err := volumeFileExists(s.rt.volSet(), v.Name, owner, rel)
		if err != nil || !exists {
			return FileLocation{}, false
		}
		tnt := s.rt.volumeTenant(v.Name, owner)
		if tnt == nil {
			return FileLocation{}, false
		}
		return FileLocation{VolumeName: v.Name, Tenant: tnt}, true
	}
	return s.rt.locateOwnerFile(owner, rel)
}
