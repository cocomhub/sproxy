// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

// fs.go 是集群出口卷（type: egress）的 sync.FS 适配：底层 = 凭证指向的持有节点
// 真实卷（经 federated.FS 挂 remoteFS，RandomAccess 经 RangeReader 透传）。
//
// **实现现状（评审 I7 措辞校准）**：一出口卷 = 一持有节点卷（NewBackend 绑定单
// holder_node/volume/fingerprint/owner）。“一个出口卷获取所有可访问节点卷”通过
// **多 egress 卷**（每卷一条凭证）实现，而非按请求动态选路——egress_node/
// egress_volume 查询参数无消费者（已删除，勿据此断言动态选路）。

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// clusterFS 是出口侧集群卷的 FS 适配（只读）：embeds federated.FS（远端只读底座），
// 叠加 holderRel（出口 owner key → 持有侧命名空间相对路径桥）与 DirectURL（302）。
//
// **自带 meta.Provider（评审 P2-10 修复）**：出口侧要对自己的下载流做逐分块校验，
// 就必须能从**持有侧**取到 sidecar。持有侧的 sidecar 在**持有侧自身的键空间**里
// （本地卷 `meta/<rel>.meta`，外部卷由其 Provider 决定），出口侧不应（也不能）直接按
// 键拼接——所以本类型实现 `meta.Provider`，经 remote 的 `meta` 面把**用户相对路径**
// 交给持有侧，由持有侧用自身可信栈回答。
//
// **为何必须自带而不是被 trusted.Wrap 包一层**：`trusted.Wrap` 对**不自带 meta** 的 FS
// 会套 `TrustedVolumeFS`，后者的 `FileMeta` 用 `inner.OpenRead(meta.MetaPath(rel))` 直取
// ——键空间错位（`<owner>/meta/...` 被当成持有侧用户相对路径）→ 持有侧恒 404 → 下载
// **静默零校验**（可信卷承诺不成立）。实现 `meta.Provider` 后 `trusted.Wrap` 保持原样
// 返回（与 secretdata 同构：自带 meta 的卷自己管），Guard 仍透传该能力。
type clusterFS struct {
	fs      syncpkg.FS // federated.FS（挂持有节点卷）
	cfg     egressConfig
	volName string // 出口卷名（B 态 302 URL 参数）
	// fetchMeta 拉取持有侧 sidecar 原始 JSON（remote `meta` 面；入参为**持有侧用户相对
	// 路径**）。nil = 未装配 → fail-closed（不静默返回 nil 让调用方误当「无 sidecar」）。
	fetchMeta func(ctx context.Context, path string) ([]byte, error)
}

