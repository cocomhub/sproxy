// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ai

import "strings"

// FileEvent 是文件变更事件的**领域视图**（装配层从 EventBus 快照转换）。
type FileEvent struct {
	Action string
	Owner  string
	Rel    string
	Size   int64
	Cursor uint64
}

// EventSource 是事件源窄接口（装配层用 EventBus 适配），避免 AI 域反向依赖装配层。
type EventSource interface {
	DrainFileEvents() []FileEvent
}

// IsFileAction 判断事件是否文件变更动作（upload/delete/rename/mkdir/rmdir/version 族）。
func IsFileAction(action string) bool {
	switch action {
	case "upload", "delete", "rename", "mkdir", "rmdir", "version", "restore":
		return true
	}
	return strings.HasPrefix(action, "file.")
}
