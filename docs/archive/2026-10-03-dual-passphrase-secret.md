<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 双口令 secret 生成方案（可记忆、可重建的 opt-in 补充）— 归档

> **归档说明**：原文 `docs/designs/2026-10-03-dual-passphrase-secret.md` 已于 2026-11-14 归档。
> 能力与用法见 `docs/secret-volume.md`（secrets 卷 / secret 管理面）与 `docs/cli.md`（`sclient secret`）、
> `docs/api.md`（`/api/secrets`）。本文件只保留**派生构造（核心不变量）/ 裁定 / 安全边界 / 踩坑 / 落地指针**。

## 1. 动机
`pkg/volume/secrets` 的 `Manager.Create` 用 `crypto/rand` 生成 32B（256-bit）随机 secret——最强默认，
但有明确痛点：**secret 一旦丢失（secrets 卷损坏/误删），加密数据永久不可解密**。本方案提供一种
**可记忆、可重建**的口令模式作为 **opt-in 补充**（个人便利，非默认）。代价仅是「极强而非 2^256」。

## 2. 派生构造（核心，勿偏离）
```
secret = scrypt( SHA256(口令A), salt=SHA256(口令B), high 档, 32B )   → hex 64 字符
```
- **双独立秘密输入**：口令B 的 SHA256 是**秘密盐**（第二因子），不是公开盐 ⇒ 破解需同时猜中两个口令。
- **KDF 必须用 scrypt**（内存硬），不是 AES（AES 是加密不是派生）。
- **复用既有 Algorithm 档位注册**：`shardseal.AlgoByVersion(AlgoV1GCMHigh)` 读 high 档 `N=2^17,r=8,p=1`
  ——单一事实源，不复制常量（shardseal 提档位此处自动跟随）。
- **输出 32B → hex 64**，与随机模式产物**同构**，可写入 secrets 卷（0600）。

### 2.1 档位策略（用户裁定）
- **公开面固定 high**：`DerivePassphraseSecret`/`Manager.CreateFromPassphrase` **不暴露 tier 参数**
  （防调用方误配 low 降档）。
- **内部 `deriver` 携带 scrypt 参数、零值回落 high**：测试经字段注入 low 档规避 2^17 耗时
  （同一 `deriveSecret` 逻辑、非 mock——此处派生语义是 scrypt，降档比 mock 更保真）。
  档位变化 = 派生结果变化（cross-tier fail-closed 语义）。

### 2.2 salt 策略（用户裁定）
- **无公开盐**：scrypt 的 salt 就是 `SHA256(口令B)`。无需生成/存储 salt ⇒ **重建 = 重输两口令 + 固定档位**，
  与「可恢复、密钥无存储」卖点自洽。代价：两口令任一方都不能单独持有到壳（组合熵打回单点）。

## 3. 安全边界（必须诚实标注，禁止夸大）
- **熵守恒**：派生密钥安全上限 = 输入熵。两个独立 12 位强口令 ≈ 各 40-70 bit，组合 ~90-140 bit；
  加 high 档成本因子 → 有效 ~110-160 bit。对现实攻击者不可破，**但不是 2^256**。
- **任何「单/双口令能到 2^256」的说法都不成立**；真正 2^256 只能靠随机成分（crypto/rand / 硬件 token）。
- **SHA-256 不增加熵**：只是把输入熵重排成 256-bit 表示，输出长度 ≠ 熵值。
- **空口令 fail-closed**：任一口令为空/全空白 ⇒ 派生拒绝（防崩塌为空熵密钥）。
- **口令分置**：两个口令不得合置一处（同备忘录/同密码管理器条目），否则组合熵打回单点——文档与注释须强调。

## 4. 接线裁定（2026-10-04 追加）
| 决策点 | 裁定 |
|--------|------|
| web 双口令 | **不做**：浏览器 WebCrypto 无 scrypt 原生支持、纯 JS 受限（high 档 2^17 主线程阻塞百毫秒）。双口令**仅 CLI**。 |
| 随机来源 | **服务端生成**（`crypto/rand` 32B→hex64），客户端不参与随机生成（**更安全**——客户端不知其值、加解密全在云端）；生成后**返回 secret 值**给创建者供立即备份。 |
| 网络协议 | 原始口令**不**上网；双口令在 CLI 本地派生，只上传派生 hex（64）。 |
| 服务端职责 | `POST /api/secrets` body 带 name + secret hex + origin 字段；服务端只**校验 + 落盘**，**不参与派生**。 |
| 导入/导出 | **要**；走**专用端点**（secrets 是 ExternalBackend，普通文件 API 不覆盖）。导出默认口令加密、可选明文。 |
| CLI 口令输入 | `x/term.ReadPassword` 交互读取（**绝不进 bash 历史**）；**非 tty 拒绝**（防明文管道）。 |

## 5. 踩坑（仍有效）
- **`/api/secrets` 初挂主 mux fileRoute 时未同步 `isFileGroupedRoute`/`isReadOnlyFileRoute` 清单**
  ⇒ 主 mux 面 500（fail-closed 防接线错误静默放行）。已三处同源补齐；RBAC 分类测试兜住。
  同类新增路由必须三处同源登记（既有教训族）。

## 6. 落地指针
- 库：`pkg/volume/secrets/passphrase.go`（`DerivePassphraseSecret`/`deriver`）、`secrets.go`
  （`CreateFromPassphrase` + 抽出的 `writeSecret` 共用 helper）。
- 服务端：`pkg/server/secrets_api.go` + 路由（`/api/secrets` 四端点，双面挂载）。
- CLI/SDK/Web：`cmd/sclient/secret.go`、`pkg/client/secrets.go`、`web/static/secrets-format.js`。
- 测试：`passphrase_test.go`、`secrets_api_test.go`、`e2e_cli_secret_test.go`、`secrets-format.test.js`。
