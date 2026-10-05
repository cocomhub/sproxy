// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

// fs.go 是集群出口卷（type: egress）的 sync.FS 适配：底层 = 凭证指向的持有节点
// 真实卷（经 federated.FS 挂 remoteFS，RandomAccess 经 RangeReader 透传）。
//
// 用户裁定（2026-10-05）：一个出口卷获取**所有可访问节点卷**（凭证池），避免每卷
// 配对应出口卷——clusterFS 按请求动态解析目标 (node, volume)，从凭证池选路。

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// clusterFS 是出口侧集群卷的 FS 适配（只读）：embeds federated.FS（远端只读底座），
// 叠加 holderRel（出口 owner key → 持有侧命名空间相对路径桥）与 DirectURL（302）。
type clusterFS struct {
	fs      syncpkg.FS // federated.FS（挂持有节点卷）
	cfg     egressConfig
	volName string // 出口卷名（B 态 302 URL 参数）
}

// holderRel 映射出口侧 owner key → 持有侧 owner 命名空间相对路径。
//
// 出口侧 resolveExternalDownload 用出口 owner 算 ownerKey（如 <egressOwner>/user/dir/f.bin
// 或 user/dir/f.bin，按卷 Shared()）；持有侧 remote_read 把 path 当**持有 mesh_readers
// 绑定 owner 的 user 桶内相对路径**。故剥出口 owner 前缀 + user 桶，得持有侧相对路径。
func (f *clusterFS) holderRel(ownerKey string) (string, error) {
	rel := ownerKey
	// 剥 <owner>/ 前缀（出口 owner key 形态：<owner>/user/<rel>）。
	if i := strings.LastIndex(rel, "/user/"); i >= 0 {
		rel = rel[i+len("/user/"):]
	}
	// 剥 user 桶（若仍带）。
	rel = strings.TrimPrefix(rel, "user/")
	if rel == "" {
		return "", fmt.Errorf("cluster: 路径 %q 剥前缀后为空（非法 owner key）", ownerKey)
	}
	return rel, nil
}

// Stat 实现 sync.FS：持有侧路径统计（经 holderRel 归一）。
func (f *clusterFS) Stat(ctx context.Context, path string) (*syncpkg.Entry, error) {
	rel, err := f.holderRel(path)
	if err != nil {
		return nil, err
	}
	return f.fs.Stat(ctx, rel)
}

// ListDir 实现 sync.FS：持有侧目录列举（经 holderRel 归一）。
func (f *clusterFS) ListDir(ctx context.Context, path string) ([]syncpkg.Entry, error) {
	rel, err := f.holderRel(path)
	if err != nil {
		return nil, err
	}
	return f.fs.ListDir(ctx, rel)
}

// OpenRead 实现 sync.FS：持有侧整流读（经 holderRel 归一）。
func (f *clusterFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	rel, err := f.holderRel(path)
	if err != nil {
		return nil, err
	}
	return f.fs.OpenRead(ctx, rel)
}

// OpenRangeRead 实现 syncpkg.RangeReader：持有侧区间读（经 holderRel 归一 →
// federated 透传 remoteFS → 持有节点 /remote/download + Range）。
func (f *clusterFS) OpenRangeRead(ctx context.Context, path string, offset, size int64) (io.ReadCloser, error) {
	rel, err := f.holderRel(path)
	if err != nil {
		return nil, err
	}
	rr, ok := f.fs.(syncpkg.RangeReader)
	if !ok {
		return nil, fmt.Errorf("cluster: 底层未实现 RangeReader（持有节点不支持随机访问）")
	}
	return rr.OpenRangeRead(ctx, rel, offset, size)
}

// 写方法（出口集群卷只读）：WriteFile/Rename/Delete/MakeDir 恒返回错误——出口只转发
// 数据不写持有节点；fail-closed（静默成功会让上层把"没写"当"已写"）。
func (f *clusterFS) WriteFile(ctx context.Context, path string, r io.Reader, size, mtime int64) error {
	return errEgressReadOnly
}
func (f *clusterFS) Rename(ctx context.Context, from, to string) error { return errEgressReadOnly }
func (f *clusterFS) Delete(ctx context.Context, path string) error     { return errEgressReadOnly }
func (f *clusterFS) MakeDir(ctx context.Context, path string) error    { return errEgressReadOnly }

// errEgressReadOnly 是出口集群卷只读语义的哨兵错误（写方法恒返回）。
var errEgressReadOnly = fmt.Errorf("cluster: 出口卷只读（数据由持有节点提供，出口仅转发）")

// DirectURL 实现 syncpkg.DirectURLProvider（B 态 302）：
//   - holder_public_base_url 非空 → 直跳持有公网端点（B2，需同认证域）；
//   - egress_base_url 非空 → 302 到出口自身公网端点 + `egress_forward=1`（出口内部再
//     转发，环由 resolveExternalDownload 的 forceForward 打破）；
//   - 两者空 → ("", false, nil) 回落 A 态（graceful）。
func (f *clusterFS) DirectURL(ctx context.Context, relPath string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", true, err
	}
	// B2 直跳持有（优先：流量不经出口，省出口带宽）。
	if f.cfg.HolderPublicBaseURL != "" {
		return f.cfg.HolderPublicBaseURL + "/download?filename=" + url.QueryEscape(relPath) +
			"&volume=" + url.QueryEscape(f.cfg.HolderVolume), true, nil
	}
	// B 态出口自转发。
	if f.cfg.EgressBaseURL != "" {
		return f.cfg.EgressBaseURL + "/download?filename=" + url.QueryEscape(relPath) +
			"&volume=" + url.QueryEscape(f.volName) +
			"&egress_node=" + url.QueryEscape(f.cfg.HolderNode) +
			"&egress_volume=" + url.QueryEscape(f.cfg.HolderVolume) +
			"&egress_forward=1", true, nil
	}
	return "", false, nil
}

// 编译期断言：clusterFS 实现全部能力接口。
var (
	_ syncpkg.FS                = (*clusterFS)(nil)
	_ syncpkg.RangeReader       = (*clusterFS)(nil)
	_ syncpkg.DirectURLProvider = (*clusterFS)(nil)
)
