# Mesh 内网穿透测试指南（云服务器 + 两台 NAT 电脑）

## 拓扑

```
[ Mac mini ] ──家庭 NAT──┐
                          ▼  出站 WSS (仅注册/信令)
[ 公司电脑 ] ──公司 NAT──┼─►  [ sg-vps-1 ]  ◄─┐
                          │     sproxy+Hub    │
                          │     (公网)        │
                          ▼                  │ 出站 WSS
[ sg-vps-2 ] ──公网───────┘  目标节点 ◄───────┘
```

- **sg-vps-1**：hub（汇合点），所有节点出站拨入
- **sg-vps-2 / 公司电脑**：叶子节点（`relay start`），可宣告服务、可作出口
- **Mac mini**：访问方（`mesh connect` / `relay dial` / `p2p connect`）

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

## 2. 部署 sg-vps-1（Hub）

1. 把 `hub.yaml` 上传到 sg-vps-1，填入 `auth_token` / `tunnel_key` / `relay_token`
2. 启动：

```bash
build/bin/sproxy --config hub.yaml
```

3. 验证 Hub 起来：
```bash
curl -k https://sg-vps-1:18083/healthz   # 期望 OK
```

---

## 3. 部署 sg-vps-2（目标节点，可作出口）

```bash
# 宣告本地 SSH 服务 + 允许出口拨号
build/bin/sclient relay start \
  --hub wss://sg-vps-1:18083/ws \
  --node-id sg-vps-2 \
  --token CHANGE_ME_RELAY_TOKEN \
  --service ssh:127.0.0.1:22 \
  --dial-allow

# 期望输出："已注册到 Hub"（注册 ACK 通过）
```

> 若还希望它作出口访问公网目标（如 `relay dial --node sg-vps-2 --tcp 8.8.8.8:53`），
> `--dial-allow` 已开启。默认仅允许公网目标；要放行内网网段再加
> `--dial-allow-cidr 10.0.0.0/8 --dial-allow-cidr 192.168.0.0/16`

---

## 4. 部署公司电脑（叶子 + 出口网关）

```bash
# 宣告内网服务 + 作为出口（替 Mac 访问新加坡等公网目标）
build/bin/sclient relay start \
  --hub wss://sg-vps-1:18083/ws \
  --node-id company \
  --token CHANGE_ME_RELAY_TOKEN \
  --service intranet-ssh:192.168.1.50:22 \
  --dial-allow \
  --dial-allow-cidr 192.168.0.0/16
```

---

## 5. Mac mini（访问方）

### 5a. 查看 mesh 服务
```bash
build/bin/sclient mesh status -s https://sg-vps-1:18083 --auth-token CHANGE_ME_AUTH_TOKEN
# 期望列出：ssh (node sg-vps-2)、intranet-ssh (node company)
```

### 5b. 经 hub 中继访问 sg-vps-2 的 SSH（webrtc 优先，失败回落中继）
```bash
build/bin/sclient mesh connect ssh -l :2222 \
  -s https://sg-vps-1:18083 --auth-token CHANGE_ME_AUTH_TOKEN
# 另一终端：
ssh -p 2222 user@127.0.0.1
```

### 5c. 经公司电脑访问内网服务（出口网关）
```bash
build/bin/sclient mesh connect intranet-ssh -l :3333 \
  -s https://sg-vps-1:18083 --auth-token CHANGE_ME_AUTH_TOKEN
# 另一终端：
ssh -p 3333 user@127.0.0.1
```

### 5d. 任意 TCP 中继（不依赖服务宣告）
```bash
build/bin/sclient relay dial --node sg-vps-2 --tcp 127.0.0.1:22 -l :2222 \
  -s https://sg-vps-1:18083 --auth-token CHANGE_ME_AUTH_TOKEN
```

---

## 6. 云端主动推数据到本地（方向对称）

在 **sg-vps-2 上**执行（Mac 需先注册 relay）：
```bash
# Mac 侧先跑：
build/bin/sclient relay start \
  --hub wss://sg-vps-1:18083/ws \
  --node-id mac \
  --token CHANGE_ME_RELAY_TOKEN

# sg-vps-2 侧向 Mac 的本地服务拨号（如 Mac 上监听 2090 的服务）
build/bin/sclient relay dial --node mac --tcp 127.0.0.1:2090 \
  -s https://sg-vps-1:18083 --auth-token CHANGE_ME_AUTH_TOKEN
# 建立后，云端可写入数据，经 hub 中继到达 Mac 本地 2090 服务
```

---

## 7. p2p 打洞直连（数据面不经 hub）

**公司电脑侧**（先注册，再用 p2p listen）：
```bash
build/bin/sclient p2p listen \
  --hub https://sg-vps-1:18083 \
  --node-id company \
  --token CHANGE_ME_RELAY_TOKEN
```

**Mac 侧**（打洞直连公司电脑，数据面不经过 hub）：
```bash
build/bin/sclient p2p connect \
  --peer company \
  --tcp 192.168.1.50:22 \
  -l :2222 \
  --hub https://sg-vps-1:18083 \
  --node-id mac \
  --token CHANGE_ME_RELAY_TOKEN
```

> 注意：`p2p connect` 要求对端正在 `p2p listen`（WebRTC 信令经 hub 完成），
> 且对端需 `--dial-allow` 才能出站拨号。打洞成功则数据面直连，不经 hub。

---

## 8. 断网测试（验证核心诉求）

1. 保持 5b 的 SSH 会话
2. 断开 **Mac → sg-vps-1** 的网络（模拟"到新加坡断断续续"）
3. 观察：
   - 若会话走 **webrtc 直连**（Mac↔公司打洞成功），数据面不依赖 Mac→hub，**会话不断**
   - 若走 **中继**，断开即断——此时需依赖 p2p 打洞路径才能扛断线
4. 重连后 `mesh connect` 会自动重新选路

---

## 常见问题

| 现象 | 原因 | 解决 |
|---|---|---|
| `relay start` 报"注册被拒绝: invalid token" | relay_token 不一致 | 检查 `--token` 与 hub.yaml 的 `hub.relay_token` |
| `mesh status` 报 401 | auth_token 不对 | 用 `--auth-token` 传 hub 的 `auth_token` |
| `mesh connect` 一直卡住 | 目标节点没注册 | 先确认目标 `relay start` 成功、`mesh status` 能看到 |
| `relay dial --tcp 内网` 连接被拒 | `DialAllowed` 默认仅公网 | 目标节点加 `--dial-allow-cidr` 放行对应网段 |
| `p2p connect` 报打洞失败 | 对端没跑 `p2p listen` | 确认对端 `p2p listen` 在运行 |
| 证书报错 | 自签证书 | sclient 加 `--insecure` |

## 安全提醒

- `hub.yaml` 中的三个密钥（auth_token / tunnel_key / relay_token）务必换成随机值
- `--dial-allow` 让叶子可出站拨号，仅对可信节点开启
- `--dial-allow-cidr` 显式放行网段，默认仅公网目标
