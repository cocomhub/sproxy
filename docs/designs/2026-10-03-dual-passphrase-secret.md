# 双口令 secret 生成方案（可记忆、可重建的 opt-in 补充）

> 日期：2026-10-03
> 状态：设计定稿（初版实现）
> 关联：`pkg/volume/secrets`（secret 文件管理封装卷，设计 `docs/designs/2026-10-01-secret-volume.md` §6.1）
>
> 代码落点：`pkg/volume/secrets/passphrase.go`（新）+ `secrets.go`（抽 `writeSecret` 共用）+ `passphrase_test.go`（新）

## 1. 背景与动机

当前 `pkg/volume/secrets` 的 `Manager.Create`（`secrets.go`）用 `crypto/rand` 生成
32B（256-bit）随机 secret 写入 secrets 卷。这是最强的安全默认，但有一个明确的可用性
痛点：**secret 一旦丢失（secrets 卷损坏/误删），加密数据永久不可解密**。

本方案提供一种**可记忆、可重建**的口令模式作为 opt-in 补充，用于个人管理便利。

**安全权衡结论（用户确认的立场）**：对个人现实威胁模型，双口令 + high 档 KDF 大概率比
「随机 256-bit + 到处存放密钥」更安全可靠——密钥从不出现在任何存储中（无同机同泄/备份
丢失风险），破解需同时猜两个独立口令，且无可丢失的密钥文件灾难模式。代价仅是「极强而非
2^256」，这个差距对个人威胁无关紧要。

## 2. 派生构造（核心，勿偏离）

```
secret = scrypt( SHA256(口令A), SHA256(口令B) 作为 salt, high 档, 32B )   → hex 64 字符
```

- **双独立秘密输入**：口令B 的 SHA256 是秘密盐（第二因子），不是公开盐——破解需同时猜中
  两个口令。
- **每口令独立 SHA-256 后作密码/盐**：避免口令间相互推导，各口令熵独立叠加。
- **KDF 必须用 scrypt**（内存硬），不是 AES：AES 是加密不是密钥派生。
- **复用系统已有的 Algorithm 档位注册机制**：`shardseal.AlgoByVersion(AlgoV1GCMHigh)`
  读取 high 档 `N=2^17, r=8, p=1`（单一事实源，不复制常量——shardseal 提档位此处自动
  跟随）。这正是档位化 high 档的本来用途（`shardseal` 设计注释：high 档为「对低熵/口令类
  secret 的保守档」）。
- **输出 32B（256-bit）→ hex 64 字符**，与现有随机模式产物同构，可写入 secrets 卷（0600）。

### 2.1 档位策略（用户裁定）

- **公开面固定 high**：`DerivePassphraseSecret` / `Manager.CreateFromPassphrase` 均不暴露
  tier 参数（防调用方误配 low 降档）。
- **内部 `deriver` 结构携带 scrypt 参数，零值回落 high**：测试经字段注入 low 档规避 2^17
  耗时（同一 `deriveSecret` 逻辑、非 mock——mockkdf 是 shardseal 专用 HKDF，这里派生语义是
  scrypt，用降档更保真）。档位变化 = 派生结果变化（类似 shardseal cross-tier fail-closed
  语义）。

### 2.2 salt 策略（用户裁定）

- **无公开盐**：scrypt 的 salt 就是 `SHA256(口令B)`（规格核心）。无需额外生成/存储 salt；
  重建 = 重输两口令 + 固定档位即得同一 secret，与「可恢复、密钥无存储」卖点完全自洽。
- 代价：两口令的任何一方都不能单独持有到壳（组合熵打回单点——见 §4 口令分置）。

## 3. 方案要点与维护成本权衡

- **密钥无存储**：口令在脑子里，偷机器/云泄露/备份泄露都碰不到 secret；无「丢密钥=丢全部
  数据」灾难。secret 文件本身（`secrets/<name>`）仍落盘 0600——那是派生结果不是口令，泄露
  后攻击者仍须猜两口令（scrypt 慢速）。
- **可恢复**：secret 丢失后重输两口令 + 固定 high 档即可重建（无需任何存档的盐/密钥）。
- **口令分置**：两个口令不得合置一处（同一备忘录/同一密码管理器条目），否则组合熵打回单点
  ——文档与代码注释必须强调。
- **接口面最小**：只加库能力（`Manager.CreateFromPassphrase` + 包级 `DerivePassphraseSecret`），
  不接线 CLI/装配——`Create` 本身当前无调用方，YAGNI；确需入口再单独评估。
