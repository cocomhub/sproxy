<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Y：跨节点只读卷访问（Y 一期）实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 让节点 A 的本地用户经 mesh 链路只读访问节点 B 的卷——列目录、单文件元信息、下载（含 Range），按节点（Ed25519 指纹 pin）授权，端到端 AES-256-GCM 加密，零一致性负担。

**架构：** B 侧把「卷」暴露为一条**只读 HTTP 面**（`/remote/{list,stat,download}`），挂在一个强制 loopback 的 listener 上；该 listener 的对端是 mesh 服务宣告（`--service volread:127.0.0.1:<port>`），由既有 `sclient mesh node` / `relay` 提供可达性。A 侧用 mesh 建链得到 `net.Conn`，在其上叠既有 `mux` + `Tunnel`（AES-256-GCM，ECDH + 双向 Ed25519 指纹 pin），发 HTTP-over-tunnel 请求。B 收到请求后不重写读逻辑，而是**在受限 context 下委派既有 `Handlers.listFiles/stat/download`**；owner 恒由配置（`mesh_readers` 条目）决定，绝不接受请求方指定。写方法不进路由表（白名单）。

**技术栈：** Go 1.26，纯 stdlib + 仓库既有 `pkg/volume`、`pkg/tunnel`、`pkg/server`、`pkg/client`。TDD。多 module：根 go.mod（主 module）+ `cmd/sproxy`、`cmd/sclient` 独立子 module（两者都 `replace` 指向根）。**不新增任何第三方依赖。**

**规格：** `docs/superpowers/specs/2026-09-11-y-cluster-read-design.md`（Y 一期）。执行者两份都读——本计划的论证依据全部来自该规格。

---

## ⚠️ 计划对规格的三处必要修正（**已确认**，2026-09-11）

这三处不是偏好选择，而是按规格原文写会**编译不过 / 死锁 / 语义错误**。

- **修正 1（静态密钥 IKM）**：经用户确认取「**B 的身份指纹**」方案。
- **修正 2（握手超时）**：经用户确认取「**新增 `tunnel.WithHandshakeTimeout` 选项**」方案。
- **修正 3（`FromNetConn` 落点）**：实现落点调整（`xfer/builtin` 既有 internal 桥），无设计取舍。

### 修正 1（安全相关）：AD-3 静态密钥派生存在循环依赖 → 改以 **B 自己的身份指纹** 为 IKM

规格 §4 AD-3 取 `staticKey = HKDF(ikm = "sproxy-remote-read/v1|" + min(nodeA,nodeB) + "|" + max(nodeA,nodeB), ...)`。**B 无法在握手前知道 nodeA**：B 侧对端身份是握手完成后才经 `Tunnel.PeerFingerprint()` 得到的（`tunnel_mux.go:286-305`），而静态密钥是握手的**输入**（`deriveSessionKey(sharedSecret, staticKey)`，`ecdh.go:196`）——构成循环依赖，无法实现。

改为以 **listener 自己的身份指纹**为 IKM：

```go
// staticKey = HKDF-SHA256(ikm = "sproxy-remote-read/v1|" + <B 的 Ed25519 身份指纹>, salt = nil,
//                         info = "sproxy-remote-read/static-key", 32B)
```

- B 算：`DeriveRemoteStaticKey(bID.Fingerprint())`；
- A 算：`DeriveRemoteStaticKey(<配置里 pin 的 B 的指纹>)`——A 侧本来就必须配这个值（AD-2），零新增配置；
- 两端天然一致，且**无需 node-id**（B 的 mesh node-id 在 sproxy 配置里本就可为空——可达性由外部 `sclient mesh node` 提供）。

安全性论证与 AD-3 完全同构且更强：该密钥在 `deriveSessionKey` 中**只作 HKDF salt**（`ecdh.go:204-207`），salt 不需要保密；真正的密钥材料是 ECDH `sharedSecret`（前向保密），其真实性由 AD-2 的双向 pin 保证；此值提供域分离，且绑定到 A 已经在 pin 的那个信任锚（B 的身份）上。AD-2「未配 pin 即拒绝」仍是硬约束。

### 修正 2：`remote_read.handshake_timeout` 无法经 `Serve` 的 ctx 实施 → 新增 `tunnel.WithHandshakeTimeout` 选项

`Tunnel.Serve` 的握手超时是包内常量 `handshakeTimeout = 30s`（`tunnel_mux.go:21`），握手 ctx 由**传入的 ctx** 派生（`:288`），而 accept 循环用的是**同一个传入 ctx**（`:308`）。因此：给 `Serve` 传一个 10s 超时的 ctx 会在 10s 后**杀掉 accept 循环**，无法只缩短握手。

新增 TunnelOption（**加性、默认值不变 = 30s，零回归**）：

```go
func WithHandshakeTimeout(d time.Duration) TunnelOption // d <= 0 时忽略（保持默认）
```

`NewTunnel` 初始化 `t.handshakeTimeout = defaultHandshakeTimeout`（原 `handshakeTimeout` 常量改名），`:104` 与 `:288` 改用 `t.handshakeTimeout`。B 侧 listener 传 `tunnel.WithHandshakeTimeout(cfg.RemoteRead.HandshakeTimeout)`。

### 修正 3：`xfer.FromNetConn` 落点改为 `xfer/builtin`（避免 import cycle 与重复实现）

规格 §6 要求新建 `pkg/tunnel/xfer/netconn.go` 且「内部委托 `internal/tcp`」。**这不可能**：`pkg/tunnel/xfer` 导入 `internal/tcp` 会形成 `xfer → internal/tcp → xfer` 的 import cycle（`internal/tcp/tcp.go:19` 导入 `xfer` 以调用 `xfer.Register`）。

仓库已有专门的**对外可见的 internal 桥**：`pkg/tunnel/xfer/builtin`（其包注释即写明「xfer/internal/tcp … 外部调用方无法直接 blank import 它。本包是对外可见的注册桥」，`cmd/sproxy/root.go:33` 已在用它）。故：

- `pkg/tunnel/xfer/internal/tcp/tcp.go` 新增导出 `FromNetConn(conn net.Conn) xfer.Conn`（复用全部 `tcpConn` 语义，零重复）；
- `pkg/tunnel/xfer/builtin/builtin.go` 新增桥 `FromNetConn(conn net.Conn) xfer.Conn`；
- `pkg/server` / `pkg/remote` 经 `builtin.FromNetConn` 使用。

---

## 全局约束（每个任务必须遵守，违反即审查缺陷）

1. **只读强制**：远程面路由表是**手写白名单**，只注册 `GET /remote/list`、`HEAD /remote/stat`、`GET /remote/download`；写方法（POST/PUT/DELETE/PATCH）由 `http.ServeMux` 方法模式天然 405。禁止「先全量注册再拦写方法」的黑名单法。
2. **owner 不可由请求指定**：owner 恒取自卷 `mesh_readers` 条目（配置），请求参数中的任何 owner/actor 一律忽略。这是防越权枚举的红线。
3. **fail-closed**：未命中 `mesh_readers`、指纹不匹配、卷不存在、对端指纹为空、A 侧未配 pin 一律拒绝（404/拒绝连接），且**拒绝路径也记审计**。
4. **零缓存**：一期不建任何缓存（含元数据缓存与负缓存），每次远程读都是实时 `Stat`/`ReadDir`/读文件。
5. **零回归红线**：未配置 `volumes[].acl.mesh_readers` 与 `remote_read` 时，行为与当前完全一致；既有本地读路径代码零改动（只新增文件与加性字段）。
6. **lint 0**：主 go.mod + 每个子 go.mod（含 `cmd/sproxy`、`cmd/sclient`、`pkg/tunnel/xfer/ext/*`）`golangci-lint run` 0 issues（含改动前已存在的历史遗留）。`go fmt ./...` + SPDX 头（`addlicense`，`make fmt` 自动注入）。
7. **Windows 兼容**：所有测试在 Windows 通过。监听地址只用 `127.0.0.1`（禁 `0.0.0.0`/`localhost`，后者在 Windows 可能触发防火墙弹窗）；路径用 `filepath.Join`/`filepath.ToSlash`。
8. **测试纯标准库**：不使用 testify/gomock/gomega，延续 `t.Fatalf`/`t.Errorf` 模式。子 module 改动需 `cd` 进对应目录单独 lint/test。
9. **每块一个功能分支、独立 PR**：`feature/y-read-acl` → `feature/y-read-surface` → `feature/y-read-transport` → `feature/y-read-cli`，逐个 squash 合入 master，每块完成后派独立对抗式审查并修复**全部**发现（含 Minor/建议级）。
10. **不使用 git worktree**（sproxy 项目执行偏好），直接在当前分支开发。

### 执行顺序与依赖

```
Y-A: T1 (pkg/volume 授权域) ──► T2 (config 解析/校验)
                                   │
Y-B:                               └──► T3 (remoteReadHandler 只读面)
                                            │
Y-C:  T4 (传输原语) ─────────────────────────┴──► T5 (B 侧 listener + 配置 + 装配)
                                                      │
                                                      └──► T6 (pkg/remote 客户端 + 端到端中测)
                                                              │
Y-D:                                                          └──► T7 (sclient --remote) ──► T8 (e2e + 文档)
```

T4 不依赖 T1–T3，可与 Y-A/Y-B 并行；但 Y-C 的 T5 需要 T3 的 handler 与 T1/T2 的授权域。

### 关键精确取值（跨任务一致，勿偏离）

- **句柄语法**：`remote://<node>/<vol>/<path>`，`<path>` 可省略（= 卷根）。`path` 语义 = **owner 的 user 桶内相对路径**（如 `/docs/a.txt`）。
- **远程面路由**：`GET /remote/list?volume=<v>&path=<p>`、`HEAD /remote/stat?volume=<v>&path=<p>`、`GET /remote/download?volume=<v>&path=<p>`（支持 `Range`）。`volume` 必填。
- **静态密钥 IKM 前缀**：`"sproxy-remote-read/v1|"`；info：`"sproxy-remote-read/static-key"`；长度 32B。
- **指纹规范形**：`"sha256:" + 64 位小写 hex`（`tunnel.FingerprintFromPublicKey`，`identity.go:92`；归一化用 `tunnel.ParseFingerprint`，`identity.go:214`）。配置里允许纯 64 hex / 大写 / 带前缀，**解析期统一归一**。
- **`AuthorizeMeshRead` 双重约束**：三元组 `(node, fingerprint, owner)` 必须命中 `mesh_readers` 条目，**且** `owner` 本身过本卷 `Authorize`（`Mode=allow` 须 ∈ Owners；`Mode=deny`/零值 不得在黑名单内）。
- **拒绝状态码**：未授权/卷不存在/文件不存在一律 **404**（不泄露存在性）；路径非法 **400**（沿用 `ValidateFilePath`）；写方法 **405**；对端指纹为空 **401**。
- **A 侧配置键**：`remote_read.peers[]`（`node` + `fingerprint`）。**B 侧配置键**：`volumes[].acl.mesh_readers[]`（`node` + `fingerprint` + `owner`）、`remote_read.{enabled,listen,handshake_timeout}`。
- **默认值**：`remote_read.enabled=false`、`remote_read.listen="127.0.0.1:19000"`、`remote_read.handshake_timeout=10s`。
- **mesh 服务名**：B 侧 loopback listener 由外部 `sclient mesh node --service volread:127.0.0.1:<port>` 宣告；A 侧按 `volread` 服务名 + `node` 做服务发现。
- **CLI**：`--remote <node>:<vol>`、`--peer-fingerprint <hex>`、`--remote-transport auto|relay|webrtc`（默认 `auto`；后两者用于把两条路径变成**确定性可测**的 e2e）。

### 现有代码锚点（implementer 先读再改）

