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

在 `volumes[]` 下声明 **secrets 卷（先）** + **secretdata 卷（后）**。secretdata 卷通过
`extra.secret_url` 引用已存在的 secrets 卷读取密钥（依赖顺序：secretdata 引用的 secrets 卷必须先装配）。

```yaml
volumes:
  - name: "default"            # 默认卷（首卷；本地），其余卷排后
    type: "local"
    root: "./storage"

  - name: "mykeys"             # type=secrets：密钥管理卷（底层卷 secrets/ 目录视图）
    type: "secrets"
    root: "./storage/keys"     # extra.target=local（缺省）→ <root>/secrets/ 下存密钥
    extra:
      target: "local"

  - name: "vault"              # type=secretdata：透明加密封装卷（卷内文件加密落盘）
    type: "secretdata"
    root: "./storage/vault"
    extra:
      target: "local"          # 底层卷本地根（secretdata 卷自身物理落盘处）
      secret_url: "secrets://mykeys/datakey"  # 从 mykeys secrets 卷读密钥（必填）
      # algorithm: "shardseal/aes-256-gcm"    # KDF 档位（缺省 = 默认档 standard）
      # erasure: false                         # XOR parity（实验性，可选）
      # max_file_bytes: 0                     # 单文件上限（0 = 不限）

# 只用默认 secrets 卷也可（不配 secrets 卷）：
#   secret_url 写 "secrets://default/<name>"，默认卷自动落 <storage_root>/secrets/。
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
| `target` | `local` | 底层卷名；`local` = 本地 `<root>/secrets/` 目录。外部 target（baidupcs/s3/webdav/加密卷嵌套）为预留片，当前仅 local 子集可用 |
| `root` | 卷根 | `target=local` 时的本地根目录 |

### 3.2 secretdata 卷（`type: "secretdata"`）

| extra 键 | 默认 | 说明 |
|----------|------|------|
| `target` | `local` | 底层卷根（secretdata 物理落盘处）。外部 target（baidupcs/s3/webdav/嵌套）为预留片 |
| `root` | 卷根 | `target=local` 时的本地根 |
| `secret_url` | 必填 | `secrets://<卷>/<name>`，从 secrets 卷读密钥；引用的 secrets 卷须先装配 |
| `algorithm` | `shardseal/aes-256-gcm` | **KDF 阶段档位**，见 §3.3 |
| `block_policy` | `{mode: random, min: 1MiB, max: 200MiB}` | 分块策略（§3.4） |
| `meta_pad_bytes` | `min_block_size` | meta 加密落盘的 pad 目标基准（§3.4） |
| `temp_dir` | 系统临时随机目录 | 本地临时空间（0700） |
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
逻辑层 Stat/ListDir 恒用 meta 密文内原始 mtime 排序，不受打散影响。`preserve_mtime: true` 才透传
原始 mtime（展示按时间排序用）。

### 3.6 targets（多副本，实验性）

`targets: [<root1>, <root2>, ...]` 声明 extra 本地副本根列表：写路径把自包含容器复制到全部 target，
读取主 target 失败回退副本。跨外部卷（baidupcs/s3/webdav/嵌套）接线留后续片；当前仅多 local root。

### 3.7 erasure（XOR parity，实验性）

`erasure: true` 启用 k-of-k+1 纠错：对 ≥2 个数据分块按最大长度补零对齐后逐字节 XOR 生成 parity
段（独立加密分块文件），任一分块丢失/损坏可由其余分块 XOR 复原。纯 stdlib，**未生产验证**，默认关
闭。

## 4. 寻址

```
secrets://<卷>/<name>           # secrets 卷内 secret 名（默认卷可省略：secrets://default/<name>）
secretdata://<卷>/<path>        # secretdata 卷内文件路径
```

- 服务启动**默认创建一个本地卷作默认 secrets 卷**（`secrets://default/`，落 `<storage_root>/secrets/`）。
- 寻址由装配层 `registry.ResolveURL` 处理；`secretdata://` 全集 `sproxy 寻址`（`volumes[]` 装配时经
  registry 注入）。

## 5. 目录与移动语义

secretdata 卷的底层**没有 `data`/`meta` 顶层目录**，而是一个随机命名容器目录：

```
<secretdata根>/<randDir 5-30>/     # 一个逻辑目录 = 一个随机命名容器目录
    目录 meta（q 标记，存 {name, parent_dir_id} 父引用）
    文件 meta（z 标记，存文件根信息）
    加密分块（无标记）
```

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
  （外部/加密卷需显式 `--volume`，见下节已知限制）；文件**改名**非原地改名（见已知限制）
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
- **卷声明顺序无关（2026-10-13 实测修正）**：`setupSecretBackends` 在**全卷集装配完成之后**补装
  secretdata（`MarkDeferred`），故 `type: secretdata` 卷可以声明在 secrets 卷之前——e2e
  `TestE2E_CLI_Secretdata_DeclaredBeforeSecrets` 钉住（此前文档声称「必须先装配好」，已不成立）。
  **仍预留**：secrets 卷外部 target（嵌套封装）与 secretdata 外部 target（baidupcs/s3/webdav/嵌套），
  当前仅 `target=local` 可用（配置成外部 target 会 fail-closed 报错，不静默降级）。
- **空文件不可写**（有意取舍）：分块存储要求至少一个数据分块（承载 meta 引用 + 内容哈希锚定），空
  文件 `EncryptShards` 报错 fail-closed。
- **随机访问粒度**：`OpenRangeRead` 当前下载整块再定位 blocklet，未做逐 blocklet 只取所需段（注释
  已纠偏）；blocklet 索引已内建，逐段随机访问接线留后续。
- **流式写覆盖边界**：可 seek 源（multipart 上传 / `*os.File` / 转存本地产物 / sync / 备份恢复 /
  video-keyframe 块模式 / trusted 装饰器）均走流式；**纯流**（无 seek）与**加密同步 `io.Pipe`**
  回退整读；**带宽限速启用时**（`rateLimitReader` 包装剥 seek）也回退整读（功能不变，仅内存不省）。