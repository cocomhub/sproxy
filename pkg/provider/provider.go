// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package provider 定义配置解码抽象接口，用于解耦 pkg 层对 viper 的直接依赖。
package provider

// Provider 抽象配置解码能力。
// pkg/server 和 pkg/client 通过此接口从任何配置源（Viper、map、JSON 等）加载配置。
type Provider interface {
	// Unmarshal 将配置解码到目标结构体中。
	// obj 必须是指向结构体的指针，其字段带有 mapstructure 或 yaml tag。
	Unmarshal(obj any) error
}

// Refresher 是可选的配置重载接口，用于 SIGHUP 信号重新读取配置文件。
type Refresher interface {
	// Refresh 重新读取配置源并更新内部状态。
	// 调用后，后续 Unmarshal 调用应返回最新配置。
	Refresh() error
}