- **授权域**：`pkg/volume/volume.go:21-23`（`ACL`）、`:27-32`（`Volume`）、`:37-49`（`Authorize`）。
- **配置**：`pkg/server/config.go:352-380`（`VolumeACLMode`/`VolumeACLConfig`/`VolumeConfig`）、`:398-401`（`Config.Placement/Volumes`）、`:487`（`Default()`）、`:570-599`（`SetDefaults()` 卷归一）、`:716`（`Validate()`）、`:744-780`（逐卷 + ACL 校验）、`:1059`（`isLoopbackHost(host)`，**收裸 host，不含端口**）。
- **装配**：`pkg/server/volumes.go:49-65`（`volumeSet`）、`:170`（`assembleVolumes`）、`:221-234`（`parseVolumeACL`）、`:633-654`（`locateForRead`）。
- **服务端读路径**：`pkg/server/list_handler.go:190`（`listFiles`）、`:302`（`listRelForOwner`）；`pkg/server/download_handler.go:255`（`download`）、`:321`（`stat`）、`:132`（`resolveDownloadPath`）、`:114-118`（`downloadPath`）。
- **身份上下文**：`pkg/server/auth.go:46-60`（`actorCtxKey`/`withActor`/`ActorFrom`）、`auth.go:453`（`isLoopbackRemote`）；`pkg/server/handlers.go:200-214`（`normalizeOwner`/`ownerFromRequest`）、`:222`（`tenantFor`）、`:269`（`tenantOf`）、`:110`（字段 `volSet`）、`:526-573`（`RegisterRoutesOpts`）、`:577`（`RegisterRoutes`）、`:606`（`assembleVolumes` + panic）、`:750`（`localMux`）、`:846`（`NewLocalHandler`）。
- **审计**：`pkg/server/audit.go:13-20`（结果常量）、`:30-39`（`AuditEvent`，含 `Mesh` 字段）、`:47-75`（`RecordAudit`）；ring `audit_ring.go`；`GET /api/audit` 注册于 `handlers.go:777`（localMux）/`:1016`（srvMux）。
- **路径校验**：`pkg/server/validate.go:31`（`ValidateFilePath`）。
- **隧道**：`pkg/tunnel/tunnel_mux.go:21`（`handshakeTimeout` 常量）、`:24-40`（`Tunnel`）、`:46/54`（两个 option）、`:60`（`NewTunnel`）、`:74`（`PeerFingerprint`）、`:96`（`ensureHandshake`）、`:131`（`encryptionKey`）、`:145`（`Do`）、`:286-314`（`Serve`）；`pkg/tunnel/ecdh.go:79`（`performHandshakeWithIdentity`）、`:196`（`deriveSessionKey`，**双返回值**）、`:215`（`identitySigMessage`）；`pkg/tunnel/identity.go:54-111`（`Identity`）、`:92`（`FingerprintFromPublicKey`）、`:117/160/194`（存/取/懒建）、`:214-235`（`ParseFingerprint`/`FingerprintMatches`）。
- **mux**：`pkg/tunnel/mux/mux.go:48-53`（`Role`）、`:140`（`func New(conn xfer.Conn, role Role) *Mux`）、`:182`（`Open`）、`:214`（`Accept`）、`:226`（`Close`）。
- **传输适配**：`pkg/tunnel/xfer/core.go`（`Conn` = `Send`/`Receive`/`io.Closer`）、`xfer/internal/tcp/tcp.go:33-39`（`tcpConn` 私有）、`:44`（`maxMessageBytes = 1 MiB`）、`:54/105`（Send/Receive）、`xfer/builtin/builtin.go`（既有 internal 桥）。
- **mesh 数据面**：`pkg/tunnel/mesh/mesh.go:45-54`（`Kind*`/`Result`）、`:117`（`WebRTCStream`）、`:133`（`func Dial(ctx, svc *client.FileClient, signaler *hub.HubSignaler, target *client.MeshService, localNode string) (*Result, error)`）、`:88-108`（`MuxStreamConn`，**Close 关闭整个 mux**）；`pkg/tunnel/hub/signaling_client.go:63`（`NewHubSignaler(baseURL, accessKey, nodeID string, secret ...string)`）。
- **客户端**：`pkg/client/client.go:96-130`（`FileClient`）、`:239/247`（`WithIdentity`/`WithPeerFingerprints`）、`:1249`（`tunnelOpts`）、`:1193-1235`（`getTunnelMux`：**一个 mux 一个 Tunnel，握手只跑一次**）、`:988`（`List`）、`:937`（`Stat`，HEAD）、`:717`（`downloadTo`）、`pkg/client/relay.go:278-291`（`MeshService`/`MeshServices`）、`:66`（`RelayStream`）；`pkg/client/config.go:44`（`PeerFingerprints` 键）、`:99-102`（加载期指纹校验范式）。
- **CLI**：`cmd/sclient/root.go:23`（仅 `cfgFile` 全局）、`:26-47`（`ConfigProvider`）、`:88-96`（`PersistentPreRunE` + flag 绑定）、`:132`（`clientfactory.New`）；`cmd/sclient/list.go`、`download.go`、`meta.go`（**`stat.go` 不是文件元信息命令**）；`cmd/sclient/internal/state/state.go:15`（`State.CurrentDir`）；`cmd/sclient/internal/clientfactory/factory.go:49`（`LoadIdentityOptional`）、`:246-271`（identity/pin 仅在 xfer 模式装配）、`:159-339`（`NewClient`）；`cmd/sclient/socks.go:117-136`（`mesh.AutoRegister` → `signaler` 的既有范式）。
- **B 侧身份（复用既有，无需新配置键）**：`pkg/server/xfer_listener.go:163`（`XferIdentityPath(cfg)`）、`:181`（`LoadXferIdentity(cfg)`）；身份配置键 `hub.xfer_identity_file`（`config.go:147`）。
- **e2e 基建**：`test/e2e_test.go:256`（`startSPROXYImpl(t, extraConfig) (baseURL, uploadsDir, cleanup)`，extraConfig 为**纯字符串追加**）、`test/e2e_relay_test.go:48`（`e2eBinPath`）、`:190`（`startHubSPROXY`）、`:454`（`startSClientRelayService`）、`:89/102`（签名 GET 辅助）；`test/e2e_mesh_node_test.go:26/106`（`startSClientMeshNode`/`...Observable`）；`test/e2e_cli_harness_test.go:28-117`（`cliEnv`/`startCLIEnv`/`sclientRun`/`sclient`/`sclientJSON`/`findFilesNamed`/`getJSON`）。

---

## 任务 1：`pkg/volume` 跨节点授权域（Y-A）

**文件：**
- 修改：`pkg/volume/volume.go`（新增 `MeshReader` 类型、`ACL.MeshReaders` 字段、`AuthorizeMeshRead`/`MeshReaderFor`/`normalizeFingerprint`）
- 测试：`pkg/volume/volume_mesh_test.go`（新建）

**职责：** 纯域授权判定（无 I/O、无 `pkg/tunnel` 依赖——保持该包「纯域模型」契约，指纹归一化在 config 解析期完成，此处只做去空白+小写+恒时比较）。

- [ ] **步骤 1：编写失败的测试** `pkg/volume/volume_mesh_test.go`

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package volume

import "testing"

const testFP = "sha256:3f2a1b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708"

func meshVol(mode Mode, owners []string, readers ...MeshReader) Volume {
	m := map[string]struct{}{}
	for _, o := range owners {
		m[o] = struct{}{}
	}
	return Volume{Name: "main", ACL: ACL{Mode: mode, Owners: m, MeshReaders: readers}}
}

func TestAuthorizeMeshRead_Matrix(t *testing.T) {
	hit := MeshReader{Node: "nodeA", Fingerprint: testFP, Owner: "alice"}
	cases := []struct {
		name        string
		vol         Volume
		node, fp, owner string
		want        bool
	}{
		{"三元组命中（deny 默认开放）", meshVol(ModeDeny, nil, hit), "nodeA", testFP, "alice", true},
		{"allow 模式且 owner 在白名单", meshVol(ModeAllow, []string{"alice"}, hit), "nodeA", testFP, "alice", true},
		{"节点对但指纹错", meshVol(ModeDeny, nil, hit), "nodeA", "sha256:" + strings.Repeat("0", 64), "alice", false},
		{"节点错", meshVol(ModeDeny, nil, hit), "nodeB", testFP, "alice", false},
		{"owner 不匹配", meshVol(ModeDeny, nil, hit), "nodeA", testFP, "bob", false},
		{"mesh_readers 为空", meshVol(ModeDeny, nil), "nodeA", testFP, "alice", false},
		{"allow 模式但 owner 不在白名单", meshVol(ModeAllow, []string{"bob"}, hit), "nodeA", testFP, "alice", false},
		{"deny 模式且 owner 在黑名单", meshVol(ModeDeny, []string{"alice"}, hit), "nodeA", testFP, "alice", false},
		{"指纹大小写/空白归一命中", meshVol(ModeDeny, nil, MeshReader{Node: "nodeA", Fingerprint: " " + strings.ToUpper(testFP) + " ", Owner: "alice"}), "nodeA", testFP, "alice", true},
		{"请求指纹带空白/大写命中", meshVol(ModeDeny, nil, hit), "nodeA", " " + strings.ToUpper(testFP) + " ", "alice", true},
		{"空 node 拒绝", meshVol(ModeDeny, nil, hit), "", testFP, "alice", false},
		{"空 owner 拒绝", meshVol(ModeDeny, nil, hit), "nodeA", testFP, "", false},
		{"空指纹拒绝", meshVol(ModeDeny, nil, hit), "nodeA", "", "alice", false},
		{"未知 mode fail-closed", Volume{Name: "main", ACL: ACL{Mode: Mode("bogus"), MeshReaders: []MeshReader{hit}}}, "nodeA", testFP, "alice", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.vol.AuthorizeMeshRead(tc.node, tc.fp, tc.owner); got != tc.want {
				t.Fatalf("AuthorizeMeshRead(%q,%q,%q) = %v, want %v", tc.node, tc.fp, tc.owner, got, tc.want)
			}
		})
	}
}

func TestMeshReaderFor(t *testing.T) {
	other := MeshReader{Node: "nodeC", Fingerprint: "sha256:" + strings.Repeat("a", 64), Owner: "carol"}
	vol := meshVol(ModeDeny, nil, MeshReader{Node: "nodeA", Fingerprint: testFP, Owner: "alice"}, other)

	if mr, ok := vol.MeshReaderFor(testFP); !ok || mr.Node != "nodeA" || mr.Owner != "alice" {
		t.Fatalf("命中条目不符: %+v ok=%v", mr, ok)
	}
	if mr, ok := vol.MeshReaderFor(" " + strings.ToUpper(testFP) + " "); !ok || mr.Node != "nodeA" {
		t.Fatalf("归一后应命中: %+v ok=%v", mr, ok)
	}
	if _, ok := vol.MeshReaderFor("sha256:" + strings.Repeat("b", 64)); ok {
		t.Fatal("未列出的指纹不应命中")
	}
	if _, ok := vol.MeshReaderFor(""); ok {
		t.Fatal("空指纹不应命中")
	}
	if _, ok := meshVol(ModeDeny, nil).MeshReaderFor(testFP); ok {
		t.Fatal("空 mesh_readers 不应命中")
	}
}
```

（测试文件需 `import "strings"`，示例中已使用。）

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -run 'TestAuthorizeMeshRead_Matrix|TestMeshReaderFor' ./pkg/volume/`
预期：FAIL — `undefined: MeshReader`、`unknown field MeshReaders`。

- [ ] **步骤 3：编写最少实现代码**

在 `pkg/volume/volume.go`：import 块改为

```go
import (
	"crypto/subtle"
	"sort"
	"strings"
)
```

`ACL` 增字段（放在 `Owners` 之后）：

```go
	// MeshReaders 是跨节点只读授权（Y 一期，AD-5）。零值 = 无任何节点被授权（fail-closed）。
	MeshReaders []MeshReader
```

在 `ACL` 定义之后新增：

```go
// MeshReader 把「一个 mesh 节点身份」绑定到「一个可只读访问的 owner 命名空间」。
// Fingerprint 为 Ed25519 身份指纹（规范形 "sha256:<64 位小写 hex>"，由 pkg/server
// 在配置解析期经 tunnel.ParseFingerprint 归一后填入）。
type MeshReader struct {
	Node        string
	Fingerprint string
	Owner       string
}

// AuthorizeMeshRead 判定节点 node（已认证指纹 fingerprint）可否只读访问 owner 命名空间。
//
// 双重约束（任一不满足即拒，fail-closed）：
//  1. 本卷 mesh_readers 中存在三元组 (node, fingerprint, owner) 的命中条目；
//  2. owner 本身能过本卷 ACL（Authorize）——Mode=allow 须在 Owners 内，
//     Mode=deny/零值 不得在黑名单内。
//
// 指纹比较前做归一化（去空白 + 转小写），比较用 crypto/subtle.ConstantTimeCompare
// 以避免早期退出的时序差异（指纹非秘密，仅为防御一致性）。
func (v Volume) AuthorizeMeshRead(node, fingerprint, owner string) bool {
	if node == "" || owner == "" || fingerprint == "" {
		return false
	}
	if !v.Authorize(owner) {
		return false
	}
	want := normalizeFingerprint(fingerprint)
	for i := range v.ACL.MeshReaders {
		mr := &v.ACL.MeshReaders[i]
		if mr.Node != node || mr.Owner != owner {
			continue
		}
		if fingerprintEqual(normalizeFingerprint(mr.Fingerprint), want) {
			return true
		}
	}
	return false
}

// MeshReaderFor 返回本卷 mesh_readers 中指纹命中 fingerprint 的首个条目。
// 空指纹或无命中返回 false（fail-closed）。供 B 侧远程 handler 由「已认证对端指纹」
// 反查 (node, owner) 绑定——owner 绝不由请求方指定。
func (v Volume) MeshReaderFor(fingerprint string) (MeshReader, bool) {
	want := normalizeFingerprint(fingerprint)
	if want == "" {
		return MeshReader{}, false
	}
	for i := range v.ACL.MeshReaders {
		if fingerprintEqual(normalizeFingerprint(v.ACL.MeshReaders[i].Fingerprint), want) {
			return v.ACL.MeshReaders[i], true
		}
	}
	return MeshReader{}, false
}

// normalizeFingerprint 归一化指纹用于比较：去首尾空白 + 转小写（前缀 "sha256:" 保留，
// 两端一致即可；配置侧已由 tunnel.ParseFingerprint 归一为规范形）。
func normalizeFingerprint(fp string) string {
	return strings.ToLower(strings.TrimSpace(fp))
}

// fingerprintEqual 恒时比较两个已归一化指纹（长度不同直接判否，不做恒时比较）。
func fingerprintEqual(a, b string) bool {
	if len(a) != len(b) || a == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
```

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -race ./pkg/volume/`
预期：PASS（既有 `volume_test.go` 同时通过——`ACL` 加字段不影响既有用例）。

- [ ] **步骤 5：Commit**

```bash
cd D:/workdir/leon/cocomhub/sproxy
git checkout -b feature/y-read-acl
git add pkg/volume/volume.go pkg/volume/volume_mesh_test.go
git commit -m "feat(volume): 跨节点只读授权域——MeshReader + AuthorizeMeshRead + MeshReaderFor

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

