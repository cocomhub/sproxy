<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 跨节点只读卷访问设计（Y 一期）

> 日期：2026-09-11
> 范围：sproxy「对等多节点存储」路线图的 **Y 一期**。X（本地多卷抽象 + 卷 ACL）已完成并合入 master。
> 状态：brainstorming 产出（用户已逐项确认范围/路由/授权/传输四个维度），待用户审阅后进实现计划。

## 1. 背景与动机

X 把单节点存储从「单根」升级为**多卷**，并留下三个扩展缝（X 规格 §17）：卷 = 可寻址一等单元、属主薄接口（VolumeSet/Router/Authorizer）、`pkg/store` KV 接缝。Y 把「卷」提升为**跨节点可寻址单元**：节点 A 的本地用户经 mesh（node 角色建网）访问节点 B 的卷。

一期只做**只读**——「先串访问后演进」。理由是只读不引入一致性负担：B 的卷数据沿用 X 的「单一属主写」约定，读者只是读者，不需要分布式锁、不需要缓存、不需要共识。写入面（跨节点写、副本、元数据目录）留作后续批次（§11）。

> 用户原方向：**mesh 集群与文件访问权限隔离**——mesh 用 node 角色建立连接，节点 A 上持用户凭据的用户经该连接访问节点 B 的卷。Y 一期把这条链路打通并保证可审计、可扩展。

## 2. 范围

### 目标

- **只读跨节点访问**：列目录、单文件下载（含 Range）、单文件元信息、流式读取。
- **显式寻址**：`remote://<node>/<vol>/<path>` 句柄；sclient `--remote <node>:<vol>` 适配器。
- **按节点授权**：B 侧卷 ACL 新增 `mesh_readers` 平行字段（owner 级绑定），授权主体是**节点身份**而非用户身份。
- **密码学身份锚**：链路跑既有 ECDH + Ed25519 握手，B 拿到**已认证的对端指纹**并映射为 node-id；hub 被攻破也无法冒名（双向 pin，fail-closed）。
- **零新增传输**：复用既有 `mux` + `Tunnel.Serve`/`Tunnel.Do`，只补一处 `net.Conn → xfer.Conn` 适配。
- **零一致性负担**：无缓存、不碰写路径、无分布式锁。
- **端到端加密**：隧道 AES-256-GCM，hub 中继路径上 hub 也看不到内容。

### 非目标（本设计不实现）

- 跨节点写（上传/删除/改名/跨节点 move）。
- 跨节点元数据目录、全局寻址广播、多副本/冗余。
- 任何形式的缓存（含元数据缓存、负缓存、TTL 缓存）。
- 跨节点配额聚合与容量调度。
- WebUI 远程浏览（一期只交付 CLI + Go SDK 面）。
- 用户级联邦（B 不感知 A 的本地用户是谁；见 AD-1）。

### DoD

1. **授权矩阵测试**：`mesh_readers` 命中（节点 + 指纹 + owner 三元组）放行；节点未列 / 指纹不匹配 / owner 不匹配 / `mesh_readers` 为空 → 一律拒绝（fail-closed）。
2. **端到端双路径**：A 经 **hub 中继**与经 **webrtc 直连**两条路径均能 list / stat / download B 的文件，且下载字节 SHA-256 与 B 磁盘原件全等。
3. **只读强制**：远程面只注册 GET/HEAD；写方法（POST/PUT/DELETE）一律拒绝（路由白名单，非黑名单）。
4. **本地读路径零回归**：未配置 `mesh_readers` / `remote_read` 时，行为与当前完全一致（全量测试绿）。
5. **审计**：每次远程读记录对端指纹、node-id、卷、owner、路径与结果。
6. **大文件**：流式下载不整体入内存（复用既有 stream chunk 帧）。

## 3. 现状锚点（file:line，供实现对照）

