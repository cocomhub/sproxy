# Secret 加密卷（Encrypted Volume）功能文档

> 本文档面向使用者：如何配置、寻址、使用加密卷（`type=secrets` + `type=secretdata`），以及安全模型与未来方向。
> 设计规格详述见 `docs/designs/2026-10-01-secret-volume.md`；历史决策与方案反复的 WHY 见
> `docs/designs/2026-10-01-secret-volume-decisions.md`；算法包文档 `pkg/cryptox/shardseal/README.md`、
> 卷包文档 `pkg/volume/secretdata/README.md`。

## 1. 能力总览

Secret 加密卷把「写入自动分块加密、读取自动解密」沉淀为通用能力，对上层文件读写透明。核心能力：

- **加密算法（shardseal）**：纯 Go 随机分块 + AES-256-GCM + scrypt 派生（仅依赖 `golang.org/x/crypto`）。
  每文件随机盐（32B）→ scrypt 派生文件密钥；每段随机 nonce（12B）。
- **流式分块加密（可 seek 源）**：可 seek 输入（multipart.File / `*os.File` / SectionReader 等）**逐块
  ReadAt + 流式哈希**，内存峰值 O(单块上限)（默认 ≤ block_policy.max，2^14 默认档下 ≤200MiB）；
  纯流不可 seek 回退读全文（功能不变，峰值 1× 文件，见 §3 `max_file_bytes`）。磁盘格式/meta/块布局
  与旧写路径逐字节同构（向后兼容）。
- **两类封装卷**：
  - `secrets` 卷（wrapper）：存加密密钥文件（secret 文件本身不加密），底层任意卷的 `secrets/` 目录。
  - `secretdata` 卷（wrapper）：写入自动分块加密、读取自动解密，底层任意卷，对上层透明。
- **匿名性（对底层存储隐藏结构）**：底层卷上所有可见名称（目录/分块/meta）都是加密随机的，
  不暴露原始文件名、路径层级、文件大小、内容指纹之外的元数据（详见 §7）。
- **冗余（实验性）**：可选 XOR parity（k-of-k+1）纠删，任一分块丢失/损坏可恢复（§3.6）。

## 2. 快速开始

在 `volumes[]` 下声明 **secrets 卷** + **secretdata 卷**。secretdata 卷通过
`extra.secret_url` 引用 secrets 卷读取密钥。**声明顺序无关**（2026-10-15）：装配按**服务级四步序**
进行——① Store 已装配卷集 → ② 先行补装「不依赖封装声明卷」的 secretdata 卷 → ③ 封装声明卷补装
（`enclosure.AttachAll`，嵌套 secrets/secretdata 链由此装配）→ ④ 后置补装「`secret_url` 指向封装声明卷」
的 secretdata 卷；故 secretdata 卷既可与被引用的 secrets 卷同处 config 且顺序任意，也可引用**嵌套**
（`extra.target: "<卷>/<子目录>"`）的 secrets 卷。

```yaml
volumes:
  - name: "default"            # 默认卷（首卷；本地），其余卷排后
    type: "local"
    root: "./storage"

  - name: "mykeys"             # type=secrets：密钥管理卷（底层卷 secrets/ 目录视图）
    type: "secrets"
    extra:
      target: "local"          # local（缺省）= 本地 `<root>/secrets/` 目录下存密钥
      root: "./storage/keys"   # 注意：**必须写在 extra 里**（顶层 root 只服务本地卷，外部卷忽略）

  - name: "vault"              # type=secretdata：透明加密封装卷（卷内文件加密落盘）
    type: "secretdata"
    extra:
      target: "local"          # 底层卷本地根（secretdata 卷自身物理落盘处）
      root: "./storage/vault"
      secret_url: "secrets://mykeys/datakey"  # 从 mykeys secrets 卷读密钥（必填）
      # algorithm: "shardseal/aes-256-gcm"    # KDF 档位（缺省 = 默认档 standard）
      # erasure: false                         # XOR parity（实验性，可选）
      # max_file_bytes: 0                     # 单文件上限（0 = 不限）

# 不声明 secrets 卷时：**per-owner 默认桶**（每个 owner 各自的
#   <storage_root>/<owner>/meta/secrets/，owner 首次使用时自动派生；在 meta 桶内 ⇒
#   密钥字节不计入该 owner 的用户用量/文件数）。
#   - 服务级声明（本示例这种 config volumes[] 写法）**没有 owner 上下文** ⇒
#     `secret_url` 必须指向**显式命名**的 secrets 卷（如 secrets://mykeys/datakey）；
#   - 经建卷 API（`POST /api/volumes/user`，SproxySig 身份）创建带 secret_url 的卷时，
#     可写 `secrets://default-secrets/<name>` → 解析到**该 owner 自己的**默认桶
#     （谁创建 = 谁的桶；与 `sclient secret create` / `/api/secrets` 同一份密钥）。
```

密钥文件（`secrets://mykeys/datakey`）需**手动放置**：在 secrets 卷底层 `<root>/secrets/` 目录放一个
64-hex 文本文件（如 `datakey`）。sclient 加密卷文件面已接线（上传/下载/改名/删文件/删目录，见 §8），
密钥文件由运维放置（`sclient trust login` 不会创建卷内密钥文件；本地工具 `sealfile` 只读该文件做
本地加密，不创建 secrets 卷密钥）。

配置完后重启 sproxy，`volumes[]` 即含 `vault`（加密卷），可经 `secretdata://vault/<path>` 读写（见 §4）。

### 2.1 本地加解密工具（`sclient sealfile`）

`sclient sealfile`（本地命令，不连服务端）用与 secretdata 卷**同一写内核**（`secretdata.NewFS`）
在本地加密文件，加密产物可直接作为 `type: secretdata` 卷根加载播放（详见 `docs/cli.md`「sealfile」）：