## 任务 2：`mesh_readers` 配置解析与校验（Y-A）

**文件：**
- 修改：`pkg/server/config.go`（`VolumeMeshReaderConfig`、`VolumeACLConfig.MeshReaders`、`SetDefaults` 指纹归一、`Validate` 校验）
- 修改：`pkg/server/volumes.go`（`parseVolumeACL` 填充 `volume.MeshReaders`）
- 测试：`pkg/server/config_mesh_readers_test.go`（新建）

**职责：** 把 YAML 的 `volumes[].acl.mesh_readers[]` 解析为 `volume.MeshReader`，在加载期做响亮校验（fail-closed），指纹归一为规范形。

- [ ] **步骤 1：编写失败的测试** `pkg/server/config_mesh_readers_test.go`

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strings"
	"testing"
)

const testReaderFP = "sha256:3f2a1b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708"

// withMeshReader 装配单卷 + 一条 mesh_readers 条目。
//
// 刻意用 Mode=deny（默认开放）且不设 Owners：既有的 Validate 会**先**校验 Owners 列表
// （config.go:770-779），若这里塞入非法 owner，报错会来自 Owners 而非 mesh_readers，
// 断言就测不到本任务新增的校验分支。
func withMeshReader(node, fp, owner string) func(*Config) {
	return func(c *Config) {
		c.Volumes = []VolumeConfig{{
			Name: "main", Root: c.StorageRoot,
			ACL: &VolumeACLConfig{
				Mode:        VolumeACLDeny,
				MeshReaders: []VolumeMeshReaderConfig{{Node: node, Fingerprint: fp, Owner: owner}},
			},
		}}
	}
}

func TestMeshReadersConfig_Validate(t *testing.T) {
	cases := []struct {
		name    string
		node    string
		fp      string
		owner   string
		wantErr string
	}{
		{"合法", "nodeA", testReaderFP, "alice", ""},
		{"纯 64 hex 合法", "nodeA", strings.TrimPrefix(testReaderFP, "sha256:"), "alice", ""},
		{"大写 hex 合法", "nodeA", strings.ToUpper(testReaderFP), "alice", ""},
		{"指纹长度非法", "nodeA", "sha256:abc", "alice", "fingerprint 非法"},
		{"指纹含非 hex", "nodeA", "sha256:" + strings.Repeat("z", 64), "alice", "fingerprint 非法"},
		{"node 为空", "", testReaderFP, "alice", "node 不能为空"},
		{"owner 为空", "nodeA", testReaderFP, "", "owner 不能为空"},
		{"owner 含路径分隔符", "nodeA", testReaderFP, "a/b", "owner 非法"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.StorageRoot = t.TempDir()
			withMeshReader(tc.node, tc.fp, tc.owner)(cfg)
			cfg.SetDefaults()
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("期望通过, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("期望含 %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestMeshReadersConfig_DuplicateFingerprintRejected(t *testing.T) {
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{
		Name: "main", Root: cfg.StorageRoot,
		ACL: &VolumeACLConfig{
			Mode: VolumeACLAllow,
			MeshReaders: []VolumeMeshReaderConfig{
				{Node: "nodeA", Fingerprint: testReaderFP, Owner: "alice"},
				{Node: "nodeB", Fingerprint: strings.ToUpper(testReaderFP), Owner: "bob"},
			},
		},
	}}
	cfg.SetDefaults()
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "指纹重复") {
		t.Fatalf("同一卷内指纹重复（归一后）应被拒绝, got %v", err)
	}
}

func TestMeshReadersConfig_ParseVolumeACL(t *testing.T) {
	acl := parseVolumeACL(&VolumeACLConfig{
		Mode:        VolumeACLAllow,
		Owners:      []string{"alice"},
		MeshReaders: []VolumeMeshReaderConfig{{Node: "nodeA", Fingerprint: strings.ToUpper(testReaderFP), Owner: "alice"}},
	})
	if len(acl.MeshReaders) != 1 {
		t.Fatalf("mesh_readers 应解析出 1 条, got %d", len(acl.MeshReaders))
	}
	mr := acl.MeshReaders[0]
	if mr.Node != "nodeA" || mr.Owner != "alice" {
		t.Fatalf("条目不符: %+v", mr)
	}
	if mr.Fingerprint != testReaderFP {
		t.Fatalf("指纹应归一为规范形 %q, got %q", testReaderFP, mr.Fingerprint)
	}
	if !VolumeMeshAuthorized(acl, "nodeA", testReaderFP, "alice") { /* 见步骤 3 说明 */ }
}
```

> 步骤 3 说明：最后一行断言改用 `pkg/volume` 的 `Volume` 包装——把实现里的 `parseVolumeACL` 结果装进 `volume.Volume{ACL: acl}` 再断言 `AuthorizeMeshRead`。为保持测试可编译，**步骤 1 写测试时该行直接写成**：

```go
	if !(volume.Volume{Name: "main", ACL: acl}).AuthorizeMeshRead("nodeA", testReaderFP, "alice") {
		t.Fatal("解析后的 ACL 应放行 nodeA/alice 只读")
	}
```

（并在 import 中加入 `"github.com/cocomhub/sproxy/pkg/volume"`。）

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -run TestMeshReadersConfig ./pkg/server/`
预期：FAIL — `undefined: VolumeMeshReaderConfig`、`unknown field MeshReaders`。

- [ ] **步骤 3：编写最少实现代码**

**(3a) `pkg/server/config.go`** — 在 `VolumeACLConfig`（`:366`）中加字段：

```go
type VolumeACLConfig struct {
	Mode   VolumeACLMode `yaml:"mode" mapstructure:"mode"`
	Owners []string      `yaml:"owners" mapstructure:"owners"`
	// MeshReaders 是跨节点只读授权条目（Y 一期，AD-5）；省略 = 无任何节点被授权。
	MeshReaders []VolumeMeshReaderConfig `yaml:"mesh_readers,omitempty" mapstructure:"mesh_readers"`
}

// VolumeMeshReaderConfig 是卷 ACL 的跨节点只读授权条目：把 mesh 节点身份
// （node + Ed25519 指纹）绑定到一个可只读访问的 owner 命名空间。
type VolumeMeshReaderConfig struct {
	Node        string `yaml:"node" mapstructure:"node"`
	Fingerprint string `yaml:"fingerprint" mapstructure:"fingerprint"`
	Owner       string `yaml:"owner" mapstructure:"owner"`
}
```

**(3b) `SetDefaults()`** — 在卷归一循环（`:577-599`）之内、ACL 缺省填充之后追加指纹归一：

```go
		// Y 一期：mesh_readers 指纹归一为规范形（去空白/大小写/可省前缀）。
		// 非法指纹在此保持原样，交由 Validate 响亮拒绝（fail-closed）。
		if ac := c.Volumes[i].ACL; ac != nil {
			for j := range ac.MeshReaders {
				if norm, err := tunnel.ParseFingerprint(ac.MeshReaders[j].Fingerprint); err == nil {
					ac.MeshReaders[j].Fingerprint = norm
				}
			}
		}
```

（`pkg/server/config.go` 需 import `"github.com/cocomhub/sproxy/pkg/tunnel"`——`pkg/server` 已依赖 `pkg/tunnel`，无新依赖、无环。）

**(3c) `Validate()`** — 在既有逐卷 ACL 校验块（`:770-779`）之后、同一卷循环内追加：

```go
			seenReaders := map[string]struct{}{}
			for _, mr := range v.ACL.MeshReaders {
				if mr.Node == "" {
					return fmt.Errorf("卷 %q 的 mesh_readers.node 不能为空", v.Name)
				}
				if mr.Owner == "" {
					return fmt.Errorf("卷 %q 的 mesh_readers.owner 不能为空", v.Name)
				}
				if err := storage.ValidSegmentName(mr.Owner); err != nil {
					return fmt.Errorf("卷 %q 的 mesh_readers.owner 非法: %w", v.Name, err)
				}
				norm, err := tunnel.ParseFingerprint(mr.Fingerprint)
				if err != nil {
					return fmt.Errorf("卷 %q 的 mesh_readers.fingerprint 非法: %w", v.Name, err)
				}
				if _, dup := seenReaders[norm]; dup {
					return fmt.Errorf("卷 %q 的 mesh_readers 指纹重复（归一后）: %s", v.Name, norm)
				}
				seenReaders[norm] = struct{}{}
			}
```

**(3d) `pkg/server/volumes.go`** — `parseVolumeACL`（`:221-234`）末尾追加（保持既有签名与 nil/空语义不变）：

```go
	// Y 一期：跨节点只读授权条目（指纹归一为规范形；非法值已由 Config.Validate 拒绝）。
	for _, mr := range ac.MeshReaders {
		fp := strings.ToLower(strings.TrimSpace(mr.Fingerprint))
		if norm, err := tunnel.ParseFingerprint(mr.Fingerprint); err == nil {
			fp = norm
		}
		acl.MeshReaders = append(acl.MeshReaders, volume.MeshReader{
			Node: mr.Node, Fingerprint: fp, Owner: mr.Owner,
		})
	}
```

（`pkg/server/volumes.go` 需 import `"strings"` + `"github.com/cocomhub/sproxy/pkg/tunnel"`。）

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -race -run 'TestMeshReadersConfig|TestVolumesConfig' ./pkg/server/`
预期：PASS。

- [ ] **步骤 5：零回归验证（未配置 mesh_readers 时）**

运行：`go test -count=1 ./pkg/server/ ./pkg/volume/`
预期：全部 PASS（新增字段为加性，省略 `mesh_readers` 时 `ACL.MeshReaders` 为 nil）。

- [ ] **步骤 6：Commit**

```bash
git add pkg/server/config.go pkg/server/volumes.go pkg/server/config_mesh_readers_test.go
git commit -m "feat(server): mesh_readers 配置解析与加载期校验（指纹归一 + 唯一性）

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

- [ ] **步骤 7：Y-A 收尾 —— lint + 全量测试 + PR**

```bash
gofmt -l pkg/volume pkg/server
make lint
go test -count=1 -race ./pkg/volume/ ./pkg/server/
git push -u origin feature/y-read-acl
gh pr create --title "Y-A：跨节点只读授权面（MeshReader + mesh_readers 配置）" --body "..."
```

PR 描述须含：DoD「未配置时零回归」证据、`make lint` 0 issues 证据、对抗式审查结论。CI 全绿后 squash 合入 master，再开 `feature/y-read-surface`。

---

## 任务 3：`remoteReadHandler` 只读面（Y-B）

**文件：**
- 创建：`pkg/server/remote_read.go`
- 测试：`pkg/server/remote_read_test.go`（新建）
- 修改：**无**。`newRemoteReadHandler` 是 `*Handlers` 的新方法（定义在 `remote_read.go`），复用既有字段 `h.volSet`/`h.RecordAudit`，`handlers.go` 与既有读 handler **零改动**（零回归红线的直接体现）。T5 接线时 `cmd/sproxy` 才引用它。

**职责：** B 侧远程只读面：指纹 → `(vol, node, owner)` 反查 → `AuthorizeMeshRead` → 受限 context → 委派既有读 handler；手写只读路由表；每次请求记审计（含拒绝）。

- [ ] **步骤 1：编写失败的测试** `pkg/server/remote_read_test.go`

测试需先有一个可用的 `*Handlers`（多卷 + 已装配 `volSet`）。新增本地辅助函数：

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	testReaderNodeA = "nodeA"
	testReaderOwner = "alice"
	testReaderRel   = "docs/hello.txt"
	testReaderBody  = "hello-remote-read\n"
)

// fakePeerFingerprint 是 peerFingerprintProvider 的测试实现（伪造已认证对端指纹）。
type fakePeerFingerprint struct{ fp string }

func (f fakePeerFingerprint) PeerFingerprint() string { return f.fp }

// remoteReadTestConfig 构造「单卷 + 一条 mesh_readers + 磁盘上 docs/hello.txt」的已校验 cfg。
func remoteReadTestConfig(t *testing.T) *Config {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = filepath.Join(t.TempDir(), "vol-main")
	cfg.LogLevel = "error"
	cfg.Audit.BufferSize = 100
	cfg.Volumes = []VolumeConfig{{
		Name: "main", Root: cfg.StorageRoot,
		ACL: &VolumeACLConfig{
			Mode:   VolumeACLAllow,
			Owners: []string{testReaderOwner},
			MeshReaders: []VolumeMeshReaderConfig{{
				Node: testReaderNodeA, Fingerprint: testReaderFP, Owner: testReaderOwner,
			}},
		},
	}}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixture config 非法: %v", err)
	}
	userDir := filepath.Join(cfg.StorageRoot, testReaderOwner, "user", "docs")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StorageRoot, testReaderOwner, "user", filepath.FromSlash(testReaderRel)), []byte(testReaderBody), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// newRemoteReadHandlers 用给定 cfg 装配 *Handlers（审计 JSON 写入 auditBuf）。
