// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// secret_register.go 是 secret 加密卷的装配（设计 docs/designs/2026-10-01-secret-volume.md
// §9）：把 secrets 卷（secret 文件管理）与 secretdata 卷（加密数据 wrapper）注册为
// volumes[] 的 backend 插件。
//
// 装配顺序（secretdata 依赖 secrets 卷；secrets 卷可嵌套）：
//  1. 装配配置声明的 secrets 卷（Extra.target 的 secrets/ 目录）；
//  2. 装配 config 声明的 secretdata 卷（secret_url → secrets 卷读密钥，推迟装配）。
//
// **默认 secrets 不是装配对象**（用户裁定 2026-10-15）：它是 per-owner 的
// `<storage_root>/<owner>/meta/secrets/` 目录（owner 租户根的 meta 桶内），由 `/api/secrets`
// 按请求 owner 解析并按需派生（pkg/server/secretsManagerFor），故本文件不再有「启动默认建本地卷」这一步。
//
// secrets 卷由装配层把底层卷的 secrets/ 前缀视图 FS 注入（secrets.NewBackend 签名），
// secretdata 卷由装配层解析 secret_url 到密钥字节后注入（secretdata.NewBackend 签名）。

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/units/sizex"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/capacity"
	"github.com/cocomhub/sproxy/pkg/volume/enclosure"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/secretdata"
	"github.com/cocomhub/sproxy/pkg/volume/secrets"
)

// ---- secrets backend ----

// secretsLocalFS 把由 rootDir 传入的依赖 caller 决定视图的本地 FS 视图建为 LocalFS。
// 调用方传 rootDir 时即期望集成 secrets 目录（本函数负责拼接 /secrets）。
//
// **注意**：仅用于「目标目录不在任何受管卷根之内」的独立目录；落在卷根之内的目录必须走
// `secretsFSFor`（经 storage.Root：at-rest 加解密 + 封印 + 用户可见长度），否则会在加密卷上
// 明文落密钥（门禁 R26 登记项，见 `pkg/server/secrets_fs.go` 的文件头）。
func secretsLocalFS(rootDir string) syncpkg.FS {
	return syncpkg.NewLocalFS(rootDir+"/secrets", nil)
}

// secretsFSFor 解析 secrets 目录（绝对路径 absDir）的 FS 视图：
//   - **落在某个已装配卷根之内** → `server.NewRootSecretsFS(volRoot, <卷根相对目录>, logger)`：
//     经 Root ⇒ at-rest 透明加解密、封印、用户可见长度全部生效（加密卷上密钥密文落盘），
//     权限 0600 经 `ChmodSecret` 回调保持（该回调由 `secrets.NewBackend` 依 FS 能力**自动**
//     接线，见其文档；无兼容迁移：见 `pkg/server/secrets_fs.go` 文件头）；
//   - **独立目录**（不在任何卷根内）→ 裸 `NewLocalFS`（无卷语义可经，零回归；R26 已登记）。
//
// 为什么按「路径归属」而不是按配置来源判定：`extra.root`/`storage_root` 都只是路径文本，
// 是否落在卷根内是**部署事实**——按事实判定才能一处收口 `type: secrets` 卷的装配入口。
func secretsFSFor(set *registry.Set, absDir string, logger *slog.Logger) syncpkg.FS {
	clean := filepath.Clean(absDir)
	if rt, relDir, ok := owningVolumeRoot(set, clean); ok {
		return server.NewRootSecretsFS(rt, relDir, logger)
	}
	// 独立目录：保持既有裸 LocalFS 语义（无卷语义可经 ⇒ 无加解密/封印可依附，R26 已登记）。
	return syncpkg.NewLocalFS(clean, logger)
}

// owningVolumeRoot 在已装配卷集中查找**包含** dir 的卷根（返回该根与 dir 的卷根相对路径）。
// 判据是路径归属（部署事实），而非配置来源——`extra.root`/`storage_root` 都只是路径文本。
func owningVolumeRoot(set *registry.Set, dir string) (*storage.Root, string, bool) {
	if set == nil {
		return nil, "", false
	}
	for _, v := range set.All() {
		rt := set.Root(v.Name)
		if rt == nil {
			continue
		}
		relDir, rerr := filepath.Rel(filepath.Clean(rt.AbsPath()), dir)
		if rerr != nil || relDir == ".." || strings.HasPrefix(relDir, ".."+string(filepath.Separator)) || filepath.IsAbs(relDir) {
			continue // 不在该卷根之内
		}
		if relDir == "." {
			relDir = ""
		}
		return rt, filepath.ToSlash(relDir), true
	}
	return nil, "", false
}

// secretsDirFSFor 是 secretsFSFor 的「目录 + /secrets 拼接」变体（`type: secrets` + `target: local`
// 形态：传父目录，内部补 secrets/ 段）。
func secretsDirFSFor(set *registry.Set, rootDir string, logger *slog.Logger) syncpkg.FS {
	return secretsFSFor(set, filepath.Join(rootDir, "secrets"), logger)
}