```bash
# 本地加密（视频自动关键帧分块，与 sproxy 同款）
sclient sealfile encrypt movie.mp4 --key ./datakey --output-dir ./sealed
# 产物目录作为 secretdata 卷根装配：extra.root=./sealed + secret_url 指向同一密钥文件
# 解密还原 / 查看产物信息
sclient sealfile decrypt ./sealed --key ./datakey -o movie.mp4.restored
sclient sealfile info ./sealed --key ./datakey
```

- 产物逻辑布局 `<owner>/user/<名>` 对齐服务器键空间（`Location.FSPath` 恒 owner 前缀）：
  `--owner` 默认 `anonymous`；**签名请求的 owner 即 AK 字符串**，产物要命中须用对应 AK。
- 密钥文件内容**原样字节**作 secret（与 `secret_url` 读取同一语义，不要求 hex）。
- 命令/配置段（`sealfile.key` / `sealfile.out_dir` / `sealfile.owner` 等）见 `docs/cli.md`。

## 3. 配置参考

### 3.1 secrets 卷（`type: "secrets"`）

| extra 键 | 默认 | 说明 |
|----------|------|------|
| `target` | `local` | 底层卷名；`local` = 本地 `<root>/secrets/` 目录。**嵌套封装（2026-10-06）**：`<卷名>/<新子目录>`（如 `main/videos`）引用已创建卷的**新空子目录**作封装根——子目录须当前不存在，同卷路径重叠互斥占用（见 §3.4）。**外部源底座（2026-10-19 更正）**：嵌套形态 `<外部卷>/<子目录>` 已支持（s3/baidupcs/pikpak/cluster 等外部源作封装底层，装配层接线 + 外部后端替身单测覆盖，暂无真实网盘 e2e，见 §9.1）；仅「外部卷**整卷**」（`target: <外部卷名>` 无 `/`）仍为预留片 |
| `root` | 无（必填） | `target=local` 时的本地根目录——**写在 `extra` 内**；它是**父目录**，`secrets/` 段由装配层拼接（密钥落 `<root>/secrets/<名>`） |

### 3.2 secretdata 卷（`type: "secretdata"`）

| extra 键 | 默认 | 说明 |
|----------|------|------|
| `target` | `local` | 底层卷根（secretdata 物理落盘处）。**嵌套封装（2026-10-06）**：`<卷名>/<新子目录>`（如 `main/videos`）引用已创建卷的新空子目录作封装根——MakeDir 创建空目录 + 可写封装子视图，密文落 `<卷>/<子目录>/`；子目录须不存在，互斥占用防重复选底层（§3.4）。**外部源底座（2026-10-19 更正）**：嵌套形态 `<外部卷>/<子目录>` 已支持（s3/baidupcs/pikpak/cluster 等外部源作封装底层，装配层接线 + 外部后端替身单测覆盖，暂无真实网盘 e2e，见 §9.1）；仅「外部卷**整卷**」（`target: <外部卷名>` 无 `/`）仍为预留片 |
| `root` | 卷根 | `target=local` 时的本地根 |
| `secret_url` | 必填 | `secrets://<卷>/<name>`，从 secrets 卷读密钥；引用的 secrets 卷须先装配 |
| `algorithm` | `shardseal/aes-256-gcm` | **KDF 阶段档位**，见 §3.3 |
| `block_policy` | `{mode: random, min: 1MiB, max: 200MiB}` | 分块策略（§3.4） |
| `meta_pad_bytes` | `min_block_size` | meta 加密落盘的 pad 目标基准（§3.4） |
| `temp_dir` | 系统临时随机目录 | **写路径**临时空间（0700）。读路径自 2026-10-18 起**不落任何临时文件**（按需解密）；启动时会清理该目录内 `decrypt-*` 明文残留（升级前崩溃遗留） |
| `preserve_mtime` | `false` | 默认 blob mtime 打散；`true` 才透传原始 mtime（§3.5） |
| `gc_interval` | `0`（禁用） | 后台孤儿/墓碑 GC 周期（§6；可选维护工具，默认关） |
| `max_file_bytes` | `0`（不限） | 单文件上限（文件级总闸；流式路径按声明 size 预检，§9） |
| `targets` | 空 | 多 local root 副本列表（write 复制全部 target、读主失败回退；§3.6） |
| `erasure` | `false` | XOR parity 基于（**实验性**，§3.7） |
| `dedup` | — | **预留、装配层不接线**（不解析 `extra.dedup`），内容去重为独立内容寻址子系统的未来能力 |

### 3.3 Algorithm 档位

`algorithm` 选 KDF 强度档位（scrypt 参数随 Algorithm 版本化；各档为**不同 AlgoVersion + 不同的
KDF 派生域**，同 secret+salt 不同档派生 key 不同，**跨档 fail-closed**）：

| 档位 | 算法名 | N | 单次派生 | 真实 scrypt 内存（RFC 7914） | 用途 |
|------|--------|-----|----------|------------------------------|------|
| standard（默认） | `shardseal/aes-256-gcm` | 2^14 | ~25ms | 16MiB | 生产默认（面对高熵密钥的性价比档） |
| high（保守） | `shardseal/aes-256-gcm-high` | 2^17 | ~202ms | 128MiB | 兼容对低熵/口令类 secret 的保守档 |
| low（测试/低配） | `shardseal/aes-256-gcm-low` | 2^12 | ~6ms | 4MiB | 测试/低配服务器 |

> 内存口径 = 真实 scrypt 内存，按 RFC 7914 = `128 × r × N` 字节，r=8/p=1 各档统一。
> **论证（为什么不担心档位）**：secretdata 的 secret 是 **256-bit 高熵随机**，暴力破解成本本就
> 不可枚举，KDF 档位（scrypt 参数）不影响高熵密钥的暴力破解——它只承担**域分离**与抗离线字典。
> 因而默认档 2^14 即「性能/防御均衡」；`high` 档仅供对低熵/口令类 secret 的保守兼容。

