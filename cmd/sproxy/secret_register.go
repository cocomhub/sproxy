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
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/secretdata"
	"github.com/cocomhub/sproxy/pkg/volume/secrets"
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
// secrets/ 目录；嵌套 target（`<卷>/<新子目录>`，用户 2026-10-06 确认语义）→ 引用已创建
// 卷的新空子目录作封装根（resolveNestedTargetFS）；其余外部 target（baidupcs/s3/加密卷
// 整卷）→ fail-closed 报错（需装配层显式解析注入）。
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
		if _, _, ok := volume.SplitNestedTarget(t); ok {
			return resolveNestedTargetFS(ctx, v.Name, t)
		}
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

// adoptNestedDirKey 是装配 ctx 标记：config 补装 / 用户卷重启 restore 重装**既有**封装卷时
// 置 true，使 resolveNestedTargetFS 对「已存在子目录」执行**重启收养**（前次本卷自建 →
// 复用，而非 fail-closed「需新空子目录」——否则 config 声明封装卷重启即 boot-fail，评审
// CRIT-①）。**未置标记**（运行时 POST /api/volumes/user 新建）→ 子目录已存在按冲突拒绝
// （与 validateNestedWrapperTarget 预检契约一致，TOCTOU 闭合）。
type adoptNestedDirKey struct{}

// adoptNestedDir 在 ctx 上置「重启收养」标记（幂等，同键覆盖）。
func adoptNestedDir(ctx context.Context) context.Context {
	return context.WithValue(ctx, adoptNestedDirKey{}, true)
}

// adoptableNestedDir 判定「重启收养」的目标子目录是否确为本封装卷自建（空性/来源校验，Minor1）：
// 本层封装卷数据落子目录顶层为**单一形态**——纯目录容器（secretdata 随机容器 / files 层 user
// 桶）/ 纯秘密文件（secrets 钥匙卷）。子目录顶层为空（本卷尚未落数据）→ 可收养；非空但全目录
// 或全文件（本卷专属存储结构）→ 可收养；**文件+目录混合** = 普通用户目录特征（文档/项目混放，
// 典型「config 误指向既有非 wrapper 目录」）→ 不可收养，fail-closed 拒绝装配并引导人工——否则
// 删除封装卷时 deleteFSContents 整流删会连带清空误指目录（数据损失窗口）。
func adoptableNestedDir(ctx context.Context, inner syncpkg.FS, subdir string) (bool, error) {
	entries, err := inner.ListDir(ctx, subdir)
	if err != nil {
		return false, fmt.Errorf("检查嵌套子目录 %s 内容失败: %w", subdir, err)
	}
	if len(entries) == 0 {
		return true, nil // 空目录：无本卷数据，收养安全。
	}
	var dirs, files int
	for _, e := range entries {
		if e.IsDir {
			dirs++
		} else {
			files++
		}
	}
	// 全目录（容器/桶）或全文件（秘密文件）→ 本卷专属形态；混合 → 拒绝（普通目录特征）。
	return dirs == 0 || files == 0, nil
}

