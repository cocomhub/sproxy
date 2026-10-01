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

// secretsLocalFS 把本地根下的 secrets 目录视图建为 LocalFS。
func secretsLocalFS(rootDir string) syncpkg.FS {
	return syncpkg.NewLocalFS(rootDir+"/secrets", nil)
}

// registerSecretsBackendWithFS 注册 secrets backend 类型构造器（可测试）：
// 接收「底层卷的 secrets/ 视图 FS」——由装配层解析 target 卷后注入。
func registerSecretsBackendWithFS(typ string, resolveFS func(ctx context.Context, v volume.Volume) (syncpkg.FS, error)) {
	registry.RegisterBackend(typ, func(ctx context.Context, v volume.Volume) (registry.ExternalBackend, error) {
		fs, err := resolveFS(ctx, v)
		if err != nil {
			return nil, err
		}
		return secrets.NewBackend(ctx, v, fs)
	})
}

// registerSecretsBackend 注册 secrets backend（生产）。
var registerSecretsOnce sync.Once

func registerSecretsBackend() {
	registerSecretsOnce.Do(func() {
		registerSecretsBackendWithFS("secrets", defaultSecretsFS)
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

// secretsManagerAdapter 把 secrets.Manager 包装为 ExternalBackend（FS 视图 = 底层 FS）。
type secretsManagerAdapter struct {
	mgr *secrets.Manager
	fs  syncpkg.FS
}

func (b *secretsManagerAdapter) FS() syncpkg.FS { return b.fs }
func (b *secretsManagerAdapter) Close() error   { return nil }

// SecretsManager 暴露内部 Manager（供装配层/测试反取，匹配 secrets.ManagerOfExternal 接口）。
func (b *secretsManagerAdapter) SecretsManager() *secrets.Manager { return b.mgr }

// ensureDefaultSecretsVolume 启动时在默认卷（本地）构造默认 secrets 卷并挂到
// registry.Set（注册名 default-secrets，避免与系统默认卷名 default 冲突；URI
// secrets://default/<name> 与省略卷名均解析到本卷）。幂等（已存在同名卷跳过）。
// 返回 secrets.Manager（供后续 secretdata 卷解析 secret_url）。
func ensureDefaultSecretsVolume(ctx context.Context, set *registry.Set, defaultRoot string, logger *slog.Logger) (*secrets.Manager, error) {
	const regName = "default-secrets"
	if be := set.External(regName); be != nil {
		// 已有同名卷（用户显式配置）——取出其 FS 视图返回。
		if fs := be.FS(); fs != nil {
			return secrets.NewManager(fs, regName, true), nil
		}
	}
	root := defaultRoot + "/secrets"
	fs := secretsLocalFS(root)
	mgr := secrets.NewManager(fs, regName, true)
	be := &secretsManagerAdapter{mgr: mgr, fs: fs}
	if err := set.AddExternalVolume(volume.Volume{Name: regName, Type: "secrets", Extra: map[string]any{"target": "local", "root": defaultRoot}}, be); err != nil {
		return nil, fmt.Errorf("装配默认 secrets 卷失败: %w", err)
	}
	if logger != nil {
		logger.Info("默认 secrets 卷已装配", "volume", regName, "root", root)
	}
	return mgr, nil
}

// ---- secretdata backend ----

// registerSecretdataBackendWithFS 注册 secretdata backend 类型构造器（生产/测试）。
// resolveSecret 按卷解析密钥字节（secret_url → secrets 卷读取；注入解耦）。
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
		opts := secretdata.Options{
			Secret:    secret,
			Algorithm: vcExtraStr(v, "algorithm"),
			Block:     vcExtraBlockPolicy(v),
			TempDir:   vcExtraStr(v, "temp_dir"),
		}
		return secretdata.NewBackend(ctx, v, targetFS, opts)
	})
}

// registerSecretdataBackend 注册 secretdata backend（生产）。
var registerSecretdataOnce sync.Once

func registerSecretdataBackend() {
	registerSecretdataOnce.Do(func() {
		registerSecretdataBackendWithFS("secretdata", defaultSecretdataSecret)
	})
}

// secretsResolver 是装配层注入的 secrets URL → 密钥解析器（默认 secrets 卷的
// Manager 封装）；nil 时 defaultSecretdataSecret 报错（fail-closed）。
var secretsResolver func(ctx context.Context, url string) ([]byte, error)

// setSecretsResolver 注入 secrets URL → 密钥解析器。
func setSecretsResolver(fn func(ctx context.Context, url string) ([]byte, error)) {
	secretsResolver = fn
}

// defaultSecretdataSecret 解析密钥：Extra.secret_url（secrets://<卷>/<name>）读取。
func defaultSecretdataSecret(ctx context.Context, v volume.Volume) ([]byte, error) {
	if secretsResolver == nil {
		return nil, fmt.Errorf("secretdata backend: 卷 %q 的 secrets 解析器未装配（需先装配 secrets 卷）", v.Name)
	}
	url, _ := v.Extra["secret_url"].(string)
	if strings.TrimSpace(url) == "" {
		return nil, fmt.Errorf("secretdata backend: 卷 %q 需配置 extra.secret_url（secrets://<卷>/<name>）", v.Name)
	}
	return secretsResolver(ctx, url)
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
	mgr, err := ensureDefaultSecretsVolume(ctx, set, localRoot, log)
	if err != nil {
		return err
	}
	// 注入 secrets → 密钥解析器：默认 secrets 卷（注册名 default-secrets）；
	// URI 卷名缺省或 "default"（设计 §6.1 的 secrets://default/）→ 默认卷；
	// 显式卷名 → 按 Set.External 分派（已装配的 secrets 卷）。
	setSecretsResolver(func(ctx context.Context, url string) ([]byte, error) {
		u := strings.TrimPrefix(url, "secrets://")
		if u == url {
			return nil, fmt.Errorf("secret 解析: 非法 secret_url %q（需 secrets://<卷>/<name>）", url)
		}
		vol, name := u, ""
		if before, after, ok := strings.Cut(u, "/"); ok {
			vol, name = before, after
		}
		if name == "" {
			return nil, fmt.Errorf("secret 解析: %q 缺 secret 名", url)
		}
		if vol == "" || vol == "default" || vol == "default-secrets" {
			return mgr.Read(ctx, name)
		}
		be := set.External(vol)
		m := secrets.ManagerOfExternal(be)
		if m == nil {
			return nil, fmt.Errorf("secret 解析: 卷 %q 不是已装配的 secrets 卷", vol)
		}
		return m.Read(ctx, name)
	})
	registerSecretsBackend()
	registerSecretdataBackend()
	return nil
}