// holderRel 映射出口侧 owner key → 持有侧 owner 命名空间相对路径。
//
// 出口侧 resolveExternalDownload 用出口 owner 算 ownerKey（如 <egressOwner>/user/dir/f.bin
// 或 user/dir/f.bin，按卷 Shared()）；持有侧 remote_read 把 path 当**持有 mesh_readers
// 绑定 owner 的 user 桶内相对路径**。故剥出口 owner 前缀 + user 桶，得持有侧相对路径。
//
// **剥边界（评审 I1 修复）**：用**第一个** "/user/" 切（owner 是单段不含 /，首个即
// 桶边界）——此前 LastIndex 命中内层同名段（如 docs/user/tutorial.mp4）会剥错。
//
// **HolderPath 子树限定（评审 I4 修复）**：配置 holder_path 后请求落在
// `<holder_path>/<rel>`（限制子树）——此前解析进 remote.Ref.Path 但被 Root() 丢弃
// （死配置），用户按文档配了以为限子树、实际暴露整卷。
func (f *clusterFS) holderRel(ownerKey string) (string, error) {
	rel := ownerKey
	// 剥 <owner>/ 前缀（出口 owner key 形态：<owner>/user/<rel>）——首个 /user/ 是桶边界。
	// **已剥过就不能再剥 user/**：否则 `<owner>/user/user/x.bin`（用户目录名恰为 user）
	// 会被误剥成 `x.bin` → 404，或命中持有侧根同名文件 → **返回另一个文件的内容**（P2）。
	if i := strings.Index(rel, "/user/"); i >= 0 {
		rel = rel[i+len("/user/"):]
	} else {
		// 未带 owner 前缀的形态（已是桶相对 `user/...`）才剥 user 桶。
		rel = strings.TrimPrefix(rel, "user/")
	}
	if rel == "" {
		return "", fmt.Errorf("cluster: 路径 %q 剥前缀后为空（非法 owner key）", ownerKey)
	}
	// 子树前缀（可选）：请求 rel 落在 <holder_path>/<rel>（空 = 全卷）。
	if prefix := strings.Trim(f.cfg.HolderPath, "/"); prefix != "" {
		rel = prefix + "/" + rel
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
func (f *clusterFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, path string, r io.Reader, size, mtime int64) error {
	return errEgressReadOnly
}
func (f *clusterFS) Rename(ctx context.Context, from, to string) error { return errEgressReadOnly }
func (f *clusterFS) Delete(ctx context.Context, path string) error     { return errEgressReadOnly }
func (f *clusterFS) MakeDir(ctx context.Context, path string) error    { return errEgressReadOnly }

// errEgressReadOnly 是出口集群卷只读语义的哨兵错误（写方法恒返回）。
var errEgressReadOnly = fmt.Errorf("cluster: 出口卷只读（数据由持有节点提供，出口仅转发）")

// FileMeta 实现 meta.Provider：把「出口 owner key」归一到**持有侧用户相对路径**后经
// remote `meta` 面取 sidecar（见 clusterFS 文档「自带 meta.Provider」）。
//
// 错误语义：持有侧无 sidecar 时保留 `os.ErrNotExist`（调用方据此区分预期降级与读取失败）；
// meta 面未装配 / 持有侧返回畸形 JSON → 报错（fail-closed，绝不当作「校验通过」）。
func (f *clusterFS) FileMeta(ctx context.Context, ownerKey string) (*meta.FileMeta, error) {
	rel, err := f.holderRel(ownerKey)
	if err != nil {
		return nil, err
	}
	if f.fetchMeta == nil {
		return nil, fmt.Errorf("cluster: 持有侧 meta 面未装配: %w", syncpkg.ErrUnsupported)
	}
	raw, ferr := f.fetchMeta(ctx, rel)
	if ferr != nil {
		return nil, ferr
	}
	fm, uerr := meta.Unmarshal(raw)
	if uerr != nil {
		return nil, fmt.Errorf("cluster: 持有侧 meta %s 非法: %w", rel, uerr)
	}
	return fm, nil
}

// DirectURL 实现 syncpkg.DirectURLProvider（B 态 302）：
//   - holder_public_base_url 非空 → 直跳持有公网端点（B2，需同认证域）；
//   - egress_base_url 非空 → 302 到出口自身公网端点 + `egress_forward=1`（出口内部再
//     转发，环由 resolveExternalDownload 的 forceForward 打破）；
//   - 两者空 → ("", false, nil) 回落 A 态（graceful）。
func (f *clusterFS) DirectURL(ctx context.Context, relPath string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", true, err
	}
	// **持有侧相对路径（评审 I2 修复）**：入参是出口侧 ownerKey（<owner>/user/<rel> 或
	// user/<rel>，来自 externalStateFor），但 302 的 filename 必须是持有侧相对路径——
	// 此前直接拼 ownerKey，B 态自转发/持有侧 404。统一经 holderRel 剥到持有侧相对路径，
	// 二次进入出口 /download 时 ResolveUserLocation 抵消前缀、环打破后仍归位（见 resolveExternalDownload）。
	rel, herr := f.holderRel(relPath)
	if herr != nil {
		return "", false, herr // 非法路径 → 回落 A 态（fail-closed，不产出病态 302）
	}
	// B2 直跳持有（优先：流量不经出口，省出口带宽）。
	if f.cfg.HolderPublicBaseURL != "" {
		return f.cfg.HolderPublicBaseURL + "/download?filename=" + url.QueryEscape(rel) +
			"&volume=" + url.QueryEscape(f.cfg.HolderVolume), true, nil
	}
	// B 态出口自转发。
	if f.cfg.EgressBaseURL != "" {
		return f.cfg.EgressBaseURL + "/download?filename=" + url.QueryEscape(rel) +
			"&volume=" + url.QueryEscape(f.volName) +
			"&egress_forward=1", true, nil
	}
	return "", false, nil
}

// 编译期断言：clusterFS 实现全部能力接口。
var (
	_ syncpkg.FS                = (*clusterFS)(nil)
	_ syncpkg.RangeReader       = (*clusterFS)(nil)
	_ syncpkg.DirectURLProvider = (*clusterFS)(nil)
	_ meta.Provider             = (*clusterFS)(nil)
)
