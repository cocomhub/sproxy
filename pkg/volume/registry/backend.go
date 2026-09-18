// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"fmt"
	"sync"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// ExternalBackend 是外部卷（非本地文件系统卷）的运行时句柄。
//
// 与本地卷的 storage.Root 对应：本地卷由装配层 OpenRoot 建根句柄，外部卷由
// backend 构造器返回本接口。FS 是外部卷的同步视图（sync.FS），供 syncexec
// 工厂按 remote.volume 查询后直接驱动同步引擎；Close 释放后端资源（连接/临时目录）。
type ExternalBackend interface {
	// FS 返回外部卷的同步视图（sync.FS）。装配后只读（多次调用返回同一实例）。
	FS() syncpkg.FS
	// Close 释放后端资源（幂等：重复调用安全）。
	Close() error
}

// BackendFactory 按卷描述构造外部后端（从 v.Extra 读类型特有配置）。
// 返回的 ExternalBackend 由装配层持有（并入 registry.Set），随 Set.Close 统一关闭。
type BackendFactory func(ctx context.Context, v volume.Volume) (ExternalBackend, error)

// backendFactories 是后端类型 → 构造器注册表（可插拔）。
// 由各后端包（或装配层）经 RegisterBackend 注册；NewBackend 按 v.Type 分派。
// backendMu 串行化读写（注册发生在装配期，查询在执行期，跨 goroutine；RWMutex 保并发安全）。
var (
	backendMu        sync.RWMutex
	backendFactories = map[string]BackendFactory{}
)

// RegisterBackend 注册卷后端类型构造器（可插拔扩展）。
//
// 重复注册同一类型 → panic（编程错误，仿标准库 Register 语义——重复注册意味着
// 两个包声明了同一类型的所有权，装配期应 fail-fast 暴露而非静默覆盖）。
// 空类型名 → panic（type 空串 = 本地卷，不允许被外部后端占用）。
func RegisterBackend(typ string, f BackendFactory) {
	if typ == "" || typ == volume.TypeLocal {
		panic(fmt.Sprintf("registry: 非法后端类型 %q（空串与 %q 保留给本地卷）", typ, volume.TypeLocal))
	}
	if f == nil {
		panic(fmt.Sprintf("registry: 后端类型 %q 的构造器为 nil", typ))
	}
	backendMu.Lock()
	defer backendMu.Unlock()
	if _, dup := backendFactories[typ]; dup {
		panic(fmt.Sprintf("registry: 后端类型 %q 重复注册", typ))
	}
	backendFactories[typ] = f
}

// NewBackend 按 v.Type 分派到已注册的后端构造器构造外部后端。
//
// 未注册的 Type → 明确错误（fail-closed，不回落 local——回落会静默把外部卷当本地
// 目录打开，产生错误数据位置）。v.Type 空串/"local" 应由装配层走本地卷路径，不应
// 调用本函数（防御：直接报错）。
func NewBackend(ctx context.Context, v volume.Volume) (ExternalBackend, error) {
	typ := v.Type
	if typ == "" || typ == volume.TypeLocal {
		return nil, fmt.Errorf("registry: 卷 %q 类型 %q 是本地卷（不应经后端构造器分派）", v.Name, typ)
	}
	backendMu.RLock()
	f, ok := backendFactories[typ]
	backendMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("registry: 卷 %q 类型 %q 未注册后端（需 RegisterBackend 注册）", v.Name, typ)
	}
	be, err := f(ctx, v)
	if err != nil {
		return nil, fmt.Errorf("registry: 卷 %q 类型 %q 后端构造失败: %w", v.Name, typ, err)
	}
	if be == nil {
		return nil, fmt.Errorf("registry: 卷 %q 类型 %q 后端构造器返回 nil（装配错误）", v.Name, typ)
	}
	return be, nil
}

// BackendTypes 返回已注册后端类型列表（V4：backend 列表 API 数据源；Web/CLI 动态感知）。
//
// 顺序不承诺稳定（map 遍历）；调用方（列表 API/UI 下拉）不得依赖顺序。
// 返回副本（防调用方改写内部 map 键）；空注册表 → 空切片（非 nil，JSON 序列化为 []）。
func BackendTypes() []string {
	backendMu.RLock()
	defer backendMu.RUnlock()
	if len(backendFactories) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(backendFactories))
	for typ := range backendFactories {
		out = append(out, typ)
	}
	return out
}

// UnregisterBackendForTest 移除测试注册的后端（测试辅助：跨包测试（如 pkg/server）注册
// fake backend 后清理，防污染共享注册表）。生产代码不得调用。
func UnregisterBackendForTest(typ string) {
	backendMu.Lock()
	defer backendMu.Unlock()
	delete(backendFactories, typ)
}
