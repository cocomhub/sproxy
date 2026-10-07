// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package volume

// volume_path.go 提供卷内路径统一工具（2026-10-05 用户裁定）：
//
// `Volume.ResolveOwnerPath(owner, bucket, rel)` 是「owner 相对路径 → 卷内最终键」的
// **唯一入口**——上传 / 下载 / 转存三侧统一经它，使用方不关心卷内键算法：
//
//   - **权限**：owner 无权访问卷 → ErrVolumeNotAuthorized（fail-closed，立即报错，
//     不暴露卷内布局/存在性）；
//   - **路径安全**：归一 + 逐段校验（拒绝 ../ 逃逸、绝对路径、空段、非法段名 /
//     Windows 保留名 / `.__` 内部前缀——防注入）；
//   - **键空间自动适配**：共享卷（多用户）→ `<owner>/<bucket>/<rel>`（owner 前缀
//     隔离，不同用户靠机制隔离互不感知）；独享卷（用户外部网盘）→ `<bucket>/<rel>`
//     （无前缀直接路径存取）。
//
// bucket 是**桶名**（调用方必须显式传入，2026-10-05 用户裁定）："user"（用户文件）、
// "meta"（元数据）、"cloud"（云任务产物）、"archive"（归档）等——与 storage 既有
// featureBuckets 语义对齐，不假设只有 user 桶。rel 为桶内相对路径（可为空 =
// 仅桶自身，如 mkdir user）。
//
// 与 storage.NormalizeRemote/ValidSegmentName 复用同一路径语义（单一权威），
// volume 包 import storage 无环（storage 不依赖 volume）。

