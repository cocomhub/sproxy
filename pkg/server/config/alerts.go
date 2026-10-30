// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// alerts.go：告警规则/告警引擎配置段的数据模型。
package config

import "time"

// AlertRule 是告警规则（source 匹配 + threshold + channels）。
type AlertRule struct {
	Source    string   `yaml:"source" mapstructure:"source"`       // disk_watermark / volume_degraded / sync_failed / login_locked / nat_failure / checksum_mismatch
	Threshold int      `yaml:"threshold" mapstructure:"threshold"` // 百分比（disk_watermark 用）
	Channels  []string `yaml:"channels" mapstructure:"channels"`
}

// AlertConfig 是告警引擎配置（Config 引用）。
type AlertConfig struct {
	Enabled bool        `yaml:"enabled" mapstructure:"enabled"`
	Rules   []AlertRule `yaml:"rules" mapstructure:"rules"`
	// PollInterval 是磁盘水位轮询间隔（默认 60s；0 = 关闭轮询只走事件驱动）。
	PollInterval time.Duration `yaml:"poll_interval" mapstructure:"poll_interval"`
}