// T5 的 in-process 中测复用本函数（同包），避免重复装配逻辑。
func newRemoteReadHandlers(t *testing.T, cfg *Config, auditBuf *bytes.Buffer) *Handlers {
	t.Helper()
	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(cfg)
	opts := defaultNoAuthRegOpts() // 既有测试辅助（server_test_common_test.go:34）
	opts.Mux = http.NewServeMux()
	opts.CfgPtr = cfgPtr
	opts.Version = "test"
	opts.BuildAt = "test"
	opts.Logger = testLogger()
	opts.AuditLogger = slog.New(slog.NewJSONHandler(auditBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := RegisterRoutes(t.Context(), opts)
	t.Cleanup(h.Close)
	return h
}

// newRemoteReadFixture 返回 (只读面 handler, cfg, 审计缓冲)。
func newRemoteReadFixture(t *testing.T, peerFP string) (http.Handler, *Config, *bytes.Buffer) {
	t.Helper()
	cfg := remoteReadTestConfig(t)
	auditBuf := &bytes.Buffer{}
	h := newRemoteReadHandlers(t, cfg, auditBuf)
	return h.newRemoteReadHandler(fakePeerFingerprint{fp: peerFP}), cfg, auditBuf
}

func doRemote(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
```

（`registerOpts` 具体字段名以 `RegisterRoutesOpts`（`handlers.go:526-573`）为准，implementer 读后照抄既有测试 `integration_test.go` 的填法。）

用例：

```go
func TestRemoteRead_AuthorizedListAndDownload(t *testing.T) {
	h, _, _ := newRemoteReadFixture(t, testReaderFP)

	rec := doRemote(t, h, http.MethodGet, "/remote/list?volume=main&path=/docs")
	if rec.Code != http.StatusOK {
		t.Fatalf("list 应 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var lr listResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &lr); err != nil {
		t.Fatalf("list 响应非法 JSON: %v", err)
	}
	if len(lr.Files) != 1 || lr.Files[0].Name != "hello.txt" || lr.Files[0].Volume != "main" {
		t.Fatalf("list 结果不符: %+v", lr.Files)
	}

	rec = doRemote(t, h, http.MethodGet, "/remote/download?volume=main&path=/"+testReaderRel)
	if rec.Code != http.StatusOK {
		t.Fatalf("download 应 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != testReaderBody {
		t.Fatalf("下载内容不符: %q", got)
	}

	rec = doRemote(t, h, http.MethodHead, "/remote/stat?volume=main&path=/"+testReaderRel)
	if rec.Code != http.StatusOK {
		t.Fatalf("stat 应 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("X-File-Size"); got != strconv.Itoa(len(testReaderBody)) {
		t.Fatalf("X-File-Size 不符: %q", got)
	}
}

func TestRemoteRead_AuthorizationMatrix(t *testing.T) {
	cases := []struct {
		name     string
		peerFP   string
		target   string
		wantCode int
	}{
		{"未认证指纹（空）", "", "/remote/list?volume=main&path=/docs", http.StatusUnauthorized},
		{"指纹未列入 mesh_readers", "sha256:" + strings.Repeat("b", 64), "/remote/list?volume=main&path=/docs", http.StatusNotFound},
		{"卷不存在", testReaderFP, "/remote/list?volume=nope&path=/docs", http.StatusNotFound},
		{"缺 volume 参数", testReaderFP, "/remote/list?path=/docs", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newRemoteReadFixture(t, tc.peerFP)
			if rec := doRemote(t, h, http.MethodGet, tc.target); rec.Code != tc.wantCode {
				t.Fatalf("want %d, got %d body=%s", tc.wantCode, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRemoteRead_WriteMethodsRejected(t *testing.T) {
	h, _, _ := newRemoteReadFixture(t, testReaderFP)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		if rec := doRemote(t, h, m, "/remote/list?volume=main&path=/docs"); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s 应 405（只读白名单）, got %d", m, rec.Code)
		}
	}
	// 写路径的既有路由（如 /upload）不在远程面上。
	if rec := doRemote(t, h, http.MethodPost, "/upload"); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
		t.Fatalf("/upload 不应出现在远程面, got %d", rec.Code)
	}
}

func TestRemoteRead_PathTraversalRejected(t *testing.T) {
	h, _, _ := newRemoteReadFixture(t, testReaderFP)
	for _, p := range []string{"/../../etc/passwd", "/docs/../../secret", "../../etc", "/a\x00b"} {
		rec := doRemote(t, h, http.MethodGet, "/remote/download?volume=main&path="+url.QueryEscape(p))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("路径 %q 应 400, got %d", p, rec.Code)
		}
	}
}

func TestRemoteRead_OwnerFromConfigNotRequest(t *testing.T) {
	h, cfg, _ := newRemoteReadFixture(t, testReaderFP)
	// 磁盘上再放一个 bob 的文件，请求方试图用 owner 参数越权。
	bobDir := filepath.Join(cfg.StorageRoot, "bob", "user")
	if err := os.MkdirAll(bobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bobDir, "secret.txt"), []byte("bob-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := doRemote(t, h, http.MethodGet, "/remote/download?volume=main&path=/secret.txt&owner=bob&actor=bob")
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "bob-secret") {
		t.Fatal("owner 必须由配置决定，不接受请求参数指定（越权枚举）")
	}
}

func TestRemoteRead_LargeFileStreamsWithoutBuffering(t *testing.T) {
	h, cfg, _ := newRemoteReadFixture(t, testReaderFP)
	const size = 6 << 20 // 6 MiB > chunk 帧 64 KiB，验证流式而非整体入内存
	big := bytes.Repeat([]byte("x"), size)
	if err := os.WriteFile(filepath.Join(cfg.StorageRoot, testReaderOwner, "user", "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := doRemote(t, h, http.MethodGet, "/remote/download?volume=main&path=/big.bin")
	if rec.Code != http.StatusOK {
		t.Fatalf("大文件下载应 200, got %d", rec.Code)
	}
	if rec.Body.Len() != size {
		t.Fatalf("字节数不符: got %d want %d", rec.Body.Len(), size)
	}
	sum := sha256.Sum256(rec.Body.Bytes())
	if got := hex.EncodeToString(sum[:]); got != testutil.SHA256Hex(big) {
		t.Fatalf("SHA-256 不符: got %s", got)
	}
}

func TestRemoteRead_RangeSupported(t *testing.T) {
	h, _, _ := newRemoteReadFixture(t, testReaderFP)
	req := httptest.NewRequest(http.MethodGet, "/remote/download?volume=main&path=/"+testReaderRel, nil)
	req.Header.Set("Range", "bytes=0-4")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("Range 应 206, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != testReaderBody[:5] {
		t.Fatalf("Range 内容不符: %q", got)
	}
}

func TestRemoteRead_AuditRecordsAllowAndDeny(t *testing.T) {
	h, _, auditBuf := newRemoteReadFixture(t, testReaderFP)
	doRemote(t, h, http.MethodGet, "/remote/list?volume=main&path=/docs")
	lines := auditBuf.String()
	// RecordAudit 以 slog kv 形式输出小写键（audit.go:65-74）：action/actor/mesh/.../result。
	if !strings.Contains(lines, `"action":"mesh_read"`) || !strings.Contains(lines, `"mesh":"nodeA"`) {
		t.Fatalf("放行路径应记审计（含 mesh 字段）: %s", lines)
	}

	h2, _, audit2 := newRemoteReadFixture(t, "sha256:"+strings.Repeat("b", 64))
	doRemote(t, h2, http.MethodGet, "/remote/list?volume=main&path=/docs")
	if !strings.Contains(audit2.String(), `"action":"mesh_read"`) ||
		!strings.Contains(audit2.String(), `"result":"denied"`) {
		t.Fatalf("拒绝路径也须记审计: %s", audit2.String())
	}
}
```

（结果常量字面值以 `audit.go:13-20` 为准：`AuditResultSuccess="success"`、`AuditResultDenied="denied"`、`AuditResultError="error"`——上方断言已按实际值写定。）

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -run TestRemoteRead ./pkg/server/`
预期：FAIL — `h.newRemoteReadHandler undefined`。

- [ ] **步骤 3：编写最少实现代码** `pkg/server/remote_read.go`

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/cocomhub/sproxy/pkg/volume"
)

// peerFingerprintProvider 抽象「已认证对端指纹」的来源。
//
// 生产实现是 *tunnel.Tunnel：Serve 在进入 accept 循环前完成双向 Ed25519 pin 握手，
// PeerFingerprint() 返回握手获得的对端指纹（tunnel_mux.go:286-305）。测试注入伪造
// 实现以驱动授权矩阵。
type peerFingerprintProvider interface {
	PeerFingerprint() string
}

// remoteReadHandler 是跨节点只读面（Y 一期，AD-7/AD-8）。
//
// 只读强制：路由表是手写白名单，仅注册三条 GET/HEAD；写方法由 http.ServeMux 的
// 方法模式天然 405，写 handler 从不出现在这张表上（非黑名单法）。
//
// 授权：owner 恒由配置（mesh_readers 条目）决定，绝不接受请求方指定；卷名必须命中
// 本节点已配置的卷，且该卷 mesh_readers 中存在指纹命中的条目。
type remoteReadHandler struct {
	h    *Handlers
	peer peerFingerprintProvider
}

// remoteTarget 是一次已授权远程读的目标上下文。
type remoteTarget struct {
	vol  volume.Volume
	node string // 审计用：对端 mesh 节点 ID（来自命中的 mesh_readers 条目）
	owner string // 受限 context 中注入的 owner（来自命中的 mesh_readers 条目）
	path string // owner user 桶内相对路径
}

// newRemoteReadHandler 构造只读路由表（每连接一个：指纹是连接级属性）。
func (h *Handlers) newRemoteReadHandler(peer peerFingerprintProvider) http.Handler {
	rh := &remoteReadHandler{h: h, peer: peer}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /remote/list", rh.handleList)
	mux.HandleFunc("HEAD /remote/stat", rh.handleStat)
	mux.HandleFunc("GET /remote/download", rh.handleDownload)
	return mux
}

func (rh *remoteReadHandler) handleList(w http.ResponseWriter, r *http.Request) {
	rh.serve(w, r, "list")
}

func (rh *remoteReadHandler) handleStat(w http.ResponseWriter, r *http.Request) {
	rh.serve(w, r, "stat")
}

func (rh *remoteReadHandler) handleDownload(w http.ResponseWriter, r *http.Request) {
	rh.serve(w, r, "download")
}

// serve 统一走「授权 → 委派 → 审计」三步；拒绝路径在 authorize 内记审计后直接返回。
func (rh *remoteReadHandler) serve(w http.ResponseWriter, r *http.Request, op string) {
	tgt, ok := rh.authorize(w, r)
	if !ok {
		return
	}
	sw := &remoteStatusWriter{ResponseWriter: w}
	rh.delegate(sw, r, tgt, op)
	rh.h.RecordAudit(r.Context(), AuditEvent{
		Action: "mesh_read", Actor: tgt.owner, Mesh: tgt.node,
		ObjectType: "file", Object: tgt.path,
		Result: remoteAuditResult(sw.status),
		Detail: "volume=" + tgt.vol.Name + " path=" + tgt.path + " status=" + strconv.Itoa(sw.status),
	})
}

// authorize 解析并校验授权；不通过时写 404/401 并记审计，返回 ok=false。
func (rh *remoteReadHandler) authorize(w http.ResponseWriter, r *http.Request) (*remoteTarget, bool) {
	volName := strings.TrimSpace(r.URL.Query().Get("volume"))
	relPath := strings.TrimSpace(r.URL.Query().Get("path"))

	denied := func(result, detail string) (*remoteTarget, bool) {
		rh.h.RecordAudit(r.Context(), AuditEvent{
			Action: "mesh_read", ObjectType: "file", Object: relPath,
			Result: result, Detail: "volume=" + volName + " path=" + relPath + ": " + detail,
		})
		writeRemoteError(w, http.StatusNotFound, "not found")
		return nil, false
	}

	fp := ""
	if rh.peer != nil {
		fp = strings.TrimSpace(rh.peer.PeerFingerprint())
	}
	if fp == "" {
		// 未完成身份握手（理论上不可达：B 侧 listener 恒用非 nil 静态密钥 →
		// Serve 已强制握手）。防御性拒绝，且不泄露任何卷/文件存在性。
		rh.h.RecordAudit(r.Context(), AuditEvent{
			Action: "mesh_read", ObjectType: "file", Object: relPath,
			Result: AuditResultDenied, Detail: "volume=" + volName + " path=" + relPath + ": 对端无已认证身份指纹",
		})
		writeRemoteError(w, http.StatusUnauthorized, "unauthorized")
		return nil, false
	}
	if volName == "" {
		return denied(AuditResultDenied, "缺少 volume 参数")
	}
	if rh.h.volSet == nil {
		return denied(AuditResultError, "卷集合未装配")
	}
	vol, ok := rh.h.volSet.ByName(volName)
	if !ok {
		return denied(AuditResultDenied, "卷不存在")
	}
	mr, ok := vol.MeshReaderFor(fp)
	if !ok {
		return denied(AuditResultDenied, "指纹未列入本卷 mesh_readers")
	}
	if !vol.AuthorizeMeshRead(mr.Node, fp, mr.Owner) {
		return denied(AuditResultDenied, "授权三元组未通过 node="+mr.Node)
	}
	return &remoteTarget{vol: vol, node: mr.Node, owner: mr.Owner, path: relPath}, true
}

// delegate 把远程请求重写为既有内部读请求并委派既有 handler（AD-8）：
//   - owner 经受限 context 注入（复用 actorCtxKey 通路，使 ActorFrom(ctx) 返回该 owner）；
//   - 卷显式锁定（?volume=），owner 的 user 桶内相对路径改写为既有 `subdir`/`filename`。
//
// 既有的 ValidateFilePath / 多卷 ACL / 跨卷定位等校验全部保留。
func (rh *remoteReadHandler) delegate(w http.ResponseWriter, r *http.Request, tgt *remoteTarget, op string) {
	q := url.Values{}
	q.Set("volume", tgt.vol.Name)
	switch op {
	case "list":
		q.Set("subdir", tgt.path)
	case "stat", "download":
		q.Set("filename", tgt.path)
	}

	r2 := r.Clone(withActor(r.Context(), tgt.owner))
	r2.URL = &url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
	r2.RequestURI = r2.URL.RequestURI()

	switch op {
	case "list":
		rh.h.listFiles(w, r2)
	case "stat":
		rh.h.stat(w, r2)
	case "download":
		rh.h.download(w, r2)
	}
}

// remoteStatusWriter 记录响应状态码，供审计落「放行/不存在/错误」。
type remoteStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *remoteStatusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *remoteStatusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// remoteAuditResult 把响应状态映射为既有审计结果常量。
func remoteAuditResult(status int) string {
	switch status {
	case 0, http.StatusOK, http.StatusPartialContent:
		return AuditResultSuccess
	default:
		return AuditResultError
	}
}

// writeRemoteError 写纯文本错误响应（不泄露卷/文件存在性）。
func writeRemoteError(w http.ResponseWriter, code int, msg string) {
	http.Error(w, msg, code)
}
```

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -race -run TestRemoteRead ./pkg/server/`
预期：PASS（全部 8 个用例）。

- [ ] **步骤 5：确认未改动既有读路径**

运行：`git diff --stat HEAD -- pkg/server/list_handler.go pkg/server/download_handler.go pkg/server/handlers.go`
预期：**空输出**（既有读路径与路由装配零改动红线）。若此处非空，说明实现越界，必须回退。

- [ ] **步骤 6：Commit 并开 PR（Y-B）**

```bash
gofmt -l pkg/server
go test -count=1 -race ./pkg/server/
git add pkg/server/remote_read.go pkg/server/remote_read_test.go
git commit -m "feat(server): 跨节点只读面 remoteReadHandler（只读白名单 + 受限 context + 审计）

Co-Authored-By: Claude Code <noreply@anthropic.com>"
git checkout master && git merge --squash feature/y-read-acl  # 若 Y-A 尚未合入
```

Y-B 开工前必须先合入 Y-A（`feature/y-read-surface` 从含 Y-A 的 master 切出）。

---

## 任务 4：传输原语（Y-C）

**文件：**
- 修改：`pkg/tunnel/xfer/internal/tcp/tcp.go`（新增导出 `FromNetConn`）
- 修改：`pkg/tunnel/xfer/builtin/builtin.go`（新增桥 `FromNetConn`）
- 创建：`pkg/tunnel/remote_key.go`（`DeriveRemoteStaticKey`）
- 修改：`pkg/tunnel/tunnel_mux.go`（`WithHandshakeTimeout` + 常量改名 + `Tunnel.handshakeTimeout` 字段）
- 测试：`pkg/tunnel/xfer/builtin/builtin_test.go`（追加）、`pkg/tunnel/remote_key_test.go`（新建）、`pkg/tunnel/tunnel_mux_timeout_test.go`（新建）

**职责：** 补齐 `net.Conn → xfer.Conn` 适配（唯一传输缺口）、远程读静态密钥的确定性派生、可配握手超时。**零回归**：默认握手超时仍 30s，既有调用方不受影响。

- [ ] **步骤 1：编写失败的测试**

**(1a)** `pkg/tunnel/remote_key_test.go`：

```go
package tunnel

import (
	"bytes"
	"testing"
)

func TestDeriveRemoteStaticKey_DeterministicAndSized(t *testing.T) {
	fp := "sha256:3f2a1b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708"
	k1 := DeriveRemoteStaticKey(fp)
	k2 := DeriveRemoteStaticKey(fp)
	if !bytes.Equal(k1, k2) {
		t.Fatal("同一指纹必须派生出同一密钥（两端确定性一致）")
	}
	if len(k1) != sessionKeyLen {
		t.Fatalf("密钥长度应为 %d, got %d", sessionKeyLen, len(k1))
	}
	if bytes.Equal(k1, DeriveRemoteStaticKey("sha256:"+strings.Repeat("0", 64))) {
		t.Fatal("不同指纹应派生出不同密钥（每 listener 域分离）")
	}
	if bytes.Equal(k1, DeriveRemoteStaticKey("")) {
		t.Fatal("空指纹不应与合法指纹同密钥")
	}
}
```

**(1b)** `pkg/tunnel/tunnel_mux_timeout_test.go`（真握手停滞场景，验证可配超时的**实效**）：

```go
func TestServe_HandshakeTimeoutHonored(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	m := mux.New(builtin.FromNetConn(server), mux.RoleListener) // 复用本任务 (3a)/(3b) 的新桥
	defer m.Close()

	tun := NewTunnel(m, []byte(testutil.TestKey()), WithHandshakeTimeout(150*time.Millisecond))
	start := time.Now()
	err := tun.Serve(t.Context(), http.NotFoundHandler())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("对端不发握手帧时应超时报错（fail-closed）")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("应受 WithHandshakeTimeout 约束（~150ms）, 实际 %v", elapsed)
	}
}
```

> 实现说明：`mux.New` 需要 `xfer.Conn`——本用例直接用步骤 (3a)/(3b) 新增的 `builtin.FromNetConn`（同时也是该适配器的第二重实证：真 mux 帧跑在它上面）。`net.Pipe` 无 deadline 语义，`tcpConn` 会回落 60s 硬写超时，不影响本用例（对端不发数据 → 读方向阻塞 → 由握手 ctx 超时兜底）。

**(1c)** `pkg/tunnel/xfer/builtin/builtin_test.go` 追加：

```go
func TestFromNetConn_RoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	ca, cb := builtin.FromNetConn(a), builtin.FromNetConn(b)
	want := []byte("hello-xfer-netconn")
	go func() { _ = ca.Send(context.Background(), want) }()

	got, err := cb.Receive(context.Background())
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("往返内容不符: %q", got)
	}
	// 单条上限 1 MiB：超限必须报错而非静默截断。
	if err := ca.Send(context.Background(), make([]byte, 1<<20+1)); err == nil {
		t.Fatal("超限消息应报错（fail-closed）")
	}
	// Close 后 Receive 返回 xfer.ErrConnClosed。
	_ = cb.Close()
	if _, err := cb.Receive(context.Background()); !errors.Is(err, xfer.ErrConnClosed) {
		t.Fatalf("关闭后 Receive 应返回 ErrConnClosed, got %v", err)
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -run 'TestDeriveRemoteStaticKey|TestFromNetConn' ./pkg/tunnel/ ./pkg/tunnel/xfer/builtin/`
预期：FAIL — `undefined: DeriveRemoteStaticKey`、`undefined: builtin.FromNetConn`。

- [ ] **步骤 3：编写最少实现代码**

**(3a)** `pkg/tunnel/xfer/internal/tcp/tcp.go`（放在 `tcpConn` 定义之后）：

```go
// FromNetConn 把一个已建立的 net.Conn 包装为 xfer.Conn（4B 大端长度前缀帧定界）。
// 复用 tcpConn 的全部语义：Send 并发锁 + 写超时兜底、Receive 逐帧读与超长拒收、
// Close 幂等、单条上限 maxMessageBytes(1 MiB)。
//
// 用途（Y 一期 AD-6）：mesh 数据面（webrtc 直连 / hub 中继）交付的是字节流 net.Conn，
// 上层 mux 需要消息语义的 xfer.Conn。
func FromNetConn(conn net.Conn) xfer.Conn { return &tcpConn{conn: conn} }
```

**(3b)** `pkg/tunnel/xfer/builtin/builtin.go`：

```go
// FromNetConn 是 internal/tcp.FromNetConn 的对外桥。
//
// 与 SetDefaultTLSConfig 同理：internal/tcp 仅能被 import 路径以 pkg/tunnel/xfer
// 为根的包引用，pkg/server（Y 一期远程只读 listener）无法直接调用，故经本包暴露。
func FromNetConn(conn net.Conn) xfer.Conn { return tcp.FromNetConn(conn) }
```

（需 import `"net"` 与 `"github.com/cocomhub/sproxy/pkg/tunnel/xfer"`。）

**(3c)** `pkg/tunnel/remote_key.go`（新建）：

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"crypto/sha256"
	"fmt"

	"golang.org/x/crypto/hkdf"
)

const (
	// remoteReadIKMPrefix 是远程只读静态密钥 HKDF 的域分离 IKM 前缀。
	remoteReadIKMPrefix = "sproxy-remote-read/v1|"
	// remoteReadStaticInfo 是该 HKDF 的 info（与 ecdhInfo/ecdhInfoStatic 区分）。
	remoteReadStaticInfo = "sproxy-remote-read/static-key"
)

// DeriveRemoteStaticKey 由 **listener 自己的 Ed25519 身份指纹**确定性派生远程只读
// 隧道的静态密钥；两端（A 从 pin 配置、B 从自己的身份）算出同一结果，零额外配置。
//
// 规格修正（见计划「修正 1」）：规格 AD-3 原式取 min/max(nodeA,nodeB) 为 IKM，但 B 在
// 握手完成前无法得知对端 node-id（静态密钥是握手的输入，对端身份是握手的输出），构成
// 循环依赖。改用 listener 自己的身份指纹——它正是 A 侧已经 pin 的信任锚。
//
// 安全论证：该密钥在 deriveSessionKey 中**只作 HKDF salt**（ecdh.go:204-207），salt 不
// 需要保密；机密性来自 ECDH sharedSecret（前向保密），真实性来自 AD-2 的双向 Ed25519
// 指纹 pin（未配 pin 即拒绝）。此值提供域分离，并把远程读的会话密钥与其它隧道用途隔开。
//
// 返回 nil 的情况在 tcp.Tunnel 中意味着「明文模式」，即静默降级为不加密——因此此处
// 对不可达错误 panic（fail-closed），绝不返回 nil。
func DeriveRemoteStaticKey(listenerFingerprint string) []byte {
	if listenerFingerprint == "" {
		panic("tunnel: DeriveRemoteStaticKey 需要非空 listener 指纹（fail-closed）")
	}
	ikm := []byte(remoteReadIKMPrefix + listenerFingerprint)
	key, err := hkdf.Key(sha256.New, ikm, nil, remoteReadStaticInfo, sessionKeyLen)
	if err != nil {
		// 不可达：hkdf.Key 仅在 keyLen > 255*hashLen 时报错，sessionKeyLen=32 恒合法。
		panic(fmt.Sprintf("tunnel: 派生远程只读静态密钥失败: %v", err))
	}
	return key
}
```

**(3d)** `pkg/tunnel/tunnel_mux.go`：

1. `:21` 常量改名并加注：
```go
// defaultHandshakeTimeout 是隧道握手的默认超时；可经 WithHandshakeTimeout 覆写。
const defaultHandshakeTimeout = 30 * time.Second
```
2. `Tunnel` struct 加字段：
```go
	// handshakeTimeout 是本次隧道握手的超时（WithHandshakeTimeout 覆写，默认 30s）。
	handshakeTimeout time.Duration
```
3. `NewTunnel` 初始化 `handshakeTimeout: defaultHandshakeTimeout`。
4. 新增 option（放在 `WithPeerFingerprints` 之后）：
```go
// WithHandshakeTimeout 覆写本次隧道握手的超时（<=0 时忽略，保持默认 30s）。
// dialer 侧作用于 ensureHandshake，listener 侧作用于 Serve 进入 accept 循环前的握手。
func WithHandshakeTimeout(d time.Duration) TunnelOption {
	return func(t *Tunnel) {
		if d > 0 {
			t.handshakeTimeout = d
		}
	}
}
```
5. `:104` 与 `:288` 的 `handshakeTimeout` 改为 `t.handshakeTimeout`。

**(3e)** 确认无遗漏引用：

运行：`grep -rn "handshakeTimeout" pkg/tunnel/*.go`
预期：仅 `tunnel_mux.go` 的 `defaultHandshakeTimeout` 定义与 `t.handshakeTimeout` 使用（`pkg/tunnel/xfer/internal/tcp/tcp_tls.go` 的同名常量属另一包，不受影响）。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -race ./pkg/tunnel/ ./pkg/tunnel/xfer/builtin/ ./pkg/tunnel/xfer/internal/tcp/`
预期：PASS。

- [ ] **步骤 5：零回归 —— 隧道既有测试全绿**

运行：`go test -count=1 -race ./pkg/tunnel/... ./pkg/client/`
预期：PASS（默认握手超时未变；`WithHandshakeTimeout` 为加性选项）。

- [ ] **步骤 6：Commit**

```bash
git add pkg/tunnel/xfer/internal/tcp/tcp.go pkg/tunnel/xfer/builtin/builtin.go \
        pkg/tunnel/remote_key.go pkg/tunnel/tunnel_mux.go \
        pkg/tunnel/remote_key_test.go pkg/tunnel/tunnel_mux_timeout_test.go \
        pkg/tunnel/xfer/builtin/builtin_test.go
git commit -m "feat(tunnel): 传输原语——net.Conn→xfer.Conn 桥 + 远程读静态密钥派生 + 可配握手超时

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

## 任务 5：B 侧 loopback listener + `remote_read` 配置 + 装配（Y-C）

**文件：**
- 修改：`pkg/server/config.go`（`RemoteReadConfig` + `Config.RemoteRead` + `Default()` + `SetDefaults()` + `Validate()` loopback 强制）
- 创建：`pkg/server/remote_read_listener.go`
- 修改：`cmd/sproxy/root.go`（按 `remote_read.enabled` 起监听 + 优雅关闭）
- 测试：`pkg/server/remote_read_listener_test.go`（新建）、`pkg/server/config_remote_read_test.go`（新建）

**职责：** B 侧把只读面挂到强制 loopback 的 listener 上，每连接建 `mux` + `Tunnel`（真握手、真加密、双向 pin），并把可读性交给外部 mesh 服务宣告。

- [ ] **步骤 1：编写失败的测试**

**(1a)** `pkg/server/config_remote_read_test.go`：默认值（`Enabled=false`/`Listen=127.0.0.1:19000`/`HandshakeTimeout=10s`）、非 loopback 拒绝（`0.0.0.0:19000`、`:19000`、`192.168.1.1:1`）、`listen` 为空拒绝、`handshake_timeout <= 0` 拒绝、**`enabled=true` 但无任何 `mesh_readers` 时拒绝**（fail-closed 防「无 pin 接受任意对端」）、未启用时不校验 `listen`。

**(1b)** `pkg/server/remote_read_listener_test.go` —— in-process 双端（规格 §9「中测」）：

```go
// TestRemoteRead_DualEnd_ListStatDownload 在 loopback 上起真 listener，A 侧建真 mux+Tunnel
// （真握手、真加密、真 pin），跑 list/stat/download 全链路，断言下载字节 SHA-256 与 B 磁盘全等。
func TestRemoteRead_DualEnd_ListStatDownload(t *testing.T) {
	bID, err := tunnel.GenerateIdentity()
	if err != nil { t.Fatal(err) }
	aID, err := tunnel.GenerateIdentity()
	if err != nil { t.Fatal(err) }

	dir := t.TempDir()
	// A 的身份写入文件，供 B 的 mesh_readers 配置 pin（指纹取自同一 identity）。
	aFP := aID.Fingerprint()
	bFP := bID.Fingerprint()

	cfg := Default()
	cfg.StorageRoot = filepath.Join(dir, "vol-main")
	cfg.Hub.XferIdentityFile = filepath.Join(dir, "b-identity.json")
	if err := tunnel.SaveIdentity(bID, cfg.Hub.XferIdentityFile); err != nil { t.Fatal(err) }
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot, ACL: &VolumeACLConfig{
		Mode: VolumeACLAllow, Owners: []string{"alice"},
		MeshReaders: []VolumeMeshReaderConfig{{Node: "nodeA", Fingerprint: aFP, Owner: "alice"}},
	}}}
	cfg.RemoteRead.Enabled = true
	cfg.RemoteRead.Listen = "127.0.0.1:0" // 端口由 OS 分配，测试读 Addr()
	cfg.RemoteRead.HandshakeTimeout = 10 * time.Second
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil { t.Fatalf("cfg: %v", err) }

	body := bytes.Repeat([]byte("remote-read-"), 5000)
	rel := filepath.Join("alice", "user", "docs", "a.bin")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(cfg.StorageRoot, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StorageRoot, rel), body, 0o644); err != nil {
		t.Fatal(err)
	}

	// 复用 T3 的同包辅助（remote_read_test.go），避免重复装配。
	h := newRemoteReadHandlers(t, cfg, &bytes.Buffer{})
	ln, err := StartRemoteReadListener(t.Context(), cfg, h, testutil.DiscardLogger())
	if err != nil { t.Fatalf("StartRemoteReadListener: %v", err) }
	defer ln.Close()

	// A 侧：真 mesh-less 直连（loopback TCP）→ xfer 桥 → mux → Tunnel（双向 pin）
	conn, err := net.Dial("tcp", ln.Addr())
	if err != nil { t.Fatal(err) }
	defer conn.Close()
	m := mux.New(builtin.FromNetConn(conn), mux.RoleDialer)
	defer m.Close()
	tun := tunnel.NewTunnel(m, tunnel.DeriveRemoteStaticKey(bFP),
		tunnel.WithIdentity(aID), tunnel.WithPeerFingerprints([]string{bFP}))

	// list
	req, _ := http.NewRequest(http.MethodGet, "/remote/list?volume=main&path=/docs", nil)
	resp, err := tun.Do(req)
	if err != nil { t.Fatalf("list Do: %v", err) }
	if resp.StatusCode != http.StatusOK { t.Fatalf("list 状态 %d", resp.StatusCode) }
	// stat + download（Range）+ 内容 SHA-256 全等 …
}

// TestRemoteRead_DualEnd_WrongPinRejected 反向对照：A pin 一个错误指纹 → 握手失败，
// Do 返回错误且不返回任何数据。
func TestRemoteRead_DualEnd_WrongPinRejected(t *testing.T) { /* … */ }
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -run 'TestRemoteRead_|TestRemoteReadConfig' ./pkg/server/`
预期：FAIL — `undefined: RemoteReadConfig`、`undefined: StartRemoteReadListener`。

- [ ] **步骤 3：编写最少实现代码**

**(3a) 配置** — `pkg/server/config.go`：

```go
// RemoteReadConfig 是跨节点只读访问（Y 一期）的服务端配置。
type RemoteReadConfig struct {
	Enabled bool `yaml:"enabled" mapstructure:"enabled"`
	// Listen 是只读面监听地址；强制 loopback（配非 loopback 启动即失败，防被用作
	// 开放 mesh 中继——与既有网关安全边界同构）。
	Listen string `yaml:"listen" mapstructure:"listen"`
	// HandshakeTimeout 是隧道握手超时（透传 tunnel.WithHandshakeTimeout）。
	HandshakeTimeout time.Duration `yaml:"handshake_timeout" mapstructure:"handshake_timeout"`
}
```

`Config` 加字段（放在 `Hub` 附近）：`RemoteRead RemoteReadConfig \`yaml:"remote_read" mapstructure:"remote_read"\``

`Default()`：`RemoteRead: RemoteReadConfig{Enabled: false, Listen: "127.0.0.1:19000", HandshakeTimeout: 10 * time.Second}`

`SetDefaults()`：`Listen == ""` → `"127.0.0.1:19000"`；`HandshakeTimeout <= 0` → `10s`。

`Validate()`（放在 hub 校验段附近）：

```go
	if c.RemoteRead.Enabled {
		if c.RemoteRead.Listen == "" {
			return fmt.Errorf("remote_read.listen 不能为空")
		}
		host, _, err := net.SplitHostPort(c.RemoteRead.Listen)
		if err != nil {
			return fmt.Errorf("remote_read.listen 格式非法: %w", err)
		}
		if !isLoopbackHost(host) {
			return fmt.Errorf("remote_read.listen 必须绑定 loopback（远程访问应经 mesh 而非直连）: %q", c.RemoteRead.Listen)
		}
		if c.RemoteRead.HandshakeTimeout <= 0 {
			return fmt.Errorf("remote_read.handshake_timeout 必须为正")
		}
		if len(meshReaderFingerprints(c)) == 0 {
			return fmt.Errorf("remote_read.enabled 但未配置任何 volumes[].acl.mesh_readers —— 无 pin 将接受任意对端，拒绝启动（fail-closed）")
		}
	}
```

`isLoopbackHost` 收**裸 host**（`config.go:1059`），故必须先 `net.SplitHostPort` ✓。

**(3b) `pkg/server/remote_read_listener.go`**：

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin"
)

// RemoteReadListener 是 B 侧跨节点只读面的 loopback listener（Y 一期 AD-6）。
type RemoteReadListener struct {
	ln     net.Listener
	logger *slog.Logger
	cfg    *Config
	h      *Handlers
	wg     sync.WaitGroup
	closeOnce sync.Once
}

// Addr 返回实际监听地址（listen 端口为 0 时供测试读取真实端口）。
func (l *RemoteReadListener) Addr() string { return l.ln.Addr().String() }

// Close 停止接受新连接（已建立的连接由其自身生命周期收敛）。
func (l *RemoteReadListener) Close() error {
	var err error
	l.closeOnce.Do(func() { err = l.ln.Close() })
	l.wg.Wait()
	return err
}

// StartRemoteReadListener 按 cfg.RemoteRead 起只读面监听；未启用时返回 (nil, nil)。
//
// 装配要点：
//   - 监听强制 loopback（Validate 已保证；此处再以实际 Addr 断言，纵深防御）；
//   - B 侧身份复用既有服务端 xfer 身份（LoadXferIdentity，hub.xfer_identity_file），
//     A 侧 pin 的即该身份指纹——无新增配置键、无新增秘密；
//   - 静态密钥由 listener 自己的身份指纹派生（DeriveRemoteStaticKey，见计划修正 1）；
//   - 对端 pin 列表 = 全部卷 mesh_readers 指纹去重（任一被授权节点均可连入）。
func StartRemoteReadListener(ctx context.Context, cfg *Config, h *Handlers, log *slog.Logger) (*RemoteReadListener, error) {
	if cfg == nil || !cfg.RemoteRead.Enabled {
		return nil, nil
	}
	if log == nil {
		log = slog.Default()
	}
	ln, err := net.Listen("tcp", cfg.RemoteRead.Listen)
	if err != nil {
		return nil, fmt.Errorf("remote_read 监听失败: %w", err)
	}
	if host, _, sErr := net.SplitHostPort(ln.Addr().String()); sErr != nil || !isLoopbackHost(host) {
		_ = ln.Close()
		return nil, fmt.Errorf("remote_read 拒绝启动：实际监听地址非 loopback (%s)", ln.Addr())
	}
	id, err := LoadXferIdentity(cfg)
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("remote_read 身份加载失败: %w", err)
	}
	pins := meshReaderFingerprints(cfg)
	if len(pins) == 0 {
		_ = ln.Close()
		return nil, fmt.Errorf("remote_read 拒绝启动：无任何 mesh_readers 指纹（fail-closed）")
	}

	l := &RemoteReadListener{ln: ln, logger: log, cfg: cfg, h: h}
	staticKey := tunnel.DeriveRemoteStaticKey(id.Fingerprint())
	log.Info("remote_read 只读面已启动", "listen", ln.Addr().String(), "fingerprint", id.Fingerprint(), "pinned_readers", len(pins))

	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		l.acceptLoop(ctx, id, staticKey, pins)
	}()
	return l, nil
}

func (l *RemoteReadListener) acceptLoop(ctx context.Context, id *tunnel.Identity, staticKey []byte, pins []string) {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				l.logger.Warn("remote_read accept 退出", "error", err)
			}
			return
		}
		l.wg.Add(1)
		go func(c net.Conn) {
			defer l.wg.Done()
			defer c.Close()
			m := mux.New(builtin.FromNetConn(c), mux.RoleListener)
			defer m.Close()
			tun := tunnel.NewTunnel(m, staticKey,
				tunnel.WithIdentity(id),
				tunnel.WithPeerFingerprints(pins),
				tunnel.WithHandshakeTimeout(l.cfg.RemoteRead.HandshakeTimeout),
			)
			handler := l.h.newRemoteReadHandler(tun)
			if sErr := tun.Serve(ctx, handler); sErr != nil {
				if ctx.Err() == nil {
					l.logger.Warn("remote_read 连接结束", "remote", c.RemoteAddr().String(), "error", sErr)
				}
			}
		}(conn)
	}
}

