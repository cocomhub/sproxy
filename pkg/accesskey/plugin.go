// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

import (
	"fmt"
	"sync"
)

// storerRegistry 是 storer（CredentialStorer / SecureStorer / 未来实现）的插件
// 注册表，仿 pkg/tunnel/xfer.Register 风格：全局单例 + 名字路由 + 类型参数化取值。
// 4C-2（KMS 加密实现）经 RegisterStorer 把加密 storer 注入，宿主按名装配。
type storerRegistry struct {
	sync.RWMutex
	m map[string]any
}

// registry 是全局 storer 注册表。包级单例：注册/反注册/取值均并发安全。
var registry = &storerRegistry{m: map[string]any{}}

// RegisterStorer 注册一个 storer（CredentialStorer / SecureStorer / 未来实现）。
// 重名注册返回错误（防覆盖静默替换）；同名反注册后可重新注册。
func RegisterStorer(name string, v any) error {
	if name == "" {
		return fmt.Errorf("accesskey: register storer: 空名字")
	}
	if v == nil {
		return fmt.Errorf("accesskey: register storer %q: nil 值", name)
	}
	registry.Lock()
	defer registry.Unlock()
	if _, ok := registry.m[name]; ok {
		return fmt.Errorf("accesskey: register storer %q: 已存在", name)
	}
	registry.m[name] = v
	return nil
}

// UnregisterStorer 反注册指定名字的 storer（测试清理 / 插件热移除用）。
// 名字不存在时为 no-op。
func UnregisterStorer(name string) {
	registry.Lock()
	defer registry.Unlock()
	delete(registry.m, name)
}

// GetStorer 按目标类型取已注册 storer：名字已注册、且值与类型参数 T 匹配时
// 返回 (值, true)；未注册或类型不匹配返回 (零值, false)。
func GetStorer[T any](name string) (T, bool) {
	var zero T
	registry.RLock()
	defer registry.RUnlock()
	v, ok := registry.m[name]
	if !ok {
		return zero, false
	}
	typed, ok := v.(T)
	if !ok {
		return zero, false
	}
	return typed, true
}