// registerSecretsBackendWithFS 注册 secrets backend 类型构造器（可测试）：
// 接收「底层卷的 secrets/ 视图 FS」——由装配层解析 target 卷后注入。
// 声明协议 "secrets"（secrets:// 可被 ResolveURL 寻址）；scheme 冲突注册期 fail-fast。
func registerSecretsBackendWithFS(typ string, resolveFS func(ctx context.Context, v volume.Volume) (syncpkg.FS, error), protocols ...string) {
	registry.RegisterBackend(typ, func(ctx context.Context, v volume.Volume) (registry.ExternalBackend, error) {
		fs, err := resolveFS(ctx, v)
		if err != nil {
			return nil, err
		}
		return secrets.NewBackend(ctx, v, fs)
	}, protocols...)
}

// registerSecretsBackend 注册 secrets backend（生产）。
var registerSecretsOnce sync.Once

func registerSecretsBackend() {
	registerSecretsOnce.Do(func() {
		registerSecretsBackendWithFS("secrets", defaultSecretsFS, "secrets")
	})
}

// defaultSecretsFS 是默认 secrets 目标解析：Extra.target 为空/本地 → 本地默认卷
// secrets/ 目录；其余外部 target（baidupcs/s3/加密卷整卷）→ fail-closed 报错（需装配层
// 显式解析注入）。
//
// **嵌套不再在此处理**（方案甲）：`target: "<卷>/<子目录>"` 由装配层翻译出封装声明词汇
// （`extra.source`/`extra.wrapper`，见 pkg/server/nested_enclosure.go）后经
// `enclosure.Parse` + `AttachAll` 装配（wrapper = secrets），后端不经本解析器构造。
func defaultSecretsFS(ctx context.Context, v volume.Volume) (syncpkg.FS, error) {
	t, _ := v.Extra["target"].(string)
	switch t {
	case "", "local":
		root, _ := v.Extra["root"].(string)
		if root == "" {
			root = v.RootDir
		}
		if root == "" {
			return nil, fmt.Errorf("secrets backend: 卷 %q target=local 但无 root（默认卷根）", v.Name)
		}
		// `root` 是**父目录**（secrets 目录 = <root>/secrets，与 `secretsLocalFS` 同布局）：
		// 必须经 secretsDirFSFor 拼接 `secrets` 段——直接传 root 会让视图指向父目录本身，
		// 密钥文件按名落错位置（e2e 实测：secretdata 卷 secret_url 解析失败 → boot fail）。
		fs := secretsDirFSFor(secretDataSet.Load(), root, nil)
		return fs, nil
	default:
		return nil, fmt.Errorf("secrets backend: 卷 %q target=%q 需由装配层解析注入（外部卷 secrets/ 视图；嵌套形态由装配层翻译为封装声明）", v.Name, t)
	}
}

// ---- secretdata backend ----

// secretdataOptionsFromVolume 从卷 Extra 解析 secretdata.Options（纯函数，供工厂与测试
// 共用）。解析键：algorithm/block_policy/temp_dir/meta_pad_bytes/erasure/targets/
// max_file_bytes/**preserve_mtime/gc_interval**（Imp-3 生产可达修复：preserve_mtime 与
// gc_interval 接入 Options，GCInterval>0 时 NewFS 启动后台可选 GC）。
//
// **Dedup 不接线（方案 A 降级）**：去重降级为预留能力，装配层不再解析 extra.dedup 键
// （该键被忽略，生产 Options.Dedup 恒 false = 不复用）。实验代码保留由测试直接构造
// Options.Dedup 验证；生产 config 不启用。
func secretdataOptionsFromVolume(v volume.Volume) secretdata.Options {
	return secretdata.Options{
		Algorithm:     vcExtraStr(v, "algorithm"),
		Block:         vcExtraBlockPolicy(v),
		TempDir:       vcExtraStr(v, "temp_dir"),
		MetaPadBytes:  vcExtraByteSize(v, "meta_pad_bytes"),
		Erasure:       vcExtraBool(v, "erasure"),
		Targets:       vcExtraStrings(v, "targets"),
		MaxFileBytes:  vcExtraByteSize(v, "max_file_bytes"),
		PreserveMTime: vcExtraBool(v, "preserve_mtime"),
		GCInterval:    vcExtraDuration(v, "gc_interval"),
	}
}

// registerSecretdataBackendWithFS 注册 secretdata backend 类型构造器（生产/测试）。
// resolveSecret 按卷解析密钥字节（secret_url → set.ResolveURL 读取；注入解耦）。
// 多 target 装配：extra.targets（多 local root 副本列表）→ 构造副本底层 FS →
// NewBackendMultiplicas（写复制全部 target、读主失败回退副本）；无副本 → 单卷 NewBackend。
func registerSecretdataBackendWithFS(typ string, resolveSecret func(ctx context.Context, v volume.Volume) ([]byte, error), protocols ...string) {
	registry.RegisterBackend(typ, func(ctx context.Context, v volume.Volume) (registry.ExternalBackend, error) {
		secret, err := resolveSecret(ctx, v)
		if err != nil {
			return nil, err
		}
		targetFS, err := resolveTargetFS(ctx, v)
		if err != nil {
			return nil, err
		}
		replicas, err := resolveReplicaTargets(ctx, v)
		if err != nil {
			return nil, err
		}
		opts := secretdataOptionsFromVolume(v)
		opts.Secret = secret
		if len(replicas) == 0 {
			return secretdata.NewBackend(ctx, v, targetFS, opts)
		}
		return secretdata.NewBackendMultiplicas(ctx, v, targetFS, replicas, opts)
	}, protocols...)
}

