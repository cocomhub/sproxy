// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import "net/http"

// mkdir / rmdir 是文件服务域（pkg/files）的**一行薄适配**：实现（含 rmdir 的
// 递归删除、配额释放、checksum 清理）已迁入 pkg/files/dirs.go，此处保留同名同签名
// 方法，使 RegisterRoutes 的两处路由注册（localMux 裸注册 + srvMux fileRoute 包裹）
// 逐字不变。
//
// 依赖装配见 Handlers.fileService（懒装配，见 pkg/server/handlers.go）。
// 按项目规程「抽象先薄包装委托保障一致 → 最终直接用新抽象」，薄适配是过渡形态；
// 重设计阶段域 API 化时再评估去除。

func (h *Handlers) mkdir(w http.ResponseWriter, r *http.Request) {
	h.fileService().Mkdir(w, r)
}

func (h *Handlers) rmdir(w http.ResponseWriter, r *http.Request) {
	h.fileService().Rmdir(w, r)
}