// meshReaderFingerprints 返回所有卷 mesh_readers 的指纹（归一化后去重）。
func meshReaderFingerprints(cfg *Config) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 4)
	for _, v := range cfg.Volumes {
		if v.ACL == nil {
			continue
		}
		for _, mr := range v.ACL.MeshReaders {
			fp := strings.ToLower(strings.TrimSpace(mr.Fingerprint))
			if norm, err := tunnel.ParseFingerprint(mr.Fingerprint); err == nil {
				fp = norm
			}
			if fp == "" {
				continue
			}
			if _, dup := seen[fp]; dup {
				continue
			}
			seen[fp] = struct{}{}
			out = append(out, fp)
		}
	}
	return out
}
```

**(3c) `cmd/sproxy/root.go` 接线** — 在 `RegisterRoutes` 返回 `h` 之后、`createHTTPServer` 之前（与 `startXferListener`（`:354`）同一段）：

```go
	rrLn, rrErr := server.StartRemoteReadListener(ctx, cfg, h, logger)
	if rrErr != nil {
		return fmt.Errorf("remote_read 启动失败: %w", rrErr)
	}
	if rrLn != nil {
		defer func() { _ = rrLn.Close() }()
	}
```

> 关闭顺序：`h.Close()` 先注册（`root.go:343`）、后执行；本 listener 后注册 → **先关闭**，先停 accept 再关卷根 ✓（与既有 teardown LIFO 注释一致）。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -race -run 'TestRemoteRead_DualEnd|TestRemoteReadConfig' ./pkg/server/`
预期：PASS（含错 pin 反向对照）。

