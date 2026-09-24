// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// config_validate.go 是**配置校验**：Validate() 逐段检查安全性/自洽性（监听地址、TLS 与 mTLS 组合、
// 凭据与主密钥、卷 ACL、配额、远程读写与身份 pinning、传输与传输回退等），失败一律以可读错误返回；
// 以及 isLoopbackHost（回环判定，被远程面校验复用）。
//
// 这一节是**唯一**能拒绝非法配置的地方——新增配置项时若只加默认值不加校验，非法值会一路走到运行时。
//
// 拆分说明见 config.go 顶部。

package server

import (
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// Validate 校验配置合理性。
func (c *Config) Validate() error {
	if c.Addr == "" {
		return fmt.Errorf("addr 为空，请配置监听地址")
	}
	if c.StorageRoot == "" {
		return fmt.Errorf("storage_root 为空，请配置存储根目录")
	}
	// max_upload_bytes 可配置（roadmap P0）：负数拒绝（0 = 回落默认，由 SetDefaults 归一）。
	if c.MaxUploadBytes < 0 {
		return fmt.Errorf("max_upload_bytes=%d 非法：不能为负（0 = 默认 1 GiB）", int64(c.MaxUploadBytes))
	}
	// 兜底归一（幂等，仅空值时）：Validate 可能在 Normalize（SetDefaults）前被调
	// （如直接构造 &Config{...}.Validate()）——volumes 空视为未配合成单默认卷
	// （name=default, root=StorageRoot）、placement 空归一 prefer-default，保证后续
	// 卷校验与消费侧总能看到非空 Volumes 与合法 placement（假定或自行归一，两种路径均
	// 得校验）。非空值（含用户显式非法值）不被改写，交由下方校验拒绝。
	if len(c.Volumes) == 0 {
		c.Volumes = []VolumeConfig{{Name: "default", Root: c.StorageRoot}}
	}
	if c.Placement == "" {
		c.Placement = "prefer-default"
	}
	// tier_policy 校验：interval 负值拒绝（fail-closed）；0 = 关闭默认（零回归）。
	// MaxAgeHot/MinSizeHot 负值拒绝（无意义配置）。
	if c.TierPolicy.Interval < 0 {
		return fmt.Errorf("tier_policy.interval 非法 %v：不能为负（0 = 关闭自动降级）", c.TierPolicy.Interval)
	}
	if c.TierPolicy.MaxAgeHot < 0 {
		return fmt.Errorf("tier_policy.max_age_hot 非法 %v：不能为负（0 = 不限龄）", c.TierPolicy.MaxAgeHot)
	}
	if c.TierPolicy.MinSizeHot < 0 {
		return fmt.Errorf("tier_policy.min_size_hot 非法 %v：不能为负（0 = 不限大小）", int64(c.TierPolicy.MinSizeHot))
	}
	// volumes/placement 校验（多卷）。卷名复用 storage.ValidSegmentName 段名规则
	// （拒绝空/绝对/..、.__ 魔法前缀、Windows 保留名与非法字符），与租户/桶段名校验
	// 同一权威。
	switch c.Placement {
	case "prefer-default", "spread":
	default:
		return fmt.Errorf("placement 非法 %q：仅支持 prefer-default|spread", c.Placement)
	}
	seen := make(map[string]bool, len(c.Volumes))
	seenRoots := make(map[string]bool, len(c.Volumes))
	for i := range c.Volumes {
		v := &c.Volumes[i]
		if !storage.ValidSegmentName(v.Name) {
			return fmt.Errorf("卷名 %q 非法（拒绝空/绝对/..、.__ 前缀、Windows 保留名与非法字符）", v.Name)
		}
		if seen[v.Name] {
			return fmt.Errorf("卷名重复 %q", v.Name)
		}
		seen[v.Name] = true
		if v.Root == "" && (v.Type == "" || v.Type == volume.TypeLocal) {
			return fmt.Errorf("卷 %q root 为空（非首卷需显式指定挂载根）", v.Name)
		}
		// 重复 root 拒绝（终审附加）：两卷共享同一物理根会破坏 owner 路径唯一性（同相对路径
		// 可散落两卷）且双卷容量池对同一盘重复记账（配额失守），配置即拒绝（filepath.Clean
		// 归一尾斜杠/点段后等值比较）。
		// 边界：本检查**仅词法等值防呆，非物理唯一性证明**——硬链接/符号链接指向同一目录、
		// 大小写不敏感 FS 的 case 变体、卷 A 根 ⊆ 卷 B 根的嵌套挂载均可绕过字符串等值；装配层
		// OpenRoot（LAYOUT_VERSION）与写路径唯一性强制（T4）为更深层兜底。
		// 外部卷（Type 非空非 local）无本地根：跳过重复 root 检查（Root 恒空）。
		if v.Root != "" {
			rootKey := filepath.Clean(v.Root)
			if seenRoots[rootKey] {
				return fmt.Errorf("卷 root 重复 %q（卷 %q 与其它卷词法等值共享 root；仅防呆，硬链接/符号链接/大小写变体等物理别名不在此列）", v.Root, v.Name)
			}
			seenRoots[rootKey] = true
		}
		if v.VolCapacity < 0 {
			return fmt.Errorf("卷 %q 容量上限 %d 非法：不能为负", v.Name, int64(v.VolCapacity))
		}
		if v.Tier != "" && v.Tier != "hot" && v.Tier != "warm" && v.Tier != "cold" {
			return fmt.Errorf("卷 %q tier %q 非法：仅支持 hot|warm|cold（缺省 hot）", v.Name, v.Tier)
		}
		if a := v.ACL; a != nil {
			if a.Mode != VolumeACLAllow && a.Mode != VolumeACLDeny {
				return fmt.Errorf("卷 %q acl mode %q 非法：仅支持 allow|deny", v.Name, a.Mode)
			}
			for _, o := range a.Owners {
				if !storage.ValidSegmentName(o) {
					return fmt.Errorf("卷 %q acl owners 含非法 owner %q", v.Name, o)
				}
			}
			// Y 一期：mesh_readers 逐条校验（加载期响亮拒绝，fail-closed）。此处排在 owners
			// 校验之后——owners 名单是既有语义，先报既有错误以保持错误面稳定。
			seenReaders := map[string]struct{}{}
			for _, mr := range a.MeshReaders {
				if mr.Node == "" {
					return fmt.Errorf("卷 %q 的 mesh_readers.node 不能为空", v.Name)
				}
				if mr.Owner == "" {
					return fmt.Errorf("卷 %q 的 mesh_readers.owner 不能为空", v.Name)
				}
				if !storage.ValidSegmentName(mr.Owner) {
					return fmt.Errorf("卷 %q 的 mesh_readers.owner 非法 %q（须为合法段名）", v.Name, mr.Owner)
				}
				norm, err := tunnel.ParseFingerprint(mr.Fingerprint)
				if err != nil {
					return fmt.Errorf("卷 %q 的 mesh_readers.fingerprint 非法: %w", v.Name, err)
				}
				// Y 二期 P3：scope 轴校验（取值集合单源在 pkg/volume.NormalizeMeshScope）。
				if _, ok := volume.NormalizeMeshScope(mr.Scope); !ok {
					return fmt.Errorf("卷 %q 的 mesh_readers.scope 非法 %q：仅支持 read|write|rw（缺省 read）",
						v.Name, mr.Scope)
				}
				if _, dup := seenReaders[norm]; dup {
					return fmt.Errorf("卷 %q 的 mesh_readers 指纹重复（归一后）: %s", v.Name, norm)
				}
				seenReaders[norm] = struct{}{}
			}
		}
	}
	// mirror_to 校验（P0 跨卷镜像）：目标卷必须存在、不能指向自身、整体不成环
	// （镜像图是有向无环图）。外部卷（非本地）不镜像（装配层忽略），此处对本地卷校验。
	// 自身/不存在在第一遍内报；成环需要全图遍历（第二遍）。
	// 单目标 mirror_to 校验（P0 跨卷镜像）+ 多目标 mirror_targets（P2 多副本）：
	// 目标必须存在、不能指向自身、不重复；两者互斥（同时设置 → 拒绝）。
	for i := range c.Volumes {
		v := &c.Volumes[i]
		isLocal := v.Type == "" || v.Type == volume.TypeLocal
		if v.MirrorTo != "" && len(v.MirrorTargets) > 0 {
			return fmt.Errorf("卷 %q 的 mirror_to 与 mirror_targets 互斥（二选一）", v.Name)
		}
		if v.MirrorTo != "" {
			if !isLocal {
				continue
			}
			if v.MirrorTo == v.Name {
				return fmt.Errorf("卷 %q 的 mirror_to 不能指向自身", v.Name)
			}
			if !seen[v.MirrorTo] {
				return fmt.Errorf("卷 %q 的 mirror_to 目标卷 %q 不存在", v.Name, v.MirrorTo)
			}
		}
		if len(v.MirrorTargets) > 0 && isLocal {
			dup := map[string]bool{}
			for _, dst := range v.MirrorTargets {
				if dst == "" {
					continue
				}
				if dst == v.Name {
					return fmt.Errorf("卷 %q 的 mirror_targets 不能指向自身", v.Name)
				}
				if !seen[dst] {
					return fmt.Errorf("卷 %q 的 mirror_targets 目标卷 %q 不存在", v.Name, dst)
				}
				if dup[dst] {
					return fmt.Errorf("卷 %q 的 mirror_targets 目标重复: %q", v.Name, dst)
				}
				dup[dst] = true
			}
		}
	}
	// 成环校验：沿镜像链（单目标 mirror_to 或多目标 mirror_targets 首目标）走，
	// 回到已访问卷 = 成环。多目标副本图每轮 pass 都是「源 → 目标」边，目标自身
	// 若又镜像出去，其链同样参与环检测。
	nextTarget := func(v VolumeConfig) string {
		if v.MirrorTo != "" {
			return v.MirrorTo
		}
		if len(v.MirrorTargets) > 0 {
			return v.MirrorTargets[0] // 多目标环以首目标为代表检测（其余同源同向）
		}
		return ""
	}
	for i := range c.Volumes {
		v := &c.Volumes[i]
		if v.Type != "" && v.Type != volume.TypeLocal {
			continue
		}
		visited := map[string]bool{v.Name: true}
		cur := nextTarget(*v)
		for cur != "" {
			if visited[cur] {
				return fmt.Errorf("卷 %q 的镜像链成环（含 %q）", v.Name, cur)
			}
			visited[cur] = true
			nv, ok := c.VolumeByName(cur)
			if !ok {
				break
			}
			cur = nextTarget(nv)
		}
	}
	// audit.buffer_size 不能为负（0 = 关闭，正整数 = 环形容量）。
	if c.Audit.BufferSize < 0 {
		return fmt.Errorf("audit.buffer_size 不能为负，当前 %d（0=关闭，正整数=环形缓冲容量）", c.Audit.BufferSize)
	}
	// 无 auth 配置（凭据 Ring / api_keys 均空）在 Validate 层是合法的——
	// fail-fast 拒绝启动在 cmd/sproxy 侧执行。
	if c.APIKeys.Enabled && len(c.APIKeys.Keys) == 0 {
		return fmt.Errorf("api_keys.enabled=true 但未配置任何密钥，认证将拒绝所有请求")
	}
	for i, k := range c.APIKeys.Keys {
		if k.Key == "" {
			return fmt.Errorf("api_keys[%d].key 为空，密钥不能为空字符串", i)
		}
		switch k.Permission {
		case PermissionRead, PermissionWrite, "":
		default:
			return fmt.Errorf("api_keys[%d].permission=%q 无效，仅允许 %q 或 %q", i, k.Permission, PermissionRead, PermissionWrite)
		}
	}
	// registration 子配置校验（TOTP 登录会话/锁定参数）：login_fail_limit 必须 ≥1
	// （<1 无法锁定，防脚枪）；session_ttl / cli_ttl / login_fail_window 必须 >0。
	if c.Registration.LoginFailLimit < 1 {
		return fmt.Errorf("registration.login_fail_limit=%d 无效，至少为 1（per-AK 失败锁定阈值）", c.Registration.LoginFailLimit)
	}
	if c.Registration.SessionTTL <= 0 {
		return fmt.Errorf("registration.session_ttl=%s 无效，必须大于 0", c.Registration.SessionTTL)
	}
	if c.Registration.CliTTL <= 0 {
		return fmt.Errorf("registration.cli_ttl=%s 无效，必须大于 0", c.Registration.CliTTL)
	}
	if c.Registration.LoginFailWindow <= 0 {
		return fmt.Errorf("registration.login_fail_window=%s 无效，必须大于 0", c.Registration.LoginFailWindow)
	}
	// bucket_limits 校验（任务 2 放行条件 2/3）：
	//   - 键是相对租户根路径（如 user/videos/hd），拒绝 .. / 绝对路径 / 前导斜杠 /
	//     空段 / 空串 / 尾部斜杠——防拼出越界或歧义路径子 Scope（quotaBucketFor 按
	//     filepath.ToSlash(path) 建键，装配期校验与消费保持一致语义）；
	//   - 与功能桶白名单（quotaBucketNames）重叠显式拒绝（fail-closed）：功能桶根上限
	//     恒 0（不单独限制，租户总上限单一执行）。若允许 "user:500" 覆盖功能桶子 Scope
	//     上限，装配顺序（先功能桶 0 后 BucketLimits 500）会做成"user 桶整体 500B 上限"，
	//     与"仅子目录限流"预期相反，且覆盖绕过静默不可查——配置即拒绝，防脚枪。
	for path, limit := range c.BucketLimits {
		n := strings.TrimSpace(path)
		bad := func() bool { // 键合法性：非空、非绝对（/ 或盘符）、非前导/尾部斜杠、无空段/.. 段
			if n == "" || strings.HasPrefix(n, "/") || strings.HasSuffix(n, "/") {
				return true
			}
			if len(n) >= 2 && n[1] == ':' {
				return true // Windows 盘符（C:/x、C:foo）视为绝对路径
			}
			if strings.Contains(n, `\`) {
				return true // 协议路径键恒用 /，拒绝反斜杠（避免跨平台歧义）
			}
			for seg := range strings.SplitSeq(n, "/") {
				if seg == "" || seg == "." || seg == ".." {
					return true
				}
			}
			return false
		}
		if bad() {
			return fmt.Errorf("bucket_limits 键 %q 非法：必须为相对租户根路径（如 user/videos/hd），不允许空、前导/尾部斜杠或 .. 段", path)
		}
		if !storage.ValidSegmentName(segNameOfBucketPath(n)) {
			return fmt.Errorf("bucket_limits 键 %q 含非法段（拒绝空/绝对/..、.__ 魔法前缀、Windows 保留名与非法字符）", path)
		}
		if slices.Contains(quotaBucketNames, n) {
			return fmt.Errorf("bucket_limits 键 %q 与功能桶根重叠：功能桶根上限由租户总 owner_quotas 单一执行，不支持单独 bucket_limits 覆盖", path)
		}
		// 键首段必须为 "user"（分层配额仅挂 user 桶 children 下）。cloud/archive/chunk/version
		// 桶内无用户子目录目录（archive/<name>、cloud/<taskID>），对它们配子目录永不生效，
		// 配置即拒绝防误导（fail-closed）。
		if first, _, ok := strings.Cut(n, "/"); !ok || first != "user" {
			return fmt.Errorf("bucket_limits 键 %q 非法：分层配额仅支持 user 桶子目录（如 user/videos/hd），其余功能桶无子目录结构", path)
		}
		if limit < 0 {
			return fmt.Errorf("bucket_limits[%q] 上限 %d 非法：配额上限不能为负", path, int64(limit))
		}
	}
	if c.RateLimit.Enabled && c.RateLimit.Requests <= 0 {
		return fmt.Errorf("rate_limit.enabled=true 但 requests=%d 无效，请设置大于 0 的值", c.RateLimit.Requests)
	}
	if c.RateLimit.Enabled && c.RateLimit.Window <= 0 {
		return fmt.Errorf("rate_limit.enabled=true 但 window=%s 无效，请设置大于 0 的 duration", c.RateLimit.Window)
	}
	// telemetry 装配校验：仅 telemetry.enabled=true 时校验采样率与显式 OTLP 端点。
	// 采样率必须 ∈ (0,1]（ParentBased(TraceIDRatioBased) 合法输入）；显式
	// otlp_endpoint 必须为 http(s) 且带 host（空 = 仅走标准环境变量，合法）。
	if c.Telemetry.Enabled {
		if c.Telemetry.SampleRatio <= 0 || c.Telemetry.SampleRatio > 1 {
			return fmt.Errorf("telemetry.sample_ratio 必须 ∈ (0,1]，当前 %v", c.Telemetry.SampleRatio)
		}
		if c.Telemetry.OTLPEndpoint != "" {
			u, perr := url.Parse(c.Telemetry.OTLPEndpoint)
			if perr != nil {
				return fmt.Errorf("telemetry.otlp_endpoint %q 非法: %v", c.Telemetry.OTLPEndpoint, perr)
			}
			if u.Scheme != "http" && u.Scheme != "https" {
				return fmt.Errorf("telemetry.otlp_endpoint scheme %q 无效，仅允许 http/https", u.Scheme)
			}
			if u.Host == "" {
				return fmt.Errorf("telemetry.otlp_endpoint 缺少 host: %q", c.Telemetry.OTLPEndpoint)
			}
		}
	}
	if c.RemoteRead.Enabled {
		// 跨节点只读面（Y 一期）。三项校验全部 fail-closed：
		//  1. 强制 loopback——只读面只允许本机 mesh 数据面接入，远程访问须经 mesh
		//     而非直连；配非 loopback 直接拒绝启动（与网关安全边界同构）。
		//  2. 握手超时为正——0/负会被 tunnel 当成「用默认值」以外的歧义语义。
		//  3. **必须至少有一条 mesh_readers 指纹**。这是控制者明令的不可省略门禁：
		//     tunnel.DeriveRemoteStaticKey 由**公开的 listener 身份指纹**派生（不是
		//     秘密），且该值在 Tunnel 中兼作 dialer 侧握手失败时的**回退加密密钥**
		//     （见 pkg/tunnel/remote_key.go 的安全前提与 tunnel_mux.go 的 default 分支）。
		//     B 侧「无 pin 就不接受任何对端」是「静态密钥回退不可达」这一安全论证的
		//     必要条件——**不得为「方便调试」删除本校验**，否则会留下无 pin 也能跑的路径。
		if c.RemoteRead.Listen == "" {
			return fmt.Errorf("remote_read.listen 不能为空")
		}
		host, _, err := net.SplitHostPort(c.RemoteRead.Listen)
		if err != nil {
			return fmt.Errorf("remote_read.listen 格式非法: %w", err)
		}
		if !isLoopbackHost(host) {
			return fmt.Errorf("remote_read.listen 必须绑定 loopback（远程访问应经 mesh 而非直连）: %q", c.RemoteRead.Listen)
		}
		if c.RemoteRead.HandshakeTimeout <= 0 {
			return fmt.Errorf("remote_read.handshake_timeout 必须为正，当前 %v", c.RemoteRead.HandshakeTimeout)
		}
		if len(meshReaderFingerprints(c)) == 0 {
			return fmt.Errorf("remote_read.enabled 但未配置任何 volumes[].acl.mesh_readers —— 无 pin 将接受任意对端，拒绝启动（fail-closed）")
		}
	}
	if c.RemoteWrite.Enabled {
		// 跨节点写面（Y 二期 P3-b）。四项校验全部 fail-closed（前三项与只读面同构）：
		//  1. 强制 loopback——写面只允许本机 mesh 数据面接入；
		//  2. 握手超时为正；
		//  3. **必须至少一条 scope 授予写的条目**：写面的 pin 列表只取「能写」的指纹，
		//     若无此类条目则 pin 为空 ⇒ listener 拒绝启动（无 pin 将接受任意对端）；
		//  4. 只读条目不算数——「配了 remote_write 却只有 read 条目」是配置脚枪，必须响亮拒绝
		//     （否则写面起来了但每个请求都 404，运维会误以为网络问题）。
		if c.RemoteWrite.Listen == "" {
			return fmt.Errorf("remote_write.listen 不能为空")
		}
		host, _, err := net.SplitHostPort(c.RemoteWrite.Listen)
		if err != nil {
			return fmt.Errorf("remote_write.listen 格式非法: %w", err)
		}
		if !isLoopbackHost(host) {
			return fmt.Errorf("remote_write.listen 必须绑定 loopback（远程访问应经 mesh 而非直连）: %q", c.RemoteWrite.Listen)
		}
		if c.RemoteWrite.HandshakeTimeout <= 0 {
			return fmt.Errorf("remote_write.handshake_timeout 必须为正，当前 %v", c.RemoteWrite.HandshakeTimeout)
		}
		if len(meshWriterFingerprints(c)) == 0 {
			return fmt.Errorf("remote_write.enabled 但没有任何 volumes[].acl.mesh_readers 条目的 scope 授予写（write|rw）—— 无写条目时写面恒拒且无 pin 可接受，拒绝启动（fail-closed）")
		}
	}
	if hasKindMeshRemote(c) {
		// A 侧 mesh 客户端（Y 二期）：只为「确有 kind=mesh 远端」的部署把关——没配 mesh 远端时
		// 本段配置无用，不做额外校验（避免未使用配置引发启动失败）。
		//
		// 1) 远端 hub：必须齐备 SproxySig 凭据（空凭据只会 401，且要到任务运行期才暴露，
		//    无从排障）；URL 必须是 http(s)。
		if c.Mesh.HubURL != "" {
			u, err := url.Parse(c.Mesh.HubURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("mesh.hub_url 非法（应为 http(s)://host:port）: %q", c.Mesh.HubURL)
			}
			if c.Mesh.AccessKey == "" || c.Mesh.AccessKeySecret == "" {
				return fmt.Errorf("mesh.hub_url 指向远端 hub 时必须配置 mesh.access_key/access_key_secret（fail-closed）")
			}
			if c.Mesh.SkeyID == "" {
				return fmt.Errorf("mesh.hub_url 指向远端 hub 时必须配置 mesh.skey_id（SproxySig v2 必传）")
			}
		}
		// 2) 显式 WebRTC 打洞必须有信令（node_id）；否则运行期只能失败或静默降级。
		for _, r := range c.SyncRemotes {
			if syncmgr.RemoteKind(r.Kind) != syncmgr.RemoteKindMesh {
				continue
			}
			if r.Transport == "webrtc" && c.Mesh.NodeID == "" {
				return fmt.Errorf("sync_remotes[%s] transport=webrtc 需要 mesh.node_id（WebRTC 信令必需；"+
					"无信令时请用 transport=auto|relay）", r.Name)
			}
		}
	}
	if c.Mesh.Node.Enabled {
		// B 侧 mesh node 角色（S5）：三项 fail-fast——
		//  1) 必须有稳定 node_id（node.node_id → mesh.node_id → hub.node_id）；
		//  2) 必须**至少有一个可宣告的服务来源**（远近面启用，或 extra_services 非空）——
		//     否则节点注册后无服务可发现，属配置脚枪；
		//  3) 若注册目标是**远端 hub**（node.hub_url / mesh.hub_url 非空）则必须齐备 SproxySig 凭据。
		if c.MeshNodeID() == "" {
			return fmt.Errorf("mesh.node.enabled 需要节点 ID（mesh.node.node_id / mesh.node_id / hub.node_id 皆空）")
		}
		hasFace := c.RemoteRead.Enabled || c.RemoteWrite.Enabled
		if !hasFace && len(c.Mesh.Node.ExtraServices) == 0 {
			return fmt.Errorf("mesh.node.enabled 但没有任何可宣告的服务：请启用 remote_read/remote_write，" +
				"或用 mesh.node.extra_services 显式声明（name:host:port）")
		}
		if hub := c.MeshNodeHubURL(); hub != "" {
			u, err := url.Parse(hub)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
				return fmt.Errorf("mesh.node.hub_url 非法（应为 http(s)/ws(s)://host:port）: %q", hub)
			}
			if c.Mesh.AccessKey == "" || c.Mesh.AccessKeySecret == "" {
				return fmt.Errorf("mesh.node.enabled 且注册到远端 hub 时必须配置 mesh.access_key/access_key_secret（fail-closed）")
			}
		}
	}
	if c.Hub.Enabled && !c.Hub.Transports.WS.Enabled && !c.Hub.Transports.TCP.Enabled && !c.Hub.Transports.QUIC.Enabled {
		// S42 演进：节点接入传输 = ws（挂载主 HTTP server）/ tcp（独立 raw TCP
		// listener）/ quic（独立 UDP listener）。hub 启用而三者皆关时节点无法注册，
		// 属配置脚枪，fail-fast 启动失败。
		return fmt.Errorf("hub.enabled=true 但 transports.ws.enabled、transports.tcp.enabled 与 transports.quic.enabled 均为 false，中继节点无法连接，请至少启用一种传输")
	}
	if c.Hub.Enabled && c.Hub.Transports.TCP.Enabled && c.Hub.Transports.TCP.Listen != "" {
		// 端口冲突校验：TCP 中继是独立 raw TCP listener，不能与主 HTTP server（addr）
		// 同端口（同端口绑定会在启动时失败，这里提前给清晰错误）。比较 host:port 的
		// port 段；非 host:port 或 :0（随机端口）跳过（由 OS 绑定兜底）。
		if _, tcpPort, tcpErr := net.SplitHostPort(c.Hub.Transports.TCP.Listen); tcpErr == nil && tcpPort != "0" {
			if _, httpPort, httpErr := net.SplitHostPort(c.Addr); httpErr == nil && httpPort != "0" && tcpPort == httpPort {
				return fmt.Errorf("hub.transports.tcp.listen 端口 %s 与主 HTTP 监听 addr 端口 %s 冲突（TCP 中继与 HTTP server 不能同端口），请改配 transports.tcp.listen", tcpPort, httpPort)
			}
		}
	}
	if c.Hub.Enabled && c.Hub.Transports.QUIC.Enabled && c.Hub.Transports.QUIC.Listen != "" {
		// 端口冲突校验：QUIC 中继是独立 raw UDP listener（QUIC over UDP），不能与
		// 主 HTTP server（addr）同端口（同端口绑定会在启动时失败，这里提前给清晰错误）。
		if _, quicPort, quicErr := net.SplitHostPort(c.Hub.Transports.QUIC.Listen); quicErr == nil && quicPort != "0" {
			if _, httpPort, httpErr := net.SplitHostPort(c.Addr); httpErr == nil && httpPort != "0" && quicPort == httpPort {
				return fmt.Errorf("hub.transports.quic.listen 端口 %s 与主 HTTP 监听 addr 端口 %s 冲突（QUIC 中继与 HTTP server 不能同端口），请改配 transports.quic.listen", quicPort, httpPort)
			}
		}
	}
	if c.Hub.Enabled && c.Hub.DHT != "" && c.Hub.DHT != "kad" {
		// 防配置打错字（"kademlia" 等）被静默忽略。门控在 hub.enabled：hub 未启用时
		// dht 不被消费，历史/闲置配置遗留不阻断启动（与 ws transport 校验一致）。
		return fmt.Errorf("hub.dht=%q 无效，仅支持 \"\"（内置内存 DHT）或 \"kad\"（Kademlia）", c.Hub.DHT)
	}
	if c.Hub.VirtualSubnet != "" {
		// 虚拟 IP 分配仅支持 IPv4（确定性分配与递增分配均做 IPv4 算术）。非法/非 IPv4
		// CIDR 在启动时拒绝，防止分配器构造时 panic 或产生不可路由地址（M-3）。
		prefix, perr := netip.ParsePrefix(c.Hub.VirtualSubnet)
		if perr != nil {
			return fmt.Errorf("hub.virtual_subnet=%q 非法: %v", c.Hub.VirtualSubnet, perr)
		}
		if !prefix.Addr().Is4() {
			return fmt.Errorf("hub.virtual_subnet=%q 必须是 IPv4 CIDR（虚拟 IP 分配仅支持 IPv4）", c.Hub.VirtualSubnet)
		}
	}
	// hub 联邦配置校验（S4F）：URL 合法性 + 远程 peering 凭据强制（fail-closed）。
	// 门控在 hub.federation.enabled：hub 未启用或联邦关闭时 peers 不被消费，
	// 历史/闲置配置遗留不阻断启动。
	if c.Hub.Federation.Enabled {
		seenPeerIDs := make(map[string]struct{}, len(c.Hub.Federation.Peers))
		for i, p := range c.Hub.Federation.Peers {
			peerURL := p.URL
			if peerURL == "" {
				// 空 URL 回落默认 loopback（安全面：默认只与本机 hub peering）。
				// 仍以默认 URL 参与重复检测——两个空 URL peer 都回落同一默认
				// 地址属配置冲突（运行时后写覆盖），启动时拦截。
				peerURL = hub.DefaultFederationPeerURL
			}
			u, perr := url.Parse(peerURL)
			if perr != nil {
				return fmt.Errorf("hub.federation.peers[%d].url 非法: %v", i, perr)
			}
			if u.Scheme != "http" && u.Scheme != "https" {
				return fmt.Errorf("hub.federation.peers[%d].url scheme %q 无效，仅允许 http/https", i, u.Scheme)
			}
			if p.URL != "" && !isLoopbackHost(u.Hostname()) && (p.AccessKey == "" || p.AccessKeySecret == "") {
				// 远程 peering 必须显式成对配置凭据（AccessKey + AccessKeySecret）——
				// 缺失任一即无有效签名，未配置时无认证直连远程 hub 属暴露面，fail-closed 拒绝。
				return fmt.Errorf("hub.federation.peers[%d].url %q 为远程地址，远程 peering 必须同时配置 access_key 与 access_key_secret", i, p.URL)
			}
			if p.AccessKeySecret != "" {
				// 与凭据 SK 的校验一致：SK 必须为 64 hex（32 字节 HMAC 密钥源）。
				if len(p.AccessKeySecret) != 64 {
					return fmt.Errorf("hub.federation.peers[%d].access_key_secret 必须为 64 个十六进制字符（32 字节），got %d 字符", i, len(p.AccessKeySecret))
				}
				if _, derr := hex.DecodeString(p.AccessKeySecret); derr != nil {
					return fmt.Errorf("hub.federation.peers[%d].access_key_secret 不是合法十六进制: %v", i, derr)
				}
			}
			// TLS 安全边界（S-Medium 闭环）：insecure_skip_verify 仅限 loopback peer
			// （本机自签开发/测试）；远程 peer 必须严格校验 TLS（受信任证书或 ca_file），
			// 跳过校验 = MITM 可窃听/篡改节点表，fail-closed 拒绝。
			if p.InsecureSkipVerify && !isLoopbackHost(u.Hostname()) {
				return fmt.Errorf("hub.federation.peers[%d].insecure_skip_verify 仅允许用于 loopback peer（本机自签开发）；远程 peering 应配置受信任证书或 ca_file（受信 CA）", i)
			}
			// ca_file 与 insecure_skip_verify 互斥（ca_file 是严格校验，跳过校验与其冲突）。
			if p.CAFile != "" && p.InsecureSkipVerify {
				return fmt.Errorf("hub.federation.peers[%d].ca_file 与 insecure_skip_verify 互斥，请二选一（ca_file 为受信 CA 严格校验）", i)
			}
			if p.CAFile != "" {
				if _, serr := os.Stat(p.CAFile); serr != nil {
					return fmt.Errorf("hub.federation.peers[%d].ca_file %q 不可读: %v", i, p.CAFile, serr)
				}
			}
			key := p.ID
			if key == "" {
				key = peerURL
			}
			if _, dup := seenPeerIDs[key]; dup {
				return fmt.Errorf("hub.federation.peers[%d].id %q 重复", i, p.ID)
			}
			seenPeerIDs[key] = struct{}{}
		}
	}
	// sync_remotes 校验：URL 合法（http/https + host）、name 唯一非空。
	// 凭据 fail-closed 在 SyncManager.CreateTask 层执行（Validate 不要求凭据——
	// 允许配置空凭据的 remote 供未登记凭据的远程节点使用，创建任务时才拒绝）。
	seenSyncRemoteNames := make(map[string]struct{}, len(c.SyncRemotes))
	for i, r := range c.SyncRemotes {
		if r.Name == "" {
			return fmt.Errorf("sync_remotes[%d].name 为空，名称不能为空字符串", i)
		}
		if _, dup := seenSyncRemoteNames[r.Name]; dup {
			return fmt.Errorf("sync_remotes[%d].name %q 重复", i, r.Name)
		}
		seenSyncRemoteNames[r.Name] = struct{}{}
		switch r.Kind {
		case "", "direct":
			// HTTP 直连（现状）
		case "mesh":
			// mesh 载体：不需要 URL，但**必须**有 node/volume/peer_pins（fail-closed：
			// 无 pin 的 mesh 目标会让隧道接受任意对端，安全论证失效）。
			if r.Node == "" {
				return fmt.Errorf("sync_remotes[%d]（kind=mesh）.node 为空", i)
			}
			if r.Volume == "" {
				return fmt.Errorf("sync_remotes[%d]（kind=mesh）.volume 为空", i)
			}
			if len(r.PeerPins) == 0 {
				return fmt.Errorf("sync_remotes[%d]（kind=mesh）.peer_pins 为空（fail-closed：无指纹 pin 将接受任意对端）", i)
			}
			if r.Transport != "" && r.Transport != "auto" && r.Transport != "relay" && r.Transport != "webrtc" {
				return fmt.Errorf("sync_remotes[%d]（kind=mesh）.transport %q 无效（可选 auto|relay|webrtc）", i, r.Transport)
			}
			continue
		case "baidupcs", "volume":
			// 本机卷载体（baidupcs = 兼容别名，归一到 volume）：不需要 URL/凭据；**必须**有
			// volume（本机卷名，供装配层按名查 Set.External——volumes[] type=xxx 或用户卷）。
			if r.Volume == "" {
				return fmt.Errorf("sync_remotes[%d]（kind=%s）.volume 为空（本机卷名）", i, r.Kind)
			}
			continue
		default:
			return fmt.Errorf("sync_remotes[%d].kind %q 无效（可选 direct|mesh|volume）", i, r.Kind)
		}
		u, perr := url.Parse(r.URL)
		if perr != nil {
			return fmt.Errorf("sync_remotes[%d].url 非法: %v", i, perr)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("sync_remotes[%d].url scheme %q 无效，仅允许 http/https", i, u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("sync_remotes[%d].url 缺少 host: %q", i, r.URL)
		}
		// 明文 http 仅限 loopback（本机调试）：远程 remote 用 http 会把 SproxySig
		// AK/SK 明文上线，对齐联邦 peering 的 TLS 安全边界（安全审查 MEDIUM）。
		if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
			return fmt.Errorf("sync_remotes[%d].url 使用明文 http 且非 loopback（AK/SK 将明文上线；远程 remote 请用 https，本机调试可用 http://127.0.0.1）: %q", i, r.URL)
		}
	}
	// baidupcs 系统盘并入 volumes[]（V3 接入 T2）：type=baidupcs 的外部卷需 extra.bduss 或
	// extra.binary_path 至少一个非空（fail-closed：无可用执行路径拒绝，而非静默跳过）。
	// extra 键名 bduss/baidu_root/binary_path/local_root 与 baidupcs backend 构造器读取一致
	// （单一事实源）；local_root 可选（空 = 回落 v.RootDir，外部卷 RootDir 恒空 → 系统默认
	// os.TempDir()）。
	// 本地卷（Type 空/local）不检查 extra（零迁移）。
	// 首卷必本地（V3 装配层 fail-closed），baidupcs 盘排后。
	for i := range c.Volumes {
		v := &c.Volumes[i]
		if v.Type != "" && v.Type != volume.TypeLocal && v.Type == "baidupcs" {
			bduss, _ := v.Extra["bduss"].(string)
			binaryPath, _ := v.Extra["binary_path"].(string)
			if bduss == "" && binaryPath == "" {
				return fmt.Errorf("卷 %q（type=baidupcs）需配置 extra.bduss 或 extra.binary_path 至少一个（fail-closed：否则二进制优先与库兜底都无可用执行路径）", v.Name)
			}
		}
	}
	// webdav 系统盘（V2）：volumes[] type=webdav 的外部卷需 extra.url（http(s)）必填 +
	// username/password 或 token 认证至少一组（fail-closed：WebDAV 无匿名目标）。
	// extra 键名 url/username/password/token/local_root 与 webdav backend 构造器读取一致（单一事实源）。
	for i := range c.Volumes {
		v := &c.Volumes[i]
		if v.Type != "" && v.Type != volume.TypeLocal && v.Type == "webdav" {
			rawURL, _ := v.Extra["url"].(string)
			u, perr := url.Parse(strings.TrimSpace(rawURL))
			if rawURL == "" || perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("卷 %q（type=webdav）需配置 extra.url（http(s)://host[:port][/webdav-root]）", v.Name)
			}
			username, _ := v.Extra["username"].(string)
			password, _ := v.Extra["password"].(string)
			token, _ := v.Extra["token"].(string)
			if token == "" && (username == "" || password == "") {
				return fmt.Errorf("卷 %q（type=webdav）需配置认证（extra.username+password 或 extra.token 至少一组）", v.Name)
			}
		}
	}
	// s3 系统盘（V2，pkg/volume/ext/s3）：volumes[] type=s3 的外部卷需 extra.endpoint/bucket/access_key/secret_key
	// 必填（fail-closed：S3 无匿名目标）——endpoint 为 host[:port] 或 http(s)://host[:port]。
	// extra 键名 endpoint/bucket/access_key/secret_key/region/use_ssl 与 s3 backend 构造器读取一致（单一事实源）。
	for i := range c.Volumes {
		v := &c.Volumes[i]
		if v.Type != "" && v.Type != volume.TypeLocal && v.Type == "s3" {
			endpoint, _ := v.Extra["endpoint"].(string)
			endpoint = strings.TrimSpace(endpoint)
			if endpoint == "" {
				return fmt.Errorf("卷 %q（type=s3）需配置 extra.endpoint（S3 服务地址 host[:port]）", v.Name)
			}
			if u, perr := url.Parse(endpoint); perr == nil && (u.Scheme == "http" || u.Scheme == "https") {
				if u.Host == "" {
					return fmt.Errorf("卷 %q（type=s3）extra.endpoint 非法（http(s)://host[:port]）: %q", v.Name, endpoint)
				}
			}
			bucket, _ := v.Extra["bucket"].(string)
			if strings.TrimSpace(bucket) == "" {
				return fmt.Errorf("卷 %q（type=s3）需配置 extra.bucket（桶名）", v.Name)
			}
			ak, _ := v.Extra["access_key"].(string)
			sk, _ := v.Extra["secret_key"].(string)
			if strings.TrimSpace(ak) == "" || strings.TrimSpace(sk) == "" {
				return fmt.Errorf("卷 %q（type=s3）需配置认证（extra.access_key + extra.secret_key）", v.Name)
			}
		}
	}
	// credential_store 加密装配校验（4C-2 / Vault Transit）：先校验 backend 枚举，再按
	// backend 分支校验 Encrypt=true 的密钥来源——
	//   - aesgcm（缺省/空）：必须能解析出 master key（master_key_file 非空，文件可读性由
	//     装配层校验，见 BootstrapServerCredentials；或环境变量 CredentialMasterKeyEnv 已设）；
	//     二者皆无 fail-fast（防误开加密后启动即解密失败、用空凭据表运行）。
	//   - vault：要求 vault 子段 addr/key_name 与 token 源（token_file 或 TokenEnv 环境变量）
	//     齐全；忽略 master_key_file（Vault 持 key，无需本地 master key）。
	switch c.CredentialStore.Backend {
	case "", "aesgcm", "vault":
	default:
		return fmt.Errorf("credential_store.backend=%q 无效，仅允许 aesgcm 或 vault", c.CredentialStore.Backend)
	}
	if c.CredentialStore.Encrypt {
		switch c.CredentialStore.Backend {
		case "vault":
			if c.CredentialStore.Vault.Addr == "" {
				return fmt.Errorf("credential_store.backend=vault 需配置 credential_store.vault.addr")
			}
			// vault.addr 解析 + scheme 校验（对齐 sync_remotes 先例）。非 loopback 主机
			// 必须 https——http 明文传输 Vault token + 凭据属泄露向量（安全审查 MEDIUM）；
			// loopback 允许 http（dev 容器 http://127.0.0.1:8200）。
			u, perr := url.Parse(c.CredentialStore.Vault.Addr)
			if perr != nil {
				return fmt.Errorf("credential_store.vault.addr=%q 非法: %v", c.CredentialStore.Vault.Addr, perr)
			}
			if u.Scheme != "http" && u.Scheme != "https" {
				return fmt.Errorf("credential_store.vault.addr=%q scheme %q 非法，仅允许 http/https", c.CredentialStore.Vault.Addr, u.Scheme)
			}
			if u.Host == "" {
				return fmt.Errorf("credential_store.vault.addr=%q 缺少 host", c.CredentialStore.Vault.Addr)
			}
			if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
				return fmt.Errorf("credential_store.vault.addr=%q 非 loopback 必须使用 https（防 Vault token/凭据明文传输）", c.CredentialStore.Vault.Addr)
			}
			if c.CredentialStore.Vault.KeyName == "" {
				return fmt.Errorf("credential_store.backend=vault 需配置 credential_store.vault.key_name")
			}
			tokEnv := c.CredentialStore.Vault.TokenEnv
			if tokEnv == "" {
				tokEnv = "VAULT_TOKEN"
			}
			if c.CredentialStore.Vault.TokenFile == "" && os.Getenv(tokEnv) == "" {
				return fmt.Errorf("credential_store.backend=vault 需配置 credential_store.vault.token_file 或环境变量 %s", tokEnv)
			}
		default: // aesgcm / ""（向后兼容）
			if c.CredentialStore.MasterKeyFile == "" && os.Getenv(CredentialMasterKeyEnv) == "" {
				return fmt.Errorf("credential_store.encrypt=true 需配置 credential_store.master_key_file 或环境变量 %s（base64 编码 32B master key）", CredentialMasterKeyEnv)
			}
		}
	}
	// 调度器维护窗口校验（roadmap 11.10-H2）：启用时 start/end 必须 HH:MM（24h）
	// fail-closed（非法值响亮拒绝，对齐配置校验整体策略）；未启用时忽略（零回归）。
	if c.Scheduler.MaintenanceWindow.Enabled {
		if !isHHMM(c.Scheduler.MaintenanceWindow.Start) {
			return fmt.Errorf("scheduler.maintenance_window.start=%q 非法：必须 HH:MM（24h）", c.Scheduler.MaintenanceWindow.Start)
		}
		if !isHHMM(c.Scheduler.MaintenanceWindow.End) {
			return fmt.Errorf("scheduler.maintenance_window.end=%q 非法：必须 HH:MM（24h）", c.Scheduler.MaintenanceWindow.End)
		}
	}
	return nil
}

// parseMaintenanceWindow 把调度器维护窗口配置解析为窗口内判定函数。
// 未启用或 start/end 非法 → nil（恒执行，零回归）。调用方（装配层）在
// Validate 之后调用（配置已校验 HH:MM）。End<=Start 视为跨午夜窗口。
func parseMaintenanceWindow(c SchedulerConfig) func(time.Time) bool {
	w := c.MaintenanceWindow
	if !w.Enabled || w.Start == "" || w.End == "" {
		return nil
	}
	sh, sm, ok1 := parseHHMM(w.Start)
	eh, em, ok2 := parseHHMM(w.End)
	if !ok1 || !ok2 {
		return nil
	}
	startMin := sh*60 + sm
	endMin := eh*60 + em
	if startMin == endMin {
		// 全天窗口（开始 == 结束）：恒在窗口内。
		return func(time.Time) bool { return true }
	}
	if startMin < endMin {
		// 同日窗口：start <= t < end。
		return func(t time.Time) bool {
			m := t.Hour()*60 + t.Minute()
			return m >= startMin && m < endMin
		}
	}
	// 跨午夜窗口：t >= start || t < end。
	return func(t time.Time) bool {
		m := t.Hour()*60 + t.Minute()
		return m >= startMin || m < endMin
	}
}

// isHHMM 报告 s 是否为 HH:MM 24h 格式（00:00-23:59）。
func isHHMM(s string) bool {
	_, _, ok := parseHHMM(s)
	return ok
}

// parseHHMM 解析 "HH:MM"（24h）为 (hour, minute, ok)。
func parseHHMM(s string) (int, int, bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, 0, false
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	// 逐字符数字校验（防非数字字符经 ASCII 算术混入）。
	for _, c := range s {
		if c == ':' {
			continue
		}
		if c < '0' || c > '9' {
			return 0, 0, false
		}
	}
	return h, m, true
}

// VolumeByName 按卷名查找卷配置（未找到返回 (zero, false)）。
func (c *Config) VolumeByName(name string) (VolumeConfig, bool) {
	for _, v := range c.Volumes {
		if v.Name == name {
			return v, true
		}
	}
	return VolumeConfig{}, false
}

// isLoopbackHost 判断主机名是否为 loopback（IPv4/IPv6 loopback 或 localhost）。
// 用于联邦 peering 的安全边界：默认 loopback 安全面，远程 peering 需显式配置。
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	// net.SplitHostPort 对 IPv6 返回带方括号的 host（如 "[::1]"），strip 后判断。
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