// resolveNestedTargetFS 解析嵌套封装 target（`<卷>/<新子目录>`）为可写封装子视图（SubFS）。
// 供 secretdata（resolveTargetFS）与 secrets（defaultSecretsFS）共用：
//
//  1. 经卷集（secretDataSet）取底层卷 FS（本地卷 → LocalFS，外部卷 → External(name).FS()）；
//  2. 确认子目录当前不存在（防与底层卷既有数据混合；已存在 fail-closed）——**重启收养**
//     （ctx 带 adoptNestedDirKey）：子目录已存在且为前次本卷自建（config/store 声明再次
//     装配）→ 复用包装，不报「已存在」；
//  3. MakeDir 创建空目录 → SubFS 包装（密文/secret 文件落 <卷>/<子目录>/ 下）。
//
// 只读保护（占用目录禁改）由互斥占用（pkg/server/volume_links）+ 写保护视图
// （pkg/sync.ReadonlySubFS）承担。
func resolveNestedTargetFS(ctx context.Context, name, target string) (syncpkg.FS, error) {
	base, subdir, ok := volume.SplitNestedTarget(target)
	if !ok {
		return nil, fmt.Errorf("secret backend: 卷 %q target=%q 非嵌套形态（需 <卷>/<新子目录> 或 local+root）", name, target)
	}
	set := secretDataSet.Load()
	if set == nil {
		return nil, fmt.Errorf("secret backend: 卷 %q 嵌套 target=%q 但卷集未就绪", name, target)
	}
	inner, ok := set.FSFor(base)
	if !ok {
		return nil, fmt.Errorf("secret backend: 卷 %q 底层卷 %q 不可用", name, base)
	}
	e, serr := inner.Stat(ctx, subdir)
	if serr != nil {
		return nil, fmt.Errorf("secret backend: 检查底层子目录 %s/%s 失败: %w", base, subdir, serr)
	}
	if e != nil {
		if adopt, _ := ctx.Value(adoptNestedDirKey{}).(bool); !adopt {
			return nil, fmt.Errorf("secret backend: 底层子目录 %s/%s 已存在（需新空子目录）", base, subdir)
		}
		// 重启收养：校验子目录空/仅本卷专属结构（Minor1）——修复「任何已存在目录都收养」的
		// overclaim：config 误指向既有非 wrapper 目录时，删除封装卷整流删会连带清空误指目录。
		okA, aerr := adoptableNestedDir(ctx, inner, subdir)
		if aerr != nil {
			return nil, aerr
		}
		if !okA {
			return nil, fmt.Errorf("secret backend: 底层子目录 %s/%s 已存在且含非本封装卷内容（文件+目录混合）——需指定新空子目录或人工清理后重试（拒绝收养，防误删）", base, subdir)
		}
		// 前次本封装卷自建（空/仅本卷专属结构，config/store 声明再次装配）→ 复用包装。
		wrap, werr := syncpkg.NewSubFS(inner, subdir)
		if werr != nil {
			return nil, fmt.Errorf("secret backend: 收养封装嵌套底层 %s/%s 失败: %w", base, subdir, werr)
		}
		return wrap, nil
	}
	if err := inner.MakeDir(ctx, subdir); err != nil {
		return nil, fmt.Errorf("secret backend: 创建底层子目录 %s/%s 失败: %w", base, subdir, err)
	}
	wrap, err := syncpkg.NewSubFS(inner, subdir)
	if err != nil {
		return nil, fmt.Errorf("secret backend: 封装嵌套底层 %s/%s 失败: %w", base, subdir, err)
	}
	return wrap, nil
}

// resolveTargetFS 解析 secretdata 的底层卷 FS：
//
//   - `target: ""/local` + extra.root（config/CLI 历史路径）：本地根整卷，不进互斥占用校验，
//     保持原行为（零回归）；
//   - `target: <卷名>/<新子目录>`（用户 2026-10-06 确认的嵌套语义）：见 resolveNestedTargetFS；
//   - 其余 target 形态 → fail-closed 报错（需外部装配/嵌套形态）。
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
		return resolveNestedTargetFS(ctx, v.Name, t)
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
			return secrets.NewManager(fs, regName, true), nil
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
	// **重启收养（CRIT-①，2026-10-07）**：config 声明封装卷重启时其自建子目录已存在——
	// 补装 ctx 置 adoptNestedDir 标记，使 resolveNestedTargetFS 收养复用而非「需新空子目录」
	// boot-fail（首次装配自建目录、二次启动必然命中该拒绝）。fresh 运行时建卷不受影响。
	for _, v := range set.All() {
		if v.Type != volume.TypeSecretdata {
			continue
		}
		if set.External(v.Name) != nil {
			continue // 已装配（测试自注册类型等）
		}
		be, berr := registry.NewBackend(adoptNestedDir(ctx), v)
		if berr != nil {
			return fmt.Errorf("secret backends: 补装 secretdata 卷 %q 失败（boot fail）: %w", v.Name, berr)
		}
		if aerr := set.AttachExternal(v.Name, be); aerr != nil {
			return fmt.Errorf("secret backends: secretdata 卷 %q 挂回卷集失败（boot fail）: %w", v.Name, aerr)
		}
		log.Info("secretdata 卷补装完成（config 声明，推迟装配）", "volume", v.Name)
	}
	return nil
}
