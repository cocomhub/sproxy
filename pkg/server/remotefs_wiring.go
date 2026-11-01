// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// remotefs_wiring.go 是 `pkg/server/remotefs`（跨节点读写面）的**装配适配**：
// 把装配根持有的能力（文件服务/卷集合/配额/审计/元数据解析/身份与凭证池）适配为该包的
// 窄依赖 `Deps`，并把 listener 的构造与类型以别名暴露给 `cmd/sproxy`（独立 module）。
//
// 为什么适配层必须留在装配根：身份加载（XDG 身份文件）、凭证池解析（cluster.credentials）、
// 元数据键空间（本地卷 sidecar / 外部卷 meta.Provider）都是**装配层的知识**；下沉会迫使
// 子包反向导入装配根（门禁 R6）。
package server

import (
	"context"
	"log/slog"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/server/remotefs"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// RemoteReadListener 是跨节点只读面 loopback listener（定义在 pkg/server/remotefs）。
// 类型别名保留，使 `cmd/sproxy`（独立 module）的引用面零改动。
type RemoteReadListener = remotefs.ReadListener

// RemoteWriteListener 是跨节点写面 loopback listener（定义在 pkg/server/remotefs）。
type RemoteWriteListener = remotefs.WriteListener

// StartRemoteReadListener 按 cfg.RemoteRead 起只读面监听；未启用时返回 (nil, nil)。
//
// 装配要点（实现见 pkg/server/remotefs.StartReadListener）：
//   - 监听强制 loopback（Validate 已保证；实现内再以实际 Addr 断言，纵深防御）；
//   - B 侧身份复用既有服务端 xfer 身份（LoadXferIdentity，hub.xfer_identity_file）；
//   - 静态密钥由 listener 自己的身份指纹派生，**双向 pin 是 fail-closed 硬前提**
//     （无任何 mesh_readers 指纹时拒绝启动，每连接恒传 pins）；
//   - 集群出口凭证池解析失败 ⇒ 装配 fail-closed。
func StartRemoteReadListener(ctx context.Context, cfg *Config, h *Handlers, log *slog.Logger) (*RemoteReadListener, error) {
	return remotefs.StartReadListener(ctx, remoteDeps(cfg, h, log))
}

// StartRemoteWriteListener 按 cfg.RemoteWrite 起写面监听；未启用时返回 (nil, nil)。
//
// 与只读面逐条对应，差别只有三处（都是为了写安全）：独立开关/监听地址、pin 列表只取
// scope 授予写的指纹（只读对端连握手都过不了）、路由表换成写面白名单（实现见
// pkg/server/remotefs.StartWriteListener）。
func StartRemoteWriteListener(ctx context.Context, cfg *Config, h *Handlers, log *slog.Logger) (*RemoteWriteListener, error) {
	return remotefs.StartWriteListener(ctx, remoteDeps(cfg, h, log))
}

// remoteDeps 把装配根的能力适配为跨节点面的依赖。
func remoteDeps(cfg *Config, h *Handlers, log *slog.Logger) remotefs.Deps {
	return remotefs.Deps{
		Files:   func() *files.Service { return h.fileService() },
		Volumes: remoteVolumeLookup(h),
		Quota:   func(owner string) *quota.Scope { return h.quotaFor(owner) },
		Audit:   remoteAudit{h: h},
		Meta:    remoteMeta{h: h},
		Config:  cfg,
		Logger:  log,
		Runtime: remoteRuntime{cfg: cfg, log: log},
	}
}

// remoteVolumeLookup 返回卷集合的只读查询面；**卷集合未装配时返回 nil 接口**——
// 远程面据此回 500（服务端装配错误 ≠ 对象不存在），与既有语义逐字一致。
func remoteVolumeLookup(h *Handlers) remotefs.VolumeLookup {
	if h == nil || h.volSet == nil {
		return nil
	}
	return h.volSet
}

// remoteAudit 把跨节点面事件落到装配根的审计与指标。
type remoteAudit struct{ h *Handlers }

func (a remoteAudit) Record(ctx context.Context, evt remotefs.Event) {
	a.h.RecordAudit(ctx, AuditEvent{
		Action: evt.Action, Mesh: evt.Mesh, Owner: evt.Owner,
		ObjectType: evt.ObjectType, Object: evt.Object,
		Result: evt.Result, Detail: evt.Detail,
	})
}

func (a remoteAudit) RecordWriteDenied(reason, node string) {
	a.h.RecordRemoteWriteDenied(reason, node)
}

// remoteMeta 把「持有侧口径」的元数据/下载路径解析（本地卷 sidecar / 外部卷 meta.Provider /
// 出口卷读取源）适配为跨节点面依赖。
type remoteMeta struct{ h *Handlers }

func (m remoteMeta) ReadRemoteMeta(ctx context.Context, owner, volName, remotePath string) ([]byte, error) {
	return m.h.readMetaForRemote(ctx, owner, volName, remotePath)
}

func (m remoteMeta) RemoteDownloadPath(ctx context.Context, owner, volName, remotePath string) (files.DownloadPath, error) {
	return m.h.downloadPathForRemote(ctx, owner, volName, remotePath)
}

// remoteRuntime 是 listener 的装配态：服务端 xfer 身份、mux 选项、集群出口凭证池。
type remoteRuntime struct {
	cfg *Config
	log *slog.Logger
}

func (r remoteRuntime) Identity() (*tunnel.Identity, error) { return LoadXferIdentity(r.cfg) }

func (r remoteRuntime) IdentityPath() string { return XferIdentityPath(r.cfg) }

func (r remoteRuntime) MuxOptions() []mux.Option { return MuxIdlePaddingOptions(r.cfg) }

func (r remoteRuntime) CredentialSource() (remotefs.CredentialSource, error) {
	creds, err := newClusterCredentialSet(r.cfg, r.log)
	if err != nil || creds == nil {
		return nil, err
	}
	return creds, nil
}

// 断言：装配层适配类型满足跨节点面包的窄接口（改动即在此处红）。
var (
	_ remotefs.VolumeLookup     = (*registry.Set)(nil)
	_ remotefs.Audit            = remoteAudit{}
	_ remotefs.Meta             = remoteMeta{}
	_ remotefs.ListenerRuntime  = remoteRuntime{}
	_ remotefs.CredentialSource = (*clusterCredentialSet)(nil)
)
