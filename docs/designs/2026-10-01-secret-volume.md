# Secret 加密卷设计（shardseal：纯 Go 分块加密 + 透明卷间移动）

> 日期：2026-10-01
> 状态：设计定稿（初版实现）
> 关联：PikPak 中转站（pkg/volume/ext/pikpak）后续片——下载 → 分块加密 → 上传任意卷

## 1. 背景与目标

把「下载 → 分块加密 → 上传到任意卷」沉淀为**通用加密卷**能力：

- **secret_data 卷**：封装卷（wrapper），写入自动分块加密、读取自动解密，对上层透明。
- **secrets 卷**：特殊抽象卷，存 secret 文件（底层指向任意卷 `secrets/` 目录）。
- **纯 Go**：加密用 `golang.org/x/crypto` 官方库（项目允许，不进 ext）。
- **分块加密后上传**（非流式）：本地原始空间 + 1 个分块空间，**边加密边上传**（避免 2 倍空间）。
- **命名无特征**：加密分块自描述（嵌入校验和截断），不暴露原始名/长度规律。
- **目录名保密**：底层卷上**所有可见名称都是加密随机生成**的——目录名随机
  5-30、文件名三段 hex 同构；不出现 `meta`/`data` 等结构词；文件 meta 不存路径层级，
  逻辑目录信息放目录 meta 内，支持目录移动后整体依旧可解析。

## 2. 算法包：pkg/cryptox/shardseal

**shardseal**（分片封印）——随机分块 + AES-256-GCM + 自描述命名。

```
pkg/cryptox/shardseal/
├── shardseal.go   # 对外 API：EncryptFile/DecryptFile/命名/meta 生成解析
├── naming.go      # 命名规则（三段校验和截断 + 随机长度 rand + 标记）
├── block.go       # 分块规划接口 BlockPlanner + 默认 random
├── meta.go        # meta 结构体 + JSON 编解码
└── crypto.go      # AES-256-GCM + scrypt 派生（x/crypto）
```

依赖：仅 `golang.org/x/crypto`。

### 2.1 对外 API（供 secretdata 卷与测试）

```go
// EncryptShards 把本地文件加密为分块文件 + meta，返回分块与 meta 信息。
// srcFile: 原始文件路径；outDir: 加密分块临时目录；secret: 密钥
func EncryptShards(srcFile string, outDir string, secret []byte, policy BlockPolicy) (*EncryptionResult, error)

// DecryptFile 用 meta + 分块还原原始文件。
// meta: meta 内容；chunkDir: 分块所在目录；dstFile: 还原目标路径；secret: 密钥
func DecryptFile(meta *Meta, chunkDir string, dstFile string, secret []byte) error

// EncryptionResult 返回加密产物（分块文件名列表 + meta 文件名 + meta 内容）。
type EncryptionResult struct {
    ChunkNames []string
    MetaName   string
    Meta       *Meta
}
```

### 2.2 BlockPolicy（分块策略）

```go
type BlockPolicy struct {
    Mode string // "random" 默认 | "video-keyframe"（plugin 扩展）
    Min  int64  // 最小块大小（默认 1MB）
    Max  int64  // 最大块大小（默认 200MB）
}

// BlockPlanner 是可插拔分块规划接口。
type BlockPlanner interface {
    Plan(origSize int64) ([]Block, error)
}
```

## 3. 命名规则（三种文件同构 + 目录随机）

底层卷上所有名称**分三类，格式统一为三段 16hex + 随机段**，靠随机段内的
**标记字符**区分类型（扫描目录时按文件名分类）：

| 类型 | 随机段标记 | 识别 |
|------|-----------|------|
| 加密分块 | 无（`[A-Za-z0-9]`） | 无标记 |
| 文件 meta | 含 `-` 或 `_` | 含 `-`/`_` |
| 目录 meta | 含 `@` | 含 `@` |

**长度范围一致性**：三种文件名**长度同一分布**——三段 16hex = 48 字符
+ 两个随机段（各 3-7 字符）= **54-62 字符**。标记字符（`-`/`_`/`@`）是**替换**
随机段内的一个字符、**不增加长度**，三种文件名长度范围完全相同——底层无法凭名称
长度区分 meta 与分块（避免出现明显太长/太短的特征）。该约定由命名测试锁定（§3.5）。

三段 16hex 的语义（三种文件一致）：`{内容原始前16hex}{rand1}{原始总前16hex}{rand2}{内容加密后前16hex}`
——**原始 hex 关联 + 加密前后内容 hex 保证完整可靠**：第一段是明文内容校验和截断、
第二段是文件级原始总校验和（跨文件关联）、第三段是加密后内容校验和截断（密文完整校验）。

### 3.1 加密分块文件名

```
{块原始前16hex}{rand1}{原始总前16hex}{rand2}{块加密后前16hex}
```

- 三段各 16 hex；顺序：块原始 → 原始总 → 块加密后（**原始总在中间**，避免同源聚集）。
- rand1/rand2：长度 **3-7 随机**；字符集 `[A-Za-z0-9]`（无 `-` `_` `@`）。
- 无后缀。

### 3.2 文件 meta 文件名（保持三段 hex）

```
{meta原始前16hex}{rand1}{原始总前16hex}{rand2}{meta加密后前16hex}
```

- 与加密分块**格式同构**（视觉上混在一起，底层无法凭格式区分 meta 与数据）。
- **rand 必含 `-` 或 `_`**（文件 meta 识别标记，见上表）。
- meta 原始前16hex = meta 明文 JSON 的 SHA-256 前 16 hex；meta 加密后前16hex = meta
  密文的 SHA-256 前 16 hex（保证 meta 完整可靠）。
- **不做改名**（不引入 `emeta` 之类前缀——那会形成新的明显特征，破坏「与分块混在一起」）。

### 3.3 目录 meta 文件名（标记 `@`）