**卷与 ACL**
- `pkg/volume/volume.go:11` `type Mode string`；`:21` `type ACL struct { Mode Mode; Owners map[string]struct{} }`（零值 = deny + 空 = 默认开放）；`:27` `type Volume struct { Name, RootDir string; Capacity int64; ACL ACL }`；`:37` `func (v Volume) Authorize(owner string) bool`（Mode=allow 要求 ∈ Owners；Mode=deny/零值 命中 Owners 才拒）；`:51` `AllowedVolumes(vols, owner)`；`:62` `DefaultVolume`。
- `pkg/server/config.go:352` `VolumeACLMode`，`:355-356` `VolumeACLAllow/VolumeACLDeny`，`:366` `type VolumeACLConfig struct { Mode VolumeACLMode; Owners []string }`。
- `pkg/server/volumes.go:49` 私有 `volumeSet`（`volumes/roots/pools/defaultName/tenants`），方法 `Default()`:68 / `All()`:74 / `ByName(name)`:79 / `Root(name)`:94 / `Pool(name)`:99 / `Tenant(volName, owner, log)`:126；装配入口 `assembleVolumes(cfg, log)`:170；ACL 解析 `parseVolumeACL(ac *VolumeACLConfig, log *slog.Logger) volume.ACL`（Y-A 实施后签名带 logger，约 `:225`；本节其余行号为实现前快照，会随改动漂移）。

**服务端读路径与身份上下文**
- `pkg/server/handlers.go:33` `type Handlers`，`:110` 字段 `volSet *volumeSet`；`:526` `type RegisterRoutesOpts`，`:577` `func RegisterRoutes(ctx, opts) *Handlers`。
- owner 取值链：`pkg/server/auth.go:47` `actorCtxKey`，`:57` `func ActorFrom(ctx) string`；`handlers.go:212` `ownerFromRequest(r)`；`:222` `tenantFor(owner)`；`:269` `tenantOf(r)`；`:204` `normalizeOwner`。
- 读 handler（均为 `*Handlers` 方法）：
  - `list_handler.go:190` `func (h *Handlers) listFiles(w, r)`（GET `/api/files`）；辅助 `resolveListDir`:64、`listRelForOwner`:302。
  - `download_handler.go:255` `func (h *Handlers) download(w, r)`（GET `/download`）；路径解析 `resolveDownloadPath(r) (*downloadPath, error)`:132（`downloadPath{filename, tnt, rel}`:114）；`:234` `checksumStoreForRead`。
  - `download_handler.go:321` `func (h *Handlers) stat(...)`（HEAD `/api/files/stat`）。

**隧道与多路复用（Y 的传输全部由此组装，无需新传输）**
- `pkg/tunnel/tunnel_mux.go:24` `type Tunnel`，`:43` `TunnelOption`，`:46` `WithIdentity(id *Identity)`，`:54` `WithPeerFingerprints(fps []string)`，`:60` `func NewTunnel(m *mux.Mux, key []byte, opts ...TunnelOption) *Tunnel`。
- `:74` `func (t *Tunnel) PeerFingerprint() string`（**握手后已认证的对端指纹**）；`:84` `HandshakeErr()`；`:145` `func (t *Tunnel) Do(req *http.Request) (*http.Response, error)`；`:286` `func (t *Tunnel) Serve(ctx, handler http.Handler) error`；`:317` `handleStream`。
- **`key` 语义（关键）**：`tunnel_mux.go:96-99` `ensureHandshake` 在 `t.key == nil` 时直接返回 → **无握手、无加密**；`:131-142` `encryptionKey` 同样在 `key == nil` 时返回 nil。即 `key` 是**静态前置密钥**，ECDH 只是把 `sharedSecret` 与它**混合派生**（`ecdh.go:196-212` `deriveSessionKey(sharedSecret, staticKey)`：先 HKDF 出 baseKey，再把 `ecdhStaticSalt||staticKey` 作 **HKDF salt** 二次派生）。
- `pkg/tunnel/ecdh.go:79` `performHandshakeWithIdentity(ctx, m, dialer, id, peerFingerprints, staticKey) (sessionKey []byte, peerFingerprint string, err error)`；`:215` `identitySigMessage(dialerECDHPub, listenerECDHPub)`（身份签名**绑定双方临时 ECDH 公钥** = proof of possession）；`:290` `readPeerIdentity`。
- fail-closed 语义：`tunnel_mux.go:116-122`（dialer 配了 pin 但握手失败 → `handshakeErr`，不回退静态密钥）；`:294-300`（keyed listener 握手失败直接返回 error）。
- `pkg/tunnel/context_key.go:11/16` `SetTunnelKey` / `GetTunnelKey`（既有 context 传递范式）。

