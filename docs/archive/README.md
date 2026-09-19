<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 历史经验归档（Archive）

> 本目录存放已实现功能与历史过程的**经验沉淀**（非现行事实源）。
> 现行事实源：`docs/` 根级文档（api / architecture / cli / config / deploy / tunnel / mesh-testing）、
> `AGENTS.md`、`README.md` 与代码本身。
> 归档内容按**功能维度**提炼，记录「当时怎么做的、踩过什么坑、哪些判断仍然有效」，
> 供后续开发与代码审查参考，不代表当前实现细节。

## 目录

- [代码审查经验总结](./code-review.md) — 跨 10+ 轮 pkg/client / pkg/server 审查沉淀的问题模式、流程方法论与测试最佳实践
- [组网与传输演进](./mesh-evolution.md) — mesh 完全组网（阶段 2–4）子任务复盘：mDNS/DHT/SOCKS5/UDP/TCP relay/联邦/虚拟 IP/文件同步
- [架构决策与协议选择](./architecture-decisions.md) — 分层架构评估、协议取舍、跨墙场景分析、过度设计识别
- [Benchmark 与 CI 稳定性](./benchmark-ci.md) — benchmark 夹具 I/O 塌陷取证、go test -timeout 对 benchmark 无效、进程外看门狗方案

## 保留原路径的活跃文档

以下学习文档被 `AGENTS.md` / `Makefile` / `ci.yml` 硬引用，**保留在 `docs/superpowers/learnings/` 原路径**，不属于归档：

- `2026-09-13-agent-operating-rules.md`（协作与实施规则）
- `2026-09-13-ci-merge-process.md`（CI 与合并流程）
- `2026-09-15-benchmark-ci-timeout-disk-io.md`（benchmark I/O 塌陷根因与处置）
- `2026-09-18-gofix-before-pr.md`（提交前 make build 教训）

以及 `docs/testing/virtual-time-conversions.md`（固定等待清理台账，被 R14 门禁引用）。
