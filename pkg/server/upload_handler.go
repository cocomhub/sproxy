// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// upload_handler.go 是文件服务写面（`POST /upload`）在装配层的残留：处理器实现已迁入
// pkg/files/write.go（接收者 *Service），此处保留两样东西：
//
//  1. **一行薄适配** `upload`——使 RegisterRoutes 的两处路由注册（localMux 裸注册 +
//     srvMux fileRoute 包裹）逐字不变；
//  2. **跨族共享的纯函数** `atomicRenameRoot` / `copyWithContext`——它们在 pkg/server 侧
//     另有消费者（跨卷 move：volumes_api.go；share.go），既不随上传族迁走、pkg/files 也
//     无法反向 import 本包，故两侧各留一份（领域侧实现见 pkg/files/service.go 与 write.go），
//     等价性由 helper_impl_drift_test.go 的源码级断言守卫。
//
// 依赖装配见 Handlers.fileService（懒装配，见 pkg/server/handlers.go）。

import (
	"context"
	"io"
	"net/http"

	"github.com/cocomhub/sproxy/pkg/storage"
)

func (h *Handlers) upload(w http.ResponseWriter, r *http.Request) {
	h.fileService().Upload(w, r)
}

// atomicRenameRoot 在 storage.Root 内原子重命名 srcRel → dstRel。
// 与 atomicRename 对齐：快速路径直接 Rename，失败（Windows 并发场景）先删除目标
// 再重命名，并使用短退避重试以应对 Windows 句柄释放延迟。
//
// 本包消费者：跨卷 move（volumes_api.go）。领域侧等价实现见 pkg/files/service.go。
// atomicRenameRoot 在 storage.Root 内原子重命名 srcRel → dstRel。
//
// 实现单源在 `storage.Root.AtomicRename`（重试/退避是存储原语）；本包装保留只为调用点
// 稳定。本包消费者：跨卷 move（volumes_api.go）。领域侧等价包装见
// pkg/files/service.go，委托关系由 helper_impl_drift_test.go 的
// TestAtomicRenameRoot_DelegatesToStorage 守卫。
func atomicRenameRoot(root *storage.Root, srcRel, dstRel string) error {
	return root.AtomicRename(srcRel, dstRel)
}

// copyWithContext 是 context-aware 的 io.Copy，每次 Read/Write 前检查 ctx.Done()。
//
// 本包消费者：跨卷 move（volumes_api.go）与分享下载（share.go）。领域侧等价实现见
// pkg/files/write.go（单次上传的原子写入用）。
func copyWithContext(w io.Writer, r io.Reader, ctx context.Context) (int64, error) {
	var total int64
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
		n, err := r.Read(buf)
		if n > 0 {
			nn, werr := w.Write(buf[:n])
			total += int64(nn)
			if werr != nil {
				return total, werr
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}
