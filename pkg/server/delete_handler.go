// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// delete_handler.go 是文件服务写面「删除族」（`POST /delete`、`POST /api/batch/delete`）
// 在装配层的**一行薄适配**：实现（含跨卷定位、文件级互斥、checksum 门禁、双账本释放、
// 审计）已迁入 pkg/files/delete.go，此处保留同名同签名方法，使 RegisterRoutes 的四处路由
// 注册（localMux 裸注册 + srvMux fileRoute 包裹各两处）逐字不变。
//
// 依赖装配见 Handlers.fileService（懒装配，见 pkg/server/handlers.go）。
// 按项目规程「抽象先薄包装委托保障一致 → 最终直接用新抽象」，薄适配是过渡形态；
// 重设计阶段域 API 化时再评估去除。

import "net/http"

func (h *Handlers) delete(w http.ResponseWriter, r *http.Request) {
	h.fileService().Delete(w, r)
}

func (h *Handlers) batchDelete(w http.ResponseWriter, r *http.Request) {
	h.fileService().BatchDelete(w, r)
}