```
{dirMeta原始前16hex}{rand1}{原始总前16hex}{rand2}{dirMeta加密后前16hex}
```

- 格式与文件 meta 相同（三段 hex + 随机段），**rand 必含 `@`**（目录 meta 识别标记）。
- dirMeta 原始前16hex = 目录 meta 明文 JSON 哈希前 16；加密后前16hex = 目录 meta 密文哈希前 16；
  原始总前16hex = 目录标识（逻辑路径）哈希前 16。
- **放在目录 meta 所在目录内部**（见 §6.2 布局），随目录移动。

### 3.4 目录名（随机 5-30）

- 逻辑目录对应底层**随机命名容器目录**，长度 **5-30 随机**，字符集小写 `[a-z0-9]`
  （跨平台/底层 FS 兼容；无 `meta`/`data` 等结构词——随机生成时若命中保留词则重掷）。
- 目录名**与目录内文件内容无直接关联**（容器名不编码任何文件信息）。

### 3.5 长度范围与测试锁定

**约定（两部分，均须测试锁定，防后续修改破坏匿名性）**：

1. **文件名长度**：分块名、文件 meta 名、目录 meta 名三类文件名长度**同一分布**
   （54–62 字符，§3 表格下方）；标记字符只替换随机段内一个字符、不改变总长；
   任何一类文件名不得出现明显更长/更短的形态（如 `emeta-<hex>` 短名属违约）。
2. **文件内容大小**：meta（文件/目录）落盘大小与加密分块**同分布**（§4.2 padding），
   底层不得凭文件大小区分 meta 与分块。
3. **文件内容首部**：所有加密文件（分块/meta）首部为**固定长度随机字节**（§4.2 R 段），
   无固定格式指纹、magic 检测全为随机数据——底层不得凭首部结构识别文件类型。

**测试锁定点（落在 naming 与加密包测试）**：

- `TestNameLengthUniform`：批量生成三类文件名（覆盖随机段长度全范围 3–7），
  断言三者长度 min/max 完全一致（54–62），且随机段长度分布均匀、无偏好。
- `TestMetaPadLengthInRange`：meta 加密 + padding 后，断言长度 ∈ `[min_block_size, 2×min_block_size]`
  （默认 [1MiB, 2MiB]），且与最小分块大小的下限重叠；不同 `meta_pad_bytes` 配置下范围仍重叠。
- `TestUniformOnDiskFormat`：分块与 meta 落盘均带固定长度随机首部 R + 8B 长度头、格式同构；
  断言 R 段内容随机（非全 0/可识别字节）、长度头与文件大小线性一致
  （`文件大小 = R + 8 + 32 + 12 + len(密文)`，即 `len(密文) = 文件大小 − R − 52`，分块/meta 一致）。
- `TestMetaPadDecryptsExact`：padding 后的 meta 能精确解出**真实 JSON**（跳 R → 读长度头
  截取密文 → GCM 解密 → 读 jsonLen 截取 JSON），padding 字节不进入 JSON；篡改 R 段不影响
  解密（R 仅首部混淆，非完整性层）、篡改密文（含 padding，GCM 认证范围）必失败（fail-closed）。
- `TestNoMetaDataKeywords`：目录名生成样本不含 `meta`/`data`/`secret` 保留词（碰撞重掷生效）。

## 4. 分块与加密（1MB-200MB + 边加密边上传）

### 4.1 分块

- 块大小 **1MB-200MB 随机**；每文件随机分块序列（最后一块为剩余）。
- **边加密边上传**：读原始 → 切块 → 加密该块 → 立即上传 → 释放该块本地临时 → 下一块。峰值 = 原始 + 1 块。
  - **已知限制（Imp-2，2026-10-03 记录）**：当前写路径为「先 `io.ReadAll` 全文 → 分块加密」，
    采用内存明文变体（`EncryptShardsBytes`，峰值 1× 文件，不再写临时源 + 二次整读）。
    未做逐块流式（blocklet 双层规划 + meta 名三段锚定要求先有整文件内容哈希）。大文件内存
    防护由 `Options.MaxFileBytes` 上限兜底（读取全文前按 size 拦截，`ErrMaxFileBytes`）；
    部署侧按卷容量/内存设置 `extra.max_file_bytes`。完整流式留后续。
- 分块落盘格式与 meta 统一（§4.2）：`[R 随机首部][8B 密文长][salt][nonce][ciphertext+GCMtag]`，无 padding。

### 4.2 加密

