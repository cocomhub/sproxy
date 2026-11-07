// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package volumectx 承载「本次调用解析出的卷上下文」（源卷 + 跨卷目标卷）。
//
// 为什么需要它：卷寻址已统一为**路径内 URI**（`<卷名>:/<路径>`，用户口径 2026-10-14：
// 「所有常见文件命令都要支持卷 uri 操作，移除多余的 --volume flag」），命令层解析 URI 后
// 必须把卷名交给客户端工厂（工厂负责 `?volume=` 查询参数与目标卷参数），而 **URI 解析在
// `main` 包、工厂在 `internal/clientfactory` 包** ⇒ 需要一个共享载体。用命令的
// `context.Context` 传值（**不引入新的用户可见 flag**，也不改协议：线上仍是 `?volume=`）。
package volumectx

import "context"

type (
	sourceKey struct{}
	targetKey struct{}
)

// With 写入**源卷**（URI 指定的卷，或当前工作目录所在卷）。
func With(ctx context.Context, vol string) context.Context {
	if vol == "" {
		return ctx
	}
	if ctx == nil { // 裸 cobra 命令在单测中直接调用时 Context() 可能为 nil
		ctx = context.Background()
	}
	return context.WithValue(ctx, sourceKey{}, vol)
}

// From 读取源卷；未设置返回 ("", false)。
func From(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	v, ok := ctx.Value(sourceKey{}).(string)
	return v, ok && v != ""
}

// WithTarget 写入**跨卷目标卷**（mv/cp 目标路径的 URI 卷；与源卷不同即跨卷操作）。
func WithTarget(ctx context.Context, vol string) context.Context {
	if vol == "" {
		return ctx
	}
	if ctx == nil { // 同上：nil 安全
		ctx = context.Background()
	}
	return context.WithValue(ctx, targetKey{}, vol)
}

// Target 读取目标卷；未设置返回 ("", false)。
func Target(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	v, ok := ctx.Value(targetKey{}).(string)
	return v, ok && v != ""
}
