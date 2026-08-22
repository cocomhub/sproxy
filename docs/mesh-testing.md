# Mesh 内网穿透测试指南（云服务器 + 两台 NAT 电脑）

## 拓扑

```
[ 本地端 ] ──客户端侧 NAT──┐
                          ▼  出站 WSS (仅注册/信令)
[ 中继端 ] ──中继端侧 NAT──┼─►  [ hub.example.com ]  ◄─┐
                          │     sproxy+Hub    │
                          │     (公网)        │
                          ▼                  │ 出站 WSS
[ target.example.com ] ──公网───────┘  目标节点 ◄───────┘
```

- **hub.example.com**：hub（汇合点），所有节点出站拨入
- **target.example.com / 中继端**：叶子节点（`relay start`），可宣告服务、可作出口
- **本地端**：访问方（`mesh connect` / `relay dial` / `p2p connect`）

---

## 0. 构建

```bash
make build
# 产物：build/bin/sproxy、build/bin/sclient
```

## 1. 生成密钥

```bash
# 在任意一台机器执行，得到 64 位 hex 密钥
build/bin/sclient genkey
# 输出形如 4f3a...（64 字符），记为 TUNNEL_KEY
```

---

## 2. 部署 hub.example.com（Hub）

1. 在 hub.example.com 上创建 `hub.yaml`（hub 配置完整结构，参照 `config.example.yaml` 的 hub 段）：

```yaml
addr: ":18083"
tunnel_key: "<64 hex>"           # 顶层隧道密钥（sclient genkey 生成）
auth_token: "<auth-token>"       # 顶层 Bearer token（mesh status / relay dial / p2p 信令用）
hub:
  enabled: true
  node_id: "hub"
  relay_token: "<relay-token>"   # 中继注册共享密钥（hub.enabled=true 时必填）
  max_connections: 256
  transports:
    ws:
      enabled: true
      # path 已废弃（固定 /ws，配置非默认值不生效）
```

2. 启动：

```bash
build/bin/sproxy --config hub.yaml
```

3. 验证 Hub 起来：
```bash
curl -k https://hub.example.com:18083/healthz   # 期望 OK
```

> **TLS 前提**：默认 `tls.auto_tls: true`，hub 使用自签 TLS（`https`/`wss`），因此下文
> 连接 hub 的 sclient 命令均带 `--insecure` 跳过证书校验。`--insecure` **仅用于开发/测试
> 的自签证书环境**；生产环境应使用受信 CA 签发的真实证书（或把自签 CA 加入客户端信任库），
> 不要在生产使用 `--insecure`。若希望测试环境免证书，可用 `sproxy --config hub.yaml --no-tls`
> 以明文部署（地址相应改 `http`/`ws`，命令去掉 `--insecure`）。

---

## 3. 部署 target.example.com（目标节点，可作出口）

```bash
# 宣告本地 SSH 服务 + 允许出口拨号
build/bin/sclient relay start \
  --hub wss://hub.example.com:18083/ws \
  --node-id target.example.com \
  --token CHANGE_ME_RELAY_TOKEN \
  --insecure \
  --service ssh:127.0.0.1:22 \
  --dial-allow

# 期望输出："已注册到 Hub"（注册 ACK 通过）
```

> 若还希望它作出口访问公网目标（如 `relay dial --node target.example.com --tcp 8.8.8.8:53`），
> `--dial-allow` 已开启。默认仅允许公网目标；要放行内网网段再加
> `--dial-allow-cidr 10.0.0.0/8 --dial-allow-cidr 192.168.0.0/16`
>
> `--hub` 地址形式：`relay start --hub` 传 **WS 端点** `ws(s)://host:port/ws`；
> `p2p` / `mesh connect --hub` 传 **HTTP 基址** `http(s)://host:port` 即可
> （也接受 `ws(s)` 自动归一为 HTTP 基址）。

---

## 4. 部署中继端（叶子 + 出口网关）

```bash
# 宣告内网服务 + 作为出口（替本地端访问公网目标）
build/bin/sclient relay start \
  --hub wss://hub.example.com:18083/ws \
  --node-id company \
  --token CHANGE_ME_RELAY_TOKEN \
  --insecure \
  --service intranet-ssh:192.168.1.50:22 \
  --dial-allow \
  --dial-allow-cidr 192.168.0.0/16
```

---

## 5. 本地端（访问方）

### 5a. 查看 mesh 服务
```bash
build/bin/sclient mesh status -s https://hub.example.com:18083 --auth-token CHANGE_ME_AUTH_TOKEN --insecure
# 期望列出：ssh (node target.example.com)、intranet-ssh (node company)
```

