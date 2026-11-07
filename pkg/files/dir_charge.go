// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"context"
	"net/http"
	"strings"

	"github.com/cocomhub/sproxy/pkg/quota"
)

// dir_charge.go 是**目录足迹记账**（配额模型 v3，用户裁定 2026-10-14：只按物理占用记账）。
//
// 为什么目录不免费：secretdata 的目录 meta blob 同样 pad 到 [base, 2·base)（默认
// `MetaPadBytes = Block.Min = 1MB` ⇒ **1–2MB/目录**，尺寸混淆设计），且 `MakeDir` 会**递归创建
// 整条祖先链**（每级一个随机容器 + 一个 dir meta blob）⇒ 建 10k 个目录即 10–20GB 物理，而此前
// 目录**完全不计账** ⇒ 违反「绝不超真实配额 / 绝不打满磁盘」。
//
// 口径（与写入/删除同源）：
//   - **实测用链差分**：操作前 `DirChainFootprint` − 操作后 = 本次**新增/移除**目录的物理字节。
//     既覆盖祖先链，又不必知道 FS 内部创建/回收了哪几级（rmdir 可能连带回收空祖先）；
//   - **预占用用上界**：`DirPhysBound × 链长`（最多创建整条链）——结算按差分收敛，多预留自动归还；
//   - 无该能力（明文/加密本地/可信卷：目录成本 ~0）⇒ 不记账，**零回归**。

// errMsgStorageQuotaExceeded 是目录足迹预留不足时的对外文案（与文件写入的 507 同语义）。
const errMsgStorageQuotaExceeded = "存储配额不足"

// dirChainLen 返回用户可见路径的层级数（= 预占用时「最多创建多少级目录」的上界）。
func dirChainLen(remotePath string) int {
	trimmed := strings.Trim(remotePath, "/")
	if trimmed == "" {
		return 1
	}
	return strings.Count(trimmed, "/") + 1
}

// dirCharge 持有一次目录写操作的预留与差分基准（无能力时为惰性零值）。
type dirCharge struct {
	scope      *quota.Scope
	scopeRes   *quota.Reservation
	pool       *quota.Pool
	poolRes    *quota.Reservation
	volName    string
	owner      string
	remotePath string
	before     int64
	on         bool
}

// reserveDirCharge 为「即将创建的目录」预留物理上界（无能力/无配额 ⇒ 惰性零值，无副作用）。
// 失败（配额/卷容量不足）⇒ 507 fail-closed（与文件写入同语义）。
func (s *Service) reserveDirCharge(ctx context.Context, owner, rel, remotePath, volName string) (dirCharge, error) {
	c := dirCharge{volName: volName, owner: owner, remotePath: remotePath}
	if s.rt.phys == nil || remotePath == "" {
		return c, nil
	}
	bound := s.rt.phys.DirPhysBound(volName)
	if bound <= 0 {
		return c, nil
	}
	need := bound * int64(dirChainLen(remotePath))
	if sc := s.rt.quotaScope(owner, rel); sc != nil {
		res, err := sc.WaitReserve(ctx, need, s.rt.waitTimeout())
		if err != nil {
			return c, &HTTPError{Status: http.StatusInsufficientStorage, Message: errMsgStorageQuotaExceeded}
		}
		c.scope, c.scopeRes = sc, res
	}
	if vs := s.rt.volSet(); vs != nil && volName != "" {
		if pool := vs.Pool(volName); pool != nil {
			res, err := pool.WaitReserve(ctx, need, s.rt.waitTimeout())
			if err != nil {
				c.release()
				return c, &HTTPError{Status: http.StatusInsufficientStorage, Message: errMsgStorageQuotaExceeded}
			}
			c.pool, c.poolRes = pool, res
		}
	}
	// 差分基准（操作**前**）。
	c.before, _ = s.rt.phys.DirChainFootprint(volName, owner, remotePath)
	c.on = true
	return c, nil
}

// settle 在目录操作**成功后**按实测差分结算（多预留部分自动归还）；无能力 ⇒ no-op。
func (s *Service) settleDirCharge(c dirCharge) {
	if !c.on || s.rt.phys == nil {
		return
	}
	after, _ := s.rt.phys.DirChainFootprint(c.volName, c.owner, c.remotePath)
	charge := max(after-c.before, 0)
	if c.scopeRes != nil {
		c.scopeRes.Commit(charge)
	}
	if c.poolRes != nil {
		c.poolRes.Commit(charge)
	}
}

// release 归还预留（操作失败路径；Commit 与 Release 二选一，Reservation 原子至多一次）。
func (c dirCharge) release() {
	if c.scopeRes != nil {
		c.scopeRes.Release()
	}
	if c.poolRes != nil {
		c.poolRes.Release()
	}
}

// dirChainBefore 取链差分基准（目录操作**前**调用；无能力 ⇒ ok=false，零回归）。
func (s *Service) dirChainBefore(owner, remotePath, volName string) (int64, bool) {
	if s.rt.phys == nil || remotePath == "" {
		return 0, false
	}
	n, ok := s.rt.phys.DirChainFootprint(volName, owner, remotePath)
	return n, ok
}

// releaseDirChainDiff 按链差分释放**被删除目录**的物理足迹（rmdir：删除**后**调用）。
//
// 差分语义：`before` 是删除前的整条链（含仍保留的祖先），删除后再取链 ⇒ 差值即**被删目录自身**
// 的 dir meta 物理字节（rmdir 若连带回收空祖先，差额也一并覆盖）。释放量与写入侧同口径。
func (s *Service) releaseDirChainDiff(owner, rel, remotePath, volName string, before int64, ok bool) {
	if !ok || s.rt.phys == nil {
		return
	}
	after, _ := s.rt.phys.DirChainFootprint(volName, owner, remotePath)
	charge := before - after
	if charge <= 0 {
		return
	}
	if sc := s.rt.quotaScope(owner, rel); sc != nil {
		sc.ReleaseUsage(charge)
	}
	if vs := s.rt.volSet(); vs != nil && volName != "" {
		if pool := vs.Pool(volName); pool != nil {
			pool.ReleaseCommitted(charge)
		}
	}
}