**mockkdf（测试档位，生产禁配）**：子包 `pkg/cryptox/shardseal/mockkdf` 提供测试/开发用的**轻量派生**
（HKDF-SHA256 ~µs，算法名 `shardseal/aes-256-gcm-mock`），供 secretdata 等组装层测试——算法可靠性
仍由 shardseal 真实档维护，mock 只轻量化派生、加密仍真实。**仅供测试/开发，生产配置不得使用。**

### 3.4 block_policy & meta_pad_bytes

`block_policy`：`{mode, min, max}` — `mode: random`（默认，1MiB-200MiB 随机分块）或 blocklet
细分 `blocklet_mode: fixed`（块内定长 blocklet，默认 64KB-4MB，支持随机访问只解目标段）；
`min`/`max` 覆盖块大小区间。空文件不支持（分块至少 1 个承载，见 §9）。

`meta_pad_bytes`：文件/目录 meta 明文加密前的 pad 目标基准（默认 = `min_block_size`）。目标 =
`meta_pad_bytes + rand(meta_pad_bytes)`，受统一格式 R 地板（8B 长度头，196B）约束，使 meta
blob 与底层分块大小分布**重叠**，底层难以凭文件大小区分 meta 与数据。

### 3.5 preserve_mtime

默认（`false`）底层 blob mtime = 原始 mtime + 随机偏移（0-48h），打散同文件分片的时间聚类特征；
逻辑层 Stat/ListDir **暴露** meta 密文内原始 mtime（UnixNano；文件条目有值、目录条目恒 0），
不受打散影响；`preserve_mtime: true` 才把原始 mtime 也透传到底层 blob。
**2026-10-18 勘误**：旧文案称「恒用 mtime 排序」与实现不符——列表**排序**是「目录优先 + 名称升序」，
且此前 `Entry.MTime` 根本没填（上层 `ModTime` 因此恒 0，已修）。

### 3.6 targets（多副本，实验性）

`targets: [<root1>, <root2>, ...]` 声明 extra 本地副本根列表：写路径把自包含容器复制到全部 target，
读取主 target 失败回退副本。跨外部卷（baidupcs/s3/webdav/嵌套）接线留后续片；当前仅多 local root。

### 3.7 erasure（XOR parity，实验性）

`erasure: true` 启用 k-of-k+1 纠错：对 ≥2 个数据分块按最大长度补零对齐后逐字节 XOR 生成 parity
段（独立加密分块文件），任一分块丢失/损坏可由其余分块 XOR 复原。纯 stdlib，**未生产验证**，默认关
闭。

### 3.8 嵌套封装（`target: <卷名>/<新子目录>`，2026-10-06 用户确认语义）

封装卷（secretdata/secrets）支持以「已创建卷 + **新空子目录**」作为底层根，**不新建底层卷**：

- **target 语法（用户面，保留兼容）**：`<卷名>/<子目录>`（如 `main/videos`，子目录可多级
  `a/b`）；必须含子目录。装配层把该语法**翻译**为装配侧声明词汇（`extra.source: "<卷>:<子目录>"`，
  见 `pkg/server/enclosure/nested.go`；`extra.wrapper` 是冗余键**不再书写**——处理层恒等于卷
  类型本身，装配侧缺省回落 v.Type，显式写出且不一致则 fail-closed）——**卷描述 Type 保持
  wrapper 类型**（enclosure 只是内部装配机制，不是对外卷类型；见 §3.9），持久化形态与用户面
  语法零变化（Web UI/sclient/config 输入不破），物理落点亦不变（`<基卷根>/<子目录>/…`）。
- **互斥占用**：同基底卷上两条**互不相关**声明的封印足迹不得重叠（父/子/同目录）→ 装配期
  fail-closed（`enclosure.ErrFootprintOverlap`；精确相等另有 `ErrConflictClaim`）。链内叠加
  （`outer: inner:` 逐层封装）**允许**重叠——外层变换的是内层已变换的字节，封印象限由最外层
  承担。同卷不同分支（`main/photos` vs `main/movies`）、跨卷互不影响。**不再有「子目录必须
  不存在」前置条件**：区域本就是既有数据的落点，同一区间可被声明唯一占有（重建/重启天然可复用）。
- **写保护（只读）**：源区间对**一切直连操作**关闭——写恒拒（`storage.ErrSealed`，HTTP 409，
  无配置开关，含「移入/移出封印区」），读默认按「不存在」处理（404 + 父目录列举过滤），可用
  `source_read_mode: readable` 放开只读透视。强制点是**公共底座**而非逐面补丁：本地源卷在
  `storage.Root`（HTTP/API/S3/上传/块写/版本/回收站/分享/预览/搜索索引全部经它），外部/远端
  源卷在后端 FS 的装配处（`enclosure.SealFS`）。唯一例外是 **S3 预签名直传**（客户端直连
  对象存储，绕过一切 FS 装饰器）——只能签发期按封印规则拦截（`presignSealedGuard`）。
- **键空间（阶段 2 迁移①起）**：封装卷与其它外部卷**同口径**——所有读写/删除/列举/转存/
  镜像/du 统一经 `volume.ResolveUserLocation().FSPath()`，即卷内键恒为
  `<owner>/user/<rel>`（物理落盘 `<底层卷>/<子目录>/<owner>/user/<rel>`）。此前过渡实现给
  嵌套封装卷开过 owner-less 例外（`user/<rel>`），导致 **cloud 转存写入的文件落错位置、
  列举/下载/删除不可见**（遗留①，已修）。注意：旧例外布局下已落盘的数据在新键空间下不可见
  （该能力未发布，无迁移包袱）。
- **历史路径零回归**：`target: local + extra.root`（config/CLI）为本地根整卷，不进互斥占用校验。
- **唯一可写声明形态 = wrapper 类型 + `extra.source`**：`type` 就是处理层名（`secretdata`/
  `secrets`），wrapper 缺省即卷类型本身。`type: enclosure` **不是可写卷类型**——配置校验
  （`Config.Validate`）与建卷 API（`POST /api/volumes/user`）均 fail-closed 拒绝并提示改用
  wrapper 类型 + source/target；`enclosure.TypeEnclosure` 的唯一用途是这些**拒绝判据的单一
  事实源**（配置校验 / 建卷 API / 存量卷恢复三处引用，无日志/诊断/构造用途）。
  wrapper 类型而**无** `extra.source` 的卷不是封装声明（如 `target: local` 的本地整卷形态），
  走按类型分派的常规构造。
