<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 审计 + WAL + 恢复 + 等待 程序级设计（三能力包）— 归档

> **归档说明**：原文 `docs/designs/2026-10-19-audit-wal-program.md` 已于 2026-11-14 归档。
> 能力见 `AGENTS.md`（能力矩阵 `pkg/audit`/`pkg/waitq`/`pkg/recovery`）、`docs/api.md` §审计、
> `docs/gates.md`（R35-R38）、`docs/architecture.md`。本文件只保留**三包分层 / 关键架构决策（路由分类 +
> tunnel 鉴权）/ YAGNI / 配置安全默认 / 落地指针**。

## 1. 三个能力包与分层（审核修订：分层显式化）
```
pkg/audit（叶子核心：审计 + WAL 基座）  —— 不 import 装配层、不 import waitq
pkg/waitq（独立叶子包：通用等待/排队）
pkg/recovery（统一恢复协调器，依赖 pkg/audit；收敛 5 处独立恢复）
外置适配器（不在叶子包内 ⇒ 不违反 R4）：配额（waitq.Resource）/ 归档（写内核）/ 读面 ACL / 通知渠道
```
- **审计与中断继续共用一条 append-only 流** ⇒ 恢复过程本身也是审计证据；**但 TTL/清理只作用于合规全局段，
  绝不触碰 WAL 段**。
- **唯一提供方/唯一实现**：`pkg/state.LocalAppendStore` 当前**零生产调用** ⇒ 不得与 `pkg/audit.Journal` 两套并存。

## 2. 关键架构决策：路由分类表 = 唯一事实源（R38）
`/tunnel` 场景下「内层是否二次鉴权」曾有两难；**硬证据**表明**不能**做内层强制鉴权：
- 浏览器 Web UI 默认走隧道且内层**显式剥离 `authorization`**（外层有独立签名）⇒ 内层鉴权会让整个 Web UI 401；
- 服务端到服务端内层无凭据（静态密钥/指纹身份）⇒ 内层鉴权会击穿 mesh 同步 / 联邦卷 / cluster 卷 / WebDAV remote；
- `/tunnel` **其实已有每请求用户归属**（ctx `Principal` → `ActorFrom` → `Principal.AK`）——「单一伪用户」是误判。

**采纳：A 改良版**——
1. **保留既有传输层身份透传**（`/tunnel` 用 ctx `Principal`；**不做内层二次鉴权**）；
2. **单一「路由分类表」作为唯一事实源**（`pattern → class`，`class ∈ {public, auth, reader, user, node, admin}`），
   **同时服务两个 mux**（消除「两个面不一致」）；
3. **xfer/mux 直连显式登记为 transport 类**（身份来源 = 握手密钥/指纹）——**承认它是一类，不宣称「零豁免」**；
4. **public 白名单必须完整**（`register`/`nonce`/`login` + `externalAuthRoutes`（OIDC/LDAP 登录/回调）+ `notify/feed`）
   ——否则隧道模式下登录页/订阅不可达；
5. **hub/mesh 用户面不得归为 node 类**（隧道内是用户面）⇒ 逐路由分类，不放 `requireRole(node)`；
6. **门禁 R38**：非测试源码中每个 `localMux.Handle*` pattern **必须归类**（未归类即红，**deny-by-default**）。

**真正的缺口（与 A/B 之争正交，均须收口）**：非文件组路由缺角色门（`PUT /api/config`、`/dav/`、`/s3/` 无角色检查）；
xfer/mux 直连路径无身份（`localMuxGate` 对 `principal==nil` fail-open 跳过）。

## 3. 不做的事（YAGNI，仍有效）
- 不引入外部 WAL 库（单机 append-only + 策略化 fsync 已满足）；
- **不做分布式共识 / 不做无 fencing 的 `Lease`**（段在本地 ⇒ 跨节点恢复另立专题）；
- 不做审计内容加密（明文 + 0700/0600；归档走加密卷）；
- 不为采样行开逐行 fsync；
- **不把「从不排队」的限额（审计热段）建模为 `waitq.Resource`**（语义错配）；
- 不重写 `pkg/files` 锁体系；
- **不新建第 4 套 append-only 实现**（审计与中断继续共用 `pkg/audit.Journal`）。

## 4. 配置安全默认（要点）
- `audit.max_total_bytes` = **`local_reserve_bytes × 4`（非零安全默认）**；`local_reserve_bytes` 256 MiB（**自身上界**语义，非物理预留）。
- `audit.no_rotate_on_read` bool **默认 false = 默认轮转**（`min_rotate_interval` 30s 防抖）。
- `audit.sync.<kind>`/`audit.sync.scope.<id>` 单一形态（**删 `sync_mode`**）；`event/span/resource=batch`、`state/done=always`。
- `audit.owner` 默认 `audit`（`Role=system`，**不允许**改为普通用户 owner）；保留名 `<卷根>/__audit__/hot/`。
- 等待按资源登记：`waitq.<resource>.{timeout,max_waiters,priority}`。

## 5. 落地指针
- `pkg/audit/`、`pkg/waitq/`、`pkg/recovery/`（`recovery.go`）、`pkg/server/`（`audit.go`/`recovery_wiring.go`/
  `sys` 租户）、`cmd/sproxy`（wire）、门禁 R35-R38（`internal/archcheck/audit_*`/`recovery_gate_test.go`/
  `waitq_gates_test.go`/`route_classification_gate_test.go` + `route_classes.tsv`）。