- **复用而非复制**：scrypt 参数从 `shardseal.AlgoByVersion(AlgoV1GCMHigh)` 读，防两处漂移；
  secret 落盘复用 `writeSecret` helper（`Create` 与新方法共用），改权限语义两者一致。

## 4. 安全边界（必须诚实标注，禁止夸大）

- **熵守恒**：派生密钥安全上限 = 输入熵。两个真正独立的 12 位强口令 ≈ 各 40-70 bit，
  组合 ~90-140 bit；加 high 档成本因子 → 有效安全 ~110-160 bit。对现实攻击者不可破，
  但不是 2^256。
- **任何声称「单/双口令能到 2^256」的说法都不成立**；真正 2^256 只能靠随机成分
  （crypto/rand / 硬件 token）。文档与代码注释已写明「口令模式熵上限由口令强度决定，非随机
  模式的最强级」。
- **SHA-256 不增加熵**：只是把输入熵重排成 256-bit 表示，输出长度≠熵值。安全由口令熵 +
  KDF 成本主导。
- **空口令 fail-closed**：任一口令为空/全空白 → 派生拒绝（防崩塌为空熵密钥）。

## 5. 测试

（TDD：先落红后转绿，`go test ./pkg/volume/secrets/` 全绿）

- **确定性**：同输入两口令 → 同 32B；A 变则 B 不变仍变、B 变则 A 不变也变（双因子各自
  独立影响熵）。
- **与随机 Create 同构**：64 hex、写盘可 `Read` 回、本地 0600。
- **空口令/全空白 fail-closed**（含 CreateFromPassphrase 路径）。
- **跨档**：同输入 low vs high → 派生不同（档位参与派生域）。
- **deriver 零值回落 high**（`effectiveN()==2^17`）。
- **CreateFromPassphrase name 校验**与 Create 一致（空/含分隔符/`.`/`..`/空白名）。
- **与随机产物相异**：双口令产物 ≠ 随机 `Create` 产物。

## 6. 不做（YAGNI）

- 不引入独立公开 salt（用户裁定 salt=SHA256(口令B)）。
- 不触碰 AES/注册表（scrypt 派生为新增，shardseal 注册表不动）。
- 不提供可配置档位公开面（固定 high 防误降）。

---

# 接线设计（2026-10-04 追加）

> 用户原始需求：处理接线，web+CLI 本地生成后上传；web 双口令若难做强加密则只做随机；
> 避免原始口令进 bash 历史、避免网络传输原始口令；随机 secret 也接线（web 必做）；
> secret 支持导入导出（文件直觉 + 内容校验）；设计清晰易用 UI。

## 用户裁定

| 决策点 | 裁定 |
|--------|------|
| web 双口令 | **不做**：浏览器 WebCrypto 无 scrypt 原生支持、纯 JS 受限（high 档 2^17 主线程阻塞百毫秒）。双口令仅 CLI。 |
| CLI/Web 随机来源 | **服务端生成**：随机 secret 由服务端 `crypto/rand` 生成（32B→hex 64），客户端不参与随机生成（更安全——客户端不知其值、加解密全在云端）。**生成后返回 secret 值**给创建者供立即备份。 |
| 网络协议 | 原始口令**不**上网；双口令在 CLI 本地派生，只上传派生结果（hex 64）。 |
| 服务端只落盘 | 增加 `POST /api/secrets`（上传）语义：body 带 name + secret 内容（hex）+ origin 字段。服务端只负责「校验 + 落盘到 secrets 卷」，不参与派生。 |
| secret 导入/导出 | **要**；支持导入导出：CLI/web 以「文件」直觉操作（本地 hex 文件），服务端走**专用端点**（secrets 是 ExternalBackend，普通文件 API 不覆盖）。导出默认需口令加密、可选明文。 |
| CLI 口令输入 | `x/term.ReadPassword` 交互读取（**绝不进 bash 历史**）。 |

## 接线落点（本次实现范围）

### 服务端（`pkg/server/` 新增 `secrets_api.go` + 路由 + Handlers 注入 secrets.Manager）
- `POST /api/secrets`：body `{name, value, origin: "random"|"passphrase"?}`——value 为客户端本地生成/派生的 secret hex；服务端校验 64-hex、写 secrets 卷（0600）。origin 仅标注来源。**口令永不上传**。
- `GET /api/secrets`：列全部 secret 名。
- `GET /api/secrets/{name}`：导出 secret 内容（hex）。
- `DELETE /api/secrets/{name}`：删除。
- 走 `h.volSet.External("default-secrets")` 反取 `*secrets.Manager`（装配层已装）；或 Handlers 注入。