- **覆盖（test）**：多层嵌套链由 `cmd/sproxy/secret_register_test.go` 钉住——
  `TestSetupSecretBackends_ConfigSecretdata_Assembled/multi_layer_nesting_chain`（**本地底座**：
  `secrets` over `secretdata` 的四层链）与同父用例的 `multi_layer_chain_over_external_source`
  （**外部底座**：`secrets` over `secretdata` over 外部卷——以测试注册的外部后端替身替代需要真实
  账号/网络的 baidupcs/s3；两者装配接线口径相同：外部源封印套在该后端 `FS()` 上、封装卷经
  `UnsealedSource` 取特权视图）。断言覆盖各层装配、写读往返、密文真落到**外部后端的物理目录**
  与「直连外部后端写封装区恒 `storage.ErrSealed`」。

### 3.9 阶段 2 迁移状态

嵌套封装（§3.8）**已完成迁移**到通用封装底座（**声明词汇**：`extra.source: "<源卷>:<子路径>"`
（`extra.wrapper` 仅为兼容读取的冗余键），见 `docs/config.md`、设计文档 `docs/designs/2026-10-11-enclosure-volume.md`）：
源目录封印（`storage.SealRule` 唯一事实源，写恒拒/读 hidden|readable）、`sync.PrefixFS` 区域
视图、`enclosure.AttachAll` 推迟装配、`quota.NewPoolWithParent` 容量继承。用户面 `target:` 语法
在**装配层翻译**为底座声明（config 装配 / 建卷 API / 重启恢复三入口共用
`pkg/server.TranslateNestedVolume`），持久化形态与物理落点均不变（无数据迁移）。

过渡层（`pkg/sync.SubFS`、`pkg/server.volume_links.go`、`pkg/volume/nested_target.go`、
`registry.Set.DelegatePool/IsDelegated`、逐面 `checkWrapperOccupiedWrite`/`occupiedGuardFS`/
`syncexec.WriteGuard`、`resolveNestedTargetFS`/`adoptNestedDir` 重启收养）**已全部删除**。
迁移清单与文件级映射见 `docs/archive/stage2-migration.md`；当前状态：

| 批次 | 内容 | 状态 |
|---|---|---|
| ① | 外部卷键空间统一 owner 维度（去掉嵌套封装 owner-less 例外；转存与读写列举同键，遗留①） | 已完成 |
| ③（部分） | 删除 `pkg/server/config_writer.go`：运行时用户卷持久化归一 `user_volume_store` 台账 | 已完成 |
| ② | 嵌套封装 `target:` → 封装装配（翻译层 + 运行期装配/拆除闭环；过渡文件与逐面 guard 全删） | 已完成 |
| ⑤ | enclosure **内部化**（方案甲）：嵌套封装卷的对外 `Type` 保持 wrapper 类型（secretdata/secrets/…），装配由**声明判据**（`enclosure.IsDeclared`）接管而非按类型分派——修正「type=enclosure」造成的 `SchemeOf` 无协议（cloud 转存/归档被拒）、`CategoryOf≠wrapper`（建卷表单/防环失据）、API/CLI 回显泄漏内部机制 | 已完成 |
| ④ | 遗留②「owner Scope 幻影释放」（cloud 转存只写目标卷 FS、不经路由 commit，删除时仍对 owner 全局 Scope 做未曾提交的释放） | 已完成（写入侧结算：转存写成功后按 `TargetQuotaScopeFor`/`TargetPhysChargeFor` 与删除侧**同账本同口径**差分入账） |
| ⑥ | enclosure **纯内部化**：`IsDeclared` 判据收敛为「wrapper 类型 + `extra.source`」（去 `type: enclosure` 分支与条件式 wrapper 回落）；`type: enclosure` 在 config 校验与建卷 API 处 fail-closed 拒绝；`ErrMissingSource`/`ErrMissingWrapper` 随判据收紧成为不可达死哨兵 → 移除 | 已完成 |
| ⑧ | **config 声明顺序限制解除**（2026-10-15）：装配序拆为四步（Store 卷集 → 先行补装 → `enclosure.AttachAll` → 后置补装），使 config 可同时声明「嵌套 secrets 卷」与「`secret_url` 指向它的 secretdata 卷」（旧序下密钥解析遇「源卷后端未装配」必然 boot fail）。判据取**静态声明**（`secretdataRefsEnclosureVolume`），不靠「试装失败」的错误文案重试 | 已完成 |
| ⑦ | **per-owner 默认 secrets 卷**（用户裁定 2026-10-15）：`default-secrets` 成为**每个 owner 各自的**默认桶（`<storage_root>/<owner>/meta/secrets/`——owner 租户根的 **meta 桶**内，免声明、首次使用自动派生、`/api/secrets` 恒作用于请求者桶 ⇒ per-owner 隔离）；删除「全局默认 secrets 卷 + 多全局卷确定性选取（`extra.default` 标记 / 基础卷优先 / 卷名升序）」整套模型与 `extra.wrapper` 冗余键书写；`secret_url` 双寻址：`secrets://default-secrets/<名>` → per-owner 桶（需请求 owner 上下文），其它卷名 → 显式声明的 secrets 卷（含多层嵌套链，如 secrets over secretdata over baidupcs） | 已完成 |

## 4. 寻址

```
secrets://<卷>/<name>           # secrets 卷内 secret 名（默认卷可省略：secrets://default/<name>）
secretdata://<卷>/<path>        # secretdata 卷内文件路径
```