- `AES-256-GCM`（x/crypto）。
- 每文件随机盐（32B）→ scrypt 从 secret 派生文件密钥。
- **KDF 强度档位（scrypt 参数随 Algorithm 版本化，2026-10-03 用户裁定）**：secretdata 的
  secret 是 **256-bit 高熵随机**（`secrets.go:72-76`），scrypt 的 `2^17`（~202ms）原为
  「为低熵口令设计的交互式登录档」，对高熵密钥纯属过度防御（KDF 档位**不影响**高熵密钥的
  暴力破解成本——256-bit 密钥本就不可枚举，KDF 只负责域分离与 SV 派生）。故注册 **high /
  standard / low** 三档（`pkg/cryptox/shardseal` init 注册；**内存口径 = 真实 scrypt
  内存 RFC 7914 128×r×N 字节**）：

  | 档位 | 名 | N | 实测耗时 / 真实内存 | 用途 |
  |------|-----|-----|------------|------|
  | standard（默认） | `shardseal/aes-256-gcm` | 2^14 | ~25ms / 16MiB | 生产默认（面对高熵密钥的性价比档） |
  | high（保守） | `...-high` | 2^17 | ~202ms / 128MiB | 兼容对低熵/口令类 secret 的保守档 |
  | low（测试/低配） | `...-low` | 2^12 | ~6ms / 4MiB | 测试（根治并行内存爆炸）/低配服务器 |

  各档为**不同 AlgoVersion + 不同 KDF 派生域**（standard `"shardseal/v1"`、high
  `"shardseal/v1-high"`、low `"shardseal/v1-low"`；high/standard/low = 1/2/3）——同
  secret+salt 不同档派生 key 不同，**跨档 fail-closed**；`Options.Algorithm` 传档位名切换
  （零绑定，装配层解析），缺省 = server 档。r=8/p=1 各档统一。

  **测试/开发轻量派生（mockkdf 子包，不占真实档位）**：secretdata 是「组装编排」而非
  算法实现——测试不需要跑真实重 scrypt（算法可靠性由 shardseal 包真实档负责）。`Algorithm`
  提供 `KDFOverride` 注入点（nil = 真实 scrypt；非 nil = 覆盖派生），子包
  `pkg/cryptox/shardseal/mockkdf` 提供 `RegisterMockAlgorithm()`（once 守卫，注册
  `shardseal/aes-256-gcm-mock`，AlgoVersion=4、域 `"shardseal/v1-mock"`）：**HKDF-SHA256
  轻量派生（~µs，secret 作 IKM、salt 作 info、固定标签）+ 真实 AES-GCM 加密**——组装
  正确性（key/salt/格式传递、密文往返）仍真实验证，仅派生轻量化。**仅供测试/开发，生产
  禁配**；`secretdata` 测试的 `testAlgo` = `mockkdf.MockAlgorithmName`。**loadGate 机制
  测试（`TestLoadIndex_LoadGate_*`）改 mock 档**：内存测算是**类型级**（ScryptMemEstimate
  纯算术，与 KDFOverride 正交），loadGate 并发钳制 / MemStats 峰值采样照测（mock 档
  N=2^12 → 4MiB，并发 = NumCPU）；high→min(2)/standard→min(16)/low=NumCPU 的钳制语义由
  `TestMaxParallelLoads_AdaptsToKDFTier` + `TestMaxParallelLoads_FormulaClamps`（直接
  `shardseal.Algorithm{ScryptN:...}` 构造、不注册）作公式权威。真实 high 内存实测收敛为
  一次性基准（`secretdata_slow_test.go`，`//go:build slow`：ScryptMemEstimate ×2 记账系数
  已由任务 13 实测 1.08GB≈2×理论标定，常规路径不需重复实测）。
- 每块随机 nonce（12B）。
- **统一落盘格式（分块与 meta 同构，含固定长度随机首部）**：

  ```
  [R 固定长度随机首部][8B 真实密文长度 BE][salt][nonce][ciphertext+GCMtag]
  ```

  - **R 段**：每文件开头 **固定长度（默认 128B）`crypto/rand` 随机字节**——首部无
    格式指纹，`file`/magic 检测全部判为随机数据，无法凭首部结构识别「这是加密卷文件」、
    更无法区分 meta 与分块；R 长度固定（解密跳过后按固定偏移读取）。
  - **分块与 meta 都带 8B 长度头**（记录真实密文段长度）——统一，避免「仅 meta 带头」
    的特征。但 **padding 不入长度头**（见下），故长度头对两者语义一致。
  - 明文可观察面：R 随机段 + 8B 长度头 + fileSalt（KDF 参数，非秘密）。
  - 原始文件名 / size / sha256 / 逻辑路径等全部密文 → 底层卷零文件名/路径/元数据泄漏。
  - GCM 认证 → 篡改（删 sha256、改 chunks、改 name/path）一律解密失败 fail-closed。

- **meta 内容加密与 padding（长度特征隐藏）**：meta 明文 JSON 加密后仅几百字节，
  若直接落盘，底层凭**文件大小**一眼区分 meta（KB 级）与分块（MB 级）——比文件名更
  明显的特征。设计：
  - **padding 移入密文内部**：meta 明文 = `[4B JSON 长度 BE][metaJSON][rand padding]`，
    整体加密 → 长度头 = 密文长度 = `len(明文)+16`，与文件大小**完全线性一致**
    （`文件大小 = R + 8 + 44 + len(密文)`，分块/meta 同式）——长度头对分块与 meta
    语义一致，**无法凭「文件大小 vs 长度头」差异识别 padding/meta**；
  - 解密：跳 R → 读长度头截取密文 → GCM 解密 → 读 4B jsonLen 截取真实 JSON；
    rand padding 是解密后明文的一部分、不进 JSON（按 jsonLen 截取）；
  - padding 目标 = 分块大小范围下限（默认 `min_block_size` 1MiB）附近随机
    （1MiB–2MiB），使 meta 文件大小与分块同分布（底层看到的所有文件 ≥1MiB，
    无 KB 级孤点）；目标下限可配（`extra.meta_pad_bytes`，默认 `min_block_size`），
    **任何配置下 meta 与分块的大小范围必须重叠**（测试锁定 §3.5）；
  - 篡改密文（含 padding，均在 GCM 认证范围）→ 解密失败 fail-closed；篡改 R 段 → 不影响
    解密（R 仅首部混淆，不参与完整性——跳过后按固定偏移读取）。

## 5. meta（文件 meta 只存文件根信息 + 目录 meta 存目录信息）

meta 落盘（文件/目录）统一为 §4.2 明文格式：`[4B jsonLen BE][metaJSON][rand padding]`
→ 整体加密 → 文件格式 `[R][8B len][salt][nonce][ct+tag]`；解密后按 jsonLen 截取 JSON，
padding 丢弃。

### 5.1 文件 meta

**只存储当前文件根信息，不关心具体的路径层级信息**（`original.name` = basename，
不存 rel 路径）。加密落盘（格式同分块，见 §4.2）：