### 5b. 经 hub 中继访问 target.example.com 的 SSH（webrtc 优先，失败回落中继）
```bash
build/bin/sclient mesh connect ssh -l :2222 \
  -s https://hub.example.com:18083 --auth-token CHANGE_ME_AUTH_TOKEN \
  --relay-token CHANGE_ME_RELAY_TOKEN --insecure
# 另一终端：
ssh -p 2222 user@127.0.0.1
```

> webrtc 直连前提：`mesh connect` 连接前自动注册自身（webrtc 信令用），但直连还要求
> 对端**同时运行 `p2p listen`** 且信令通过 per-node secret 校验。本示例对端只跑
> `relay start`（未跑 `p2p listen`），故默认走 **hub 中继**；webrtc 直连需另见 §7。

### 5c. 经中继端访问内网服务（出口网关）
```bash
build/bin/sclient mesh connect intranet-ssh -l :3333 \
  -s https://hub.example.com:18083 --auth-token CHANGE_ME_AUTH_TOKEN \
  --relay-token CHANGE_ME_RELAY_TOKEN --insecure
# 另一终端：
ssh -p 3333 user@127.0.0.1
```

### 5d. 任意 TCP 中继（不依赖服务宣告）
```bash
build/bin/sclient relay dial --node target.example.com --tcp 127.0.0.1:22 -l :2222 \
  -s https://hub.example.com:18083 --auth-token CHANGE_ME_AUTH_TOKEN --insecure
```

---

## 6. 云端主动推数据到本地（方向对称）

在 **target.example.com 上**执行（本地端需先 `relay start` 注册并宣告服务）：
```bash
# 本地端（被访问方）先跑：注册 + 宣告本地 2090 服务 + 允许出站拨号
#（--service app:127.0.0.1:2090 精确放行出口拨号到该地址，B9 NewServiceDialPolicy）
build/bin/sclient relay start \
  --hub wss://hub.example.com:18083/ws \
  --node-id local \
  --token CHANGE_ME_RELAY_TOKEN \
  --insecure \
  --dial-allow --service app:127.0.0.1:2090

# target.example.com 侧向本地端的服务拨号（如本地端监听 2090 的服务）
build/bin/sclient relay dial --node local --tcp 127.0.0.1:2090 \
  -s https://hub.example.com:18083 --auth-token CHANGE_ME_AUTH_TOKEN --insecure
# 建立后，云端可写入数据，经 hub 中继到达本地端 2090 服务
```

> 备选：若本地服务端口不固定，可用 `--dial-allow-cidr 127.0.0.0/8` 放行整个 loopback
> 网段，但放行面宽于最小授权；`--service` 精确放行是推荐做法。

---

## 7. p2p 打洞直连（数据面不经 hub）

`p2p connect` / `p2p listen` 信令经 hub 完成，且连接前会**自动注册自身**（B17，声明
per-node-secret 能力）。`--token` 是信令 Bearer（hub 的 `auth_token`）；
`--relay-token` 是自动注册用的 relay_token（与 `relay start --token` 一致；
两者相同时可省略 `--relay-token`）。

**中继端侧**（`p2p listen` 以精确 node-id 注册，供对端 `--peer` 寻址）：
```bash
build/bin/sclient p2p listen \
  --hub https://hub.example.com:18083 \
  --node-id relay \
  --token CHANGE_ME_AUTH_TOKEN \
  --relay-token CHANGE_ME_RELAY_TOKEN \
  --insecure
```

**本地端**（打洞直连中继端，数据面不经过 hub）：
```bash
build/bin/sclient p2p connect \
  --peer relay \
  --tcp 192.168.1.50:22 \
  -l :2222 \
  --hub https://hub.example.com:18083 \
  --node-id local \
  --token CHANGE_ME_AUTH_TOKEN \
  --relay-token CHANGE_ME_RELAY_TOKEN \
  --insecure
```

> 注意：`p2p connect` 要求对端正在 `p2p listen`（WebRTC 信令经 hub 完成），
> 且对端需 `--dial-allow` 才能出站拨号。打洞成功则数据面直连，不经 hub。
> `--hub` 传 HTTP 基址即可（`http(s)://host:port`；relay start 需传 WS 端点，见 §3）。

---

## 7b. p2p 手工 SDP 打洞（--manual，无 hub 兜底）

**适用场景**：本地端完全无法访问公网服务器上的 hub 时，仍能与中继端打洞直连
（WebRTC 打洞本身用 Google STUN，只需手工交换 SDP，不依赖 hub）。