- [ ] **步骤 5：Commit**

```bash
gofmt -l pkg/server cmd/sproxy
go build ./... && go vet ./pkg/server/ ./cmd/sproxy/
git add pkg/server/config.go pkg/server/remote_read_listener.go cmd/sproxy/root.go \
        pkg/server/remote_read_listener_test.go pkg/server/config_remote_read_test.go
git commit -m "feat(server): remote_read loopback listener（真握手双向 pin + 每连接 mux/Tunnel）

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

## 任务 6：`pkg/remote` 客户端（Y-C）

**文件：**
- 创建：`pkg/remote/route.go`（`Ref` 解析/格式化）
- 创建：`pkg/remote/client.go`（`Client` + 每节点 mux/Tunnel 缓存 + `List`/`Stat`/`Download`）
- 测试：`pkg/remote/route_test.go`、`pkg/remote/client_test.go`（新建）

**职责：** A 侧：`remote://<node>/<vol>/<path>` 解析、mesh 建链、mux+Tunnel 缓存（**一个 mux 一个 Tunnel，握手只跑一次**——照抄 `pkg/client/client.go:1193-1235` 的范式）、三个只读方法。

- [ ] **步骤 1：编写失败的测试**

**(1a)** `pkg/remote/route_test.go`：table-driven —— `remote://nodeB/main/docs/a.txt`、`remote://nodeB/main`（卷根）、`remote://nodeB`（缺卷 → 错误）、`remote:///main/x`（缺节点 → 错误）、非 `remote://` 前缀（→ 错误）、路径归一（`//docs//a.txt` → `/docs/a.txt`）、`Ref.String()` 往返一致。

