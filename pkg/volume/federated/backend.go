// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package federated

import (
	"context"
	"fmt"
	"strings"

	"github.com/cocomhub/sproxy/pkg/remote"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// backend.go 是 federated 卷后端（roadmap 3.3 P2 F2+F3 合一）：
// 把远端 mesh 节点的卷以只读挂载暴露到本地卷视图。
//
// 装配：BackendFactory（Extra 读 node/volume/path）→ remote.Client（注入 Dialer，
// 经 hub 中继）→ Client.FS(ref) 即 sync.FS（只读面已有；写面未配 writeDialer →
// fail-closed）。HealthProbe：拨号探测（可达 = healthy）。

// federatedBackend 是 ExternalBackend 实现：持有 remote.Client + 远端卷引用。
type federatedBackend struct {
	client  *remote.Client
	ref     remote.Ref
	fs      syncpkg.FS
	closeFn func()
}

// NewBackend 构造 federated 后端（测试/装配共用：dialer 注入——生产 RelayDialer）。
func NewBackend(ctx context.Context, v volume.Volume, dialer remote.Dialer, opts ...remote.Option) (registry.ExternalBackend, error) {
	if v.Type == "" || v.Type == volume.TypeLocal {
		return nil, fmt.Errorf("federated backend: 卷 %q 类型 %q 不是外部 federated 卷", v.Name, v.Type)
	}
	if dialer == nil {
		return nil, fmt.Errorf("federated backend: 卷 %q 拨号器未注入（需 hub 中继 Dialer）", v.Name)
	}
	node, _ := v.Extra["node"].(string)
	node = strings.TrimSpace(node)
	volName, _ := v.Extra["volume"].(string)
	volName = strings.TrimSpace(volName)
	if node == "" || volName == "" {
		return nil, fmt.Errorf("federated backend: 卷 %q 缺 node/volume（Extra 配置）", v.Name)
	}
	path, _ := v.Extra["path"].(string)
	c := remote.New(dialer, opts...)
	ref := remote.Ref{Node: node, Volume: volName, Path: path}
	rfs := c.FS(ref) // remoteFS（读面 + 写面，写走 volwrite）
	// 联邦卷回写（roadmap P2）：Extra["writable"]=true 时经 federated.FS 注入写面
	// （写方法转发 remoteFS → volwrite 写面）；false/缺省 → 不注入，写操作 fail-closed
	// （ErrReadOnly，**只读约束真正生效**）。
	//
	// 审查 P1 修复（批次9）：此前 `_ = v.Extra["writable"]` 占位——remoteFS 自带写面
	// （writeDialer 全局注入后写恒可执行），writable=false 无法阻止写（fail-open，配置
	// 静默失效）；federated.FS.WithWriter 也成死代码。现改为始终经 federated.FS 包装：
	// writable=true 注入 remoteFS 为写面（可写），false 不注入（只读 fail-closed）。
	writable, _ := v.Extra["writable"].(bool)
	fs, err := New(rfs)
	if err != nil {
		return nil, fmt.Errorf("federated backend: 卷 %q 构造失败: %w", v.Name, err)
	}
	if writable {
		fs = fs.WithWriter(rfs)
	}
	return &federatedBackend{
		client:  c,
		ref:     ref,
		fs:      fs,
		closeFn: func() { c.Close() },
	}, nil
}

// FS 返回远端卷的只读 sync.FS 视图。
func (b *federatedBackend) FS() syncpkg.FS { return b.fs }

// Close 释放 client（幂等）。
func (b *federatedBackend) Close() error {
	if b.closeFn != nil {
		b.closeFn()
	}
	return nil
}

// Stats 实现 registry.VolumeStatsProvider：经隧道查询远端 /api/stats 配额段，
// 映射 VolumeStats（TotalBytes = 远端配额上限，UsedBytes = 远端已用）。
// 远端无配额段（Quota=nil）→ nil,nil（与「后端不支持」同语义，不失败）。
func (b *federatedBackend) Stats(ctx context.Context) (*registry.VolumeStats, error) {
	st, err := b.client.Stats(ctx, b.ref)
	if err != nil {
		return nil, err
	}
	if st == nil || st.Quota == nil {
		return nil, nil
	}
	return &registry.VolumeStats{
		TotalBytes: st.Quota.MaxBytes,
		UsedBytes:  st.Quota.Usage,
	}, nil
}

// Ping 远端可达性探测（registry.HealthProbe）：经 client.Probe（拨号 + 建链到 node）——
// 成功 = healthy（探针调用方按 degraded 处理失败）。链路建立失败/远端不可达 → 错误。
func (b *federatedBackend) Ping(ctx context.Context) error {
	if err := b.client.Probe(ctx, b.ref.Node); err != nil {
		return fmt.Errorf("federated: 远端 %q 不可达: %w", b.ref.Node, err)
	}
	return nil
}

// TypeFederated 是 federated 卷后端类型名（registry 注册键）。
const TypeFederated = "federated"

// RegisterBackend 注册 federated 后端（装配层调用；dialer 为生产注入的 hub 中继
// Dialer）。重复注册 → registry panic（编程错误）。
func RegisterBackend(dialer remote.Dialer, opts ...remote.Option) {
	registry.RegisterBackend(TypeFederated, func(ctx context.Context, v volume.Volume) (registry.ExternalBackend, error) {
		return NewBackend(ctx, v, dialer, opts...)
	})
}
