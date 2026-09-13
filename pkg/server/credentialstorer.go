// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"reflect"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// normalizeStorer 把注入的 CredentialStorer 归一为可比较的 nil 语义：
// 接口不为 nil、但底层是 nil 指针（如 `var s *accesskey.CredentialStore = nil` 赋值进
// 接口）时，返回真正的 nil——否则 persistCredentials 的 `== nil` 守卫生效不了，
// 会对 nil 接收者调用 Save 触发 panic（旧具体指针字段不存在此问题；接口提取后
// **唯一**的运行时行为差异点，须在注入边界归一，测试基座零迁移）。
// 注意：不能裸用 reflect.ValueOf(s).IsNil()——对非指针值类型（含满足接口的
// struct 实现）会 panic；先按 Kind 限定 nil 敏感类型再判空。
//
// 归属：本函数是**装配层的注入边界**守卫（它归一的是 options/Handlers 收到的可选项），
// 故留在 pkg/server；它消费的接口与具体 store 都在 pkg/accesskey（S2 归位后）。
func normalizeStorer(s accesskey.CredentialStorer) accesskey.CredentialStorer {
	if s == nil {
		return nil
	}
	v := reflect.ValueOf(s)
	if v.Kind() == reflect.Pointer && v.IsNil() {
		return nil
	}
	return s
}
