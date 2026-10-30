// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// notify.go：notify 配置段的数据模型（通知规则/渠道/各渠道凭据）。运行时的 NotifyCenter 留在 pkg/server。
package config

import "time"

// NotifyRule 是通知路由规则（action glob → channels）。
// Action 支持 "*"（全部）与精确匹配（如 "upload"）；Object 同 Action 粒度。
type NotifyRule struct {
	Action   string   `yaml:"action" mapstructure:"action"`
	Object   string   `yaml:"object" mapstructure:"object"` // 空 = 全部对象
	Channels []string `yaml:"channels" mapstructure:"channels"`
}

// NotifyChannelsConfig 是渠道配置（wecom / serverchan）。
type NotifyChannelsConfig struct {
	Wecom        WecomConfig        `yaml:"wecom" mapstructure:"wecom"`
	ServerChan   ServerChanConfig   `yaml:"serverchan" mapstructure:"serverchan"`
	Email        EmailConfig        `yaml:"email" mapstructure:"email"`
	Webhook      WebhookConfig      `yaml:"webhook" mapstructure:"webhook"`
	Alertmanager AlertmanagerConfig `yaml:"alertmanager" mapstructure:"alertmanager"`
	Grafana      GrafanaConfig      `yaml:"grafana" mapstructure:"grafana"`
}

// WecomConfig 企业微信机器人配置。
type WecomConfig struct {
	Webhook string `yaml:"webhook" mapstructure:"webhook"`
}

// ServerChanConfig Server 酱配置。
type ServerChanConfig struct {
	SCTKey string `yaml:"sct_key" mapstructure:"sct_key"`
}

// NotifyConfig 是 notify 配置段（Config 引用）。
type NotifyConfig struct {
	Enabled   bool                 `yaml:"enabled" mapstructure:"enabled"`
	Rules     []NotifyRule         `yaml:"rules" mapstructure:"rules"`
	Debounce  time.Duration        `yaml:"debounce" mapstructure:"debounce"`
	Retry     int                  `yaml:"retry" mapstructure:"retry"`
	RetryBase time.Duration        `yaml:"retry_base" mapstructure:"retry_base"`
	Channels  NotifyChannelsConfig `yaml:"channels" mapstructure:"channels"`
	// AIAdvisor 是告警根因建议配置（roadmap 11.9-⑥；默认关零回归）。
	AIAdvisor AIAdvisorConfig `yaml:"ai_advisor" mapstructure:"ai_advisor"`
	// FeedMax 是 /api/notify/feed 最大条目数（roadmap 11.7-⑦；<=0 默认 20，上限 50）。
	FeedMax int `yaml:"feed_max" mapstructure:"feed_max"`
	// AIInsight 是 AI 文件洞察配置（roadmap 11.9-⑤；默认关零回归）。
	AIInsight AIInsightConfig `yaml:"ai_insight" mapstructure:"ai_insight"`
	// AISearch 是语义搜索配置（roadmap 11.9-④；默认关零回归）。
	AISearch AISearchConfig `yaml:"ai_search" mapstructure:"ai_search"`
	// AIQuota 是 AI 调用配额配置（roadmap 11.9-⑦；默认关零回归）。
	AIQuota AIQuotaConfig `yaml:"ai_quota" mapstructure:"ai_quota"`
	// AIPrivacy 是 AI 派生数据隐私配置（roadmap 11.9-⑧；默认关零回归）。
	AIPrivacy AIPrivacyConfig `yaml:"ai_privacy" mapstructure:"ai_privacy"`
	// AIEvents 是 AI 事件流水线配置（roadmap 12.2-4；默认关零回归）：
	// enabled=true 且 EventBus 已装配 → 变更事件去重入队 → AI 处理任务队列。
	AIEvents AIEventsConfig `yaml:"ai_events" mapstructure:"ai_events"`
	// FeedToken 是 /api/notify/feed 可选访问令牌（roadmap 11.7-⑦）：
	// 空 = 公开（默认零回归）；非空 = GET /api/notify/feed 必须带 `?token=<t>`
	// 或 `Authorization: Bearer <t>`（常量时间比较），否则 401。
	// feed 会暴露审计信息：生产建议配置。token 不随 SIGHUP 重载（重启生效）。
	FeedToken string `yaml:"feed_token" mapstructure:"feed_token"`
}

// EmailConfig 邮箱渠道配置。
type EmailConfig struct {
	SMTPHost string   `yaml:"smtp_host" mapstructure:"smtp_host"`
	Port     int      `yaml:"port" mapstructure:"port"`
	From     string   `yaml:"from" mapstructure:"from"`
	To       []string `yaml:"to" mapstructure:"to"`
	Username string   `yaml:"username" mapstructure:"username"`
	Password string   `yaml:"password" mapstructure:"password"`
}

// WebhookConfig 通用 Webhook 渠道配置。
// secret 为空 = 出站不签名（零回归；仅建议内网/受信网络使用）；
// 非空 = 出站携带 HMAC-SHA256 签名头（防伪造回调/篡改）。
type WebhookConfig struct {
	URL string `yaml:"url" mapstructure:"url"`
	// Secret 是共享签名密钥（yaml `secret`）；空 = 不加签名头，零回归。
	// 仅配置注入，**不入审计日志**。
	Secret string `yaml:"secret" mapstructure:"secret"`
	// SignHeader 是签名头名（默认 X-Sproxy-Signature）。
	SignHeader string `yaml:"sign_header" mapstructure:"sign_header"`
	// TimestampHeader 是时间戳头名（默认 X-Sproxy-Timestamp）。
	TimestampHeader string `yaml:"timestamp_header" mapstructure:"timestamp_header"`
	// ClockSkew 是接收侧允许的时钟漂移（默认 5m；仅 VerifyWebhookSignature 用）。
	ClockSkew time.Duration `yaml:"clock_skew" mapstructure:"clock_skew"`
}

// AlertmanagerConfig Alertmanager 渠道配置（roadmap P2 Webhook 通用插件残余）：
// 以 Alertmanager webhook v2 格式（alerts 数组）POST 到 AM 接收端点。
type AlertmanagerConfig struct {
	URL string `yaml:"url" mapstructure:"url"`
}

// GrafanaConfig Grafana 标注适配（roadmap 同残余）：推送 annotations 到 Grafana
// 标注 API（basic auth 可选），通知即落 Grafana 面板标注（dashboard 联动）。
type GrafanaConfig struct {
	URL      string `yaml:"url" mapstructure:"url"`
	Token    string `yaml:"token" mapstructure:"token"`
	User     string `yaml:"user" mapstructure:"user"`
	Password string `yaml:"password" mapstructure:"password"`
}
