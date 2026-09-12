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
// 窄函数一律是**取用函数**而非快照值：pkg/server 的字段会在运行期热替换（例如
// PUT /api/config 就地重建 h.logger），快照会让领域包一直读到旧值。
//
// # 子包约定（pkg/files/chunked、pkg/files/version 等）
//
// 门禁 R1 规定子包（L3）不得导入父域（L4），故**子包不复用本文件的 Deps**：
//
//  1. 每个子包在自己的包里定义**只含自身所需能力**的窄接口（Go 的「消费者定义接口」惯用法）；
//  2. 这些接口的实现**只有一处**：pkg/server 的装配层（同一个适配器可实现多个窄接口）；
//  3. 子包若需要 HTTP DTO 与响应写出，DTO 必须定义在**写出它的那个包**里——R1 使它
//     无法复用父域的 DTO；本包（领域根）自带 UploadResponse 与 sendJSON 正是此约定的一例。
//
// 于是「接口可以多份，实现只有一处」：被消除的是「各族自己重写文件读写逻辑」，
// 而不是「各族各自的依赖声明」。
//
// 示例（`pkg/files/chunked` 的示意草案；实际签名以该族落地时为准）：
//
//	package chunked
//
//	// Deps 是分块域自身需要的能力，由 pkg/server 的装配适配器实现。
//	type Deps interface {
//		ChunkSize() int64           // 配置热读：cfg.ChunkSize
//		SessionTTL() time.Duration  // 配置热读：cfg.UploadSessionTTL
//		Logger() *slog.Logger       // 与 files.Deps.Logger 同源（同一个装配适配器）
//	}
//
// pkg/server 侧一处实现（示意）：
//
//	func (d *fileServiceDeps) ChunkSize() int64          { return d.h.cfgPtr.Load().ChunkSize }
//	func (d *fileServiceDeps) SessionTTL() time.Duration { return d.h.cfgPtr.Load().UploadSessionTTL }
//	func (d *fileServiceDeps) Logger() *slog.Logger      { return d.h.logger }
package files

import (
	"encoding/json"
	"log/slog"
	"net/http"

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
// **typed-nil 陷阱（装配层必须处理）**：把 nil 的 `*registry.Set` 直接赋给本接口会得到
// 一个**非 nil 接口**，使领域包里的 `deps.VolSet == nil` 判断失效——单卷（未装配卷）路径
// 会被误判为多卷路径。装配层必须只在 volSet 非 nil 时才赋值（见 pkg/server.fileService）。
type VolumeSet interface {
	All() []volume.Volume
	Root(name string) *storage.Root
	Pool(name string) *quota.Pool
}

// Deps 是文件服务领域根的依赖接缝：**只放必须由装配层（pkg/server）注入的装配项**。
// 下层能力（路径校验、存储根/租户、配额池、卷集合、校验和台账的类型）直接 import
// 使用，不经接缝——接缝越小，包边界越清楚。
type Deps struct {
	// Logger 返回当前生效的业务日志器。**取用函数而非快照值**：pkg/server 在日志配置
	// 热更新时就地替换其 logger 字段，快照会让领域包一直写旧 handler（级别/格式不再生效）。
	Logger func() *slog.Logger

	// ActorFromRequest 返回请求的操作主体（未认证返回 ""）。必须注入：主体由 pkg/server
	// 的认证中间件写入请求 ctx，其 ctx key 是 pkg/server 的内部实现，领域包无从读取。
	ActorFromRequest func(*http.Request) string

	// VolSet 是装配后的运行时卷集合。必须注入：配置解析与卷根打开都留在 pkg/server，
	// 装配产物才交进来。nil = 未装配卷功能的旧装配路径（单卷语义），与 pkg/server 的
	// h.volSet 同义。**装配层注意**：不要把 nil 的 *registry.Set 直接赋进来（typed-nil
	// 陷阱见 VolumeSet 注释）。
	VolSet VolumeSet

	// TenantFor 返回 owner 在默认卷上的租户（懒创建缓存，空 owner → anonymous）。
	// 必须注入：租户句柄的懒建与缓存是装配层状态（pkg/server 的 tenantRoots），
	// 领域包只消费，不持有第二份句柄。
	TenantFor func(owner string) *storage.Tenant

	// PrimaryViewTenant 返回 owner 视图内首个卷的租户（「不跨卷写」入口用，如新建目录）。
	// 必须注入：「视图内首个卷」的排序与默认卷 ACL 回落策略由装配层决定。
	PrimaryViewTenant func(owner string) *storage.Tenant

	// VolumeTenant 返回指定卷上 owner 的租户（默认卷委托 TenantFor）。必须注入：
	// 非默认卷的租户懒建缓存由卷集合持有（registry.Set.Tenant），领域包不自建缓存。
	VolumeTenant func(volName, owner string) *storage.Tenant

	// QuotaScopeFor 按文件相对路径（含功能桶前缀，如 "user/dir/f.txt"）解析最长前缀
	// 配额子 Scope。必须注入：Scope 树按 owner 懒建并缓存在装配层（pkg/server 的
	// quotaBuckets），其层级/上限语义是装配期配置。
	QuotaScopeFor func(owner, rel string) *quota.Scope

	// ChecksumStoreFor 返回 owner 的 per-tenant 校验和台账（懒创建）。必须注入：
	// 台账按 owner 懒建并缓存在装配层（pkg/server 的 checksumStores）。
	ChecksumStoreFor func(owner string) *checksum.ChecksumStore
}

// Service 是文件服务领域实例：持有接缝 Deps，承载各能力族的 HTTP 处理器。
//
// 除约定俗成的一次性构造外**无自身可变状态**——所有能力都经 deps 从装配层取用，
// 故同一实例可被并发请求安全使用。
type Service struct {
	deps Deps
}

// NewService 构造文件服务领域实例。
//
// Logger 缺省回落 slog.Default()：领域包不再持有 pkg/server 的全局 logger，缺省时
// 不得 panic（与 pkg/volume/registry、pkg/checksum 随包搬迁的同名私有辅助同旨）。
func NewService(deps Deps) *Service {
	if deps.Logger == nil {
		deps.Logger = slog.Default
	}
	return &Service{deps: deps}
}

// anonymousOwner 是未认证请求的默认租户名（结构与其他租户完全同构）。
// 与 pkg/server 的 anonymousOwner 同值：租户名是存储布局契约（<root>/<owner>/…）。
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
// JSON 形状与 pkg/server.UploadResponse 逐字一致（字段名/顺序/omitempty 相同）：
// 迁移期文件面响应体不得变化，pkg/server 侧的其余端点仍用自己的同名类型。
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
