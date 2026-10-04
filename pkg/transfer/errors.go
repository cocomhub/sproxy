// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package transfer

import "errors"

// 哨兵错误（调用方经 errors.Is 分类；fail-closed 语义）。
var (
	// ErrEmptySource 源条目为空。
	ErrEmptySource = errors.New("transfer: 源条目为空")
	// ErrNoDestination 目标卷未装配。
	ErrNoDestination = errors.New("transfer: 目标卷未装配")
	// ErrIndividualMulti individual 模式要求单条目。
	ErrIndividualMulti = errors.New("transfer: individual 模式仅支持单条目")
	// ErrExists 目标已存在且不允许覆盖（WriteIfAbsent 拒绝）。
	ErrExists = errors.New("transfer: 目标已存在（拒绝覆盖）")
)