- **默认 secrets 是 per-owner 的**（用户裁定 2026-10-15）：不再有全局单卷——每个 owner 各有
  一个同名默认卷，落**该 owner 租户根的 meta 桶内** `<storage_root>/<owner>/meta/secrets/`（免声明、
  首次使用时自动派生，MkdirAll 0700 + 密钥文件 0600）。放在 meta 桶内（与 `<owner>/meta/credentials.json`
  同族）使密钥字节按**服务端内部账本**口径归类：`stats`/`du` 与容量扫描只按功能桶分类，裸的未知段会
  被兜底算作该 owner 的**用户文件**（污染配额水位与文件计数），meta 桶则天然排除。`/api/secrets` 的创建/列表/导出/删除恒作用于
  **请求者 owner** 的桶 ⇒ 不同 owner 的同名 secret 互不可见（per-owner 隔离）。
  - 因此**没有「多个全局 secrets 卷选哪个」的模型**（`extra.default` 标记与「基础卷优先/卷名升序」
    的启发式选取已随本次重构移除）。
  - **显式自定义 secrets 卷并存**：`volumes[]` 里声明的 `type: secrets` 卷（本地 root，或经
    `extra.source`/`extra.target` 封装在任意已装配卷/子目录之上——含多层嵌套链，如
    `secrets` over `secretdata` over `baidupcs`）仍然支持，供跨 owner 共享密钥库/Federation 等场景；
    它们只是**不被默认寻址**（`/api/secrets` 恒用 per-owner 桶）。
  - **边界（owner 限定的封装声明可封印密钥桶 ⇒ fail-closed，不静默损坏）**：per-owner 桶位于
    owner 租户根的 **meta 桶内**，而 owner 限定封装声明的封印足迹覆盖该 owner 的**全部保留功能桶**
    （`storage.ReservedBucketNames()`：user/cloud/archive/chunk/version/**meta**/trash/backup）。
    故若显式把封装区声明到 `<owner>/meta/…`（覆盖到 `meta/secrets`），该 owner 的密钥桶会同被封印
    ⇒ `/api/secrets` 读写与该 owner 的 `secret_url`（per-owner 默认桶）密钥解析**全部 fail-closed**
    （拒绝而非腐坏；`errors.Is(err, storage.ErrSealed)`）。属稀有显式配置——需要封装的目录请避开 `meta/`。
- **`secret_url` 的两条寻址路径**（`pkg/volume/secretdata` 消费）：
  - `secrets://default-secrets/<name>`（亦接受 `secrets://default/<name>`）：per-owner 默认桶——
    需要**请求 owner 上下文**（带身份的建卷请求注入），装配期（boot / config 声明）无 owner ⇒
    fail-closed 并提示改用显式命名的 secrets 卷；
  - `secrets://<卷名>/<name>`：该**显式声明**的 secrets 卷（须先装配；`default-secrets` 之外的名字
    不与 per-owner 桶冲突）。
- 寻址由装配层 `registry.ResolveURL` 处理；`secretdata://` 全集 `sproxy 寻址`（`volumes[]` 装配时经
  registry 注入）。

## 5. 目录与移动语义

secretdata 卷的底层**没有 `data`/`meta` 顶层目录**，而是一个随机命名容器目录；容器**物理嵌套**
（子容器落在父容器目录内，2026-10-14 用户口径）⇒ **底层目录树与逻辑目录树同形**：

```
<secretdata根>/<randDir 5-30>/          # 逻辑根目录的容器（顶层唯一）
    目录 meta（q 标记，存 {name, parent_dir_id} 父引用）
    文件 meta（z 标记，存文件根信息）
    加密分块（无标记）
    <randDir 5-30>/                     # 逻辑子目录的容器（落在**父容器内**）
        目录 meta / 文件 meta / 加密分块
        …                               # 逐级嵌套，深度 = 逻辑目录深度
```

- **为什么嵌套**：扁平布局（所有容器都是卷根下的兄弟目录）在**存储上看不出父子关系**——备份/迁移/
  人工排查无法对应逻辑树。嵌套后底层目录树即逻辑树（名字仍全匿名）。
- **只支持嵌套布局（不提供兼容）**：加载侧**递归**收集容器（`dirContainers`）后校验
  **容器物理位置 == 逻辑父目录的容器位置**（`validateNestedContainers`）；旧**扁平**布局（所有容器都是
  卷根下的兄弟目录）⇒ **fail-closed 拒绝挂载**（报错提示重建该卷）。逻辑路径仍沿 `parent_dir_id`
  父引用重算（与物理位置解耦，但物理位置必须与之一致）。

- **父引用解耦**：目录 meta 不关心父目录物理在哪，逻辑路径沿 parent 链重算（根 → … → name）。
  目录间完全解耦（task10），父引用断裂 fail-closed 跳过该容器。
- **目录移动/改名 = 仅重写被移动目录的 `name`/`parent_dir_id` 引用（一个文件集）**，子树内任何
  目录/文件 meta 与分块零改动 → 校验上完全解耦。
- **文件移动（跨目录、basename 不变）**：文件 blob 自包含 → 物理复制分块 + 文件 meta 到目标容器
  （零改内容、哈希不变）→ 删源副本 → 索引迁移，重启后按新容器路径解析。文件改名（basename 变）仍
  走 delete+write。
- **空目录语义**：删目录内最后文件时连目录 meta + 注册一并删，并递归回收成空祖先目录；根容器恒
  保留（重启不复现已删的空目录）。

## 6. 删除与 GC（即时物理删 + GC 可选孤儿兜底）

- **删除默认即付物**：Delete 一次删完 meta + 全部分块（+ parity），**不再写墓碑**——删即释放，
  正确性不依赖后台 GC。顺序先删 meta 再删 chunks（meta 删失败则分块全在 → 重启可重建完整文件，
  无幽灵）。重启不复现、无残留。
