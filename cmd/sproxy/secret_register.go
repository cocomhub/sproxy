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

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
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
// secrets/ 目录；外部 target（baidupcs/s3/加密卷嵌套）→ 需装配层显式解析注入
// （当前返回错误提示，嵌套封装留后续片）。
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

// registerSecretdataBackendWithFS 注册 secretdata backend 类型构造器（生产/测试）。
// resolveSecret 按卷解析密钥字节（secret_url → set.ResolveURL 读取；注入解耦）。
// 多 target 装配：extra.targets（多 local root 副本列表）→ 构造副本底层 FS →
// NewBackendMultiplicas（写复制全部 target、读主失败回退副本）；无副本 → 单卷 NewBackend。
func registerSecretdataBackendWithFS(typ string, resolveSecret func(ctx context.Context, v volume.Volume) ([]byte, error)) {
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
		opts := secretdata.Options{
			Secret:       secret,
			Algorithm:    vcExtraStr(v, "algorithm"),
			Block:        vcExtraBlockPolicy(v),
			TempDir:      vcExtraStr(v, "temp_dir"),
			MetaPadBytes: vcExtraInt64(v, "meta_pad_bytes"),
			Erasure:      vcExtraBool(v, "erasure"),
			Targets:      vcExtraStrings(v, "targets"),
		}
		if len(replicas) == 0 {
			return secretdata.NewBackend(ctx, v, targetFS, opts)
		}
		return secretdata.NewBackendMultiplicas(ctx, v, targetFS, replicas, opts)
	})
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

// vcExtraInt64 解析 extra.<key> 的整数值。常见形态：解码器（JSON/YAML）把数字读为
// float64、Go 内联 map 为 int/int64。仿 vcExtraBlockPolicy 只接受>0，非正数/缺省
// 返回 0（调用方以 0 传默认，如 secretdata.MetaPadBytes 默认 = Block.Min，0 由
// metaPadTarget 兜底）——避免负数 pad 基准送入 Options。
func vcExtraInt64(v volume.Volume, key string) int64 {
	// 注：Go JSON/YAML 数字恒解码为 float64，float32 分支不可达（M8：删除死分支）。
	switch n := v.Extra[key].(type) {
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

// vcExtraBlockPolicy 解析 extra.block_policy（map；mode/min/max）。
func vcExtraBlockPolicy(v volume.Volume) shardseal.BlockPolicy {
	bp := shardseal.DefaultBlockPolicy()
	if m, _ := v.Extra["block_policy"].(map[string]any); m != nil {
		if vv, ok := m["mode"].(string); ok && vv != "" {
			bp.Mode = vv
		}
		// min/max 可能为 float64（JSON 数字）或 int64。
		if vv, ok := m["min"].(float64); ok && vv > 0 {
			bp.Min = int64(vv)
		}
		if vv, ok := m["max"].(float64); ok && vv > 0 {
			bp.Max = int64(vv)
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

// setupSecretBackends 装配 secrets + secretdata 后端并确保默认 secrets 卷。
// 需在 RegisterRoutes（装配本地卷集合）之后调用；localRoot 为默认卷物理根。
func setupSecretBackends(ctx context.Context, set *registry.Set, localRoot string, logger *slog.Logger) error {
	if set == nil {
		return fmt.Errorf("secret backends: volSet 未装配")
	}
	log := slog.Default()
	if logger != nil {
		log = logger
	}
	if _, err := ensureDefaultSecretsVolume(ctx, set, localRoot, log); err != nil {
		return err
	}
	// 注册 secrets + secretdata 后端（secrets 声明 protocol "secrets"）。
	// secretdata 的密钥经 ResolveURL(secrets://<卷>/<name>) 解析（依赖已装配的默认
	// secrets 卷 + 其 OpenURL 能力）——不再使用全局 secretsResolver 注入（移除可变
	// 全局状态，审查 F-1）。Once 保护重复调用（root.go + 多测试共享注册表）。
	registerSecretsBackend()
	setupSecretdataOnce.Do(func() {
		registerSecretdataBackendWithFS("secretdata",
			func(ctx context.Context, v volume.Volume) ([]byte, error) {
				return defaultSecretdataSecret(ctx, v, set)
			})
	})
	return nil
}
