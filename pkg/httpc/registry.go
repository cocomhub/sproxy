// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package httpc

// registry.go —— **按用途收敛客户端**（设计 docs/design-http-client-policy.md §3）。
//
// 目标：同一用途全进程复用**同一个** `*Client`（含策略/超时/连接池/指标）——
// ① 连接池与 TLS 复用；② 策略与超时一处配置；③ 装配层可一次性替换某个用途的行为。

import (
	"sync"
)

// Purpose 是客户端用途（决定共享池与默认策略归属）。
type Purpose string

const (
	// PurposeLLM 大模型调用（llmgate）。
	PurposeLLM Purpose = "llm"
	// PurposeEmbed 向量化调用（aiembed）。
	PurposeEmbed Purpose = "embed"
	// PurposeSelfUpdate 自更新检查/下载（selfupdate）。
	PurposeSelfUpdate Purpose = "selfupdate"
	// PurposeNotify 通知渠道外发（notify）。
	PurposeNotify Purpose = "notify"
	// PurposeCloudAPI 云盘控制面 API（pikpak/baidupcs 等）。
	PurposeCloudAPI Purpose = "cloud-api"
)

var (
	sharedMu       sync.Mutex
	sharedByUseFor = map[Purpose]*Client{}
)

// Shared 返回该用途的进程级**共享**客户端（首次调用生效；后续调用忽略 opt）。
//
// 语义（重要）：
//   - 同 purpose 只创建一次 ⇒ **连接池/策略/指标全局一致**；
//   - opt 的零值有安全默认（共享池 + PolicyStandard）⇒ 调用点可以什么都不传；
//   - 需要**独立**客户端（测试替身、一次性短命 client、库持有）请用 `New`/`NewHTTPClient`
//     并显式 `Options{Isolated: true}`。
func Shared(p Purpose, opt Options) *Client {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if c, ok := sharedByUseFor[p]; ok {
		return c
	}
	c := New(opt)
	sharedByUseFor[p] = c
	return c
}

// SharedCount 返回已创建的共享客户端数量（可观测/测试用）。
func SharedCount() int {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	return len(sharedByUseFor)
}

// ResetSharedForTest 清空共享注册表（仅测试使用）。
func ResetSharedForTest() {
	sharedMu.Lock()
	sharedByUseFor = map[Purpose]*Client{}
	sharedMu.Unlock()
}