**mesh 数据面与服务宣告**
- `pkg/tunnel/mesh/mesh.go:51` `type Result struct { Conn net.Conn; Kind string }`；`:44-47` `KindWebRTC`/`KindRelay`；`:117` `WebRTCStream(ctx, conn, addr)`（webrtc 上 `mux.New` 后开流写拨号帧，返回 `*MuxStreamConn`）；`:133` `func Dial(ctx, svc *client.FileClient, signaler *hub.HubSignaler, target *client.MeshService, _ string) (*Result, error)`（webrtc 直连优先，失败 `svc.RelayStream(ctx, node, addr)` 回落中继）；`:62` `WriteDialFrame(w io.Writer, addr string)`（`[4B len][{"dial":addr}]`）。
- 关键性质：`mesh.Dial` 返回的 `net.Conn` 是**字节流**，对端 `relay.Serve` 读拨号帧后向 `addr`（B 的本地 TCP 地址）出站拨号并双向 pump。**数据面本身不经隧道**。
- `pkg/tunnel/relay/leaf.go:61` `func Serve(ctx, m *mux.Mux, localAddr string, dialAllow bool, httpClient *http.Client, logger *slog.Logger, opts ...ServeOptions) error`；`:529` `NewServiceDialPolicy(allowCIDRs, serviceAddrs)`。
- 服务宣告：`cmd/sclient/relay.go:159-177` 解析 `--service name:addr` → `hub.Service{Name, Addr}` → `:182` `hub.NewRegisterFrame(nodeID, ak, proof, ts, nonce, meta, caps...)`（`pkg/tunnel/hub/router.go:215`，`Service` :90）。
- 服务发现：`pkg/server/hub_handler.go:211` `hubServicesHandler`（`GET /api/hub/services`）→ `pkg/client/relay.go:285` `func (c *FileClient) MeshServices(ctx) ([]MeshService, error)`（`MeshService{Name,Node,Addr}` :278）。

**客户端既有装配（A 侧可参照，但不直接复用其连接目标）**
- `pkg/client/client.go:239` `WithIdentity`，`:247` `WithPeerFingerprints`，`:1193` `getTunnelMux(ctx)`，`:1249` `tunnelOpts()`，`:1309` `Identity()`，`:1315` `PeerFingerprints()`。

**需要补的唯一传输缺口**
- `pkg/tunnel/xfer/core.go` `type Conn interface { Send(ctx, []byte) error; Receive(ctx) ([]byte, error); io.Closer }`；`pkg/tunnel/xfer/internal/tcp/tcp.go:32-34` 已有 `tcpConn` 把 `net.Conn` 包成 `xfer.Conn`（**未导出**，且 `internal` 仅允许 `pkg/tunnel/xfer/**` 导入）。

**测试基建（Y 直接沿用）**
- `pkg/testutil/`：`TestKey()`、`DiscardLogger()`、`SHA256Hex()`、`mockserver`、`mockxfer`。
- CLI e2e harness：`test/e2e_cli_harness_test.go:28` `cliEnv`、`:37` `startCLIEnv`、`:82` `sclientRun`、`:99` `sclient`、`:110` `sclientJSON`。
- 服务端 in-process：`web/e2e/helpers_e2e_test.go:90` `testServerCfg`（`server.Default()` → `RegisterRoutes`）。
- 入口：`make test-e2e`（递归 `./test/...`）、`make test-all`、`make check-ci`。

## 4. 架构决策

### AD-1 信任模型：按节点的代理信任（非用户级联邦）

B **只判定「对端是哪个 mesh 节点」**，不判定「对端是哪个用户」。B 授权给节点 A 的语义是：*「我信任节点 A 这个 mesh 成员，允许它只读访问我指定的 (卷, owner) 命名空间」*。A 本地哪些用户能用这条通路，由 A 自己的凭据与准入负责（A 侧 `sclient` 仍走既有 SproxySig 认证）。

