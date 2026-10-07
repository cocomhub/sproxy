// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// 哨兵错误（对外语义分类）。
var (
	// ErrNotFound 表示文件/目录不存在。
	ErrNotFound = errors.New("baidupcs: not found")
	// ErrAlreadyExists 表示目标已存在（未覆盖时）。
	ErrAlreadyExists = errors.New("baidupcs: already exists")
	// ErrPermissionDenied 表示权限不足（未登录/凭据失效等）。
	ErrPermissionDenied = errors.New("baidupcs: permission denied")
	// ErrInvalidParam 表示参数非法。
	ErrInvalidParam = errors.New("baidupcs: invalid param")
	// ErrTransient 表示可重试的瞬时错误（网络/超时/接口抖动）。
	ErrTransient = errors.New("baidupcs: transient error")
	// ErrUnsupported 表示该能力无直接支持（如无会话/二进制模式执行服务端 Move/Copy）。
	// 用户裁定：无直接能力则明确报错不支持（fail-closed），不静默降级。
	ErrUnsupported = errors.New("baidupcs: unsupported operation")
)

// mapPCSError 把 BaiduPCS 库错误归类为哨兵错误。
// 错误码映射参考 cocom 沉淀（31066/-3/-9 → NotFound；31061/-8 → AlreadyExists 等）。
func mapPCSError(err error) error {
	if err == nil {
		return nil
	}
	// D-M2 修复：ctx 取消原样透传（context.Canceled/DeadlineExceeded 非百度语义错误）——
	// 上层按 ctx 取消识别为任务中止，不归 ErrTransient/目标卷异常。
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	msg := err.Error()
	lower := strings.ToLower(msg)

	switch {
	case containsAny(lower, "not found", "no such file", "does not exist", "文件不存在", "目录不存在"):
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	case containsAny(lower, "already exists", "file exists", "文件已存在", "同名文件"):
		return fmt.Errorf("%w: %v", ErrAlreadyExists, err)
	case containsAny(lower, "permission denied", "access denied", "未登录", "cookie", "权限", "登录"):
		return fmt.Errorf("%w: %v", ErrPermissionDenied, err)
	case containsAny(lower, "deadline exceeded", "timeout", "timed out", "超时", "请稍后再试"):
		return fmt.Errorf("%w: %v", ErrTransient, err)
	}
	return err
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