```json
{
  "version": 1,
  "algorithm": "shardseal/aes-256-gcm",
  "kdf": "scrypt",
  "type": "file",
  "secret_url": "secrets://<卷>/<name>",
  "salt": "<base64>",
  "original": {
    "name": "today.txt",
    "size": 123456,
    "sha256": "<64hex>",
    "mtime": "...", "ctime": "...", "mode": 420, "media_type": "text/plain"
  },
  "chunks": [
    {"index":0, "offset":0, "file_name":"<加密分块名>", "orig_size":1048576, "orig_sha256":"<16hex>",
     "enc_size":1048700, "enc_sha256":"<16hex>", "nonce":"<base64>"},
    {"index":1, "offset":1048576, "file_name":"<加密分块名>", "orig_size":900000, "orig_sha256":"<16hex>",
     "enc_size":900064, "enc_sha256":"<16hex>", "nonce":"<base64>"},
    ...
  ],
  "block_policy": {"mode":"random", "min":1048576, "max":209715200}
}
```

- 文件 meta **不依赖所在目录路径**——目录内东西相对独立；目录整体移动后文件依旧可解析。

### 5.2 目录 meta

记录该逻辑目录的信息（本目录逻辑名 + 父目录 dir_id 引用），加密落盘（格式同分块）。
目录间完全解耦（task10）：目录 meta 不关心父目录物理在哪，逻辑路径沿 parent 链重算：

```json
{
  "version": 1,
  "algorithm": "shardseal/aes-256-gcm",
  "kdf": "scrypt",
  "type": "dir",
  "name": "2026",
  "parent_dir_id": "<news 的 dir_id，根为空串>",
  "dir_id": "<16hex>",
  "mtime": "..."
}
```

- `name` = 本目录逻辑名（不含路径；根容器为空串）；`parent_dir_id` = 父目录 dir_id
  （根为空串）。逻辑路径沿 parent 链重算（根 → … → name）；loadIndex 建 `dir_id → dirMeta`
  表后逐容器沿链解析。父引用断裂（parent 缺失）fail-closed 跳过该容器。
- **目录移动/改名 = 仅重写被移动目录的 `name`/`parent_dir_id` 引用**（一个文件），子树内
  **任何**目录/文件 meta 与分块零改动（子树父引用指向本 dir_id 不变）→ 目录间完全解耦。
- **文件移动**（跨目录、basename 不变）：文件 blob 自包含 → 物理复制分块 + 文件 meta 到
  目标容器（零改内容、哈希不变）→ 删源副本 → 索引迁移，重启后按新容器路径解析。文件改名
  （basename 变）仍走 delete+write。
- **Imp-1 空目录语义**：删除目录内最后文件时连目录 meta + 注册一并删（彻底成空、重启
  不复现），并递归回收成空祖先目录；根容器恒保留。
- **旧格式兼容**：task10 未上线，旧格式（存完整 `path`）loadIndex 以明文含 `path` 键
  fail-closed，不静默压平。

## 6. 卷模型（secrets + secret_data 双 wrapper）

### 6.1 secrets 卷（封装卷，存 secret 文件）

- **定位**：封装卷（wrapper），底层指向**任意卷的 `secrets/` 目录**——底层可以是
  local/baidupcs/s3/webdav，**也可以是 secret_data 加密卷**（嵌套封装）。
- **寻址**：`secrets://<卷名>/<name>`；默认卷可省略：`secrets://default/<name>`。
- **只存普通文件**（secret 文件本身不加密）。
- **本地盘权限**：创建 secret 文件时 0600（ssh 密钥式）；外部盘（baidupcs/s3）无权限要求（能拿到 = 有权限）。
- **嵌套**：secret_data 卷的 `extra.secret_url` 指向**最底层已存在的 secrets 卷**（不循环——secret_data 引用的 secrets 卷必须先装配好）。
- **服务启动默认**：默认创建一个本地卷作为默认 secrets 卷（`secrets://default/`）。
- **管理 API**：
  - 创建 secret：`secrets://<卷>/<name>` 写入随机密钥（32B hex），本地 0600
  - 列出 secrets：`secrets://<卷>/` 列
  - 选择默认：`extra.default_secret` 或全局默认
  - 读取：返回密钥字节

### 6.2 secret_data 卷（封装卷，加密数据）

- **定位**：封装卷（wrapper），附本地临时空间，可配：
  - `extra.target`：底层卷名（local/baidupcs/s3/webdav... → 支持所有卷）
  - `extra.algorithm`：加密算法（默认 shardseal/aes-256-gcm）
  - `extra.secret_url`：secret 文件（`secrets://<卷>/<name>`）
  - `extra.block_policy`：分块策略（mode/min/max）
  - `extra.temp_dir`：本地临时空间（默认 volume.RootDir staging）
- **底层布局（无顶层 `data`/`meta` 目录，全随机）**：

  ```
  <secretdata根>/<randDir 5-30>/        # 一个逻辑目录 = 一个随机命名容器目录
      目录meta（@ 标记，含逻辑 path）
      文件meta（-/_ 标记，含文件根信息）
      加密分块（无标记）
  ```

  目录内三种文件按 §3 命名规则分类；**目录名与文件内容无直接关联**。
- **写入**（移入，覆盖写原子化）：源 → 本卷 = 分块加密 → 在目标逻辑目录的随机容器内
  以**新随机三段 hex 名**上传新文件 meta + 全部分块 → 全部成功 → 内存索引切换到新条目 +
  best-effort 删除旧版本 meta + 分块。中途失败 → 删新留旧，旧数据完好
  （修复「覆盖写非原子：先删旧后传新导致中途失败永久丢失旧版本」，审查 F-2）。
- **读取**（移出）：本卷 → 目标 = 读文件 meta（解密）→ 按 meta 定位分块解密还原。
- **MakeDir**：逻辑目录 = 递归创建随机容器目录（整条祖先链）+ 写目录 meta（@ 标记，
  {name, parent_dir_id} 父引用）；**不转发明文目录到底层**（旧行为 `inner.MakeDir(rel)`
  会向底层泄漏目录名）。