这是**显式委托**：A 的本地 ACL 是 B 授权的前提，但不是 B 的判据。选它是因为用户级联邦需要跨节点凭据分发/映射，成本与攻击面都远大于一期所需；且与「mesh 用 node 角色建网、文件访问权限与 mesh 隔离」的原始意图一致。

### AD-2 身份锚点：双向 Ed25519 指纹 pin，fail-closed

链路两端跑既有 `performHandshakeWithIdentity`：身份签名**绑定双方临时 ECDH 公钥**（`ecdh.go:215`），因此 pin 提供的是完整防 MITM（而非 TOFU）：

- **B pin A**：`mesh_readers[].fingerprint`（本次已确认「配置显式绑定」）。
- **A pin B**：A 侧 peer 配置里的 `fingerprint`。
- **任一方向未配置 pin → 拒绝连接**（fail-closed）。A 侧不允许「无 pin 接受任意 listener」——否则恶意 hub 可冒充 B（见 AD-3 的密钥论证依赖双向 pin）。

指纹取自 `sclient identity fingerprint`，运维带外交换。

### AD-3 隧道静态密钥：由节点对确定性派生（不引入第二个带外秘密）

`Tunnel` 的 `key` 非 nil 才握手与加密（`tunnel_mux.go:97`、`:132`），所以两端必须有同一个静态密钥。但该密钥在 `deriveSessionKey` 中**只作 HKDF salt**（`ecdh.go:204-207`，`ecdhStaticSalt || staticKey`），HKDF 的 salt **不需要保密**；真正的密钥材料是 ECDH 的 `sharedSecret`，其真实性由 AD-2 的双向 pin 保证。

因此 Y 取：**`staticKey = HKDF-SHA256(ikm = "sproxy-remote-read/v1|" + min(nodeA,nodeB) + "|" + max(nodeA,nodeB), salt = nil, info = "sproxy-remote-read/static-key", 32B)`**——两端用**排序后的** node-id 计算，结果必然一致，零额外配置。

安全论证：ECDH 提供机密性（前向保密）；双向 pin 提供认证；静态密钥提供域分离（把远程读的会话密钥与其它隧道用途隔开）。缺了双向 pin 这条论证不成立——这也是 AD-2 把「未配 pin 即拒绝」定为硬约束的原因。

> 备选（若审阅时认为公开 salt 不可接受）：在 `mesh_readers` 条目里再加一个带外交换的 `key`（64 hex），与既有 `POST /tunnel` 的「SK → HKDF 派生」同构。代价是多一个需保管的秘密。**本设计默认取确定性派生**。

### AD-4 路由形态：`remote://<node>/<vol>/<path>` 句柄 + 客户端适配器

- 句柄语法：`remote://<node>/<vol>/<path>`（`<path>` 可省略 = 卷根）。解析/格式化收敛到一个薄解析器，sclient 与 Go SDK 共用。
- sclient 表面：`--remote <node>:<vol>` 标志，与既有 `--volume` 并列；`list` / `download` / `stat`（`meta`）三个子命令支持。
- 适配器职责单一：本地命令参数 → 对 B 的 HTTP-over-tunnel 请求 → 结果渲染/落盘。**不做**路径映射魔法（不把 `remote://` 混进既有多卷 auto 路由）。
- 一期不做透明网关（本地 sproxy 把 `remote://` 当普通路径代理）。理由是透明网关需要本地 sproxy 持对端凭据与路由表，属二期。

### AD-5 授权面：ACL 新增 `mesh_readers` 平行字段（owner 级绑定）

**不改 `Owners` 语义**（它是 owner 名单，承载 allow/deny 双语义，混入节点主体会显著提高误配风险）。新增：

```go
// pkg/volume
type MeshReader struct {
    Node        string // mesh 节点 ID
    Fingerprint string // 该节点的 Ed25519 身份指纹（小写 hex）
    Owner       string // 允许只读访问的 owner 命名空间
}

type ACL struct {
    Mode        Mode
    Owners      map[string]struct{}
    MeshReaders []MeshReader // Y 新增；零值 = 无任何节点授权
}
```