### 8. CLI（`cmd/sclient` 新增 `secret` 子命令）
- `sclient secret create <name>`（随机：调服务端 `POST /api/secrets` mode=random，服务端生成返回）
- `sclient secret create <name> --passphrase`（CLI 本地派生：`x/term.ReadPassword` 读两口令 → `DerivePassphraseSecret` → 上传 derived hex，origin=passphrase）
- `sclient secret list` / `secret delete <name>` / `secret export <name> [--out path]` / `secret import <name> <file>`
- 导出默认加密（用户口令派生密钥 + AES-GCM）；`--plain` 可选明文。
- 导入校验 hex。

### 9. Web（`web/static/` 新增 UI）
- 只做随机 secret：monitoring 弹窗新增「Secret」tab（或卷 tab 内），支持创建（随机，展示结果供备份）、列出、删除、导出（下载明文/加密）。
- **不实现双口令**（web 无 scrypt）。

### 10. 传输安全
- HTTP + TLS/SproxySig 已有；secret 内容（hex）为敏感值，走既有加密隧道。
- CLI 派生仅本地；口令与整体不出网。

## 验证

- `pkg/server/secrets_api_test.go`：创建（随机/赋值）、列表、导出、删除、非法 hex/重复 fail-closed。
- CLI `secret` 子命令 subprocess e2e（真实副作用断言）。
- Web `secrets-format.js` 单测（node --check + 渲染）。

## 实现完成记录（2026-10-04）

落地（worktree `dual-passphrase-secret`，分支 `worktree-dual-passphrase-secret`，4 commits）：

| 层 | 文件 | 要点 |
|----|------|------|
| 库 | `pkg/volume/secrets/secrets.go` | 新增 `Import`（校验 64 小写 hex 落盘，大写/非 hex 拒绝 fail-closed）、`Remove`（fs.Delete 防误删）；`writeSecret` helper 供 Create/Import/CreateFromPassphrase 共用 |
| 服务端 | `pkg/server/secrets_api.go` + `routes.go` | `/api/secrets` 四端点（POST 创建 random/import、GET 列表、GET/{name} 导出、DELETE/{name} 删除）；双面挂载（localMux 隧道 + srvMux fileRoute/fileRouteRead）；`secretsManager()` 按卷类型反取 Manager；**服务端只校验+落盘，不参与派生** |
| CLI | `cmd/sclient/secret.go` + `pkg/client/secrets.go` | `secret create [--passphrase]` / `list` / `export`（默认口令加密 AES-GCM+scrypt，`--plain`）/ `import`（明文 hex 或加密信封）/ `delete`；`secretReadPassphrase` 用 `x/term.ReadPassword` 且**非 tty 拒绝**（防明文管道）；双口令本地 `DerivePassphraseSecret` 只上传派生 hex |
| Web | `web/static/secrets-format.js` + `app.js` + `index.html` + `Makefile` | monitoring 弹窗新增「Secret」tab：随机创建（返回 value 供备份）、列表、导出、删除；名称/值全部 `escHtml` 防 XSS；`make web-test` 纳入 node --check + node --test |
| 测试 | `secrets_api_test.go`、`secrets_test.go`、`e2e_cli_secret_test.go`、`secrets-format.test.js` | 白盒 handler 测试 + mock 服务器客户端测试 + 真二进制 e2e（CRUD/交叉核对/非法名）+ web 渲染单测 |

**接线中的关键修复**：`/api/secrets` 初挂主 mux fileRoute 时未同步 `isFileGroupedRoute`/`isReadOnlyFileRoute` 清单 → 主 mux 面 500（fail-closed 防接线错误静默放行）——已三处同源补齐，RBAC 分类测试兜住。

**验证证据**：`go test ./pkg/server/ ./pkg/volume/secrets/ ./pkg/client/` 全绿；`make web-test` 57 tests 0 fail；`go test -tags=e2e ./test/` 全绿（含既有用例回归）；lint 0 issues；pre-commit 门禁真实跑通。

**不做（延续）**：web 双口令派生（无 scrypt）；通用外部卷 HTTP 文件操作面（单独立项，YAGNI）。
