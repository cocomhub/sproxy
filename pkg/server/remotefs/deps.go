// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package remotefs 是 B 侧**跨节点读写面**（只读面 + 写面 + 各自的 loopback listener）。
//
// 为什么是一个包：读写两面刻意**反向同构**——同一套授权三重约束、同一套错误映射
// （401/403/404/409/500）、同一套审计落点，只是写面额外要求 `scope` 授予写。把两面放在
// 一起，才可能用同一份实现与同一组测试守住这个不变量；分开成两个包即等于允许它们各自漂移。
//
// 边界：
//   - 进包：HTTP 协议面（路由白名单 / 授权 / 委派 / 错误映射 / 审计）与传输面（listener）；
//   - 不进包：域语义（checksum 门禁、原子改名、版本、配额、文件锁、卷路由）全部由
//     `pkg/files` 承担；装配态（配置、身份、凭证池、审计/指标落点）经 `Deps` 注入。
//
// 本包**不导入装配根** `pkg/server`（门禁 R6）：所需能力一律以窄接口表达，由装配层在
// 构造时适配。
package remotefs

import (
	"context"
	"log/slog"

	"github.com/cocomhub/sproxy/pkg/clustercred"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/server/config"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// Deps 是跨节点读写面的装配依赖（≤8 字段，见门禁 R52）。
type Deps struct {
	// Files 惰性返回文件服务域实例（每次调用取当前实例）。用函数而不是指针，
	// 是为了保持装配根的懒装配时序：远程面首个请求到来时才构造领域实例。
	Files FileService
	// Volumes 是卷集合的**只读查询面**（授权判定用 volume.Volume 自身的方法）。
	Volumes VolumeLookup
	// Quota 返回 owner 的配额 Scope（nil = 未装配，/remote/stats 回空对象）。
	Quota QuotaFor
	// Audit 落跨节点面的业务审计与写面拒绝指标。
	Audit Audit
	// Meta 是「持有侧口径」的元数据/下载路径解析（含外部卷分支，装配层承担）。
	Meta Meta
	// Config 是启动期快照：listener 开关/监听地址/握手超时/卷 mesh_readers 的读取源。
	Config *config.Config
	// Logger 是 listener 生命周期日志（nil → slog.Default）。
	Logger *slog.Logger
	// Runtime 是 listener 的装配态（身份、mux 选项、集群出口凭证池）。
	Runtime ListenerRuntime
}

// FileService 返回文件服务域实例（nil = 未装配）。
type FileService func() *files.Service

// VolumeLookup 是按名解析卷的只读能力（*registry.Set 直接满足）。
type VolumeLookup interface {
	ByName(name string) (volume.Volume, bool)
}

// QuotaFor 返回 owner 的配额 Scope（nil = 未装配或该 owner 无配额）。
type QuotaFor func(owner string) *quota.Scope

// Event 是跨节点面写入审计的单条事件。
//
// 字段是装配层 AuditEvent 的**子集**（行动作/归属/节点/对象/结果/详情）：远程面从不设置
// actor（它由装配层从 ctx 取），故不需要在包里见到完整审计模型。
type Event struct {
	Action     string
	Mesh       string
	Owner      string
	ObjectType string
	Object     string
	Result     string
	Detail     string
}

// AuditResult 取值（与装配层审计结果常量同值）。
const (
	AuditResultSuccess = "success"
	AuditResultDenied  = "denied"
	AuditResultError   = "error"
)

// Audit 是审计与拒绝指标的落点（装配层适配到 pkg/audit 与 pkg/server/metrics）。
type Audit interface {
	// Record 记一条跨节点面业务审计（只读/写面的放行与拒绝都记）。
	Record(ctx context.Context, evt Event)
	// RecordWriteDenied 记一次写面授权拒绝（reason 是低基数机器可读原因码）。
	RecordWriteDenied(reason, node string)
}

// Meta 是「持有侧口径」的元数据与下载路径解析。
//
// 两者都必须由装配层承担：本地卷与外部卷（secretdata 等）的键空间不同，而出口侧不应知道
// 任何内部布局——把这份知识留在装配层，本包只见 `files.DownloadPath` 这一领域入参。
type Meta interface {
	// ReadRemoteMeta 返回用户相对路径对应的 sidecar 原始 JSON（无 sidecar ⇒ 404 HTTPError）。
	ReadRemoteMeta(ctx context.Context, owner, volName, remotePath string) ([]byte, error)
	// RemoteDownloadPath 把 (owner, 已授权卷, 用户相对路径) 解析为领域下载路径。
	RemoteDownloadPath(ctx context.Context, owner, volName, remotePath string) (files.DownloadPath, error)
}

// ListenerRuntime 是 listener 的装配态（身份/传输选项/凭证池）。
type ListenerRuntime interface {
	// Identity 加载本节点 xfer 身份（**可能有启动副作用**：身份文件不存在时创建并落盘）。
	Identity() (*tunnel.Identity, error)
	// IdentityPath 返回身份文件路径（启动日志用，供运维核对副作用落点）。
	IdentityPath() string
	// MuxOptions 返回每连接 mux 的装配选项（空闲填充等）。
	MuxOptions() []mux.Option
	// CredentialSource 返回集群出口凭证池（nil, nil = 未配置凭证池，仅静态 mesh_readers）。
	CredentialSource() (CredentialSource, error)
}

// CredentialSource 是按对端指纹验签的集群出口凭证池（装配期解析，运行期只读）。
type CredentialSource interface {
	CredentialFor(volName, fingerprint string) (clustercred.Credential, bool)
}
