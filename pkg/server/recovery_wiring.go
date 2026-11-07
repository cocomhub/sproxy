// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// recovery_wiring.go 是 **M3 统一恢复协调器在服务端的装配与启动单一起点**
// （设计 docs/designs/2026-10-19-audit-service.md §6.3 + program M3）。
//
// 为什么放在装配层：`pkg/recovery` 只依赖 `pkg/audit`（不 import cloud/syncexec，避免循环依赖）；
// 各任务族的注册（作用域恢复 / legacy 兜底）都由装配层完成 —— 于是「启动恢复」在全服务**只有
// 一个调用点**（本文件），而不是每个子系统的构造函数各自扫目录。

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cocomhub/sproxy/pkg/audit"
	"github.com/cocomhub/sproxy/pkg/recovery"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// startupRecoveryTimeout 是启动恢复的总预算（超时即放弃本轮，**不丢任务**：作用域保持未完成，
// 下次启动继续；恢复是幂等的）。
const startupRecoveryTimeout = 5 * time.Minute

// setupRecovery 构造统一恢复协调器并按需注册任务族。
//
// 无注册任务族 ⇒ 返回 nil（**跳过**）：否则每次启动都会全量扫一遍审计段（合规段可达 GiB 级），
// 却无任何任务可恢复 —— 明确跳过并留日志，而不是空转。
func setupRecovery(auditSvc *audit.Service, log *slog.Logger, regs []recovery.Registration, legacies []recovery.Legacy) *recovery.Coordinator {
	if len(regs) == 0 && len(legacies) == 0 {
		log.Info("统一恢复：未注册任务族，跳过（M3 单一起点已就绪）")
		return nil
	}
	useAudit := auditSvc != nil && !auditSvc.Disabled()
	if !useAudit {
		// 审计关闭 ⇒ 没有 WAL 作用域可恢复；但 **legacy 子系统与本能力无关**，仍须触发
		// （否则关审计就等于关掉云任务恢复 ✗）。
		log.Warn("统一恢复：审计服务不可用（关闭或未装配），跳过作用域恢复（legacy 兜底照常执行）")
		auditSvc = nil
		regs = nil
	}
	if len(regs) == 0 && len(legacies) == 0 {
		// 作用域注册全被跳过、又没有 legacy ⇒ 确实无事可做：显式返回 nil（不假装恢复过）。
		log.Warn("统一恢复：无可执行的恢复入口（作用域恢复需审计；无 legacy 注册）")
		return nil
	}
	c := recovery.New(auditSvc, log)
	for _, r := range regs {
		if err := c.Register(r); err != nil {
			// 注册表错误是**装配错误**：记录并跳过该任务族（不阻断启动，但要可发现）。
			log.Error("统一恢复：任务族注册失败", "kind", r.Kind, "prefix", r.Prefix, "error", err)
		}
	}
	for _, lg := range legacies {
		if err := c.RegisterLegacy(lg); err != nil {
			log.Error("统一恢复：legacy 注册失败", "kind", lg.Kind, "error", err)
		}
	}
	return c
}

// cloudLegacies 返回云下载任务的 legacy 恢复入口（M3：其恢复触发点从构造函数收敛到协调器）。
//
// 为什么是 legacy 而不是 WAL 作用域：云任务已有自己的持久化台账（meta/cloud/*.json）与
// 2026-10-18 修过 race 的恢复语义（收集→持锁一次性插入→再激活）；本步只收敛**触发点**，
// 不重写其恢复逻辑（设计 M3「首版保留 legacy 兜底」）。
func cloudLegacies(h *Handlers) []recovery.Legacy {
	if h == nil || h.cloudMgr == nil {
		return nil
	}
	mgr := h.cloudMgr
	return []recovery.Legacy{{
		Kind: "cloud",
		// ctx 透传（P2 修复，2026-10-21）：协调器的 startupRecoveryTimeout 必须能真正中断
		// 恢复（此前闭包丢弃 ctx ⇒ 恢复挂住会让 ListenAndServe 永不执行，预算形同装饰）。
		Recover: func(ctx context.Context) (int, error) {
			return mgr.RecoverAll(ctx)
		},
	}}
}

