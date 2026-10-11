// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// secret_register.go 是 secret 加密卷的装配（设计 docs/designs/2026-10-01-secret-volume.md
// §9）：把 secrets 卷（secret 文件管理）与 secretdata 卷（加密数据 wrapper）注册为
// volumes[] 的 backend 插件，并处理「启动默认建本地卷作默认 secrets 卷」。
//
// 装配顺序（secretdata 依赖 secrets 卷；secrets 卷可嵌套）：
//  1. 装配默认 secrets 卷（本地卷 secrets/ 目录）；
//  2. 装配配置的 secrets 卷（Extra.target 的 secrets/ 目录）；
//  3. 装配 secretdata 卷（secret_url → secrets 卷读密钥）。
//
// secrets 卷由装配层把底层卷的 secrets/ 前缀视图 FS 注入（secrets.NewBackend 签名），
// secretdata 卷由装配层解析 secret_url 到密钥字节后注入（secretdata.NewBackend 签名）。

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/units/sizex"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/capacity"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/secretdata"
	"github.com/cocomhub/sproxy/pkg/volume/secrets"
	"github.com/cocomhub/sproxy/pkg/volume/trusted"
)

// ---- secrets backend ----

// secretsLocalFS 把由 rootDir 传入的依赖 caller 决定视图的本地 FS 视图建为 LocalFS。
// 调用方传 rootDir 时即期望集成 secrets 目录（本函数负责拼接 /secrets）。
func secretsLocalFS(rootDir string) syncpkg.FS {
	return syncpkg.NewLocalFS(rootDir+"/secrets", nil)
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
// secrets/ 目录；外部 target（baidupcs/s3/加密卷嵌套）→ 需装配层显式解析注入
// （当前返回错误提示，嵌套封装留**后续片**——设计 §6.1「底层亦可为 secret_data 加密卷
// （嵌套）」未在当前实现兑现，仅 local 子集可用；config 声明 secrets 卷使用外部 target
// 会在此 fail-closed 报错，不会静默降级）。
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
		return secretsLocalFS(root), nil
	default:
		return nil, fmt.Errorf("secrets backend: 卷 %q target=%q 需由装配层解析注入（外部卷 secrets/ 视图）", v.Name, t)
	}
}

// ---- secrets 卷本地默认装配 ----

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

// 注：不再有独立的 registerSecretdataBackend()——setupSecretBackends 内联注册并注入
// set 派生解析器（defaultSecretdataSecret(ctx, v, set)），避免全局解析器状态。
// setupSecretdataOnce 守卫注册（root.go + 多测试共享注册表，防重复注册 panic）。
var setupSecretdataOnce sync.Once