**p2p vs mesh 分工**：
- `mesh connect`：需 hub（服务发现 + 自动选路），日常使用
- `p2p connect --manual`：无需 hub（手工 SDP），hub 不可达时的兜底；
  支持两种交换方式：**文件**（`--offer`/`--answer`）或 **stdin/stdout（默认）**。
  后者把 SDP JSON 输出到 stdout、从 stdin 读一行，直接复制粘贴即可，不留文件。

**文件交换流程**（两台机器，同机验证时用不同文件路径）：

```bash
# 1. 本地端（dial 侧）：生成 offer，阻塞等待 answer（信令等待默认 10 分钟）
build/bin/sclient p2p connect --peer relay --tcp 127.0.0.1:22 -l :2222 \
  --manual --offer /tmp/o.sdp --answer /tmp/a.sdp --node-id local

# 2. 把 /tmp/o.sdp 拷到中继端

# 3. 中继端（listen 侧）：读 offer，生成 answer
build/bin/sclient p2p listen --manual \
  --offer /tmp/o.sdp --answer /tmp/a.sdp --node-id relay

# 4. 把 /tmp/a.sdp 拷回本地端，本地端的 connect 自动读到并完成打洞
# 打洞成功后本地端 -l :2222 即连到中继端出口的 127.0.0.1:22
# 注：读到的 offer/answer 文件会被自动删除，不留垃圾。
```

**stdin/stdout 交互流程**（不带 `--offer`/`--answer`，直接把 JSON 复制粘贴）：

```bash
# 1. 本地端（dial 侧）：stdout 输出一行 offer，阻塞读 stdin 等 answer
build/bin/sclient p2p connect --peer relay --tcp 127.0.0.1:22 -l :2222 \
  --manual --node-id local

# 2. 把 stdout 那行 offer 复制 → 粘贴到中继端 listen 的 stdin 并回车

# 3. 中继端（listen 侧）：从 stdin 读 offer，stdout 输出一行 answer
build/bin/sclient p2p listen --manual --node-id relay

# 4. 把 stdout 那行 answer 复制 → 粘贴回本地端 connect 的 stdin 并回车即完成打洞
```

> **时间窗口**：信令等待（offer ↔ answer 交换）放宽到 **10 分钟**，
> 足够人工拷文件/粘贴；但 **ICE 打洞**本身仍受 30s 窗口约束，
> offer/answer 就位后需尽快完成复制以免打洞阶段超时。
> 注意 `--manual` 的 listen 侧单次连接后即退出，不再等待后续拨号。

---

## 8. 断网测试（验证核心诉求）

1. 保持 5b 的 SSH 会话
2. 断开 **本地端 → hub.example.com** 的网络（模拟"到公网服务器断断续续"）
3. 观察：
   - 若会话走 **webrtc 直连**（本地端↔中继端打洞成功），数据面不依赖本地端→hub，**会话不断**。
     webrtc 直连要求对端同时运行 `p2p listen` 且信令通过 per-node secret 校验（见 §7）；
     5b 示例对端只跑 `relay start`，默认走 hub 中继，断开即断。
   - 若走 **中继**，断开即断——此时需依赖 p2p 打洞路径才能扛断线
4. 重连后 `mesh connect` 会自动重新选路

---

## 常见问题

| 现象 | 原因 | 解决 |
|---|---|---|
| `relay start` 报"注册失败: invalid token" | relay_token 不一致 | 检查 `--token` 与 hub.yaml 的 `hub.relay_token` |
| `mesh status` 报 401 | auth_token 不对 | 用 `--auth-token` 传 hub 的 `auth_token` |
| `mesh connect` 报"mesh 服务不可用（节点离线或未宣告）" | 目标节点没注册/离线 | 先确认目标 `relay start` 成功、`mesh status` 能看到该服务 |
| `relay dial --tcp 内网` 连接被拒 | `DialAllowed` 默认仅公网 | 目标节点加 `--dial-allow-cidr` 放行对应网段 |
| `p2p connect` 报打洞失败 | 对端没跑 `p2p listen` | 确认对端 `p2p listen` 在运行 |
| 证书报错 | hub 使用自签证书 | sclient 加 `--insecure`（仅开发/测试；生产用真实证书） |

## 安全提醒

- `hub.yaml` 中的三个密钥（auth_token / tunnel_key / relay_token）务必换成随机值
- `--dial-allow` 让叶子可出站拨号，仅对可信节点开启
- `--dial-allow-cidr` 显式放行网段，默认仅公网目标
- `--insecure` 关闭 TLS 证书校验，存在 MITM 风险：**仅限开发/测试的自签证书环境**；
  生产环境应使用受信 CA 签发的真实证书（或把自签 CA 加入客户端信任库），不要使用 `--insecure`