- **GC 收敛为可选维护工具**：默认 `gc_interval` 缺省 0 不启动后台 GC；显式维护用 `fs.GC()`（仅远程
  卷/多进程共享场景由 config `gc_interval > 0` 启用），作孤儿兜底（磁盘既有墓碑 meta / 孤儿分块
  清理）。墓碑仅在「多进程共享卷 + 后台 GC 启用」场景保留（跨进程并发读保护）；已知 epoch 窗口作
  可选工具可接受，不再作为默认正确性依赖。
- 覆盖写（`secretdata`）原子化：新随机名先传成功后 switch 索引再删旧；中途失败删新留旧，旧数据完好。

## 7. 安全性

### 7.1 段边界保密（仅密文可见）

底层卷上每段 = `[R 固定长度随机首部（默认 128B）][8B 密文长][salt][nonce][密文+GCM tag]`，首部是每文件
随机字节，无格式指纹（magic/file 检测全随机；首部无 `meta` 特征）；R 段不参与校验。分块内置 blocklet
段边界**不明文**（type/off/len 只作 GCM AAD），段间 0-64B 随机 padding 间隙混淆边界，观察者只见随机
字节流、不可区分段。

### 7.2 匿名性三维

底层不可观察：
1. **文件名长度**：分块/file meta/目录 meta 三类文件名**同一分布 35-43 字符**（三段 9 字符 base62：加密 blob SHA-256 两窗口 + HMAC 组签，+ 随机段），
   标记字符（`-`/`_`/`@`）替换随机段内一个字符、不改变总长——无法凭长度区分类型。
2. **文件大小**：meta 明文加密后仅几百字节，padding 到分块大小范围（密文内 padding + 4B json 前缀，
   长度头与文件大小线性一致）→ meta 与分块大小同分布（≥1MiB，无 KB 级孤点）。
3. **文件首部**：固定长度随机首部 R——无格式指纹、magic 检测全随机，无法凭首部结构识别「这是加密卷
   文件」、更无法区分 meta 与分块。

另：目录名随机 5-30 `[A-Za-z0-9]`，生成时重掷规避 `meta`/`data`/`secret` 等结构词；分块文件名中的加密
blob SHA-256 窗口属「密文自校验层」——**密文 blob 已防内容，保留为命名关联**，不扩大内容风险；HMAC 组签
（中段）无密钥无法反推明文或做内容存在性探测。

### 7.3 meta 加密

文件 meta 只存文件根信息（basename + stat + chunks），文件 meta 明文 JSON 加密后落盘线上
`[4B jsonLen][metaJSON][rand]` → 整体 GCM 加密（meta 名三段哈希锚定 MetaBlob）；原始文件名 / size /
sha256 / 逻辑路径 / 目录层级**全部密文** → 底层卷零文件名/路径/元数据泄漏。GCM 认证 → 任何篡改（删
sha256、改 chunks、改 name/path）解密失败 fail-closed。

### 7.4 KDF 档位论证

见 §3.3：secret 是 256-bit 高熵随机，暴力破解不可枚举；KDF（scrypt 档）只做域分离与抗离线字典，不影
响高熵密钥的暴力破解成本。淘宝低档期（low）只影响派生速度，不弱化高熵密钥的安全性——跨档 fail-closed
保证不同档派生 key 不同，不会用错误 key 解出内容。

## 8. 未来方向与预留

> 完整预留全景见设计文档 §13；以下为使用者的演进路径（每项给状态）。

### 8.1 已实现但实验性 / 预留

| 能力 | 状态 | 演进路径 |
|------|------|----------|
| **Erasure（XOR parity）** | 已实现、**实验性**（`extra.erasure`），未生产验证，默认关 | 生产采用前需专项验证单块丢失/损坏恢复的可靠性；后续扩展 Reed-Solomon 跨块纠错（§8.3） |
| **乐观锁（base_version）** | 已实现写入 Meta.BaseVersion，多进程 `WriteFileIfVersion`/`CurrentVersion` 预留 API | 多进程覆盖写场景启用 CAS 校验；单进程写路径版本天然单调 |
| **GC（孤儿兜底）** | 已实现、**可选、默认关**（`gc_interval` 缺省 0） | 远程卷/多进程共享场景显式启用；仅作孤儿兜底，不参与默认正确性 |
| **Dedup（内容去重）** | **降级为预留**：不接线（装配层不解析 `extra.dedup`）、不实现整文件池 | 未来由独立内容寻址子系统承接（Meta.RefCount/BlobTypeRef 槽位已预留） |
| **墓碑 + GC** | 可选、默认关（Delete 默认即时物理删、弃墓碑） | 仅「多进程共享卷 + GC 启用」场景保留作跨进程并发读保护 |

### 8.2 未实现

- **压缩**（`Meta.Compressed` / `Compression`，独立注册表，改压缩算法不升级加密版本）
- **Reed-Solomon 纠删码**（GF(2^8) 手写 ~千行负担大；k-of-k+1 单块起步，奇偶系统多块纠错平滑演进）
- ~~CLI 接线加密卷文件面~~ **已接线（2026-10-13 收口）**：`pkg/files` 经
  `WriteTarget` 外部卷分支（#765 统一写内核）与外部卷对象接缝（Stat 明文 checksum / 列举 /
  改名 / 硬删）覆盖加密卷的文件面；e2e `TestE2E_CLI_Secretdata_CliFileOps` 钉住
  upload/list/meta/download/mv/delete 全链。**仍限**：`list` 未指定 `--volume` 时只列本地卷
  （外部/加密卷需显式 `--volume`，见下节已知限制）；文件**改名**已收口为「只重写 meta」（O(meta)，
  内容零改动，2026-10-13；旧文案「非原地改名」已过时）
- **内容寻址去重**（由独立内容寻址子系统承接；Dedup 预留的权威落点）

### 8.3 KDF 档位扩展

`algorithm` 新增档位即注册新 `AlgoVersion` + 派生域（`RegisterAlgorithm`），新增档 fail-closed、不破坏既有
档位 blob；解/解密按注册表升序试派生定位，secretdata 零改动。所有预留字段/类型位于密文内 + AAD 内，
`omitempty` 空余——**旧版本永远能安全读取新版本留下的空白**。

