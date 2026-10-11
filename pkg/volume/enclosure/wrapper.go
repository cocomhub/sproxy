// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enclosure

import (
	"context"
	"fmt"
	"sync"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// wrapper.go 是**封装处理层注册表**：把「对字节做变换」的实现（加解密/镜像/压缩/脱敏…）
// 变成可插拔组件，使封装卷能力可被任意处理层复用（新增处理层只需实现 Wrapper 并注册，
// 视图/封印/配额/直链抑制全部沿用本包）。
//
// 与卷类型解耦：wrapper 不认识源卷是谁（只拿到已就绪的源视图 FS），也不做配额与防护。

// Wrapper 是封装处理层：给定源视图（已按声明前缀映射）构造外部卷后端。
type Wrapper interface {
	// Name 返回注册名（volumes[].extra.wrapper 引用它）。
	Name() string
	// Build 用源视图 src 构造后端（opts = 该卷 extra，处理层自解析自身配置）。
	// 实现必须：只经 src 读写源数据（不得绕过视图直连源存储）；不承担封印/配额职责。
	Build(ctx context.Context, v volume.Volume, src syncpkg.FS, opts map[string]any) (registry.ExternalBackend, error)
}

var (
	wrapperMu sync.RWMutex
	wrappers  = map[string]Wrapper{}
)

// RegisterWrapper 注册封装处理层（重复注册同名 panic——装配期 fail-fast，防静默覆盖）。
func RegisterWrapper(w Wrapper) {
	if w == nil || w.Name() == "" {
		panic("enclosure: RegisterWrapper 需要非空 Name")
	}
	wrapperMu.Lock()
	defer wrapperMu.Unlock()
	if _, exists := wrappers[w.Name()]; exists {
		panic("enclosure: wrapper 重复注册: " + w.Name())
	}
	wrappers[w.Name()] = w
}

// LookupWrapper 按名取处理层（未注册 → ok=false）。
func LookupWrapper(name string) (Wrapper, bool) {
	wrapperMu.RLock()
	defer wrapperMu.RUnlock()
	w, ok := wrappers[name]
	return w, ok
}

// WrapperNames 返回已注册处理层名（诊断/错误信息用）。
func WrapperNames() []string {
	wrapperMu.RLock()
	defer wrapperMu.RUnlock()
	out := make([]string, 0, len(wrappers))
	for n := range wrappers {
		out = append(out, n)
	}
	return out
}

// Build 按声明构造封装卷后端：源视图 = PrefixFS(源 FS, 声明前缀)，再交给处理层。
// 装配顺序约束：调用方必须在上游**尚未注入封印**时取得 src（封装卷自身不受封印影响）。
func (d Decl) Build(ctx context.Context, v volume.Volume, src syncpkg.FS) (registry.ExternalBackend, error) {
	w, ok := LookupWrapper(d.Wrapper)
	if !ok {
		return nil, fmt.Errorf("%w: %q（卷 %q；已注册: %v）", ErrUnknownWrapper, d.Wrapper, v.Name, WrapperNames())
	}
	if src == nil {
		return nil, fmt.Errorf("enclosure: 卷 %q 源视图为空（装配顺序错误）", v.Name)
	}
	view := syncpkg.NewPrefixFS(src, d.KeySpacePrefix())
	return w.Build(ctx, v, view, v.Extra)
}
