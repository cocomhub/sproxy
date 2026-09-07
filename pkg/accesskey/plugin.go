// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

import (
	"fmt"
	"reflect"
	"sync"
)

// storerRegistry 是 storer（CredentialStorer / SecureStorer / 未来实现）的插件
// 注册表，仿 pkg/tunnel/xfer.Register 风格：全局单例 + 名字路由 + 类型参数化取值。
//
// 设计说明：本注册表用 `map[string]any` 混存多接口类型（CredentialStorer /
// SecureStorer / 未来实现），而非复用 pkg/plugin.Registry[T]（泛型单接口表）——
// 泛型注册表按接口类型注册需各建实例、再叠加名字路由，一张表混存时复杂度不减；
// 保持单一注册表 + GetStorer[T] 类型参数化取值更贴合 4C（KMS 插件多接口接入）。
//
// 4C-2（KMS 加密实现）经 RegisterStorer 把加密 storer 注入，宿主按名装配。
type storerRegistry struct {
	sync.RWMutex
	m map[string]any
}

// registry 是全局 storer 注册表。包级单例：注册/反注册/取值均并发安全。
var registry = &storerRegistry{m: map[string]any{}}

// RegisterStorer 注册一个 storer（CredentialStorer / SecureStorer / 未来实现）。
// 重名注册返回错误（防覆盖静默替换）；同名反注册后可重新注册。
//
// 值校验：真实 nil 与 typed-nil（即把 nil 的 nil-able 值装箱进接口，如
// `(*CredentialStore)(nil)`、nil map 派生类型）一律拒绝——typed-nil 入库后
// GetStorer[T] 可能返回 ok=true 的 nil 底层值，调用方再调其方法会对 nil 接收者
// panic（comma-ok 类型断言本身不 panic）。
func RegisterStorer(name string, v any) error {
	if name == "" {
		return fmt.Errorf("accesskey: register storer: 空名字")
	}
	if v == nil {
		return fmt.Errorf("accesskey: register storer %q: nil 值", name)
	}
	// 拒绝 nil-able kind 的 typed-nil（reflect.IsNil 仅对 Chan/Func/Map/Pointer/
	// Slice/Interface 合法；值类型 struct 实现不入此分支）。
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.Slice, reflect.Interface:
		if rv.IsNil() {
			return fmt.Errorf("accesskey: register storer %q: typed-nil 值（nil 底层装箱）", name)
		}
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
//
// 注意：typed-nil 值入库后本函数可能返回 ok=true 的 nil 底层值，调用方再调其
// 方法会对 nil 接收者 panic——RegisterStorer 已拒绝 nil 指针等 typed-nil 入库，
// 调用方也不应绕过注册表直接写入 registry。
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
