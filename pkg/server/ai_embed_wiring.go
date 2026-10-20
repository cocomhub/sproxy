// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// ai_embed_wiring.go 把 embedding 客户端的「成功嵌入」接进审计（`ai.embed`）。
//
// 为什么需要这一层：`pkg/aiembed` 是通用客户端（也用于 CLI/离线工具），**不能**依赖
// `pkg/server` 的审计服务；因此把「观察者」作为可选接缝（`aiembed.Config.Observer`），
// 由服务端装配时注入本文件的实现。这样：
//   - 嵌入动作（会产生**外部 API 调用**并携带用户内容）必须可追溯：谁、用哪个 provider/model、几条；
//   - Detail **只写规模元数据**，绝不写被嵌入的文本（文本=用户内容，进审计即扩散）。

import (
	"context"
	"fmt"

	"github.com/cocomhub/sproxy/pkg/aiembed"
)

// NewEmbedClient 构造 embedding 客户端并注入审计观察者。
//
// 服务端所有需要向量化的路径（语义检索、聚类等）都应经本构造函数取客户端 ⇒ 自动留痕；
// 直接 `aiembed.New` 只应在测试/一次性工具中使用。
func (h *Handlers) NewEmbedClient(cfg aiembed.Config) *aiembed.Client {
	cfg.Observer = func(ctx context.Context, provider, model string, count int) {
		h.RecordAudit(ctx, AuditEvent{
			Action: ActionAIEmbed, ObjectType: "ai", Object: provider,
			Result: AuditResultSuccess,
			Detail: fmt.Sprintf("model=%s n=%d", model, count),
		})
	}
	return aiembed.New(cfg)
}
