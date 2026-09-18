# sclient 多环境多用户配置重构（context 模型）设计

日期：2026-09-19
状态：设计定稿（用户已确认 8 个决策点）
作者：pi agent（suixibing 需求）

## 1. 背景与问题

当前 sclient 只能连接**一个** sproxy 服务端：

- 配置为**单份平铺** `~/.config/sproxy/sclient.yaml`（`server_url` / `access_key` /
  `access_key_secret` / `access_key_id` / `hub_url` / `node_id` 等全在一个文件）；
- `SCLIENT_ENV` 环境变量只能切换**整份文件**（`sclient.prod.yaml`），无法在同一
  环境内切换用户、也无法脚本化指定「环境 + 用户」；
- `register` / `login` / `renew` 等凭据操作直接读写这份平铺配置；
- mesh / relay / p2p / socks 等命令从同一份配置取 `hub_url` / `node_id` 与凭据，
  与文件面耦合。

结果：维护多套部署（新加坡 A/B、办公室、家用）或同环境多用户（admin / node 账号 /
家庭各成员）时，要么改配置、要么设 `SCLIENT_ENV` 再重启，无法像 kubectl/kubecm
那样**切换上下文**，脚本里也无法声明「用哪个环境哪个用户」。

## 2. 目标（用户确认）

对标 kubectl/kubecm：

1. **environments + users + contexts 三件套**：environments[]（连接面：
   server_url / hub_url / node_id / TLS / TURN / STUN / virtual_subnet 等）、
   users[]（凭据面：access_key / access_key_secret / access_key_id / owner）、
   contexts[]（env + user 组合，可含额外覆盖）、`current-context` 指针。
2. **旧配置自动导入**：检测到旧 `sclient.yaml` / `sclient.<env>.yaml` → 自动导入为
   默认 context（名 = env 或 default），标记已迁移；`SCLIENT_ENV` 仍兼容（映射到
   对应 context）。零破坏迁移。
3. **current-context 写在 config.yaml**（kubectl 同款，单文件）。
4. **凭据明文 + 600 权限**（保持现状，不引入密钥环）。
5. **全局 flags：`--env` / `--user` / `--context`**：脚本中指定环境/用户/上下文；
   优先级：`--context` > `--env`+`--user` 组合 > `current-context`。
6. **新增 `sclient context` 命令族**：`context list/use/get/set/delete/rename` +
   `env list/use` + `user list/use`。
7. **register 作用于当前环境**：`trust register` 在当前 context 的 env 创建用户，
   成功后**自动设为当前用户**并回填凭据；`trust login` 同理作用于当前 env 当前用户。
8. **env 管 mesh 面，user 管凭据**：mesh connect / relay start / p2p / socks / udp
   自动从 env 取 hub_url / node_id / TURN / STUN / virtual_subnet，从 user 取凭据；
   现有 `--hub` / `--node-id` / `--access-key` 等 flag 仍优先覆盖。

## 3. 配置模型（新 config.yaml）

```
# ~/.config/sproxy/config.yaml（新单一事实源；sclient.yaml 旧格式自动导入后废弃）
apiVersion: sclient/v1
kind: Config
current-context: sg-prod
environments:
  - name: sg-prod
    server_url: https://hub.example.com:18083
    hub_url: wss://hub.example.com:18083/ws
    node_id: home
    tls: { ca_file: "", insecure: false }        # 连接面 TLS
    turn: [{ uri: turn:turn.example.com:3478, user: u, pass: p }]  # mesh/p2p 兜底
    stun: []
    virtual_subnet: "100.64.0.0/10"
  - name: office
    server_url: https://office-proxy.corp:18083
    hub_url: ""
    node_id: office-wsl
users:
  - name: alice
    user:
      access_key: ak-xxx
      access_key_secret: <64hex>                  # 明文，600
      access_key_id: skey-xxx
      owner: alice                                # 用户名（register/login 用）
  - name: alice-sg                                 # 同人不同环境用独立 user 名（凭据可不同）
    user:
      access_key: ak-yyy
      access_key_secret: <64hex>
      access_key_id: skey-yyy
contexts:
  - name: sg-prod
    context:
      environment: sg-prod
      user: alice
      volume: ""                                  # 卷覆盖（可选）
  - name: sg-prod-admin
    context:
      environment: sg-prod
      user: alice-sg
```

**解析优先级**（全命令统一）：

```
--context <name>                    # 完整选中（env+user 都在 context 里）
  > --env <name> + --user <name>    # 分别覆盖 environment / user
  > current-context 指针
  > （旧兼容）SCLIENT_ENV → 对应 context
```

- `--env` / `--user` 未显式指定时取当前 context 的 environment / user。
- context 未设置 current 且无 flag → 报错指引 `sclient context use`（首启除外，
  见 §5 迁移）。

## 4. 新增 / 改动命令

### 4.1 `sclient context` 命令族（新增）

```
sclient context list                # 列出全部 context（标 * 当前）
sclient context use <name>          # 切换 current-context（写 config.yaml）
sclient context get [name]          # 显示某 context 解析后的完整配置（含 env+user 合并）
sclient context set <name> --env <e> --user <u> [--volume <v>]   # 创建/更新 context
sclient context delete <name>       # 删除 context（current 不可删，先 use 其它）
sclient context rename <old> <new>  # 重命名
sclient env list                    # 列 environments
sclient env use <name>              # 快捷：切环境（保留当前 user 若可解析，否则置空）
sclient user list                   # 列 users
sclient user use <name>             # 快捷：切用户（保留当前 environment）
```

### 4.2 凭据命令改造

