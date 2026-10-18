// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

// counter_mutation_atomicity_test.go —— 记账变更原子性与基线注入的**取证记录**（独立审核 P2，2026-10-18）。
//
// 本文件**不含用例**，只记录两条结论，避免后人重复投入（并按本项目纪律：**不保留无判别力的用例**）：
//
// 1. **变更原子性已结构根治**：`TryAdd`/`Release` 各自持 `c.mu`，而 `CorrectTo` 曾**漏在锁外**
//    （读 `Total()` 与 `Add()` 之间可被并发变更插入）⇒ 两个并发 `CorrectTo(同一真值)` 各读
//    `cur=0` 并各加真值 ⇒ **双计**。修法：收敛为**唯一**临界区原语 `LedgerCounter.withLock`
//    （`total` 只在锁内交给回调 ⇒ 漏锁在结构上不可能）。
//    **为何不写并发用例**：这是**逻辑竞态**（`Total()`/`Add()` 各自内部持锁 ⇒ `-race` 不报 ✗），
//    而两个极短临界区之间的交错**无法确定性构造**（实测 200 轮 × 2 goroutine 从不交错 ⇒
//    用例无判别力 ✗）。⇒ 改用**结构门禁**锁定根因：
//    `internal/archcheck/ledger_mutation_gate_test.go`（直接调用 `c.l.Add(`/`c.l.Total(`
//    的方法必须在允许清单内 ⇒ 新增变更方法漏锁会**必红**）。
//
// 2. **基线注入失败已 fail-closed + 可观测**：三处注入点（卷内账本 / 节点本地账本 / 内存回退）
//    曾 `_ = led.Add(baselineUsed)` **静默吞错** ⇒ 理论上「基线丢失 ⇒ 同名重建从 0 起算」
//    （fail-open）。现改为：失败 ⇒ `ledgerBaselineFail` 计数 + `initErr` **拒写**。
//    **为何不写用例**：`fsStore.appendLine` 在写失败时**降级到内存账本**（设计 §3.6.2，
//    可观测）⇒ `Add` 在该形态下**不返回错误** ✗ ⇒ 该分支**实践中不可达**（防御性代码）。
//    实测注入分片写失败 ⇒ 计数不变、`TryAdd` 成功（降级路径生效）⇒ 构造不出能变红的用例。
//    ⇒ 保留为**防御性 fail-closed**（若 `Add` 真的报错，静默丢基线才是 fail-open），
//    可观测面为 `sproxy_quota_ledger_baseline_fail_total`。
