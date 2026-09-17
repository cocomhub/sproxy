// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// errors.go 定义百度网盘后端的哨兵错误与错误分类。
package baidupcs

import (
	"errors"
	"fmt"
)

// 哨兵错误（errors.Is 可判）。
var (
	ErrNotFound      = errors.New("baidupcs: not found")
	ErrAlreadyExists = errors.New("baidupcs: already exists")
	ErrPermission    = errors.New("baidupcs: permission denied")
	ErrTransient     = errors.New("baidupcs: transient error")
	ErrInvalidParam  = errors.New("baidupcs: invalid parameter")
)

// PCSErrorCategory 是语义错误分类。
type PCSErrorCategory int

const (
	ErrCategoryUnknown PCSErrorCategory = iota
	ErrCategoryNotFound
	ErrCategoryAlreadyExists
	ErrCategoryPermissionDenied
	ErrCategoryTransient
)

// PCSError 是带分类的错误（实现 error + Unwrap）。
type PCSError struct {
	Op       string
	Category PCSErrorCategory
	Err      error
}

func (e *PCSError) Error() string {
	return fmt.Sprintf("baidupcs %s: %v", e.Op, e.Err)
}

func (e *PCSError) Unwrap() error { return e.Err }
