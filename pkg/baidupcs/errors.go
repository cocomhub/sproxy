// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
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
)

// mapPCSError 把 BaiduPCS 库错误归类为哨兵错误。
// 错误码映射参考 cocom 沉淀（31066/-3/-9 → NotFound；31061/-8 → AlreadyExists 等）。
func mapPCSError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	lower := toLower(msg)

	switch {
	case containsAny(lower, "not found", "no such file", "does not exist", "文件不存在", "目录不存在"):
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	case containsAny(lower, "already exists", "file exists", "文件已存在", "同名文件"):
		return fmt.Errorf("%w: %v", ErrAlreadyExists, err)
	case containsAny(lower, "permission denied", "access denied", "未登录", "cookie", "权限", "登录"):
		return fmt.Errorf("%w: %v", ErrPermissionDenied, err)
	case containsAny(lower, "deadline exceeded", "timeout", "timed out", "超时", "请稍后再试"),
		errors.Is(err, errTimeout), errors.Is(err, errNotExist):
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

// 内部辅助（避免依赖标准库别名冲突）。
func toLower(s string) string {
	return strings.ToLower(s)
}

// errTimeout / errNotExist 是哨兵（供 mapPCSError 分类）。
var (
	errTimeout  = fmt.Errorf("timeout")
	errNotExist = fmt.Errorf("not exist")
)