import (
	"errors"
	"strings"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// ErrVolumeNotAuthorized 是用户无权访问卷的哨兵错误（fail-closed：立即报错，
// 不静默降级到其它卷/默认路径——防 ACL 绕过）。
var ErrVolumeNotAuthorized = errors.New("volume: 用户无权访问该卷")

// ErrInvalidUserPath 是用户路径非法（逃逸/注入/保留名）的哨兵错误。
var ErrInvalidUserPath = errors.New("volume: 非法用户路径")

// Owner/Bucket/Path 是卷内位置三字段的 **string 别名**（导出，供包外直接使用）。
// 基础实现用别名表达语义；完整默认实现（Location 结构体）字段即这些类型。
type Owner = string
type Bucket = string
type Path = string

// OwnerBucketLocator 是卷内位置的强类型接口（用户裁定 2026-10-07；接口名表达
// owner+bucket 定位维度，完整默认实现见 Location 结构体）。
//
// **强制所有卷相关函数/方法使用 OwnerBucketLocator 作为 src/dst**——owner 是否已拼接
// 进路径的歧义根除：中间过程 path 绝对未拼接 owner 和 bucket；拼接只发生在 Volume 层
// `v.FSPath(loc)`（按共享性决定是否加 owner 前缀）。FS 无变化，正常提供底层能力。
//
// 默认实现是 `Owner/Bucket/Path` 三字段结构体（见 Location）；接口化便于扩展——后续
// 新卷/新定位方式（如不含 owner 的纯桶定位）可实现本接口，**不需要的调整方法内部
// no-op 即可兼容**（如 Rebucket 对无桶概念的位置原样返回，可内嵌 NoopLocation）。
//
// 调整方法（便于扩展）：Rebucket 换桶；WithPath 重设桶内路径；WithOwner 重设 owner。
type OwnerBucketLocator interface { // NOSONAR: S8196 — 定位语义接口（非 -er 角色命名），设计保留
	// Owner 返回归一 owner（匿名空串的归一由 Volume.FSPath 完成）。
	Owner() Owner
	// Bucket 返回功能桶名（"user"/"meta"/"cloud"/...）。
	Bucket() Bucket
	// Path 返回桶内相对路径（可为空 = 桶自身；**不拼接 owner/bucket**）。
	Path() Path
	// Rebucket 返回同 owner/path、新 bucket 的定位（sidecar 换桶）。
	// 无桶概念的实现 no-op 返回自身。
	Rebucket(bucket Bucket) OwnerBucketLocator
	// WithPath 返回同 owner/bucket、新 path 的定位（重定位）。
	WithPath(path Path) OwnerBucketLocator
	// WithOwner 返回同 bucket/path、新 owner 的定位（跨 owner 重定位）。
	WithOwner(owner Owner) OwnerBucketLocator
	// FSPath 返回该定位在卷内的最终 FS 键（**拼接唯一发生点**，owner 归一 + 卷共享性，
	// 由构造方注入卷上下文——调用方不自行拼接 owner/bucket/path）。
	FSPath() string
}

// Location 是 OwnerBucketLocator 的**完整默认实现**（结构体，导出）。
// **volume 名称与卷指针也内化**（用户裁定 2026-10-07）：构造方（Volume.ResolveLocation）
// 注入 vol+name，locator 完全自足——FS 键经 `FSPath()` 拼接（owner 归一 + 卷共享性），
// 调用方不需要外部 Volume 上下文。字段私有，访问经导出方法。
type Location struct {
	vol    Volume
	name   string // 卷名（装配元数据；可由 vol.Name 派生，保留独立避免依赖 vol 零值）
	owner  Owner
	bucket Bucket
	path   Path
}

// NewLocation 构造 Location（默认绑定零值 Volume——独享语义；共享卷场景请经
// Volume.ResolveLocation 构造（注入 vol+name+shared），不要直接 NewLocation）。
func NewLocation(owner Owner, bucket Bucket, path Path) *Location {
	return &Location{owner: owner, bucket: bucket, path: path}
}

func (l Location) Owner() Owner   { return l.owner }
func (l Location) Bucket() Bucket { return l.bucket }
func (l Location) Path() Path     { return l.path }
func (l Location) Name() string   { return l.name }
func (l Location) Volume() Volume { return l.vol }
func (l Location) FSPath() string {
	owner := storage.NormalizeOwner(l.owner)
	base := l.bucket
	if l.path != "" {
		base = l.bucket + "/" + l.path
	}
	if l.vol.Shared() {
		return owner + "/" + base
	}
	return base
}
func (l Location) Rebucket(bucket Bucket) OwnerBucketLocator {
	return &Location{vol: l.vol, name: l.name, owner: l.owner, bucket: bucket, path: l.path}
}
func (l Location) WithPath(path Path) OwnerBucketLocator {
	return &Location{vol: l.vol, name: l.name, owner: l.owner, bucket: l.bucket, path: path}
}
func (l Location) WithOwner(owner Owner) OwnerBucketLocator {
	return &Location{vol: l.vol, name: l.name, owner: owner, bucket: l.bucket, path: l.path}
}

// NoopLocation 是 OwnerBucketLocator 的 **noop 基础实现**（用户裁定 2026-10-07）：
// 所有调整方法 no-op 返回自身，Owner/Bucket/Path/FSPath/Name/Volume 返回零值。外部
// 扩展可**内嵌 NoopLocation** 快速实现自定义定位——只覆盖需要的字段/方法，不需要的
// 调整（如无 owner/bucket/volume 概念的定位）自动 no-op 兼容。例如：
//
//	type meshLoc struct {
//	    NoopLocation            // 内嵌：Rebucket/WithPath/WithOwner/FSPath 自动 no-op
//	    node Owner              // 扩展字段
//	}
//	func (m meshLoc) Owner() Owner { return m.node }
//
// 零值可用（嵌入后仅需覆盖自己关心的字段访问器）。
type NoopLocation struct{}

func (NoopLocation) Owner() Owner                         { return "" }
func (NoopLocation) Bucket() Bucket                       { return "" }
func (NoopLocation) Path() Path                           { return "" }
func (NoopLocation) FSPath() string                       { return "" }
func (NoopLocation) Name() string                         { return "" }
func (NoopLocation) Volume() Volume                       { return Volume{} }
func (l NoopLocation) Rebucket(Bucket) OwnerBucketLocator { return l }
func (l NoopLocation) WithPath(Path) OwnerBucketLocator   { return l }
func (l NoopLocation) WithOwner(Owner) OwnerBucketLocator { return l }

// _ 断言两种实现都满足 OwnerBucketLocator。
var _ OwnerBucketLocator = NoopLocation{}
var _ OwnerBucketLocator = (*Location)(nil)

// FSPath 返回 Location 在卷内的最终 FS 键（**拼接唯一发生点**）。
//   - 共享卷：`<归一 owner>/<bucket>/<path>`（owner 前缀隔离，不同 owner 靠机制隔离）；
//   - 独享卷：`<bucket>/<path>`（无前缀直接存取）。
//
// 已内化到 locator（Location.FSPath()）——本方法是兼容便捷入口（内部转调 loc.FSPath()，
// 卷共享性由 locator 构造时注入），供已持有 string locator 的旧调用方平滑迁移。
func (v Volume) FSPath(loc OwnerBucketLocator) string {
	return loc.FSPath()
}

// ResolveLocation 把 owner 相对路径解析为强类型 Location（权限门 + 路径安全）。
// bucket 为桶名（"user"/"meta"/...）；rel 为桶内相对路径（可空）。
// 任意参数含绝对路径 / ../ 逃逸 / 空段 / 非法段 → ErrInvalidUserPath。
//
// 取代 ResolveOwnerPath：调用方拿到 Location 后经 loc.FSPath() 得 FS 键（拼接内化
// locator）——不再返回拼接字符串，杜绝「路径是否已带 owner」歧义。
func (v Volume) ResolveLocation(owner Owner, bucket Bucket, rel Path) (Location, error) {
	// 1. 权限门（fail-closed）：无权访问本卷立即报错，不进入路径计算。
	if !v.Authorize(owner) {
		return Location{}, ErrVolumeNotAuthorized
	}
	// 2. bucket 校验（单段合法名——防桶名注入/多段拼写）。
	if !storage.ValidSegmentName(bucket) {
		return Location{}, ErrInvalidUserPath
	}
	// 3. rel 归一（复用 storage 单一权威：拒绝对路径/../空段/Windows 卷名）。
	//    空 rel（仅桶）单独放行（mkdir user 等场景）。
	norm := ""
	if rel != "" {
		n, ok := storage.NormalizeRemote(rel)
		if !ok {
			return Location{}, ErrInvalidUserPath
		}
		norm = n
		// 逐段校验（ValidSegmentName：拒绝对端名/保留设备名/.__ 内部前缀）。
		for seg := range strings.SplitSeq(norm, "/") {
			if !storage.ValidSegmentName(seg) {
				return Location{}, ErrInvalidUserPath
			}
		}
	}
	// Location 持有未拼接字段 + 卷元信息；拼接在 FSPath（内化 locator）。
	return Location{vol: v, name: v.Name, owner: owner, bucket: bucket, path: Path(norm)}, nil
}

// ResolveUserLocation 是 ResolveLocation 的 user 桶便捷封装（用户文件路径）。
func (v Volume) ResolveUserLocation(owner Owner, userPath Path) (Location, error) {
	return v.ResolveLocation(owner, "user", userPath)
}

// BucketOf 解析卷内键的**功能桶段**（用户裁定 2026-10-07：桶解析收敛到 volume 包，
// 避免外部靠字符串匹配误判）。结构识别（非字符串子串匹配，防用户真实目录误判）：
//
//	user/x.bin            → (bucket="user", rest="x.bin")
//	<owner>/user/x.bin    → (bucket="user", rest="x.bin")   // 共享卷 owner 前缀
//	user/dir/meta/x.bin   → (bucket="user", rest="dir/meta/x.bin")  // 内层 meta 是用户目录
//	user/user/x.bin       → (bucket="user", rest="user/x.bin")      // 内层 user 是用户目录
//
// 规则：**首段若为功能桶名 → 桶段=首段**；否则（共享卷 `<owner>/user/...`，且 owner
// 不得为保留桶名——NewTenant 已拒）→ 桶段=次段。返回 ok=false = 无桶段（非桶键）。
func BucketOf(key string) (bucket, rest string, ok bool) {
	segs := splitSegsVolume(key)
	if len(segs) == 0 {
		return "", "", false
	}
	if storage.IsReservedBucketName(segs[0]) {
		return segs[0], joinSegsVolume(segs[1:]), true
	}
	if len(segs) >= 2 && storage.IsReservedBucketName(segs[1]) {
		// 共享卷 `<owner>/<bucket>/...`：owner 首段非桶名（NewTenant 已拒保留桶名 owner），
		// 桶段=次段。桶名后段即 rest。
		return segs[1], joinSegsVolume(segs[2:]), true
	}
	return "", "", false
}

// RebucketTo 把键的 user 桶段替换为目标桶（sidecar 换桶——meta 桶隔离）。与 BucketOf
// 同结构识别：首段命中桶 → 替换首段；否则共享卷 owner 前缀 → 替换次段。返回 ok=false
// = 无桶段（键未含功能桶，原样返回）。
func RebucketTo(key, toBucket string) (string, bool) {
	segs := splitSegsVolume(key)
	if len(segs) == 0 {
		return key, false
	}
	if storage.IsReservedBucketName(segs[0]) {
		rest := joinSegsVolume(segs[1:])
		if rest == "" {
			return toBucket, true
		}
		return toBucket + "/" + rest, true
	}
	if len(segs) >= 2 && storage.IsReservedBucketName(segs[1]) {
		rest := joinSegsVolume(segs[2:])
		if rest == "" {
			return segs[0] + "/" + toBucket, true
		}
		return segs[0] + "/" + toBucket + "/" + rest, true
	}
	return key, false
}

// splitSegsVolume 按 / 拆键段（去空段；键已由 ResolveOwnerPath/NormalizeRemote 校验）。
func splitSegsVolume(key string) []string {
	var out []string
	for _, s := range strings.Split(key, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// joinSegsVolume 拼回 / 分隔键段。
func joinSegsVolume(segs []string) string {
	return strings.Join(segs, "/")
}