判定入口（与既有 `Authorize` 组合，两种 ACL mode 都自洽）：

```go
// AuthorizeMeshRead 判定节点 node（已认证指纹 fp）可否只读访问 owner 的命名空间。
// 双重约束：三元组必须命中某条 MeshReaders，且该 owner 本身未被本卷 ACL 拒绝
// （Mode=allow 时 owner 必须在 Owners 内；Mode=deny 时 owner 不得在黑名单内）。
// 任何一项不满足 → false（fail-closed）。
func (v Volume) AuthorizeMeshRead(node, fingerprint, owner string) bool
```

指纹比较前做规范化（去空白 + 转小写），比较用 `crypto/subtle.ConstantTimeCompare` 以避免早期退出的时序差异（指纹非秘密，仅为防御一致性）。

### AD-6 传输：mesh 字节流之上叠 mux + Tunnel（零新增传输实现）

```
A: mesh.Dial(...) ──► net.Conn ──► [net.Conn→xfer.Conn 适配] ──► mux.New(conn, RoleDialer)
                                                                      │
                                                    NewTunnel(m, staticKey, WithIdentity(aId),
                                                              WithPeerFingerprints([bFP]))
                                                                      │
                                                            Tunnel.Do(GET/HEAD ...)
────────────────────────── mesh 链路（webrtc 直连 或 hub 中继）──────────────────────────
B: 本地 loopback listener ──► net.Conn ──► [同一适配] ──► mux.New(conn, RoleListener)
                                                                      │
                                                    NewTunnel(m, staticKey, WithIdentity(bId),
                                                              WithPeerFingerprints([aFP]))
                                                                      │
                                                          Tunnel.Serve(ctx, remoteReadHandler)
```

- 唯一新增的传输代码：`net.Conn → xfer.Conn` 适配（`internal/tcp` 的 `tcpConn` 未导出且 `internal` 仅对 `pkg/tunnel/xfer/**` 开放，故导出版本落在 `pkg/tunnel/xfer`）。
- B 侧监听**强制 loopback**（fail-closed：配非 loopback 地址拒绝启动），对外可达性完全交给 mesh 服务宣告（`--service volread:127.0.0.1:<port>`）。这与既有的网关安全边界同构。
- 加密：隧道 AES-256-GCM，**hub 中继路径上 hub 只见密文**。

### AD-7 只读强制：路由白名单

远程面只有一张手写路由表，**只注册 GET/HEAD**；未注册的方法由 `http.ServeMux` 天然 405/404。不使用「先全量注册再拦写方法」的黑名单法。写路径的 handler 从不出现在这张表上。

### AD-8 handler 复用：受限 context 驱动既有读逻辑

B 侧远程 handler 不重写读逻辑，而是**在受限 context 下调用既有 `*Handlers` 方法**：

1. 取 `tun.PeerFingerprint()` → 在配置里反查 node-id（未 pin 或未命中 → 401/404，fail-closed）；
2. 由请求解析出目标 `<vol>`（卷名白名单校验）与 `<owner>`（只能取 `mesh_readers` 条目里绑定的那个，**不接受请求方指定**）；
3. `volSet.ByName(vol)` + `vol.AuthorizeMeshRead(node, fp, owner)` → 不通过则 **404**（不泄露存在性，沿用既有风格）；
4. 构造受限 context：写入 owner（复用既有 `actorCtxKey` 通路，使 `ActorFrom(ctx)` 返回该 owner）；请求路径重写为租户相对路径；
5. 调用既有 `h.listFiles` / `h.download` / `h.stat`。

**关键点**：owner 由配置决定而非请求参数，杜绝越权枚举；`ValidateFilePath` 等既有校验全部保留。

### AD-9 一致性：零缓存直读

一期不建任何缓存（含元数据缓存与负缓存），每次远程读都是实时 `Stat`/`ReadDir`/读文件。不做分布式锁——B 的卷只有一个属主写，读者与写者的并发由既有文件锁/OS 语义处理，远程读不新增写路径。已知取舍：跨节点读取延迟 = 一次 mesh 往返 + B 的本地 IO，不做优化。

