// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// ratelimit.go：限流配置段的数据模型。
package config

import "time"

// EndpointLimit 是 per-endpoint 限流规则（rate_limit.endpoints 段：path → {limit, window}）。
// Limit<=0 / Window<=0 沿用 NewRateLimiter 的默认化（归 5 / 1s，与 UpdateConfig 一致）。
type EndpointLimit struct {
	Limit  int           `yaml:"limit" mapstructure:"limit"`
	Window time.Duration `yaml:"window" mapstructure:"window"`
}