**(1b)** `pkg/remote/client_test.go` —— 用 T5 的 `StartRemoteReadListener` 起真 B 侧，A 侧用 `remote.New(...)` 跑全链路：

```go
func TestClient_ListStatDownload_EndToEnd(t *testing.T) {
	// 复用 pkg/server 的 fixture（external test package remote_test 导入 pkg/server）
	// 断言：List 返回条目与 Volume=main；Stat 字段与磁盘一致；
	//       Download 落盘字节 SHA-256 与 B 磁盘原件全等。
}

func TestClient_WrongPeerFingerprint_FailsClosed(t *testing.T) {
	// pin 一个错误指纹 → List 返回错误，且无数据。
}

func TestClient_MissingPin_FailsClosed(t *testing.T) {
	// 未配 pin → New/首次调用即报错（不 TOFU）。
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 ./pkg/remote/`
预期：FAIL — `undefined: ParseRef`（包不存在）。

- [ ] **步骤 3：编写最少实现代码**

**(3a)** `pkg/remote/route.go`：

```go
// Package remote 提供跨节点只读卷访问的客户端（Y 一期）：把 remote://<node>/<vol>/<path>
// 句柄解析为 mesh 链路之上的一次 HTTP-over-tunnel 请求。
package remote

const scheme = "remote://"

// Ref 是 remote://<node>/<vol>/<path> 句柄。
type Ref struct {
	Node   string
	Volume string
	Path   string // owner user 桶内相对路径，以 "/" 开头；空串 = 卷根
}

// ParseRef 解析句柄；节点与卷必填，path 可省略。
func ParseRef(s string) (Ref, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), scheme)
	if !ok {
		return Ref{}, fmt.Errorf("remote: 句柄须以 %q 开头: %q", scheme, s)
	}
	parts := strings.SplitN(rest, "/", 3)
	node := strings.TrimSpace(parts[0])
	if node == "" {
		return Ref{}, fmt.Errorf("remote: 缺少节点 ID: %q", s)
	}
	vol := ""
	if len(parts) > 1 {
		vol = strings.TrimSpace(parts[1])
	}
	if vol == "" {
		return Ref{}, fmt.Errorf("remote: 缺少卷名（格式 remote://<node>/<vol>/<path>）: %q", s)
	}
	p := ""
	if len(parts) > 2 {
		p = "/" + strings.Trim(parts[2], "/")
		if p == "/" {
			p = ""
		}
	}
	return Ref{Node: node, Volume: vol, Path: p}, nil
}

func (r Ref) String() string { return scheme + r.Node + "/" + r.Volume + r.Path }
```

**(3b)** `pkg/remote/client.go`：

```go
// Client 是跨节点只读客户端：持一个已指向 hub 的 *client.FileClient（服务发现 +
// 中继拨号）与 mesh 信令器，按节点缓存 mux+Tunnel。
type Client struct {
	svc       *client.FileClient
	signaler  *hub.HubSignaler
	identity  *tunnel.Identity
	peerPins  map[string][]string // node → 指纹 pin 列表
	transport string              // auto | relay | webrtc

	mu       sync.Mutex
	links    map[string]*nodeLink // node → 已建链路（mux + tunnel）
}
```

要点（实现要点，非完整代码——implementer 照抄 `pkg/client/client.go:1193-1246` 的缓存/失效范式）：

1. `New(svc, opts...)`；`WithIdentity`、`WithSignaler`、`WithPeerPin(node, fp)`、`WithTransport(mode)`。
2. `link(ctx, node)`：加锁 → 复用未关闭且 `HandshakeErr() == nil` 的链路 → 否则
   - `target := &client.MeshService{Name: meshServiceVolread, Node: node}`（服务发现：`svc.MeshServices(ctx)` 找到 `node` 上名为 `volread` 的条目，取其 `Addr`）；
   - `transport` 为 `auto` 时 `mesh.Dial(ctx, svc, signaler, target, node)`；`relay` 时直接 `svc.RelayStream(ctx, node, target.Addr)`；`webrtc` 时要求 `signaler != nil` 并走 `mesh.WebRTCStream`（经 `mesh.DialDirect`/等价路径）——两条路径都必须产出 `net.Conn`；
   - `mux.New(builtin.FromNetConn(conn), mux.RoleDialer)` → `tunnel.NewTunnel(m, tunnel.DeriveRemoteStaticKey(pin), tunnel.WithIdentity(identity), tunnel.WithPeerFingerprints([]string{pin}))`；
   - 缓存并返回。
3. `List/Stat/Download`：构造 `GET|HEAD /remote/{list,stat,download}?volume=<v>&path=<p>`，`tun.Do(req)`，把 `resp.Body` 交给调用方/解码。
4. **fail-closed**：`peerPins[node]` 为空 → 立即返回错误（不 TOFU）；`identity == nil` → 错误。

常量：`const meshServiceVolread = "volread"`。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -race ./pkg/remote/`
预期：PASS。

- [ ] **步骤 5：Commit 并开 PR（Y-C）**

```bash
gofmt -l pkg/remote
go test -count=1 -race ./pkg/tunnel/... ./pkg/server/ ./pkg/remote/
go build ./...
git add pkg/remote/
git commit -m "feat(remote): 跨节点只读客户端——remote:// 句柄 + 每节点 mux/Tunnel + list/stat/download"
```

---

## 任务 7：sclient `--remote`（Y-D）

**文件：**
- 创建：`cmd/sclient/remote.go`（flags + `remote.Client` 装配）
- 修改：`cmd/sclient/list.go`、`download.go`、`meta.go`（接入 `--remote`）
- 修改：`pkg/client/config.go`（A 侧 `remote_read.peers[]` 配置 + 加载期指纹校验 + `config set` 支持）
- 修改：`cmd/sclient/config.go`（文案）
- 测试：`cmd/sclient/remote_test.go`、`pkg/client/config_remote_peers_test.go`（新建）

**职责：** A 侧 CLI 表面：`--remote <node>:<vol>`、`--peer-fingerprint`、`--remote-transport`，以及持久化的 `remote_read.peers` 配置。

- [ ] **步骤 1：编写失败的测试**

**(1a)** `pkg/client/config_remote_peers_test.go`：`remote_read.peers[]` 解析；非法指纹加载期响亮报错（与既有 `peer_fingerprints` 校验同构，`config.go:99-102`）；重复 node 报错。

**(1b)** `cmd/sclient/remote_test.go`：
- `parseRemoteFlag("nodeB:main")` → `ref{node:"nodeB", vol:"main"}`；`"nodeB"`（缺卷）→ 错误；`":main"` → 错误。
- `resolvePeerPin(node, flagFP, cfg)`：flag 优先；flag 为空时取 `cfg.RemoteRead.Peers[node]`；两者皆空 → **错误（fail-closed，不 TOFU）**，错误信息含「未配置对端指纹 pin」。
- 指纹规范化：纯 64 hex / 大写 / 带 `sha256:` 前缀归一为规范形。

**(1c)** e2e 用例在任务 8。

- [ ] **步骤 2：运行测试验证失败**

运行：`cd cmd/sclient && go test -count=1 -run TestRemote ./...` 与 `go test -count=1 -run TestRemotePeers ./pkg/client/`
预期：FAIL（函数未定义）。

- [ ] **步骤 3：编写最少实现代码**

**(3a)** `cmd/sclient/remote.go`：

```go
// addRemoteFlags 给支持跨节点只读的命令挂载 flags（Y 一期）。
func addRemoteFlags(cmd *cobra.Command) {
	cmd.Flags().String(flagRemote, "", "经 mesh 只读访问远端节点的卷（格式 <node>:<vol>，如 nodeB:main）")
	cmd.Flags().String(flagPeerFingerprint, "", "对端节点 Ed25519 身份指纹（sha256:<64 hex> 或纯 64 hex；未配置即拒绝，fail-closed）")
	cmd.Flags().String(flagRemoteTransport, "auto", "mesh 传输选择：auto|relay|webrtc（relay/webrtc 用于确定性验证两条路径）")
}
```

`newRemoteClient(cmd, factory, ios, node)` 装配顺序：
1. `svc, err := factory.NewClient(cmd)`（已带 `--server`/AK/SK/`hub_url`/`node_id`）；
2. 读 sclient 配置（`cfgSvc`/`client.LoadFromProvider`）取 `remote_read.peers`；
3. `identity, err := clientfactory.LoadIdentityOptional()`；nil → 报错（A 必须能证明「我是谁」）；
4. `pin` 解析（`--peer-fingerprint` > 配置 > **错误**）；
5. `signaler`：经 `mesh.AutoRegister`（照抄 `cmd/sclient/socks.go:117-136` 范式）——仅在 `transport != relay` 时尝试；失败则告警并回落 `relay`（`auto` 模式）。
6. `remote.New(svc, remote.WithIdentity(identity), remote.WithSignaler(signaler), remote.WithPeerPin(node, pin), remote.WithTransport(transport))`。

**(3b)** 三个命令的接入（每个 ~10 行）：

```go
	// list.go RunE 开头
	remoteFlag, _ := cmd.Flags().GetString(flagRemote)
	if remoteFlag != "" {
		return runRemoteList(cmd, factory, ios, st, remoteFlag)
	}