### AD-10 审计

每次远程读经既有 `AuditLogger`（`RegisterRoutesOpts.AuditLogger`）记一条：对端指纹、解析出的 node-id、卷、owner、方法、路径、结果（放行/拒绝/不存在）。**拒绝路径同样记账**（安全事件）。

## 5. 配置模型

### B 侧（sproxy）

```yaml
volumes:
  - name: main
    root: ./storage
    acl:
      mode: allow
      owners: [alice]              # 既有语义不变：卷的 owner 白名单
      mesh_readers:                # Y 新增（省略 = 无任何节点授权）
        - node: nodeA
          fingerprint: 3f2a...9c   # A 的 Ed25519 身份指纹（小写 hex）
          owner: alice             # 只读 alice 在该卷的命名空间

remote_read:
  enabled: true
  listen: 127.0.0.1:19000          # 强制 loopback；配非 loopback 启动即失败
  handshake_timeout: 10s           # 可选，缺省 10s
```

### A 侧（sclient，独立配置文件）

```yaml
# sclient.yaml（与 B 侧 sproxy 配置是两个文件，键名相同但结构不同）
remote_read:
  peers:
    - node: nodeB
      fingerprint: 7c1d...4e     # B 的指纹（AD-2：未配 = 拒绝连接，不 TOFU）
```

CLI 等价覆盖：`--peer-fingerprint <hex>`（配合 `--remote nodeB:main`）。配置键经既有 `sclient config set` 管理。

## 6. 组件与接口（文件结构）

| 文件 | 职责 | 新增/修改 |
|------|------|-----------|
| `pkg/volume/volume.go` | `MeshReader` 类型、`ACL.MeshReaders` 字段、`AuthorizeMeshRead` | 修改 |
| `pkg/volume/volume_mesh_test.go` | 授权矩阵单测（命中/未命中/指纹不符/owner 不匹配/空表/deny 模式） | 新增 |
| `pkg/server/config.go` | `VolumeMeshReaderConfig`、`VolumeACLConfig.MeshReaders`、`RemoteReadConfig{Enabled,Listen,HandshakeTimeout}` + `Validate`（loopback 强制） | 修改 |
| `pkg/server/volumes.go` | `parseVolumeACL` 解析 `mesh_readers`（含指纹规范化） | 修改 |
| `pkg/server/remote_read.go` | `remoteReadHandler`：指纹→node-id、ACL 判定、受限 context、委派既有读 handler；`newRemoteReadMux(h)` 手写只读路由表 | 新增 |
| `pkg/server/remote_read_listener.go` | loopback listener + `mux.New(RoleListener)` + `NewTunnel` + `Serve`；静态密钥派生 | 新增 |
| `pkg/server/remote_read_test.go` | handler 单测（授权矩阵 × 方法白名单 × 路径穿越 × 大文件流式） | 新增 |
| `pkg/tunnel/xfer/netconn.go` | `func FromNetConn(net.Conn) Conn`（导出的适配，内部委托 `internal/tcp`） | 新增 |
| `pkg/tunnel/remote_key.go` | `DeriveRemoteStaticKey(nodeA, nodeB string) []byte`（AD-3 的确定性派生，双方共用） | 新增 |
| `pkg/remote/route.go` | `remote://<node>/<vol>/<path>` 解析/格式化 + `RemoteRef` 类型 | 新增 |
| `pkg/remote/client.go` | A 侧：`Client`（持 `net.Conn` 工厂）、`List/Stat/Download` 三方法，内部建 mux+Tunnel 并发 HTTP-over-tunnel 请求 | 新增 |
| `cmd/sclient/remote.go` | `--remote` / `--peer-fingerprint` 标志解析 + 委派 `pkg/remote`；接入 `list`/`download`/`meta` | 新增 |
| `cmd/sproxy`（root 装配） | 按 `remote_read.enabled` 起监听 | 修改 |
| `cmd/sclient`（`relay`/`mesh node`） | **无需代码改动**：B 侧 loopback 监听由 sproxy 提供，mesh 侧只用既有 `--service volread:127.0.0.1:19000` 宣告 | 无 |