- **逻辑路径与索引键（修复 F-1）**：逻辑文件路径 = 所在容器目录的**目录 meta 沿 parent 链
  重算的逻辑路径** + 文件 meta 的 basename；索引键 = 完整逻辑 rel，**写路径与重启恢复一致**
  （重启扫描目录 meta → {name, parent_dir_id} 建 dir_id→dirMeta 表、沿链解析路径；文件 meta
  → name，重建完整路径索引）。loadIndex 并行化（Imp-2）：按容器并行扫描 + 容器内文件 meta
  并行解密，配 (salt→key) 派生缓存缓解重复 scrypt；**派生并发上界 max(4, NumCPU)**（scrypt
  ~128MB/次防大卷挂载内存爆炸）；**父引用成环（含自环）fail-closed 跳过**（visited 集防
  未信任存储上损坏/篡改的父引用环导致挂载栈溢出）。
- **目录移动/改名**：仅重写被移动目录的 `name`/`parent_dir_id` 引用（一个文件），子树内
  其它目录/文件 blob 零改动（子树父引用指向本 dir_id 不变）→ 目录间完全解耦。
- **文件移动**（跨目录、basename 不变）：物理复制自包含 blob（分块 + 文件 meta）到目标
  容器（零改内容）→ 删源副本 → 索引迁移；文件改名（basename 变）仍走 delete+write。
- **Imp-1 空目录语义**：Delete 后目录无文件且无子目录 → 连目录 meta + 注册一并删（彻底
  成空、重启不复现），并递归回收成空祖先目录（根恒保留）。
- **删除（方案 A 2026-10-03：默认即时物理删，删即释放）**：Delete 持锁一次删完 meta +
  全部分块（+ parity + 去重引用递减），**不再写墓碑默认路径**——删即释放，正确性**不依赖**
  后台 GC。重启不复现、无残留。GC **收敛为可选维护工具**：默认禁用（`gc_interval` 缺省 0
  不启动后台 GC，显式维护用 `fs.GC()`），仅远程卷/多进程共享场景由配置 `gc_interval>0`
  启用，作孤儿兜底（磁盘既有墓碑 meta / 孤儿分块清理）；已知 epoch 窗口作为可选工具可
  接受，不再作为默认正确性依赖。墓碑仅在「多进程共享卷 + GC 启用」场景保留（跨进程并发
  读保护）。
- **实现**：`sync.FS` wrap（OpenRead/WriteFile/Rename 透明），底层委托 target 卷 FS。

### 6.3 底层寻址

```
secretdata://<卷名>/<path>    # secret_data 卷内路径
secrets://<卷名>/<name>       # secrets 卷内 secret
```

底层卷内所有名称（目录 5-30 随机、文件三段 hex）无结构词、无明文路径。

## 7. 可插拔分块算法（plugin）

- 接口：`BlockPlanner(origSize) ([]Block, error)`。
- 默认：`random`（1MB-200MB 随机）。
- 可扩展：`video-keyframe`（关键帧边界 + 大小综合分片）。
- 装配：`extra.block_policy: {mode, min, max}`；注册：`shardseal.RegisterPlanner("video-keyframe", fn)`。

## 8. 目录结构（落点）

```
pkg/cryptox/shardseal/    # 算法包（纯 x/crypto 依赖）
pkg/volume/secrets/       # secrets 抽象卷（secret 文件管理 + ExternalBackend）
pkg/volume/secretdata/    # secret_data 封装卷（wrapper，sync.FS 透明加解密）
cmd/sproxy/secret_register.go  # 装配：注册 secrets/secretdata 后端（仿 baidupcs_sync.go）
config.example.yaml       # volumes[].type: secrets / secretdata 示例
```

## 9. 装配流程

```
注册后端（cmd/sproxy，仿 baidupcs）：
  registerSecretsBackend()      # type=secrets：Extra.target 卷 → secrets FS wrapper
  registerSecretdataBackend()   # type=secretdata：Extra.target + secret_url → 加密 FS wrapper

构造顺序（secretdata 依赖 secrets 卷；secrets 卷可嵌套）：
  1. 装配默认 secrets 卷（启动默认：本地卷 secrets/ 目录）
  2. 装配配置的 secrets 卷（Extra.target 的 secrets/ 目录；target 可以是加密卷——先装配 target）
  3. 装配 secretdata 卷（引用已存在的 secrets 卷拿密钥）
  4. 依赖解析：secretdata.secret_url → secrets 卷 → 若 secrets 卷 target 是加密卷 → 递归到最底层
```

## 10. 旧卷加载

- 扫描底层根目录 → 每个随机容器目录内按文件名标记分类：
  - 含 `@` → 目录 meta（解密 → 逻辑 path）
  - 含 `-`/`_` → 文件 meta（解密 → basename + stat + chunks）
  - 无标记 → 分块（由文件 meta 引用）
- 索引键 = 目录 meta.path + 文件 meta.name（完整 rel），写/恢复一致。
- 按需还原：读取时按 meta 定位分块 → 解密 → 本地缓存。
- 新 sproxy 实例挂载旧卷可直接用（不重下载）。

## 11. 状态

设计定稿（初版实现）：
1. secrets 卷 = 封装卷（底层任意卷，含 secret_data 嵌套）✓
2. secret_data 卷的 secret 寻址到最底层已存在 secrets 卷 ✓
3. 服务启动默认建本地卷作默认 secrets 卷 ✓
4. secretdata 卷内 data/ + meta/ 分目录（历史布局，已改为随机容器目录）✓
5. 底层寻址 secretdata:// / secrets:// ✓
6. 旧卷加载 = 扫目录建索引 + 按需还原 ✓