| 命令 | 变化 |
|---|---|
| `trust register [username]` | 用当前 context 的 environment（server_url）注册；成功后**自动设该用户为当前 user**（context 的 user 指向新 user 或创建同名 user）并回填凭据 |
| `trust login [username]` | 用当前 context 的 environment + 指定/当前 user 登录；回填凭据到该 user 段 |
| `trust renew` | 对当前 context 的 user 段 renew 并回写该 user |
| `config show/set` | 保留（读当前 context 解析后的扁平视图；set 写对应 env/user 段） |

### 4.3 mesh / relay / p2p / socks 联动

- 各命令 `--hub` / `--node-id` / `--turn*` / `--stun` / `--virtual-subnet` 未显式
  指定时，从**当前 context 的 environment** 回落；凭据从 **user** 回落（现有
  `--access-key` 等 flag 仍优先）。
- `clientfactory.NewClient` 改为从「context 解析结果」构建（不再直接读平铺
  `client.Config`），`serverFlagNotSet` 语义保留。

### 4.4 脚本化（CI/自动化）

```bash
sclient --env sg-prod --user alice upload big.bin      # 指定环境+用户
sclient --context sg-prod-admin delete old.bin         # 指定完整上下文
export SCLIENT_CONTEXT=sg-prod-admin                   # 环境变量同效
```

`SCLIENT_CONTEXT` / `SCLIENT_ENV` / `SCLIENT_USER` 三个环境变量与 flag 同优先级
（flag > env > current-context）。

## 5. 迁移（零破坏）

启动/首次使用时：

1. 若 `config.yaml` 不存在但检测到旧 `sclient.yaml`（或 `sclient.<env>.yaml`）：
   - 读取旧平铺配置 → 生成 `environments[0]`（名 = `SCLIENT_ENV` 或 `default`，
     server_url/hub_url/node_id/TLS 等连接面字段）+ `users[0]`（凭据面）+ 一个
     context 指向两者 + `current-context` 指向它；
   - 写新 `config.yaml`（600），旧文件**不删**（留作回滚），打印迁移提示
     「已导入为 context `<name>`；旧文件保留，可手动删除」；
   - 后续命令全部走新模型。
2. `SCLIENT_ENV=prod` 时若存在 `sclient.prod.yaml` 且无同名 context → 同样导入；
   已有同名 context → 优先 context（SCLIENT_ENV 映射到该 context 的 environment）。

## 6. 内部结构（包划分）

```
cmd/sclient/
  internal/contextcfg/            # 新包：context 配置模型 + 解析
    model.go                      # Environment/User/Context/Config + YAML tags
    load.go                       # 加载/迁移/current-context 指针读写
    resolve.go                    # flag/env/current 解析 → ResolvedContext
  internal/contextcfg/contextcfg_test.go
  context.go                      # sclient context/env/user 命令族（薄 CLI）
```

`clientfactory.Factory.NewClient` 签名不变（仍 `NewClient(cmd)`），内部改为从
`contextcfg` 解析结果构建；`ConfigProvider` 接口保留（内部实现改为 context 解析），
避免 70+ 命令工厂签名改动。**破坏面收敛到 root.go / factory.go / trust_* /
context.go 新文件**；其余命令文件不改签名，只享受新配置解析结果。

## 7. 测试策略（TDD）

1. **模型层**（internal/contextcfg）：加载/迁移/解析/优先级纯函数单测：
   - 旧 sclient.yaml → 自动导入为 context（连接面+凭据面字段映射正确）；
   - flag 优先级：--context > --env+--user > current-context；
   - current-context 读写（写 config.yaml 原子）；
   - SCLIENT_ENV 兼容映射。
2. **命令层**（cmd/sclient context_test.go）：context list/use/set/delete/rename
   + env/user list/use 的行为测试（隔离 HOME + 临时 config.yaml）。
3. **凭据命令**：trust register/login/renew 在「当前 context 的 env」下创建/登录
   用户并自动切换（复用 trustLoginEnv mock 端点，注入 contextcfg 解析）。
4. **CLI 集成**：`sclient --env a --user b list` 走对应 server（mock server 按
   server_url 区分）；mesh/socks 从 env 取 hub、从 user 取凭据的回落测试。
5. **端到端**（web/e2e 不受影响；test/e2e 可选加一条双 context 切换冒烟）。

## 8. 破坏性变更清单（用户已允许）

1. `~/.sclient.yaml` 平铺格式**不再是解析来源**（迁移后自动导入为 context，
   之后 config.yaml 为唯一事实源）；
2. `SCLIENT_ENV` 语义从「切整份文件」变为「映射/选择 environment」（同名 context
   优先，旧文件导入的 context 兼容）；
3. `--config <path>` 指向新 config.yaml（不再是 sclient.yaml）；旧 sclient.yaml 的
   `--config` 传参在迁移后报错指引。
4. `config show/set` 输出从「平铺」变为「当前 context 解析后的扁平视图」。
5. mesh/relay/p2p/socks 从 context 的 env/user 取默认值（未显式 flag 时）。

## 9. 不做（YAGNI）

- 不做多 context 同时活跃（并发连接多个 sproxy）；
- 不做凭据密钥环加密（保持明文 600，未来可升级）；
- 不做 context 合并/继承（kubectl 无，保持简单）；
- 不做远端 context（KUBECONFIG 式多文件合并）——单文件足够，后续可按需。

## 10. 验证方式

- `go test ./cmd/sclient/... ./internal/contextcfg/... -race` 全绿；
- `make test/lint/lint-all/deadcode-check` 全绿；串行棘轮登记新测试；
- 手工冒烟：迁移旧配置 → context list/use → trust register（新 env）→ 自动切用户
  → upload/download 走新 context → mesh connect 用 env 的 hub + user 凭据；
- 端到端（可选）：test/e2e 双 context 切换冒烟。
