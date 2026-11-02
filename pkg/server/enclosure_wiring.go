// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// enclosure_wiring.go 是封装卷切片（`pkg/server/enclosure`）的**装配侧适配**：该切片只依赖
// 卷集（`*registry.Set`）等下层能力，装配根负责把自身持有的卷集显式传入（领域包不反向依赖
// `pkg/server`，R6）。

import serverenclosure "github.com/cocomhub/sproxy/pkg/server/enclosure"

// enclosureSealedAt 判定卷上 rootRel 是否落在封装封印区间内（装配侧薄适配；实现见
// `serverenclosure.SealedAt`）。保留方法形态供既有旁路写面（presign 签发）调用。
func (h *Handlers) enclosureSealedAt(volName, rootRel string) error {
	if h == nil {
		return nil
	}
	return serverenclosure.SealedAt(h.volSet, volName, rootRel)
}
