# 运行中凭据自动轮换设计（Credential Auto-Rotation）

> 文档：`docs/designs/2026-09-27-credential-rotation.md`
> 关联：roadmap 12.2-2（#641 已实现无设计文档，补设计 + 深化）
> 实现：PR #641（client 热替换 + credrotate 工具）、#643（mesh node/relay start）、#644（mesh node flag 修复）、#645（p2p）

## 一、背景与目标

SproxySig 凭据（AccessKey + AccessKeySecret + skey-id）有有效期（服务端 `credential_ttl`，默认 30d）。
常驻 sclient 进程（http-proxy / socks / mesh connect / mesh up / mesh node / relay start / p2p listen）
**SK 到期前需手动 `trust renew` + 重启进程**——断连秒级且依赖人工。

**目标**：常驻进程运行中自动轮换 SK，新 SK 热替换到签名器（同进程立即生效，无需重启）。

## 二、服务端现状（已支持）

- **多 SK 共存**：服务端凭据 Ring 每个 AK 可有多条 SK 条目（新 SK 生效，旧 SK 宽限期内仍可用）。
- **renew API**：`POST /api/credentials/<ak>/renew`——用当前 SK 签名调用，服务端生成新 SK（信封加密
  wrap 返回），客户端用旧 SK 解信封得新 SK 明文 + 新 skey-id。
- **wrap 语义**：持旧 SK 才能解新 SK（`HKDF(旧SK, AK, mesh)` 信封密钥）——防未授权轮换。
- **可选自动轮换调度**（服务端）：`credentials.rotation.interval > 0` 时服务端到期前自动 renew
  （新 SK 存 Ring）；但客户端不知新 SK——需客户端主动 renew 才拿到。

## 三、客户端设计（已实现 #641/#643/#645）

### 3.1 凭据热替换（client 层）

```
FileClient
├── credentialsMu（RWMutex）保护 accessKeySecret/accessKeyID
├── RenewAccessKey(ctx)：调 /api/credentials/<ak>/renew → 解信封 → 回填字段（走锁）
└── credentialsSnapshot()：签名点读一致快照（RLock）——热替换后同进程签名立即用新 SK
```

签名点（`configSigner.Sign` / `signRequest` / doRequest 判断）全部走 `credentialsSnapshot()`——
**renew 后无需重启，后续请求即用新 SK**。

### 3.2 统一轮换工具（internal/credrotate）

```
cmd/sclient/internal/credrotate/
├── Start(ctx, svc, Options{Interval, Logger, OnRotate}) (stop, started)
│     ├─ interval <= 0 或 svc nil → 不启动
│     └─ 每 interval 调 svc.RenewAccessKey → OnRotate(newSK, newID) 回调
├── Credentials（动态凭据容器：mutex 保护 Get/Update）
└── Options.OnRotate：轮换成功回调（热替换到 Credentials 容器）
```

### 3.3 各常驻场景接入

| 场景 | 凭据来源 | 热替换机制 |
|------|----------|-----------|
| http-proxy / socks / mesh up | FileClient svc（直接签名） | `credrotate.Start` → RenewAccessKey 热替换签名器 |
| mesh connect | FileClient svc | 同上（meshForwardListen 常驻循环） |
| mesh node | AutoRegister（每次重连注册帧） | `NodeConfig.Credentials func() (ak,sk,id)` provider——每次重连取最新 |
| relay start | runRelayOnce 注册帧 | creds 参数贯穿 3 层，每次重连 `creds.Get()` |
| p2p listen | registerSignaler 注册帧 | `f.creds` 动态容器 + credrotate.Start |

## 四、关键语义

### 4.1 幂等安全

- 服务端多 SK 共存：renew 后旧 SK 宽限期仍可用（不踢在途连接）。
- 轮换失败：记 Warn 下次重试（`continue`，不中断常驻）。
- 并发：credentialsMu 保证读写一致（renew 与签名并发安全）。

### 4.2 向后兼容

- `NodeConfig.Credentials` nil = 回落静态字段（未接入的调用方零改动）。
- `--renew-interval` 默认 24h，0 = 关闭（显式禁用）。

### 4.3 动态凭据 provider（mesh node）

- AutoRegister 每次重连（断线退避循环）调 `cfg.Credentials()` 取最新 SK。
- 轮换后下次重连即用新 SK——无需重启（重连由断线自愈机制触发）。

## 五、深化项（roadmap 12.2-2 指明，未实施）

| 项 | 内容 | 状态 |
|----|------|------|
| **在途请求 drain** | 轮换时在途请求（用旧 SK 签名中）的处理——旧 SK 宽限期保证不失败，但需确认 drain 语义（是否等存量请求完成再切） | 未实施（依赖宽限期兜底） |
| **跨实例协调** | 多副本（多常驻进程同一 AK）同时轮换——各自 renew 会产生多条 SK（服务端 keep_old 裁剪）；需协调谁轮换（leader？）避免条目膨胀 | 未实施（服务端 keep_old 已部分缓解） |

## 六、验证

- `TestFileClient_RenewAccessKey`（黑盒：renew 后请求用新 SK 验签，旧 SK 验不过）
- credrotate 各场景测试（flag 断言 + 编译级）
- 生产验证：本机 http-proxy + 新加坡 sg-t 均带 `--renew-interval=24h`，轮换日志
  `msg=凭据已自动轮换（新 SK 热替换生效）`

## 七、配置示例

```bash
# 常驻代理（http-proxy/socks/mesh connect/mesh up）
sclient http-proxy ... --renew-interval=24h

# 常驻节点（mesh node / relay start / p2p listen）
sclient mesh node ... --renew-interval=24h
sclient relay start ... --renew-interval=24h
sclient p2p listen ... --renew-interval=24h
```

服务端：`sproxy.yaml`
```yaml
credentials:
  rotation:
    interval: 24h        # 服务端自动轮换（可选；客户端主动 renew 已足够）
    notify_before: 72h
    keep_old: 3
```