// defaultSecretdataSecret 解析密钥：Extra.secret_url（secrets://<卷>/<name>）经
// set.ResolveURL 读取（通用 URL 能力；scheme 由 secrets backend 注册声明）。
func defaultSecretdataSecret(ctx context.Context, v volume.Volume, set *registry.Set) ([]byte, error) {
	secretURL, _ := v.Extra["secret_url"].(string)
	if strings.TrimSpace(secretURL) == "" {
		return nil, fmt.Errorf("secretdata backend: 卷 %q 需配置 extra.secret_url（secrets://<卷>/<name>）", v.Name)
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

// resolveTargetFS 解析 secretdata 的底层卷 FS：Extra.target（默认 local → 本地根）。
func resolveTargetFS(ctx context.Context, v volume.Volume) (syncpkg.FS, error) {
	t, _ := v.Extra["target"].(string)
	switch t {
	case "", "local":
		root, _ := v.Extra["root"].(string)
		if root == "" {
			return nil, fmt.Errorf("secretdata backend: 卷 %q 需 extra.root（本地 target）", v.Name)
		}
		return syncpkg.NewLocalFS(root, nil), nil
	default:
		// 外部 target（baidupcs/s3/webdav/secretdata 嵌套）：由装配层在依赖解析后
		// 注入。简化版直接报错提示需外部装配（嵌套封装留后续片）。
		return nil, fmt.Errorf("secretdata backend: 卷 %q target=%q 需外部装配（嵌套封装后续片）", v.Name, t)
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

// ---- 公共装配入口 ----

func ensureDefaultSecretsVolume(ctx context.Context, set *registry.Set, defaultRoot string, logger *slog.Logger) (*secrets.Manager, error) {
	const regName = "default-secrets"
	if be := set.External(regName); be != nil {
		// 已有同名卷（用户显式配置）——取出其 FS 视图返回。
		if fs := be.FS(); fs != nil {
			return secrets.NewManager(trusted.Guard(fs), regName, true), nil
		}
	}
	// 设计 §6.1/§9：默认 secrets 卷落 <StorageRoot>/secrets 恰一次——secretsLocalFS
	// 内部已把入参拼 /secrets，此处直接传 defaultRoot，避免双 append（secrets/secrets
	// 层级错误）。
	root := defaultRoot
	fs := secretsLocalFS(root)
	mgr := secrets.NewManager(fs, regName, true)
	// 统一用 secrets backend（实现 URLResolver，secrets:// 可寻址）而非裸 adapter：
	// 默认卷也是 URL 可寻址目标（secrets://default/ 或 secrets://<name>/）。
	be, err := secrets.NewBackend(ctx, volume.Volume{Name: regName, Type: "secrets",
		Extra: map[string]any{"target": "local", "root": defaultRoot}}, fs)
	if err != nil {
		return nil, fmt.Errorf("装配默认 secrets 卷 backend 失败: %w", err)
	}
	if err := set.AddExternalVolume(volume.Volume{Name: regName, Type: "secrets", Extra: map[string]any{"target": "local", "root": defaultRoot}}, be); err != nil {
		return nil, fmt.Errorf("装配默认 secrets 卷失败: %w", err)
	}
	if logger != nil {
		logger.Info("默认 secrets 卷已装配", "volume", regName, "root", root)
	}
	return mgr, nil
}

// secretDataSet 是 secretdata 后端解析密钥所依赖的装配后卷集持有者。
// secretdata backend 工厂在 registry.NewBackend 时才调用 resolver 解析 secret_url，
// 而装配后卷集（registry.Set）要到 RegisterRoutes 卷集合装配完成后才可用——故 resolver
// 经本持有者读取：注册（registerSecretVolumeBackends，早于 assemble）与装配
// （setupSecretBackends，装完后 Store）解耦。仅生产 "secretdata" 类型使用；测试用
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
// set 就绪后由 setupSecretBackends 遍历已登记卷补装（registry.NewBackend + AttachExternal）
// ——config 声明的 type:secretdata 卷可真正装配（config.example vault 示例可用）。
func registerSecretVolumeBackends() {
	registerSecretsBackend()
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

// setupSecretBackends 装配 secrets + secretdata 后端并确保默认 secrets 卷。需在
// RegisterRoutes（卷集合装配完成后）调用；localRoot 为默认卷物理根。注册（工厂）部分
// 经 registerSecretVolumeBackends 无条件先行（见 setupServerCore），本函数负责把已装配
// 卷集 Store 进 secretDataSet（供工厂懒读）+ 确保默认 secrets 卷 + **补装 config 声明的
// secretdata 卷**（Imp-2 时序修复：assembleVolumes 因 set 未就绪跳过了其 backend 构造，
// 此处 set 已就绪、按卷元数据逐个构造并挂回 Set.External）。
//
// **fail-closed 语义（方案 A 修复轮 I1）**：仅**配置声明的 secretdata/secrets 卷补装失败**
// 返回错误（调用方 root.go 视为 boot fail，与本地卷 load failure 同层——operator 显式声明的
// 加密封装不得静默消失）；**默认 secrets 卷**（`<StorageRoot>/secrets`，可选能力）装配失败仅
// WARN 降级、不阻断 boot（不存在默认卷不致命，仅当 config 引用 secrets://default 时才在
// 补装阶段 fail-closed）。
func setupSecretBackends(ctx context.Context, set *registry.Set, localRoot string, logger *slog.Logger) error {
	if set == nil {
		return fmt.Errorf("secret backends: volSet 未装配")
	}
	log := slog.Default()
	if logger != nil {
		log = logger
	}
	// 装配完成后 Store 卷集，供 registerSecretVolumeBackends 注册的 secretdata 工厂
	// 在后续 NewBackend 时经 ResolveURL(secrets://...) 解析密钥。
	secretDataSet.Store(set)
	// 默认 secrets 卷为可选能力：失败仅 WARN 降级（不阻断 boot）。
	if _, err := ensureDefaultSecretsVolume(ctx, set, localRoot, log); err != nil {
		log.Warn("默认 secrets 卷装配失败（默认 secrets 降级为不可用，不阻断启动）", "err", err)
	}
	// 补装 config 声明的 secretdata 卷：assembleVolumes 已把卷元数据登记进 set
	// （deferred 类型跳过 backend 构造），此处 set 已就绪、secretdata 工厂可解析密钥。
	// 任一卷补装失败 → 返回错误（boot fail，fail-closed）。
	for _, v := range set.All() {
		if v.Type != volume.TypeSecretdata {
			continue
		}
		if set.External(v.Name) != nil {
			continue // 已装配（测试自注册类型等）
		}
		if err := attachSecretdataVolume(ctx, set, v, localRoot, log); err != nil {
			return err
		}
	}
	return nil
}

// attachSecretdataVolume 补装单个 config 声明的 secretdata 卷（包卷级容量记账后挂回卷集）。
func attachSecretdataVolume(ctx context.Context, set *registry.Set, v volume.Volume, localRoot string, log *slog.Logger) error {
	be, berr := registry.NewBackend(ctx, v)
	if berr != nil {
		return fmt.Errorf("secret backends: 补装 secretdata 卷 %q 失败（boot fail）: %w", v.Name, berr)
	}
	// 卷级容量记账（FS 层）：与非延迟外部卷同—包 CapacityFS（PoolCounter 复用该卷
	// vol_capacity 池，并**持久化**——重启后仍从已占用起算）。
	if pool := set.Pool(v.Name); pool != nil {
		counter, cerr := capacity.NewPoolCounterPersistent(pool, capacity.VolumeCounterPath(localRoot, v.Name))
		if cerr != nil {
			log.Warn("secretdata 卷容量快照恢复失败，本次进程从零累计", "volume", v.Name, "error", cerr)
			counter = capacity.NewPoolCounter(pool)
		}
		be = capacity.WrapBackend(be, counter)
	}
	if aerr := set.AttachExternal(v.Name, be); aerr != nil {
		return fmt.Errorf("secret backends: secretdata 卷 %q 挂回卷集失败（boot fail）: %w", v.Name, aerr)
	}
	log.Info("secretdata 卷补装完成（config 声明，推迟装配）", "volume", v.Name)
	return nil
}