**远程面路由表（`remoteReadMux`，手写、只读）**

| 方法 | 路径 | 语义 |
|------|------|------|
| GET | `/remote/list?volume=<v>&path=<p>` | 列目录（委派 `h.listFiles`） |
| HEAD | `/remote/stat?volume=<v>&path=<p>` | 单文件元信息（委派 `h.stat`） |
| GET | `/remote/download?volume=<v>&path=<p>` | 下载，支持 `Range`（委派 `h.download`） |

`<p>` 为租户相对路径（`user/` 桶内），仍走 `ValidateFilePath`。

## 7. 数据流

1. A 侧：`sclient list --remote nodeB:main --subdir /docs`
2. 解析为 `RemoteRef{Node: "nodeB", Volume: "main", Path: "/docs"}`
3. 服务发现：`FileClient.MeshServices(ctx)` 找到 `nodeB` 宣告的 `volread` 服务（`MeshService{Name, Node, Addr}`）
4. 链路建立：`mesh.Dial(ctx, svc, signaler, target, "")` → webrtc 直连优先，失败回落 hub 中继 → `net.Conn`
5. A：`xfer.FromNetConn(conn)` → `mux.New(conn, RoleDialer)` → `NewTunnel(m, DeriveRemoteStaticKey(a,b), WithIdentity(aId), WithPeerFingerprints([bFP]))`
6. A：`Tunnel.Do(GET /remote/list?volume=main&path=/docs)`（首次调用触发 ECDH+身份握手）
7. B：loopback listener accept → 同一适配 → `mux.New(conn, RoleListener)` → `NewTunnel(..., WithIdentity(bId), WithPeerFingerprints([aFP]))` → `Serve(ctx, remoteReadHandler)`（`Serve` 进入 accept 循环前完成握手）
8. B handler：`PeerFingerprint()` → node-id 反查 → 卷/owner 白名单 → `AuthorizeMeshRead` → 受限 context → 既有 `listFiles` → 响应经隧道流式回传
9. A：渲染列表。`download` 同链路，响应体按既有 stream chunk 流式落盘（不在内存中整体持有）。

## 8. 错误处理与安全面

| 场景 | 行为 |
|------|------|
| A 未配 B 的指纹 pin | A 侧**拒绝发起**（fail-closed，不 TOFU） |
| B 未 pin A 的指纹 / 指纹不匹配 | 握手失败 → `Serve` 返回 error，连接关闭；记审计 |
| 节点未列入 `mesh_readers` | 404（不泄露卷/文件是否存在）；记审计 |
| 请求的 owner 与绑定 owner 不一致 | 一律按绑定 owner 处理（忽略请求参数），不存在越权枚举 |
| 卷名非法 / 不存在 | 404 |
| 路径穿越（`..`、绝对路径、NUL、Windows 非法字符） | 复用既有 `ValidateFilePath` → 400 |
| 写方法（POST/PUT/DELETE…） | 路由未注册 → 405/404（白名单） |
| `remote_read.listen` 配非 loopback | 启动即失败（fail-closed） |
| 大文件下载 | 流式（既有 chunk 帧），内存占用与文件大小无关 |
| 握手超时 | `handshake_timeout`（默认 10s）到期关闭连接 |

**信任边界陈述**：B 信任「持有被 pin 私钥的 mesh 节点」；hub 仅承担链路中继与信令，**不在信任链上**（既不能冒名，也看不到内容）。A 对其本地用户的准入负全责。

## 9. 测试策略

**单测（`pkg/volume`）** — 授权矩阵：`mesh_readers` 命中放行；节点对但指纹错 → 拒；节点错 → 拒；owner 不匹配 → 拒；`mesh_readers` 为空 → 拒；`Mode=deny` 且 owner 在黑名单 → 拒；指纹大小写/空白规范化；`Mode=allow` 且 owner 不在 `Owners` → 拒。

**单测（`pkg/server`）** — `remoteReadHandler`：以伪造的 `PeerFingerprint`（测试注入）驱动 6 种授权结果；GET/HEAD 放行、POST/PUT/DELETE 405；路径穿越 400；owner 由配置而非请求决定（构造带恶意 `owner` 参数的请求，断言仍读绑定 owner）；大文件响应体字节数与 SHA-256 与源文件全等。