目录名保密设计（设计定稿、待实现）：
7. 底层全随机可见名称：目录随机 5-30、文件三段 hex 同构，无 `meta`/`data` 结构词
8. meta 保持三段 hex 命名（不引入 emeta 特征）；meta 内容加密（格式同分块，无 magic）
9. 文件 meta 只存文件根信息（basename），不存路径层级；目录 meta（@）存
   {name, parent_dir_id} 父引用（task10：目录间完全解耦，逻辑路径沿 parent 链重算）
10. 逻辑目录 = 随机容器目录 + 目录 meta；目录移动/改名 = 仅重写根容器 meta 的 name/parent
    引用（一个文件），子树零改动；文件移动 = 物理搬 blob（零改内容）；Imp-1 空目录删干净
11. 覆盖写 = 新随机名先传后删旧（原子化，中途失败不损旧数据）
12. **长度范围锁定**：三类文件名长度同分布（54–62）；meta 落盘 padding 到分块大小
    范围（密文内 padding + 4B jsonLen，长度头线性一致）——文件名/文件大小/首部均无
    meta 特征，命名与 padding 测试锁定（§3.5）
## 12. 目录名保密：泄漏面审计

保密边界：**对底层存储（云盘/他端）隐藏目录结构、文件名、路径与文件元数据**。
底层可观察面逐项审计：

| 底层可观察项 | 内容 | 状态 |
|--------------|------|------|
| 顶层目录名 | 随机 5-30（`[a-z0-9]`） | 无泄漏 |
| 加密分块文件名 | 三段内容校验和截断 + 随机段（§3.1） | 已知边界：暴露内容指纹截断与同文件分块关联 |
| 文件 meta 文件名 | 三段 hex 同构 + `-`/`_` 标记（§3.2） | 无特征（与分块视觉同构） |
| 目录 meta 文件名 | 三段 hex 同构 + `@` 标记（§3.3） | 无特征 |
| **文件名长度** | 三类同分布 54–62 字符（§3.5） | 无长度特征 |
| **文件大小** | meta padding 到分块范围 [1MiB,2MiB]，长度头线性一致（§4.2/§3.5） | 无大小特征、无 padding 痕迹 |
| **文件首部** | 固定长度随机首部 R（§4.2） | 无格式指纹（magic 检测全随机） |
| 分块/meta 内容 | `[R][8B len][salt][nonce][ct+tag]`（§4.2） | 仅 R 随机段 + KDF salt 可见 |
| 逻辑目录结构 | 目录 meta 内（加密） | 无明文泄漏 |
| 文件根信息（名/size/sha256） | 文件 meta 内（加密） | 无明文泄漏 |
| 目录与文件关联 | 目录名随机，与文件内容无直接关联 | 无编码关联 |

已知边界（明确不承诺）：
- 分块/meta 文件名中的内容校验和截断属「内容指纹层」——密文 blob 已防内容，指纹截断
  不扩大内容风险；三段 hex 是「原始 hex 关联 + 加密前后内容 hex 完整可靠」的既定载体，
  保留不改。
- 目录名 `[a-z0-9]` 随机碰撞到 `meta`/`data` 等词的概率极低，生成时重掷规避。

---

## 13. 架构演进预留全景（任务 9b：字段/常量预留，语义在 9c 与后续）

> 状态：**全量预留固化**（2026-10-02 用户指令「预留所有」）。本段只**加字段/常量、定方向**，
> 不写值不实现逻辑（压缩/去重/乐观锁/GC/纠删码等语义在任务 9c 与后续逐项落地）。
> 已落盘的 meta 全量预留字段见 `pkg/cryptox/shardseal/meta.go`；块类型常量
> `BlockletTypeRef=0x12` / `BlockletTypeParity=0x13` 见 `crypto.go`——均为 `omitempty` 零值
> 空余、旧 meta 可正常解密加载（无兼容负担）。

### 13.1 版本化机制：AlgoVersion 注册表 = 平滑演进的唯一保证

加密格式演进的正交前提：**任何未来语义（压缩、去重、多副本、纠删码）都必须能在不破坏
在途/存量密文的前提下引入**。为此：

- 每引入一类新能力，若改变派生/解密路径，**登记新 `AlgoVersion`**（`RegisterAlgorithm`
  注册表），KDF 派生域随之分离（`deriveKey` 输入 = `secret || kdfDomain(v)`，域不同 key
  不同，**版本不明文进 blob**）。
- **KDF 强度档位版本化（2026-10-03）**：scrypt 参数（N/r/p）随 Algorithm 登记
  （`ScryptN/R/P`），不同档 = 不同 Version + KDF 域（§4.2 三档）。调整强度（升/降档）
  即登记新版本，不破坏既有档位 blob；同 secret+salt 跨档派生 key 不同，**跨档
  fail-closed**——与 §4.2「高熵密钥档位不影响暴力破解成本」一致。
- 解 / 解密按注册表「升序试派生」定位（`DecryptMetaStandalone` / `DecryptChunkStandalone`），
  新增版本仅注册即生效、`secretdata` 零改动；未知版本 **fail-closed**（无法确定派生域，
  拒绝以错误 key 解密）。
- **约束（保持演进通道开放）**：所有预留字段/类型都位于密文内 + AAD 内，观察者不可见；
  字段 `omitempty` 保证空不膨胀；`BlockletType` 未知值 fail-closed（`validateIndexEntries`
  default 分支拒绝）——**旧版本永远能安全读取新版本留下的空白，新版本能校验未知旧格式**。

### 13.2 12 项审计预留全景（字段/常量 → 方向）

