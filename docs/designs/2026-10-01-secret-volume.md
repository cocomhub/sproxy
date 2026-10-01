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

## 2. 算法包：pkg/cryptox/shardseal

**shardseal**（分片封印）——随机分块 + AES-256-GCM + 自描述命名。

```
pkg/cryptox/shardseal/
├── shardseal.go   # 对外 API：EncryptFile/DecryptFile/命名/meta 生成解析
├── naming.go      # 命名规则（三段校验和截断 + 随机长度 rand + meta 标记）
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

## 3. 命名规则（用户确认 v2）

### 3.1 加密分块文件名

```
{块原始checksum前16hex}{rand1}{原始总checksum前16hex}{rand2}{块加密后checksum前16hex}
```

- 三段各 16 hex；顺序：块原始 → 原始总 → 块加密后（**原始总在中间**，避免同源聚集）。
- rand1/rand2：长度 **3-7 随机**；字符集 `[A-Za-z0-9]`（无 - _）。
- 无后缀。

### 3.2 meta 文件

```
{meta原始前16hex}{rand1}{原始总前16hex}{rand2}{meta加密后前16hex}
```

- 同规则（块 = meta 自身前后校验和）。
- **rand 必含一个 '-' 或 '_'**（meta 可识别标记）。
- 扫描目录时：含 `-`/`_` 的 = meta，不含 = 分块。

## 4. 分块与加密（1MB-200MB + 边加密边上传）

### 4.1 分块

- 块大小 **1MB-200MB 随机**；每文件随机分块序列（最后一块为剩余）。
- **边加密边上传**：读原始 → 切块 → 加密该块 → 立即上传 → 释放该块本地临时 → 下一块。峰值 = 原始 + 1 块。

### 4.2 加密

- `AES-256-GCM`（x/crypto）。
- 每文件随机盐（32B）→ scrypt 从 secret 派生文件密钥。
- 每块随机 nonce（12B）。
- 块格式：`[salt][nonce][ciphertext+GCMtag]`。

## 5. meta（全 stat + 每分块 stat）

```json
{
  "version": 1,
  "algorithm": "shardseal/aes-256-gcm",
  "kdf": "scrypt",
  "secret_url": "secrets://<卷>/<name>",
  "salt": "<base64>",
  "original": {
    "name": "xxx.mp4",
    "size": 123456,
    "sha256": "<64hex>",
    "mtime": "...", "ctime": "...", "mode": 420, "media_type": "video/mp4"
  },
  "chunks": [
    {"index":0, "file_name":"<加密分块名>", "orig_size":1048576, "orig_sha256":"<16hex>",
     "enc_size":1048700, "enc_sha256":"<16hex>", "nonce":"<base64>"},
    ...
  ],
  "block_policy": {"mode":"random", "min":1048576, "max":209715200},
  "meta_file_name": "<meta文件名>"
}
```

- **支持旧卷加载**：扫描 meta → 还原文件（stat 全在，建立本地缓存不重下载）。

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
- **写入**（移入）：源 → 本卷 = 分块加密 → 上传底层卷 `secretdata/<校验和前16>/<分块名>`。
- **读取**（移出）：本卷 → 目标 = 读 meta → 解密还原。
- **实现**：`sync.FS` wrap（OpenRead/WriteFile/Rename 透明），底层委托 target 卷 FS。

### 6.3 底层寻址

```
secretdata://<卷名>/<path>    # secret_data 卷内路径
secrets://<卷名>/<name>       # secrets 卷内 secret
```

secret_data 卷内布局：
```
<secretdata根>/data/<校验和前16>/<加密分块文件>
<secretdata根>/meta/<校验和前16>/<meta文件>
```

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

- 扫描 secretdata 卷 `meta/` 目录 → 解析 meta（含全 stat）→ 建立本地缓存索引。
- 按需还原：读取时按 meta 定位分块 → 解密 → 本地缓存。
- 新 sproxy 实例挂载旧卷可直接用（不重下载）。

## 11. 状态

设计定稿（初版实现）：
1. secrets 卷 = 封装卷（底层任意卷，含 secret_data 嵌套）✓
2. secret_data 卷的 secret 寻址到最底层已存在 secrets 卷 ✓
3. 服务启动默认建本地卷作默认 secrets 卷 ✓
4. secretdata 卷内 data/ + meta/ 分目录 ✓
5. 底层寻址 secretdata:// / secrets:// ✓
6. 旧卷加载 = 扫 meta 建索引 + 按需还原 ✓

按 TDD 实现（独立 agent 在 worktree）。