**中测（in-process 双端）** — 在 loopback 上建 `net.Pipe`/`net.Listener`，两端各起 `mux` + `Tunnel`（真握手、真加密、真 pin），跑 `list`/`stat`/`download` 全链路；断言下载字节 SHA-256 全等；**反向对照**：把 pin 改错 → 断言握手失败且无数据返回。

**e2e（`test/`，真二进制）** — 两个 sproxy + 两个 mesh node 子进程：
1. 路径一（hub 中继）：A `--remote` 读 B 的文件，断言磁盘收到的字节 SHA-256 与 B 侧原件全等；
2. 路径二（webrtc 直连）：同断言（若环境不允许打洞则显式 skip 并记录，不伪绿）；
3. 未授权节点：C 节点尝试读 B → 断言被拒 + B 侧审计日志出现拒绝记录；
4. 写方法：`POST` 到远程面 → 断言 405。

**回归红线** — 未配置 `remote_read` 时：`make test`、`make test-all`、`make test-e2e` 全绿；既有本地读路径代码零改动。

## 10. 分块与交付（PR 切分）

| 块 | 内容 | 独立可测交付物 |
|----|------|----------------|
| **Y-A 授权面** | `MeshReader`/`ACL.MeshReaders`/`AuthorizeMeshRead` + config 解析与校验 + 授权矩阵单测 | `pkg/volume` 单测绿；未配置时零回归 |
| **Y-B 只读面** | `remoteReadHandler` + 受限 context + 只读路由表 + 单测 | handler 级授权/方法/穿越/大文件全绿 |
| **Y-C 传输接线** | `xfer.FromNetConn` + `DeriveRemoteStaticKey` + B 侧 loopback listener + `pkg/remote` 客户端 + 中测（真握手双端） | in-process 双端 list/stat/download 全绿 + 错 pin 反向对照 |
| **Y-D CLI 与 e2e** | sclient `--remote`/`--peer-fingerprint` + 真二进制 e2e（中继/直连/未授权/写拒绝）+ 审计 + 文档（README/CHANGELOG/config.md） | `make test-e2e` 全绿；DoD 1–6 逐条核对 |

每块一个功能分支、独立 PR、CI 全绿后 squash 合入 master；每块完成后派独立对抗式审查并修复**全部**发现（含 Minor/建议级）。

## 11. 未来扩展缝（写批次及后续）

- **写批次（Y 二期）**：`mesh_readers` 条目加 `scope: read|write`（一期固定 read）；写路径需要节点可见性协调（谁持有该路径的写权），届时才引入协调机制——**不在本设计内**。
- **元数据目录**：跨节点「逻辑路径 → (node, volume)」解析需要一个目录服务，落在 `pkg/store` KV 接缝之上（X §17 缝 3）；本设计显式使用 `remote://<node>/<vol>` **显式寻址**，正是为了不依赖目录即可工作。
- **透明网关**：本地 sproxy 把 `remote://` 当普通路径代理（客户端零感知），二期候选。
- **用户级联邦**：若未来需要 B 感知 A 的用户身份，`mesh_readers` 的 `owner` 字段可扩展为映射表；本设计预留了「主体 → 命名空间」的显式绑定形态。
- **静态密钥替换**：若未来 `Tunnel` 支持「无静态密钥的纯匿名 ECDH + pin」，AD-3 的确定性派生可直接移除。

## 12. 参考

- X 规格（三缝与卷 ACL）：`docs/superpowers/specs/2026-09-08-storage-volumes-acl-design.md`
- 全 mesh 路线图与 stage5（身份 pin / 服务宣告 / mesh 选路）：`docs/superpowers/specs/2026-08-29-sproxy-fullmesh-roadmap.md`、`2026-08-31-sproxy-fullmesh-stage5-design.md`
- 多租户布局：`docs/superpowers/specs/2026-09-01-multitenant-storage-layout-design.md`
- 隧道与 AK 派生：`docs/superpowers/specs/2026-08-24-tunnel-accesskey-design.md`