| # | 审计项 | 预留落点 | 演进方向（9c 及后续） |
|---|--------|----------|------------------------|
| 1 | **去重引用 ref** | `Meta.RefCount` + `BlockletTypeRef=0x12` | 块级内容寻址：同内容块复用、引用计数记账，删除减引用不为零清理 |
| 2 | **乐观锁** | `Meta.BaseVersion` | 多进程覆盖写前 CAS 校验 `base_version`，防并发写穿（版本符 → 409 重读） |
| 3 | **墓碑 + GC** | `Meta.Deleted` | **可选、默认关（方案 A 2026-10-03）**：删除默认即时物理删（删即释放，弃墓碑）；墓碑仅在「多进程共享卷 + GC 启用」场景保留（跨进程并发读保护）；GC 收敛为可选维护工具，默认不启动（`gc_interval` 缺省 0），仅在远程卷/多进程共享场景作孤儿兜底（磁盘既有墓碑 meta / 孤儿分块清理；已知 epoch 窗口作为可选工具可接受） |
| 4 | **usage 记账** | `RefCount`（+ `AccessCount`） | 存储占用/共享引用记账，配额（`owner_quotas`/`max_storage_bytes`）精确核算 |
| 5 | **溯源** | `WriterID` / `SourceURL` / `ExportedFrom` | PikPak 下载来源、备份导出溯源；`ExportedFrom` = 来源卷/任务 |
| 6 | **版本保留** | `VersionSeq` / `Supersedes` | 覆盖写保留 N 个旧版本（`versioning.max_versions` 对齐），版本链回溯 |
| 7 | **流式** | blocklet 索引已内建（随机访问） | 边解密边输出 / 视频关键帧边界（`BlockletMode=video-keyframe` 预留） |
| 8 | **多副本** | `BlockletTypeParity=0x13`（XOR parity） | k-of-k+1 纯 stdlib 异或冗余（见 §13.3），恢复单块丢失/损坏 |
| 9 | **纠删码** | 新 `AlgoVersion` 注册（后续） | Reed-Solomon / 奇偶方程组多块纠错，跨块恢复（区别于单块 parity） |
| 10 | **访问计数** | `AccessCount` / `LastAccess` | 热数据统计（成本/配额调度），`LastAccess` 供冷数据归档判据 |
| 11 | **meta 签名** | `Meta.Signature` | meta HMAC（HKDF 子域派生签名密钥），防 meta 替换/篡改（独立于 GCM） |
| 12 | **版本时钟** | `Meta.VClock` | 防跨时区/时钟漂移的覆盖误判；多同步节点版本收敛（HLC/向量时钟扩展） |

> 落位说明：**压缩**（`Compressed` / `Compression`，独立 `RegisterCompression` 注册表，
> 改压缩算法不升级加密版本）、**KeyID**（多 secret 池轮换互读）、**Extra**（扩展 kv）作为
> 横向能力亦一并预留（见 meta.go 注释）。

### 13.3 多副本 = XOR parity（k-of-k+1 纯 stdlib）→ Reed-Solomon 后续

> **实验性标注（2026-10-03 方案 A）**：XOR parity 已实现但**未生产验证**，标注
> `experimental`（Options.Erasure + config.example erasure 键均标注）；默认关闭，不参与
> 默认正确性承诺。启用时对 ≥2 数据分块生成单块冗余（k-of-k+1），任一分块丢失可恢复；
> 单块损坏/丢失恢复为实验性能力，生产采用前需专项验证。

- **首期（纯 stdlib）**：多副本采用 **XOR parity**——k 数据块 + 1 校验块（k-of-k+1），
  校验块 = 前 k 块的按位异或，任一数据块丢失/损坏可由其余 k 块异或复原。仅 `^` 位运算，
  纯标准库无新依赖；`BlockletTypeParity=0x13` 标记校验段（当前无消费逻辑、未知
  fail-closed）。
- **后续（纠删码）**：扩展到跨多块的 Reed-Solomon 奇偶方程组（容忍 t 块随机损坏）经
  **新 `AlgoVersion` 注册**（§13.1 通道）引入，不破坏既有 parity 块——k-of-k+1 是 RS
  的 n=m=1 特例，单块恢复起步、多块纠错平滑演进。
- **取舍**：parity 冗余代价 = 1/k（与块数反比）、计算 = O(k) 线性异或（极低）、纯 stdlib
  可靠；弱点是仅覆盖单块。Reed-Solomon 覆盖多块但引入依赖/复杂度——故 parity 先行、
  RS 后续，均由注册表通道平滑演进。

### 13.4 已实现但实验性清单（2026-10-03 方案 A）

| 项 | 实验性/预留状态 | 落地说明 |
|------|------|------|
| **去重引用（Dedup）** | **预留/实验性，未接线** | 整文件池实现（writeFileDedup/卷级池/引用计数）保留为实验代码，测试框住正确性但**装配层不解析 `extra.dedup` 键**（生产不可达）；未来由独立内容寻址子系统承接 |
| **Erasure（XOR parity）** | **实验性** | k-of-k+1 单块损坏/丢失恢复已实现，未生产验证（见 §13.3）；默认关闭 |
| **后台 GC** | **可选、默认关** | `gc_interval` 缺省 0 不启动；仅远程卷/多进程共享场景显式启用作孤儿兜底，不参与默认正确性（见 §6.2） |
| **乐观锁（BaseVersion）** | **多进程预留 API** | `WriteFileIfVersion`/`CurrentVersion` 为多进程预留；单进程写路径内建版本（落盘 `meta.BaseVersion`），单实例天然版本单调 |
| **墓碑 + GC** | **可选、默认关** | Delete 默认即时物理删（弃墓碑）；墓碑仅「多进程共享卷 + GC 启用」场景保留（见 §13.2 条目 3） |

### 13.5 对照类似实现（gocryptfs / restic / age / Tahoe）