### 8.4 units 单位抽象

字节大小配置已统一 `pkg/units/sizex.ByteSize`（人类可读 `"1GiB"`/`"5GB"`/纯数字；YAML 与 viper 双解码），
替代裸 int64 字节字段。internal/size 逻辑保留不动，后续单独统一迁到本包。

## 9. 已知限制

- **加密卷文件面已接线，但有四条边界要知道**：
  ① 未指定卷的 `list` 只列**本地卷**（搜索索引不含外部卷）——浏览加密卷请显式指定卷
  （`sclient list vault:/`，或 `sclient cd vault:/` 后直接 `list`；见 `docs/cli.md`「卷感知工作目录」）；
  ② 文件**改名**（basename 变化）= **只重写 meta blob**（文件名只存在于加密 meta 内；分块名 = 密文
  内容哈希 ⇒ 内容零改动、不重加密；先写新 meta 成功再删旧 meta ⇒ 崩溃只留双份，不丢数据）；
  ③ **跨卷移动/复制**（`mv … --to-volume` / `cp` / `volume copy|move`）**已覆盖外部/加密卷**
  （2026-10-13）：复制段走统一写内核（配额双账本 / sidecar / 封印与上传同口径），移动 = 复制成功
  **之后**删源（外部卷经接缝硬删）；移动非原子（删源失败会报错并说明「已复制到目标，源删除失败」）。
  同一 rel 在**其它**卷已存在 → 409（AD-4；移动豁免源卷本身）；
  ④ 加密卷上**删除目录（`rmdir`）已接线**：经接缝**递归删除**（先子后父；卷侧新增显式「删空目录」
  能力——目录不参与文件索引，`Delete` 对目录键是 no-op），删除只落在该卷键空间，**绝不**牵连本地
  同名目录；服务端恒要求 `force=true` 确认。
  （mkdir / 列目录 / 上传 / 下载 / 删文件 / 改名 / 删目录 / 明文 checksum 均已可用。）
- **同卷复制（已接线：`cp` / `/api/volumes/copy`）**：`CopyFile` = 同容器**零 blob 写共享**（引用计数为
  「存在即引用」，删源或删副本都安全、重启天然恢复）／跨容器**字节拷贝**（内容零改动，不重新加密）；
  跨卷复制因目标卷独立密钥仍需重新加密。
  文档首个 secret 密钥需手工放置（无 CLI 交互创建）。
- **卷声明顺序无关（2026-10-13 实测修正；2026-10-15 扩展到嵌套 secrets 卷）**：补装在**全卷集装配
  完成之后**进行（secretdata 走 `MarkDeferred`，嵌套封装卷走 `enclosure.IsDeclared` 判据），四步序见 §2；
  故 `type: secretdata` 卷既可与被引用的 secrets 卷任意排序，也可让 `secret_url` 指向**嵌套** secrets
  卷。锚点：e2e `TestE2E_CLI_Secretdata_DeclaredBeforeSecrets`（平铺顺序无关）+ 单测
  `TestSetupSecretBackends_ConfigSecretdata_Assembled/config_secretdata_refs_nested_secrets`
  （config 声明「嵌套 secrets 卷 + 引用它的 secretdata 卷」，旧序必红）。
  **两种 target 形态**（2026-10-19 校正）：① **嵌套子目录** `target: "<源卷>/<子路径>"`——已支持，由装配层
  翻译为封装声明（`extra.source` + `enclosure.BuildBackend`，如「外部/远端卷的某个子目录作加密层底层」）；
  ② **独立整卷** `target: "<外部卷名>"`（无 `/`）——**未支持**，解析不到嵌套语法 ⇒ `resolveTargetFS` fail-closed（不静默降级为本地）。
  secrets 卷的嵌套封装 target 已支持（多层链如 secrets over secretdata 亦然）。
- **空文件不可写**（有意取舍）：分块存储要求至少一个数据分块（承载 meta 引用 + 内容哈希锚定），空
  文件 `EncryptShards` 报错 fail-closed。
- **随机访问粒度（2026-10-18 更正）**：底层实现 `RangeReader` 时（LocalFS/S3 等）`OpenRangeRead`
  **已按 blocklet 段局部读取**——只拉含目标区间的段密文 + 一次块头 salt（不再整块下载）；仅
  **未实现 `RangeReader` 的底层**才回退「整块下载 + 段内定位」。块内 blocklet 索引内建；`OpenRead`
  的可 seek 句柄经 `sync.RangeSeeker`（4MiB 窗口、跨窗续读）复用**同一**原语，无第二套实现。
- **流式写覆盖边界**：可 seek 源（multipart 上传 / `*os.File` / 转存本地产物 / sync / 备份恢复 /
  video-keyframe 块模式 / trusted 装饰器）均走流式；**纯流**（无 seek）与**加密同步 `io.Pipe`**
  回退整读；**带宽限速启用时**（`rateLimitReader` 包装剥 seek）也回退整读（功能不变，仅内存不省）。

### 9.1 生产加固审核（2026-10-18）已修 + 已登记残余