// resolveReplicaTargets 解析 extra.targets 为副本底层 FS 列表（多 local root；primary 由
// resolveTargetFS 提供）。副本 target 同为 local root：每个元素作为一个独立底层卷根。
// 跨外部卷（baidupcs/s3/webdav/嵌套）接线留后续片（与 resolveTargetFS 的外部 target
// 边界一致）。空列表 = 无副本（单卷）。
func resolveReplicaTargets(ctx context.Context, v volume.Volume) ([]syncpkg.FS, error) {
	roots := vcExtraStrings(v, "targets")
	out := make([]syncpkg.FS, 0, len(roots))
	for _, root := range roots {
		out = append(out, syncpkg.NewLocalFS(root, nil))
	}
	return out, nil
}

// 注：不再有独立的 registerSecretdataBackend()——registerSecretVolumeBackends 内联注册并注入
// set 派生解析器（defaultSecretdataSecret(ctx, v, set)），避免全局解析器状态。
// setupSecretdataOnce 守卫注册（root.go + 多测试共享注册表，防重复注册 panic）。
var setupSecretdataOnce sync.Once

// defaultSecretdataSecret 解析密钥：Extra.secret_url（secrets://<卷>/<name>）经
// set.ResolveURL 读取（通用 URL 能力；scheme 由 secrets backend 注册声明）。
//
// **两条寻址路径**（用户裁定 2026-10-15）：
//  1. authority = 空 / `default` / `default-secrets` → **per-owner 默认桶**
//     （`<storage_root>/<owner>/meta/secrets/`）：需要 ctx 中的 owner（`secrets.WithOwner`，由带身份的
//     建卷请求注入，见 pkg/server/userVolumeHandler）。装配期（boot）没有请求身份 ⇒ 无法定位该桶，
//     显式 fail-closed 并给出替代写法（服务级声明请用**显式命名**的 secrets 卷）。
//  2. 其它 authority = **显式声明的 secrets 卷名**（`volumes[]` 中 `type: secrets`，含多层嵌套
//     封装链）→ 既有的 `set.ResolveURL` 按卷名分派。
func defaultSecretdataSecret(ctx context.Context, v volume.Volume, set *registry.Set) ([]byte, error) {
	secretURL, _ := v.Extra["secret_url"].(string)
	if strings.TrimSpace(secretURL) == "" {
		return nil, fmt.Errorf("secretdata backend: 卷 %q 需配置 extra.secret_url（secrets://<卷>/<name>）", v.Name)
	}
	name, nerr := secrets.NameFromURL(secretURL)
	if nerr != nil {
		return nil, fmt.Errorf("secretdata backend: 卷 %q 的 extra.secret_url=%q 非法: %w", v.Name, secretURL, nerr)
	}
	if secrets.IsDefaultVolumeRef(secretURL) {
		return perOwnerDefaultSecret(ctx, v, set, secretURL, name)
	}
	rc, err := set.ResolveURL(ctx, secretURL)
	if err != nil {
		return nil, fmt.Errorf("secretdata backend: 卷 %q 解析密钥 %q: %w", v.Name, secretURL, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("secretdata backend: 卷 %q 读密钥 %q 失败: %w", v.Name, secretURL, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("secretdata backend: 卷 %q 密钥 %q 为空（fail-closed）", v.Name, secretURL)
	}
	return data, nil
}

// perOwnerDefaultSecret 在**有 owner 上下文**时解析 per-owner 默认桶里的密钥；无 owner
// （装配期/服务级声明）→ 明确 fail-closed 并指明替代写法。
func perOwnerDefaultSecret(ctx context.Context, v volume.Volume, set *registry.Set, secretURL, name string) ([]byte, error) {
	owner := secrets.OwnerFromContext(ctx)
	if owner == "" {
		return nil, fmt.Errorf("secretdata backend: 卷 %q 的 extra.secret_url=%q 指向 per-owner 默认 secrets 桶，"+
			"但当前上下文无 owner（装配期/服务级声明无请求身份，无法定位该桶）；请改用**显式命名**的 secrets 卷"+
			"（volumes[] 中 type: secrets 卷，如 secrets://<卷名>/<name>），或经带身份的建卷请求"+
			"（POST /api/volumes/user）创建本卷", v.Name, secretURL)
	}
	// 一次性租户句柄读取（`server.ReadOwnerDefaultSecret`）：**不建第二份默认卷租户缓存**
	// （默认卷租户缓存权威由 Handlers.tenantFor 单一持有，见其文档与 registry.Set.Tenant）。
	data, rerr := server.ReadOwnerDefaultSecret(ctx, set, owner, name, nil)
	if rerr != nil {
		return nil, fmt.Errorf("secretdata backend: 卷 %q 读 owner %q 默认桶的密钥 %q 失败: %w", v.Name, owner, name, rerr)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("secretdata backend: 卷 %q 的 owner %q 密钥 %q 为空（fail-closed）", v.Name, owner, name)
	}
	return data, nil
}

// resolveTargetFS 解析 secretdata 的底层卷 FS：`target: ""/local` + extra.root（config/CLI
// 历史路径）→ 本地根整卷（保持原行为，零回归）；其余形态 fail-closed（需装配层注入）。
//
// **嵌套不再在此处理**（方案甲）：`target: "<卷>/<子目录>"` 由装配层翻译出封装声明词汇后经
// `enclosure.BuildBackend` 构造（wrapper = secretdata，源视图 = PrefixFS）。
func resolveTargetFS(_ context.Context, v volume.Volume) (syncpkg.FS, error) {
	t, _ := v.Extra["target"].(string)
	switch t {
	case "", "local":
		root, _ := v.Extra["root"].(string)
		if root == "" {
			return nil, fmt.Errorf("secretdata backend: 卷 %q 需 extra.root（本地 target）", v.Name)
		}
		return syncpkg.NewLocalFS(root, nil), nil
	default:
		return nil, fmt.Errorf("secretdata backend: 卷 %q target=%q 需由装配层解析注入（嵌套形态由装配层翻译为封装声明）", v.Name, t)
	}
}

// ---- Extra 解析辅助 ----

func vcExtraStr(v volume.Volume, key string) string {
	s, _ := v.Extra[key].(string)
	return s
}

// vcExtraBool 解析 extra.<key> 的布尔值（true/false；缺省 false）。
func vcExtraBool(v volume.Volume, key string) bool {
	b, _ := v.Extra[key].(bool)
	return b
}

// vcExtraByteSize 解析 extra.<key> 的字节大小（secretdata Options 的字节字段：
// meta_pad_bytes / max_file_bytes）。接受两种形态（2026-10-03 用户裁决：字节大小配置
// 统一 sizex.ByteSize，YAML/viper 可配 "8MiB" 人类可读）：
//   - 字符串 Go 单位语法（"8MiB"/"1GiB"，经 sizex.ByteSize.UnmarshalText）；
//   - 数字（JSON/YAML 解码为 float64/int64/int，纯数字字节）。
//
// 非法/缺省/非正数返回 0（Options 以 0 传默认：meta_pad_bytes 0 = Block.Min，
// max_file_bytes 0 = 不限制）。sizex.ByteSize 是 int64 命名类型，float64 小数截断语义
// 与 vcPositiveInt 一致（仅整数配置有意义）。
func vcExtraByteSize(v volume.Volume, key string) sizex.ByteSize {
	raw, ok := v.Extra[key]
	if !ok {
		return 0
	}
	switch n := raw.(type) {
	case string:
		var b sizex.ByteSize
		if err := b.UnmarshalText([]byte(n)); err == nil && b > 0 {
			return b
		}
	case float64:
		if n > 0 {
			return sizex.ByteSize(n)
		}
	case int64:
		if n > 0 {
			return sizex.ByteSize(n)
		}
	case int:
		if n > 0 {
			return sizex.ByteSize(n)
		}
	}
	return 0
}

// vcExtraDuration 解析 extra.<key> 的时长（后台 GC 间隔等）。接受两种形态：
//   - 字符串 Go duration 语法（"30s"、"5m"——与全仓超时字段约定一致）；
//   - 整数纳秒（JSON/YAML 数字，YAML 整数为 int / int64）。
//
// 非法/缺省/非正数返回 0（Options.GCInterval 0 = 默认禁用后台 GC，调用方显式 fs.GC()）。
func vcExtraDuration(v volume.Volume, key string) time.Duration {
	raw, ok := v.Extra[key]
	if !ok {
		return 0
	}
	switch n := raw.(type) {
	case string:
		d, err := time.ParseDuration(strings.TrimSpace(n))
		if err == nil && d > 0 {
			return d
		}
	case float64:
		if n > 0 {
			return time.Duration(n)
		}
	case int64:
		if n > 0 {
			return time.Duration(n)
		}
	case int:
		if n > 0 {
			return time.Duration(n)
		}
	}
	return 0
}

// vcExtraStrings 解析 extra.<key> 的字符串列表（副本卷名 / 其它多值配置）。兼容 []string
// 与 []any（JSON 解码形态）。缺省返回 nil。
func vcExtraStrings(v volume.Volume, key string) []string {
	switch raw := v.Extra[key].(type) {
	case []string:
		out := make([]string, 0, len(raw))
		for _, s := range raw {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(raw))
		for _, item := range raw {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// vcPositiveInt 统一解析整数字面（YAML/JSON 数字与 Go 内联 map 的多种形态）：
// float64（JSON 数字经 encoding/json 恒解码）、int64、int（gopkg.in/yaml.v3 整数）。
// 只接受 >0；非正数/缺省/非数字返回 0（调用方以 0 传默认，如 meta_pad_bytes 0 由
// metaPadTarget 兜底 = Block.Min）——避免负数送入 Options。**唯一实现**：vcPositiveInt 供
// vcExtraBlockPolicy 的 min/max 均走本 helper（M-1 修复：YAML 整数不再被 float64 分支
// 静默忽略——统一 int/int64/float64 三分支一次解析，消除两处不一致）。
func vcPositiveInt(raw any) int64 {
	switch n := raw.(type) {
	case float64:
		if n > 0 {
			return int64(n)
		}
	case int64:
		if n > 0 {
			return n
		}
	case int:
		if n > 0 {
			return int64(n)
		}
	}
	return 0
}

// vcExtraBlockPolicy 解析 extra.block_policy（map；mode/min/max）。min/max 经 vcPositiveInt
// 统一解析（M-1 修复：YAML 整数不再静默忽略——原实现只读 float64，`{min: 4096}` 在 YAML
// 下解码为 int 被跳过、回退默认 1MiB–200MiB，与数字解析的 int 分支不一致）。
// vcExtraBlockPolicy 解析 extra.block_policy（map；mode/min/max/blocklet_mode）。min/max 经
// vcPositiveInt 统一解析（M-1 修复：YAML 整数不再静默忽略——原实现只读 float64，`{min: 4096}`
// 在 YAML 下解码为 int 被跳过、回退默认 1MiB–200MiB，与数字解析的 int 分支不一致）。
//
// **blocklet_mode 关键帧开关（2026-10-04 用户指令）**：显式配置优先于自动选型——
//   - "fixed" → 强制定长 blocklet（关闭视频关键帧分块，运维可关）；
//   - "video-keyframe" → 强制关键帧分块（显式开启，即使文件非视频也生效）；
//   - 缺省 → 自动按扩展名选型（secretdata blockletPolicyFor 决定）。
func vcExtraBlockPolicy(v volume.Volume) shardseal.BlockPolicy {
	bp := shardseal.DefaultBlockPolicy()
	bp.BlockletMode = "" // 缺省 = 自动选型（而非默认 fixed）
	if m, _ := v.Extra["block_policy"].(map[string]any); m != nil {
		if vv, ok := m["mode"].(string); ok && vv != "" {
			bp.Mode = vv
		}
		if vv, ok := m["blocklet_mode"].(string); ok && vv != "" {
			bp.BlockletMode = vv
		}
		if minVal := vcPositiveInt(m["min"]); minVal > 0 {
			bp.Min = minVal
		}
		if maxVal := vcPositiveInt(m["max"]); maxVal > 0 {
			bp.Max = maxVal
		}
	}
	return bp
}

// ---- 封装处理层（wrapper）注册 ----

// secretdataWrapper 把 secretdata 注册为**封装处理层**（enclosure.Wrapper）：使
// `type: enclosure` + `extra.wrapper: secretdata` 的配置真正可用（设计 §S4 收尾项，
// `wrapper` 可插拔的处理层之一）。
//
// 与 `type: secretdata` 独立卷的关系：二者共用 `secretdata` 的 FS 实现与 Options 解析，
// 差别只在**底层来自哪里**——独立卷由 `extra.target`（本地根）解析，封装卷由装配层注入的
// **源视图**（`EnclosureFS(PrefixFS(源卷特权 FS, 声明子路径))`）提供。
type secretdataWrapper struct {
	resolveSecret func(ctx context.Context, v volume.Volume) ([]byte, error)
}

// Name 返回注册名（`extra.wrapper` 引用它；与文档/示例一致）。
func (secretdataWrapper) Name() string { return "secretdata" }

// Build 用封装卷的源视图构造 secretdata 后端：视图中「源卷子目录」已等价于本卷根，
// 处理层只改字节（分块加密 + meta pad + 可选 parity），**不承担**封印与配额职责
// （封印由源卷 `Root.SetSeals` 注入；配额由装配层 `capacity.WrapBackend` 包装）。
func (w secretdataWrapper) Build(ctx context.Context, v volume.Volume, src syncpkg.FS, _ map[string]any) (registry.ExternalBackend, error) {
	secret, err := w.resolveSecret(ctx, v)
	if err != nil {
		return nil, err
	}
	opts := secretdataOptionsFromVolume(v)
	opts.Secret = secret
	// 多副本（extra.targets：多本地根）与 `type: secretdata` 同语义：主底层 = 封装卷源视图，
	// 副本为独立本地根（写复制、读主失败回退副本）。
	replicas, err := resolveReplicaTargets(ctx, v)
	if err != nil {
		return nil, err
	}
	if len(replicas) == 0 {
		return secretdata.NewBackend(ctx, v, src, opts)
	}
	return secretdata.NewBackendMultiplicas(ctx, v, src, replicas, opts)
}

// secretsWrapper 把 secrets 注册为**封装处理层**（enclosure.Wrapper）：使
// `type: enclosure` + `extra.wrapper: secrets` 的配置真正可用（与 secretdata 同型）。
//
// 与 `type: secrets` 独立卷的关系：二者共用 `secrets` 的 FS 实现，差别只在**底层来自哪里**
// ——独立卷由 `extra.target` 解析，封装卷由装配层注入的**源视图**提供。此前只注册了
// secretdata ⇒ `wrapper: secrets` 的声明在 `enclosure.Parse` 恒 `ErrUnknownWrapper`（boot fail）。
//
// **不做 0600 chmod**：封装源视图是 `PrefixFS`（非 `*syncpkg.LocalFS`），secrets 的 perm 收紧
// 对封装底层天然不适用（与过渡实现的 SubFS 同口径——`localChmod` 断言失败即 no-op）。
type secretsWrapper struct{}

// Name 返回注册名（`extra.wrapper` 引用它；与文档/示例一致）。
func (secretsWrapper) Name() string { return "secrets" }

// Build 用封装卷的源视图构造 secrets 后端：视图中「源卷子目录」已等价于本卷根。
func (secretsWrapper) Build(ctx context.Context, v volume.Volume, src syncpkg.FS, _ map[string]any) (registry.ExternalBackend, error) {
	if src == nil {
		return nil, fmt.Errorf("enclosure: secrets 封装卷 %q 源视图为空（装配顺序错误）", v.Name)
	}
	return secrets.NewBackend(ctx, v, src)
}

// enclosureWrapperOnce 守卫注册（进程内一次；wrapper 注册表重复注册会 panic）。
var enclosureWrapperOnce sync.Once

// registerEnclosureWrappers 注册生产封装处理层。**必须早于** `enclosure.Parse`（即在
// RegisterRoutes → assembleVolumes 之前调用）：`Parse` 对未注册的 `extra.wrapper` 判
// `ErrUnknownWrapper` → 封装卷装配失败 → boot fail。此前生产侧没有任何 wrapper 注册
// （仅测试注册替身）⇒ 文档与 config.example 承诺的 `wrapper: secretdata` **恒不可用**。
//
// 密钥解析与 `type: secretdata` 同源（`extra.secret_url` → `secretDataSet` 卷集
// `ResolveURL`），故同样经**懒解析**：注册发生在卷集就绪前，Build 发生在补装期（卷集已就绪）。
func registerEnclosureWrappers() {
	enclosureWrapperOnce.Do(func() {
		enclosure.RegisterWrapper(secretdataWrapper{resolveSecret: func(ctx context.Context, v volume.Volume) ([]byte, error) {
			set := secretDataSet.Load()
			if set == nil {
				return nil, fmt.Errorf("enclosure: 封装卷 %q 补装时卷集未就绪", v.Name)
			}
			return defaultSecretdataSecret(ctx, v, set)
		}})
		enclosure.RegisterWrapper(secretsWrapper{})
	})
}

// ---- 公共装配入口 ----

// secretDataSet 是 secretdata 后端解析密钥所依赖的装配后卷集持有者。
// secretdata backend 工厂在 registry.NewBackend 时才调用 resolver 解析 secret_url，
// 而装配后卷集（registry.Set）要到 RegisterRoutes 卷集合装配完成后才可用——故 resolver
// 经本持有者读取：注册（registerSecretVolumeBackends，早于 assemble）与装配
// （装配期 storeSecretDataSet，卷集就绪后 Store）解耦。仅生产 "secretdata" 类型使用；测试用
// registerSecretdataBackendWithFS 注册独立类型自带 set，不受影响。
var secretDataSet = atomic.Pointer[registry.Set]{}

// registerSecretVolumeBackends 无条件注册 secrets + secretdata 后端类型（幂等，Once 守卫）。
// 必须早于 RegisterRoutes → assembleVolumes（若 config `type: secretdata/secrets` 卷，
// assembleVolumes 会对它们调用 registry.NewBackend）——否则「未注册后端」启动 panic
// （Imp-1 装配门控的具体爆发点）。secretdata 工厂经 secretDataSet 懒解析已装配卷集。
//
// **Imp-2 时序修复**：secretdata 类型标记为「推迟装配」（registry.MarkDeferredType）——
// 其构造依赖已装配卷集（secret_url → secrets 卷密钥），而 assembleVolumes 在 RegisterRoutes
// 内部执行、彼时 set 尚未 Store 到 secretDataSet，若直接构造恒「卷集未就绪」失败。
// 故 assembleVolumes 对 secretdata 卷跳过 backend 构造（仅登记卷元数据 + 容量池），
// set 就绪后由 attachSecretdataVolumes 遍历已登记卷补装（分「先行/后置」两轮，见本文件「装配
// 序」注释；registry.NewBackend + AttachExternal）
// ——config 声明的 type:secretdata 卷可真正装配（config.example vault 示例可用）。
func registerSecretVolumeBackends() {
	registerSecretsBackend()
	registerSecretSchemas()
	registry.MarkDeferredType("secretdata")
	setupSecretdataOnce.Do(func() {
		registerSecretdataBackendWithFS("secretdata", func(ctx context.Context, v volume.Volume) ([]byte, error) {
			// 协议声明（scheme=secretdata，ResolveURL 可寻址转存后的加密文件）。
			set := secretDataSet.Load()
			if set == nil {
				return nil, fmt.Errorf("secretdata backend: 卷 %q 装配完成但卷集未就绪", v.Name)
			}
			return defaultSecretdataSecret(ctx, v, set)
		}, "secretdata")
	})
}

// registerSecretSchemasOnce 守卫静态 schema 登记（registerSecretVolumeBackends 无条件
// 多次调用——root 装配 + 多测试；RegisterBackendSchema 重复登记即 panic，须 Once）。
var registerSecretSchemasOnce sync.Once

// registerSecretSchemas 登记 secrets + secretdata 的**静态**创建表单 schema：两者都是
// wrapper 卷，均声明 volume-select 必填字段 `target`（底层卷，allow_wrapper=true 允许
// 嵌套封装）；secretdata 额外声明必填 text 字段 `secret_url`（密钥引用，后端工厂
// defaultSecretdataSecret 缺此键 fail-closed），使 UI 表单能提交到后端做校验。静态登记
// 不依赖构造后端实例（secretdata 构造需已装配卷集/密钥，空 Extra 直接失败）——建卷 API
// 与 /api/backends 靠它做防环字段校验。协议 scheme 不可作建卷字段（是注册期元数据，
// 非用户可填），故不在此 schema 声明。
func registerSecretSchemas() {
	registerSecretSchemasOnce.Do(func() {
		registry.RegisterBackendSchema("secrets", []registry.FieldSchema{{
			Key: "target", Label: "底层卷", Type: "volume-select",
			Required: true, AllowWrapper: true,
		}})
		registry.RegisterBackendSchema("secretdata", []registry.FieldSchema{
			{Key: "target", Label: "底层卷", Type: "volume-select", Required: true, AllowWrapper: true},
			{Key: "secret_url", Label: "密钥引用", Type: "text", Required: true},
		})
	})
}

// secret 卷装配（RegisterRoutes 返回后，卷集合已装配；localRoot 为默认卷物理根）：
// 后端**注册**部分由 registerSecretVolumeBackends 无条件先行（见 setupServerCore），本文件的
// `storeSecretDataSet` + `attachSecretdataVolumes`（分两轮调用）完成**推迟装配**——Store 已装配
// 卷集（供 secretdata 工厂懒读）+
// 补装 config 声明的 secretdata 卷（Imp-2 时序修复：assembleVolumes 因 set 未就绪跳过了其
// backend 构造，此后 set 已就绪、按卷元数据逐个构造并挂回 Set.External）。
//
// **不再装配「全局默认 secrets 卷」**（用户裁定 2026-10-15）：默认 secrets 是 **per-owner** 的
// ——每个 owner 租户根下 meta 桶内的 `meta/secrets/` 目录，由 `/api/secrets` 按请求 owner 解析并
// 首次使用自动派生（`pkg/server/secretsManagerFor`）。故此处没有任何默认 secrets 卷需要装配，也不
// 存在「多个全局 secrets 卷选哪个」的模型。
//
// **fail-closed 语义（方案 A 修复轮 I1）**：仅**配置声明的 secretdata/secrets 卷补装失败**
// 返回错误（调用方 root.go 视为 boot fail，与本地卷 load failure 同层——operator 显式声明的
// 加密封装不得静默消失）。
//
// ---- 装配序：拆成四步（2026-10-15 装配序修复） ----
//
// 拆点：拆分前的单一入口函数把「Store 卷集」与「补装 config 声明 secretdata 卷」合成一步，
// 且整体早于
// `enclosure.AttachAll`（封装声明卷补装）。这造成一条**声明顺序限制**：config 里同时声明
// 「嵌套 secrets 卷」与「`secret_url` 指向它的 secretdata 卷」时，前者只有经 AttachAll 才装配，
// 于是后者补装时密钥解析必然「源卷后端未装配」→ boot fail。拆成：
//
//	① `storeSecretDataSet` —— Store 卷集（供 secretdata 工厂与封装 wrapper 懒解析密钥）；
//	② `attachSecretdataVolumes(..., secretdataRefsIndependent)` —— 先行补装**不依赖封装声明卷**
//	   的 config 声明 secretdata 卷（封装卷的**源**可能就是它们，故必须早于 AttachAll）；
//	③ `enclosure.AttachAll` —— 封装声明卷（嵌套 secrets/secretdata 链由此装配）；
//	④ `attachSecretdataVolumes(..., secretdataRefsEnclosure)` —— 后置补装 `secret_url` 指向
//	   **封装声明卷**的 config 声明 secretdata 卷（须待 ③ 之后才能解析密钥）。
//
// 为什么不是「单轮 + 失败重试」：判据取**静态声明**（`secretdataRefsEnclosureVolume`），不靠
// 错误文案识别「依赖未就绪 vs 真失败」——后者会把不可重试的真故障也拖到下一轮。
// 已知边界（记录在案）：若某 secretdata 卷**既**引用封装卷、**又**是另一个封装卷的源，则本序无解
// （单轮内无法同时满足两端）——该形态等价于跨层循环依赖，属不支持配置。
//
// 注意：**嵌套封装卷**（`extra.target: "<卷>/<子目录>"`，装配侧等价 `extra.source`）不走本文件的
// 补装路径——其 backend 由 `enclosure.AttachAll` 装配（源特权视图 + wrapper），本文件按类型分派会
// 给它构造「target 解析失败」的错误后端（方案甲：此类卷 Type 仍是 secretdata，须用声明判据排除）。

// secretdataRefPolicy 决定一轮补装处理哪一类 config 声明的 secretdata 卷（见上方装配序）。
type secretdataRefPolicy int

const (
	// secretdataRefsIndependent：`secret_url` **不**指向封装声明卷的卷（或未配 URL / 指向
	// per-owner 默认桶）——先行补装（AttachAll 之前）。
	secretdataRefsIndependent secretdataRefPolicy = iota
	// secretdataRefsEnclosure：`secret_url` **指向**封装声明卷（如嵌套 secrets 卷）的卷——
	// 后置补装（AttachAll 之后）。
	secretdataRefsEnclosure
)

// storeSecretDataSet 把装配后的卷集 Store 进 secretDataSet（供 secretdata 工厂与封装卷的
// secretdata wrapper 懒解析 `secret_url`）。必须在任何 secretdata 后端构造**与**
// `enclosure.AttachAll`（其 wrapper 会读该持有者）之前调用。
func storeSecretDataSet(set *registry.Set) error {
	if set == nil {
		return fmt.Errorf("secret backends: volSet 未装配")
	}
	secretDataSet.Store(set)
	return nil
}

// attachSecretdataVolumes 补装**本轮所属**的 config 声明 secretdata 卷（非封装声明卷；
// 按 policy 分轮，见上方装配序）。任一卷补装失败 → 返回错误（调用方 boot fail，fail-closed）。
func attachSecretdataVolumes(ctx context.Context, set *registry.Set, localRoot string, logger *slog.Logger, policy secretdataRefPolicy) error {
	if set == nil {
		return fmt.Errorf("secret backends: volSet 未装配")
	}
	log := slog.Default()
	if logger != nil {
		log = logger
	}
	wantEnclosureRef := policy == secretdataRefsEnclosure
	for _, v := range set.All() {
		if v.Type != volume.TypeSecretdata {
			continue
		}
		if set.External(v.Name) != nil {
			continue // 已装配（测试自注册类型等）
		}
		if enclosure.IsDeclared(v) {
			continue // 嵌套封装卷：由 enclosure.AttachAll 补装（同属 boot 阶段，见上方装配序）
		}
		if secretdataRefsEnclosureVolume(set, v) != wantEnclosureRef {
			continue // 属另一轮（先行/后置），本轮不处理
		}
		if err := attachSecretdataVolume(ctx, set, v, localRoot, log); err != nil {
			return err
		}
	}
	return nil
}

// secretdataRefsEnclosureVolume 报告 secretdata 卷的 `extra.secret_url` 是否指向**封装声明卷**
// （`enclosure.IsDeclared`：已注册 wrapper 类型 + `extra.source`）——装配层据此把它排到
// `AttachAll` **之后**补装（判据是静态声明，不靠「试装失败」的错误文案）。
//
// 返回 false 的情形：未配 `secret_url`（工厂会 fail-closed 报错）、指向 per-owner 默认桶
// （`secrets://default-secrets/<名>`，与封装卷无关）、URL 非法/无卷名（同上，报错交给工厂）、
// 卷名未被登记或不是封装声明卷（含**运行期**新增的普通卷——它们此时已装配）。
func secretdataRefsEnclosureVolume(set *registry.Set, v volume.Volume) bool {
	urlStr, _ := v.Extra["secret_url"].(string)
	if strings.TrimSpace(urlStr) == "" || secrets.IsDefaultVolumeRef(urlStr) {
		return false
	}
	volName, verr := secrets.VolumeFromURL(urlStr)
	if verr != nil || volName == "" {
		return false
	}
	sv, ok := set.ByName(volName)
	return ok && enclosure.IsDeclared(sv)
}

// attachSecretdataVolume 补装单个 config 声明的 secretdata 卷（包卷级容量记账后挂回卷集）。
func attachSecretdataVolume(ctx context.Context, set *registry.Set, v volume.Volume, localRoot string, log *slog.Logger) error {
	be, berr := registry.NewBackend(ctx, v)
	if berr != nil {
		return fmt.Errorf("secret backends: 补装 secretdata 卷 %q 失败（boot fail）: %w", v.Name, berr)
	}
	// 卷级容量记账（FS 层）：与非延迟外部卷同—包 CapacityFS（**唯一实现**：分片日志账本）。
	// 账本落点按**底层本地性**（本地性探测在 WrapStorageVolume 内部）：`target: local`
	// （单实例专用）⇒ 落**节点本地**明文目录（`<storageRoot>/meta/quota/<卷名>/`）——
	// secretdata 是加密卷，账本写卷 FS 上会被加密成随机名**内容分块**，污染卷物理布局
	// （E2E「内容分块清空」断言实测拦截）；共享底层 ⇒ 账本留卷 FS（跨进程正确，配额不被绕过）。
	if pool := set.Pool(v.Name); pool != nil {
		be = capacity.WrapStorageVolume(v.Name, be, nil, pool, pool.MaxBytes(), 0,
			capacity.WithLedgerDirIfLocal(filepath.Join(localRoot, "meta", "quota", v.Name)))
	}
	if aerr := set.AttachExternal(v.Name, be); aerr != nil {
		return fmt.Errorf("secret backends: secretdata 卷 %q 挂回卷集失败（boot fail）: %w", v.Name, aerr)
	}
	log.Info("secretdata 卷补装完成（config 声明，推迟装配）", "volume", v.Name)
	return nil
}