| 实现 | 核心形态 | 与 shardseal 的差异 / 借鉴 |
|------|----------|----------------------------|
| **gocryptfs** | 流式加密（thin / 透明 FS），逐文件加解密 | 我们为**分块存储 + 随机容器目录**（非 thin 布局）；借鉴其随机访问/按需解密 → 本设计 blocklet 索引分段读取 |
| **restic** | 备份仓库：内容寻址去重 + 压缩（chunk 级） | 借鉴**内容寻址去重**（§13.1 Ref/RefCount）与压缩（Compressed）；差异在 restic 备份/快照模型、非单文件卷 |
| **age** | 多接收者公钥加密（每接收者可独立解密 X25519 材料） | 借鉴**多 key 互读**（§13.1 KeyID 轮换）；差异在 age 面向公钥、多人，我们面向卷 secret 轮换 |
| **Tahoe-LAFS** | 纠删码（Reed-Solomon）分片分布多节点 | 对应 §13.3 RS 后续；我们以「卷共存 + XOR parity 起步」演进到其容错语义 |

---

# 附录 A：PikPak 多账号管理设计（会话文件池）

> 状态：设计定稿（初版实现）
> 关联：PikPak 中转站——下载 → 分块加密 → 上传任意卷的多账号支持

## A.1 背景

- 免费账号下载带宽 ~1.15MB/s（账号级，多并发不加速），**20GB/日 下载配额**（PikPak FAQ，Connected Apps 共享 25% = 5GB/日）。
- 大文件（4.5GB）单账号 1 天仅能下 1 个 → 需多账号轮换接力。
- 分享转存到个人网盘**不占下载配额**（只占存储 6GB）。

## A.2 实测机制（2026-10-01 探索）

| 机制 | 实测结果 |
|---|---|
| CLI 会话 | 单 `.credentials.json`（access_token + refresh_token），**refresh 自动**（过期 access_token + 有效 refresh → CLI 自动刷新成功） |
| PIKPAK_TOKEN env | 纯 access_token 覆盖，**无 refresh**（进程级隔离但过期需外部刷新） |
| 路径隔离 | CLI 硬编码 `~/.pikpak`，HOME/USERPROFILE/config-dir 均无效 |
| REST 配额 | `drive/v1/about`：storage 6GB + `cloud_download` 任务数（limit=5/complimentary=2）；**带宽 20GB/日 不可查** → 本地记录 |

## A.3 设计：会话文件池（方案 A，用户确认）

```
账号管理（pkg/volume/ext/pikpak/account.go）
├── 会话文件池：每账号保存完整 credentials 会话 JSON
│   （含 refresh_token —— CLI 自动刷新，可靠无 captcha）
├── 下载时：切换会话 → 写 ~/.pikpak/.credentials.json → 调用 CLI
├── 串行：同进程内 CLI 调用顺序执行（下载本身限速，串行可接受）
├── 配额追踪：本地记录（下载字节按日累计；REST 无带宽配额 API）
├── 账号选择：按剩余配额（下载前查本地日消耗，不足换下一账号）
└── 凭据存储：secrets 卷（每账号一个 secret 文件，复用 shardseal 设计）
```

### A.3.1 会话文件池 API

```go
// Account 是 PikPak 账号。
type Account struct {
    Name       string          // 账号名（唯一）
    SecretURL  string          // secrets://<卷>/<name> 指向会话凭据
    UserID     string          // 账号 user_id（识别用）
    DailyUsed  int64           // 今日已下载字节（本地记录，按日重置）
    LastReset  time.Time       // 上次日重置时间
}

// AccountPool 是账号池（会话文件池）。
type AccountPool struct {
    accounts []*Account
    mu       sync.Mutex        // 串行化 CLI 会话切换
    cli      *Cli
    secrets  *secrets.Manager  // 读账号会话凭据
}

// Select 按剩余配额选账号：剩余配额 >= neededBytes 的第一个可用账号。
func (p *AccountPool) Select(ctx context.Context, neededBytes int64) (*Account, error)

// Use 切换 CLI 会话到该账号（写 ~/.pikpak/.credentials.json）并执行操作。
func (p *AccountPool) Use(ctx context.Context, acct *Account, fn func() error) error

// RecordUsage 记录账号已下载字节（按日累计）。
func (p *AccountPool) RecordUsage(acct *Account, bytes int64) error
```

### A.3.2 会话凭据（secrets 卷存储）

- 每账号一个 secret 文件：`secrets://<卷>/pikpak-<name>.json`
- 内容：完整 `.credentials.json`（client_id/device_id/user_id/access_token/refresh_token/token_expiry）
- 首次登录：`sproxy pikpak account add <name>` → OAuth device 授权 → 会话存 secrets 卷
- 后续：`AccountPool.Use` 把会话写入 `~/.pikpak/.credentials.json` → CLI 自动 refresh

### A.3.3 配额追踪（本地）

- 每账号本地状态：`~/.pikpak-<name>/quota.json`（daily_used + last_reset）
- 按日重置（跨天清零）；下载前查 `Select`，下载后 `RecordUsage`。
- 阈值：默认 20GB/日（可配 `pikpak.accounts[].daily_quota`）。

### A.3.4 轮换策略

- `Select`：遍历账号，剩余配额 >= neededBytes 的第一个返回。
- 失败标记：下载失败（网络/配额）→ 冷却（暂不选该账号）。
- 顺序：round-robin 起点（避免总用第一个账号）。

### A.3.5 与 shardseal 结合（后续片）

- 当前：单账号整文件下载（简单可靠）。
- 后续：多账号分块并行下载（每账号下不同 shardseal 分块 → 合并 → 加密上传），绕账号限速。

## A.4 目录落点

```
pkg/volume/ext/pikpak/account.go      # Account + AccountPool（会话文件池）
pkg/volume/ext/pikpak/account_test.go # TDD 测试
cmd/sproxy/pikpak_cmd.go              # account add/list/remove 子命令
config.example.yaml                   # pikpak.accounts[] 示例
```

## A.5 状态

设计定稿（初版实现）：
1. 会话文件池（方案 A）✓
2. 凭据存 secrets 卷 ✓
3. 配额本地记录（20GB/日，按日重置）✓
4. 按剩余配额轮换 ✓
5. 首期单账号整文件，分块并行后续 ✓