```

`runRemoteList`：`ref := remote.ParseRef("remote://" + remoteFlag + pathArg)` → `client.List(ctx, ref)` → 复用既有 `OutputFormatter.PrintFileList`（`cmd/sclient/output.go:57`）。`download` 与 `meta` 同构（`--remote` 时把远端内容落到 `--output` 路径 / 打印元信息）。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd cmd/sclient && go test -count=1 ./...` + `go test -count=1 ./pkg/client/`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
cd cmd/sclient && gofmt -l . && go build ./...
git add cmd/sclient/remote.go cmd/sclient/list.go cmd/sclient/download.go cmd/sclient/meta.go \
        cmd/sclient/config.go cmd/sclient/remote_test.go \
        pkg/client/config.go pkg/client/config_remote_peers_test.go
git commit -m "feat(sclient): --remote 跨节点只读访问（list/download/meta + peer pin 配置）"
```

---

## 任务 8：e2e 与文档（Y-D）

**文件：**
- 创建：`test/e2e_remote_read_test.go`（`//go:build e2e`）
- 修改：`README.md`、`CHANGELOG.md`、`docs/config.md`、`CLAUDE.md`（路由表 + 配置键 + mesh 章节）

**职责：** 真二进制端到端（两条路径 + 未授权 + 写拒绝 + 审计）、DoD 1–6 逐条核对、文档。

- [ ] **步骤 1：编写失败的 e2e**

`test/e2e_remote_read_test.go`（`//go:build e2e`，`package sproxy_test`），复用既有 harness：

| 用例 | 断言（**必须落到真实副作用**） |
|------|------|
| `TestE2E_RemoteRead_RelayPath` | hub（`startHubSPROXY`）+ B 的 sproxy（`remote_read` + 卷 + `mesh_readers` pin A）+ B 主机 `sclient mesh node --service volread:127.0.0.1:<port> --dial-allow`；A `sclient download --remote nodeB:main /docs/a.bin --remote-transport relay` → **磁盘落盘文件 SHA-256 与 B 的 storage 内原件全等**（`findFilesNamed` 定位 + `testutil.SHA256Hex` 比对） |
| `TestE2E_RemoteRead_WebRTCPath` | 同上但 `--remote-transport webrtc`；同 SHA-256 断言。环境不支持打洞时 `t.Skip` 并打印原因（**不伪绿**） |
| `TestE2E_RemoteRead_UnauthorizedNodeRejected` | 第三身份 C 不在 `mesh_readers` → CLI 非零退出且**未落盘任何文件**；`GET /api/audit`（签名）返回含 `action=mesh_read` + `result=denied` 的事件 |
| `TestE2E_RemoteRead_WriteMethodRejected` | 经隧道发 `POST /remote/list` → 405；**并断言 B 的 storage 内文件集合与请求前完全一致**（`findFilesPrefixed`/目录快照） |
| `TestE2E_RemoteRead_AuditAllow` | 成功读后 `GET /api/audit` 含 `action=mesh_read` + `mesh=nodeA` + `result=success` |

关键 harness 细节（implementer 必须处理）：

1. **B 侧身份指纹可预测**：B 的配置注入 `hub:\n  xfer_identity_file: <tmp>/b-identity.json`，测试在 B 启动后调 `tunnel.LoadIdentity(<path>)` + `.Fingerprint()` 取 pin 值（或用 `identity.fingerprint` 等价路径）。
2. **A 侧 pin**：写入 A 的 `sclient.yaml`（路径 = `<cliEnv.TmpDir>/sclient.yaml`，`cliPrefixFlags()` 已指向它）：
   ```yaml
   hub_url: <hubURL>
   remote_read:
     peers:
       - node: nodeB
         fingerprint: <sha256:...>
   ```
3. **B 的 storage 内容**：`startSPROXYImpl` 返回 `uploadsDir`，测试预先写入 `<uploadsDir>/alice/user/docs/a.bin`。
4. **审计 ring**：B 的 extraConfig 注入 `audit:\n  buffer_size: 200`（否则 `GET /api/audit` 无 ring）。
5. **`localMux` 覆盖测试**：若新增任何路由，`handlers_localmux_test.go:173 TestLocalMuxCoversAllTunnelRoutes` 必须同步——**Y 的远程面走独立 listener，不进 localMux/srvMux，故该测试应保持不变**（这是红线断言，若它失败说明实现越界）。

- [ ] **步骤 2：运行验证**

运行：`make test-e2e`（递归 `./test/...`；Windows 上约 3–6 分钟）
预期：全部 PASS（或 webrtc 用例按明确原因 skip）。

- [ ] **步骤 3：DoD 逐条核对**

| DoD | 落地位置 | 证据 |
|-----|----------|------|
| 1 授权矩阵（含 fail-closed） | T1 单测 + T3 单测 | `go test -run 'TestAuthorizeMeshRead_Matrix\|TestRemoteRead_AuthorizationMatrix'` |
| 2 双路径端到端 + SHA-256 全等 | T8 e2e | `make test-e2e` 两条路径用例 |
| 3 只读强制（白名单） | T3 单测 + T8 e2e | `TestRemoteRead_WriteMethodsRejected` |
| 4 本地读路径零回归 | 全程 | `git diff --stat HEAD -- pkg/server/list_handler.go pkg/server/download_handler.go` 为空 + 全量测试绿 |
| 5 审计 | T3 单测 + T8 e2e | `TestRemoteRead_AuditRecordsAllowAndDeny` + `/api/audit` 断言 |
| 6 大文件流式 | T3 单测 + T8 e2e | `TestRemoteRead_LargeFileStreamsWithoutBuffering`（6 MiB）+ 中测大文件 |

- [ ] **步骤 4：文档**

- `docs/config.md`：新增 `remote_read.{enabled,listen,handshake_timeout}` 行 + `volumes[].acl.mesh_readers[]` 说明（含指纹格式与 fail-closed 语义）；A 侧 `remote_read.peers[]`。
- `README.md`：「关键路由」章节**不新增**（远程面不在主 mux 上）——补「跨节点只读访问（Y）」小节，给运维最小示例（B 侧 config + `sclient mesh node --service volread:...` + A 侧 `sclient list --remote`）。
- `CHANGELOG.md`：`## [Unreleased]` → `### Added` 增条目（跨节点只读卷访问、`mesh_readers`、`remote_read`、sclient `--remote`）。
- `CLAUDE.md`（sproxy 子项目）：配置表补 3 个键 + `mesh_readers`；sclient 命令表补 `--remote`/`--peer-fingerprint`/`--remote-transport`；「mesh 内网穿透」章节补只读卷访问用法。

- [ ] **步骤 5：Commit 并开 PR（Y-D）**

```bash
make check-ci
git add test/e2e_remote_read_test.go README.md CHANGELOG.md docs/config.md CLAUDE.md
git commit -m "test(e2e)+docs: 跨节点只读访问端到端（双路径/未授权/写拒绝/审计）+ 文档"
```

---

## 收尾（全部任务后）

1. **整分支最终审查**（每块 PR 前各一次）：SDD 最强模型 code-reviewer，覆盖 `merge-base..HEAD`，对抗式，修复**全部**发现（含 Minor/建议级）。
2. **端到端实证清单**（人工/脚本）：A 侧 `list` / `meta` / `download`（含 `--chunked`）→ 对照 B 磁盘 SHA-256；错 pin → 拒连；未授权节点 → 404 + 审计 denied；写方法 → 405；`Range` 续传 → 206；大文件（>100 MiB）内存占用平稳。
3. **安全复核重点**：① A 未配 pin 必须拒绝（不 TOFU）；② B 无 `mesh_readers` 时**拒绝启动**；③ owner 不可由请求指定（构造恶意 `owner`/`actor` 参数断言无效）；④ 拒绝路径不泄露卷/文件存在性（404 而非 403/409）。
4. **收尾**：每块 Push 分支 → PR → CI（ubuntu+windows test、lint、build-all、e2e）→ squash 合并 → 清分支 → 更新 memory。

## 自检记录（writing-plans 自检）

- **规格覆盖**：§2 目标 6 条 → 只读访问（T3/T6）、`remote://` 显式寻址（T6/T7）、按节点授权（T1/T2）、密码学身份锚（T4/T5 双向 pin）、零新增传输（T4/T5 复用 mux+Tunnel）、零一致性负担（T3/T6 无缓存）、端到端加密（T5 静态密钥 + 既有 AES-256-GCM）。§2 DoD 1–6 → 见任务 8 步骤 3 对照表。AD-1（代理信任）→ T1/T3；AD-2（双向 pin fail-closed）→ T5 `WithPeerFingerprints` + A 侧未配 pin 报错（T7）；AD-3 → **修正 1**（T4）；AD-4（句柄 + 适配器）→ T6/T7；AD-5（`mesh_readers`）→ T1/T2；AD-6（传输）→ T4/T5；AD-7（白名单）→ T3；AD-8（受限 context 委派）→ T3；AD-9（零缓存）→ T3/T6；AD-10（审计）→ T3/T8。§5 配置模型 → T2（`mesh_readers`）+ T5（`remote_read`）+ T7（A 侧 peers）。§6 组件表 → T1–T7 逐文件对应（三处落点按「修正 1/2/3」调整）。§8 错误表 10 行 → T3 授权矩阵/写拒绝/穿越 + T5 loopback 强制 + T4 握手超时。§9 测试策略 → T1（矩阵）/T3（handler）/T5（中测双端 + 错 pin 反向对照）/T8（e2e 四场景）；**回归红线** → T2 步骤 5 + T3 步骤 5 + 任务 8 DoD 4。§11 未来缝 → 本计划不实现（写批次/目录/透明网关/用户级联邦/匿名 ECDH 均显式排除）。
- **规格缺口补足（规格未写、计划必须给答案的）**：
  1. **B 侧长时身份来源**：规格 §5 B 侧配置无身份键，但 AD-2 要求 A pin B。→ 复用既有服务端 xfer 身份（`LoadXferIdentity`，`hub.xfer_identity_file`），**零新增配置键、零新增秘密**（T5）。
  2. **`remote_read.listen` 端口探测**：测试需真实端口 → `RemoteReadListener.Addr()`（T5）。
  3. **两条路径的可测性**：`--remote-transport auto|relay|webrtc`，否则 DoD 2「两条路径」无法确定性断言（T7/T8）。
  4. **e2e 审计断言通道**：用 `GET /api/audit`（AuditRing）而非抓子进程日志，断言落在真实副作用上（T8）。
  5. **`sclient config set` 支持 `remote_read.peers`**：规格 §5 末句要求；列为 T7 步骤 3 的一部分（`node:fingerprint` 逗号分隔语法）。
- **占位符扫描**：无「待定/TODO」。T7/T8 的 scaffold 步骤标注「照抄既有范式（`socks.go:117-136` / `client.go:1193-1246`）」并给出精确行号与所需常量，不留空洞步骤。
- **类型一致性**：`volume.MeshReader`（T1）↔ `VolumeMeshReaderConfig`（T2）字段名 `Node/Fingerprint/Owner` 一致；`volume.AuthorizeMeshRead/MeshReaderFor`（T1）↔ T3 调用一致；`tunnel.DeriveRemoteStaticKey`（T4）↔ T5/T6 调用一致（**单参数**，见修正 1）；`builtin.FromNetConn`（T4）↔ T5/T6 调用一致；`RemoteReadConfig{Enabled,Listen,HandshakeTimeout}`（T5）↔ T8 文档键名一致；`remote.Ref{Node,Volume,Path}`（T6）↔ T7 CLI `--remote <node>:<vol>` 一致；拒绝码 404/400/405/401（T3）↔ T8 断言一致；`GET|HEAD /remote/{list,stat,download}`（T3）↔ T6 请求构造 ↔ T8 e2e 断言一致。
