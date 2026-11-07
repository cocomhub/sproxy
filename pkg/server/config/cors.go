// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// cors.go：CORS 配置段的数据模型。
package config

// CORSConfig 定义 CORS 跨域配置。
type CORSConfig struct {
	// AllowedOrigins 允许的跨域来源列表，设置 ["*"] 允许任意来源。
	// 为空时 CORS 中间件直接透传（保持向后兼容）。
	AllowedOrigins []string `yaml:"allowed_origins" mapstructure:"allowed_origins"`
	// AllowedHeaders 允许的请求头列表，为空时使用默认值。
	AllowedHeaders []string `yaml:"allowed_headers" mapstructure:"allowed_headers"`
	// MaxAge 预检请求缓存时间（秒），默认 86400。
	MaxAge int `yaml:"max_age" mapstructure:"max_age"`
}

// DefaultMaxAge 是 CORS 预检请求的默认缓存时间（秒）。
const DefaultMaxAge = 86400