// ledgerLegacies 返回**容量账本**的 legacy 恢复入口（用户裁定 2026-10-22）。
//
// 为什么需要：账本自 2026-10-22 起热数据落**节点本地**（外部卷远端抖动不再打断上传/转存，
// 见 `pkg/server/volumes.go` 的 `WithLedgerDir`），锁定「启动时把本地分片重扫回内存」
// 为**显式恢复步骤**（而不是隐含在 `OpenLedger` 的构造中）——恢复报告可见、可告警，
// 且与「启动恢复只有一个编排点」一致（M3 纪律）。
//
// 语义：账本分片是 append-only 日志（已是 WAL 形态），恢复 = 把本地目录内全部分片/基线
// 重读求和；已从已占用起算，无需额外重放逻辑（`Add` 附录即持久）。故本入口只做
// **可观测的确认扫描**（读一次 Total 并计入报告），失败则告警（不静默）。
func ledgerLegacies(h *Handlers) []recovery.Legacy {
	names := externalVolumeNames(h)
	if h == nil || len(names) == 0 {
		return nil
	}
	return []recovery.Legacy{{
		Kind: "quota-ledger",
		Recover: func(ctx context.Context) (int, error) {
			return recoverVolumeLedgers(ctx, h, names)
		},
	}}
}

// externalVolumeNames 返回装配卷集合中全部**外部卷**名（容量账本有本地热数据需恢复的）。
func externalVolumeNames(h *Handlers) []string {
	if h == nil || h.volSet == nil {
		return nil
	}
	var out []string
	for _, v := range h.volSet.All() {
		if v.Type != "" && v.Type != volume.TypeLocal {
			out = append(out, v.Name)
		}
	}
	return out
}

// recoverVolumeLedgers 逐外部卷确认本地热账本可重建（Used() 非降级即算恢复）。
// 降级 ⇒ 该卷拒写并告警（计入失败让运维可见）；ctx 取消即中止。
func recoverVolumeLedgers(ctx context.Context, h *Handlers, names []string) (int, error) {
	recovered := 0
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return recovered, err
		}
		lc := h.volumeLedgerCounter(name)
		if lc == nil {
			continue // 非 LedgerCounter 装配（封印包装等）：不参与
		}
		used := lc.Used()
		if lc.Degraded() {
			h.logger.Warn("容量账本恢复：降级（该卷将拒写，需运维介入）", "volume", name, "used", used)
			return recovered, fmt.Errorf("容量账本 %q 降级（used=%d）", name, used)
		}
		recovered++
		h.logger.Info("容量账本恢复完成（本地热账本已重建）", "volume", name, "used", used)
	}
	return recovered, nil
}

// runStartupRecovery 执行一次启动恢复（**同步**：调用方在后台 goroutine 中调用即可）。
//
// 语义：超时/取消 ⇒ 放弃本轮（作用域保持未完成 ⇒ 下次启动重试，幂等）；报告与失败逐条落日志。
func runStartupRecovery(ctx context.Context, c *recovery.Coordinator, log *slog.Logger) {
	if c == nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, startupRecoveryTimeout)
	defer cancel()
	rep, err := c.Run(rctx, recovery.Options{})
	if err != nil {
		// 扫描失败（如 Journal 不可读）⇒ 恢复未完成：必须告警，绝不静默。
		log.Error("启动恢复失败（任务保持未完成，下次启动重试）", "error", err)
		return
	}
	if rep.Total == 0 && len(rep.Legacy) == 0 {
		log.Debug("启动恢复：无待恢复任务")
		return
	}
	log.Info("启动恢复完成", "summary", rep.String())
	if rep.Unknown > 0 {
		log.Warn("启动恢复：存在未注册前缀的作用域（保持未完成，需补注册）", "unknown", rep.Unknown)
	}
	if rep.Failed > 0 {
		log.Warn("启动恢复：存在失败任务（保持未完成，将重试）", "failed", rep.Failed)
	}
}
