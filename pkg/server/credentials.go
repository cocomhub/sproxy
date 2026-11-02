// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// credentials.go 是**凭据的装配与自省**：bootstrapCredentials（启动时把凭据 Ring 与关联
// store 装到 Handlers 上，含零凭据启动语义）与 SelfCredential（只暴露本进程自身凭据）。
//
// 迁移说明：凭据的**存储后端与主密钥解析**已下沉 `pkg/server/credentials`
// （BootstrapServerCredentials / ResolveCredentialMasterKey / Ring 凭据选取）。本文件只保留
// 依赖装配态（`h`）或装配 opts 的部分：启动装配回调、自用凭据自省、凭据轮换循环
// （credential_rotation.go）。
//
// 拆分说明见 handlers.go 顶部。

package server

import (
	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/server/credentials"
)

// bootstrapCredentials 装配凭据 Ring 与关联 store（RegisterRoutes 启动时调用一次）：
//   - 显式注入（opts.CredentialRing）→ 直接使用；
//   - 否则从 opts.CredentialStore 载入快照（真实/损坏处理见 CredentialStore.Load）；
//   - **U3：零凭据启动**——不再生成首启 anonymous 凭据（4A 的 generateBootstrapCredential
//     路径已移除）。store 为空 = 系统以零凭据等待注册：register 公开端点是唯一用户
//     入口，首个经回环注册的用户由 AddRegistration 原子授 admin（DEC-F/D2）。
//     空 store 时记启动日志提示「首次注册经回环，将成为 admin」（S2）。
func (h *Handlers) bootstrapCredentials(opts RegisterRoutesOpts) {
	store := normalizeStorer(opts.CredentialStore)
	if opts.CredentialRing != nil {
		h.credentialRing = opts.CredentialRing
		h.credentialStore = store
		return
	}
	ring := accesskey.NewRing()
	if store != nil {
		if keys, err := store.Load(); err != nil {
			h.logger.Error("载入凭据 store 失败（fail-closed：拒绝启动，防止用空凭据表运行）", "error", err)
			panic("载入凭据 store 失败: " + err.Error())
		} else if len(keys) > 0 {
			if rerr := ring.Replace(keys); rerr != nil {
				h.logger.Error("重建凭据 Ring 失败（fail-closed）", "error", rerr)
				panic("重建凭据 Ring 失败: " + rerr.Error())
			}
			h.logger.Info("已从凭据 store 载入", "keys", len(keys))
		}
	}
	// U3：零凭据等待注册——store 为空即不生成任何凭据（ring.Len()==0），
	// 首个注册者（回环）由 register 端点经 AddRegistration 原子授 admin。
	if ring.Len() == 0 {
		h.logger.Info("零凭据启动：首次注册请在本机回环执行 /api/credentials/register，首位注册者将成为 admin")
	}
	h.credentialRing = ring
	h.credentialStore = store
}

// SelfCredential 返回本服务端**自用**凭据（AK / SK hex / skeyID），供**同进程内**的组件以
// SproxySig 访问本机 HTTP 面（Y 二期 P3-d：A 侧 mesh 中继经本机 hub API
// `/api/hub/services`、`/api/relay/stream`）。
//
// ok=false 表示凭据 Ring 为空或无存活条目 ⇒ 调用方必须 fail-closed（拿一对空串去签名只会
// 得到 401，且掩盖「本机无可用凭据」这一装配事实）。
//
// 安全边界：本方法只暴露「本进程自己的」凭据，不引入任何新的导出面给外部调用者；调用方
// 必须处于同进程（同进程即已持有 Ring 内存副本，故无提权）。
func (h *Handlers) SelfCredential() (ak, skHex, skeyID string, ok bool) {
	return credentials.BestCredential(h.credentialRing)
}