**本批已修（4 路独立子 agent 对抗审核 + 主管逐条复核，均为确定性用例先红后绿）**：
| 缺陷 | 性质 | 修法 |
|---|---|---|
| 覆盖写清理旧版本时 `chunkSetStillReferenced` **在锁外**遍历 `s.index` | **P1**：Go `fatal error: concurrent map iteration and map write`（不可 recover ⇒ 打崩服务） | 判定与删除回到锁内（与 `Delete` 路径一致）；确定性用例 `TestConcurrentOverwrite_NoIndexDataRace`（预置大索引拉长窗口 ⇒ 修复前必红） |
| 同卷**跨容器**移动逐 blob `Rename`（真 mv）且 meta 最后搬 | **P1**：崩在中途 ⇒ 源 meta 引用已被搬走的分块、目标分块无引用被 GC 清扫 ⇒ **单文件永久不可恢复** | 改为「**复制分块 → 搬/换 meta → 最后删源**」（与 §5 设计一致）；用例 `TestRename_CrossContainer_CrashAtAnyStep_NoDataLoss`（枚举每个崩溃点，修复前 k=1..8 全部「源与目标都不可读」） |
| **目录**跨父移动「先搬容器、后写 meta」 | **P1**：崩在两步之间 ⇒ 物理位置 ≠ meta 声明 ⇒ 加载侧严格校验 fail-closed ⇒ **整卷拒挂载 = 启动失败** | ① 顺序改为**先写 meta、后搬容器**（中间态信息不丢）；② 加载侧新增 `repairInterruptedDirMoves` **按 meta 声明位置确定性修复**（新序=完成、旧序遗留态=回滚），旧扁平布局仍 fail-closed（判据：物理父**不是已识别容器**）；用例 `TestRenameDir_InterruptedMove_CrashAtAnyStep_MountsAndKeepsData` |
| `Stat`/`ListDir` **不填 `Entry.MTime`** | **P2**：上层 `ModTime` 恒 0（`list --json`/`meta` 的 `mod_time` 为 0，并削弱同步比对） | 暴露 `metaEntry.mtime`（UnixNano）；用例 `TestStat_ListDir_ExposeLogicalMTime`（含重启后重载） |
| 内存写路径 `writeCtx` **漏填 size** | **P2**：`Usage()` 记账恒 +0（覆盖写还会误减） | 显式填 `size`（`commitEntry`/覆盖写清理按 `wc.size` 记账）。**注**：两条写路径对**调用方声明 size** 的口径仍**有意不同**（流式按 size 读、内存读全），已自陈并列为已登记残余（见本节末项） |
| 流式路径的 Erasure 分支传 `data=nil` | **P2**（当前不可达的**陷阱**） | 改为显式 fail-closed + 准确错误信息（放宽分流条件时不会静默写坏 parity） |
| 文档/注释漂移 6 处（README「mtime/模式」与「副本卷名」、§3.5「按 mtime 排序」、§8.2 旧「非原地改名」、`files_phys.go` 头注释自相矛盾、`writeFileStream` 的 keyframe 注释等） | **P2**：注释强于实现会误导维护者 | 逐条按实现勘误。**更正（2026-10-19）**：此前“keyframe 走上层 ReadAll 回退”的结论已被**证伪**——`shardseal` 的 keyframe parseOnce 经 io.ReaderAt 扫描、与流式逐块加密兼容，**上层无 ReadAll 回退分支**（`shardseal_stream_test.go:TestEncryptShardsReader_VideoKeyframeParity` 锁定），该条注释已一并修正 |

**已登记残余（如实记录，未修）**：
- ~~读放大~~ **已根治（2026-10-18，Step 1 + Step 2）**：读路径不再复制密文、也不再落明文——
  `openRead` 返回**按需解密、可 seek** 的句柄（`pkg/volume/secretdata/read_seeker.go`）：只解密
  「当前读位置所在的 blocklet」并缓存（内存上界 = 单 blocklet；legacy meta 无 blocklet 索引时
  退化为单块），**temp → 0**；底层实现 `RangeReader` 时只拉目标 blocklet 段的密文（最小下载），
  缺失块仍可走 parity 恢复。**语义变化（已文档化）**：整文件 SHA-256 由「服务首字节之前全量校验」
  改为「从头顺序读完时于 EOF 校验」——逐 blocklet 的 GCM 认证始终生效，且下载路径消费侧
  `meta.VerifyReadSeeker` 另有逐块校验层 ⇒ 端到端保证不减；任何 seek 使该顺序校验失效。
  顺带修复：parity 恢复结果现按 `ci.OrigSHA256` 校验（此前缺失多于一块时会静默返回垃圾 ✗）。
  **启动清理**：temp 内 `decrypt-*` 明文残留（升级前崩溃遗留）在 `NewFS` 时清除。
  已知残余：legacy meta（无 blocklet 索引）读时内存退化为单块；读句柄持 ctx 以保留审计 span 与
  取消语义（`containedctx` 门禁已按理由豁免）。
- ~~外部/加密卷账本不收敛~~ **已修复（2026-10-18）**：账本 → 真值收敛的唯一入口
  `correctLedgerToTruth` 此前只被 `adjustVolumePool` 调用，而后者只对出现在 `volumeBuckets` 里的卷
  执行；`reconcileVolumesFromDisk` 明确跳过 `RootDir == ""` 的卷（外部/加密卷无本地根）⇒ 这类卷的
  LedgerCounter **永不收敛** ✗。**修法**：在跳过分支里对这类卷**只**做账本真值收敛
  （`h.correctLedgerToTruth(v.Name)`；不做本地桶扫描——无本地根，扫描语义不适用；无真值报告者时是
  no-op ⇒ 未接线卷零回归）。回归用例只走**生产入口** `reconcileVolumesFromDisk`
  （既有用例走 `reconcileVolumes` 直接喂桶 ⇒ 绕过该门、掩盖了缺口），**变异验证**：撤销修复 ⇒ 必红。

- **卷非空却无可用根容器时挂载仍放行**：根目录 meta 缺失/损坏/父引用断裂 ⇒ 子容器父链断裂、索引
  近乎为空，而 `NewFS` 仍成功 ⇒ 用户看到「空卷」（静默空视图）。**未修**：与「密钥错误」在挂载期不可
  区分，且既有用例 `TestWrongSecretFails` 显式锁定「挂载放行、读时失败」的语义（便于运维诊断）⇒ 需
  产品口径（是否给「卷非空但根不可解析」单独的 fail-closed 判据）。
- **两条写路径对声明 size 的口径不同**：流式（可 seek）按 size 读、多余丢弃；内存（非 seek）读全、
  **忽略**声明 size（`TestWriteFile_NonSeekableReadsAll` 锁定）。**未修**：统一会破坏既有锁定场景。
