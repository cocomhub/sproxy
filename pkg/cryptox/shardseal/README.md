# shardseal (pkg/cryptox/shardseal)

纯 Go 随机分块 + AES-256-GCM + scrypt 派生的**自描述加密分块算法**（设计
`docs/designs/2026-10-01-secret-volume.md`；使用者文档 `docs/secret-volume.md`；未来方向见
`docs/secret-volume.md §8`）。

## 能力

- **随机分块加密**：`EncryptShards` / `EncryptShardsBytes` 把文件加密为分块 + meta；`DecryptFile` 还原。
- **统一落盘格式**：分块与 meta 同构，首部/大小/长度三维度均无 meta/分块特征，无法凭 file/magic/长度
  区分（见下「统一格式」）。
- **自描述 blob（blocklet 索引）**：每个分块 blob 内含 boot 引导段 + 乱序 blocklet 段 + 索引块——拿到
  blob + secret 即可经 boot→index 随机访问、只解目标段，不依赖外部 meta。
- **算法注册表**：`RegisterAlgorithm` 版本化登记（不同 AlgoVersion + KDF 派生域），新增算法仅注册即
  生效，未知版本 fail-closed。
- **KDF 档位**：scrypt 参数随 Algorithm 版本化，high / standard / low 三档（不同派生域，跨档
  fail-closed）。
- **mockkdf 测试子包**：`pkg/cryptox/shardseal/mockkdf` 提供 HKDF 轻量派生 mock（仅供测试/开发，生产
  禁配；算法可靠性仍由本包真实档维护）。

## 统一格式

meta blob（与旧分块同构）：

```
[R 128B 随机首部][8B 密文长 BE][salt 32B][nonce 12B][ciphertext+GCMtag]
```

块 blob（分块内置 blocklet 索引）：

```
[R 128B][8B 密文流总长][salt 32B]
+ [boot 引导段][gap][数据/padding/extra 段乱序][gap][index 索引块]
```

- R 段：每文件开头固定长度 `crypto/rand` 随机字节——首部无格式指纹，`file`/magic 检测全判随机；
  R 段仅首部混淆，篡改不影响解密。
- 8B 长度头：meta = 密文长度（`len(明文)+16`），块 = 密文流总长；与文件大小线性一致。
- 段边界不明文：blocklet 的 type/off/len 只作 GCM AAD，段间 0-64B 随机 padding 间隙混淆边界。
- meta 明文额外带 4B jsonLen 前缀 + 随机 padding（`encryptMetaJSON`），padding 在密文内、GCM 认证，
  解密按 jsonLen 截取真实 JSON。

## 命名规则（三类同构）

分块 / file meta / dir meta 三种文件名**长度同分布 54-62**：`{段1 16hex}{rand1 3-7}{段2 16hex}
{rand2 3-7}{段3 16hex}`。靠随机段内标记字符区分类型：

| 类型 | 标记 | 识别 |
|------|------|------|
| 加密分块 | 无（`[A-Za-z0-9]`） | `ClassifyName` 无标记 |
| 文件 meta | 含 `-`/`_` | `IsMetaName` |
| 目录 meta | 含 `@` | `IsDirMetaName` |

目录名随机 5-30 `[a-z0-9]`（`RandDirName`，规避 `meta`/`data`/`secret` 保留词）。

## 算法注册表（KDF 档位）

`init()` 注册三档（标准 = `AlgorithmName`，即 `shardseal/aes-256-gcm`）：

| 档位 | 算法名 | N | 真实 scrypt 内存（128×r×N） |
|------|--------|-----|------------------------------|
| standard（默认） | `shardseal/aes-256-gcm` | 2^14 | 16MiB |
| high（保守） | `shardseal/aes-256-gcm-high` | 2^17 | 128MiB |
| low（测试/低配） | `shardseal/aes-256-gcm-low` | 2^12 | 4MiB |

`ResolveAlgorithm(name)` 按名解析版本；`AlgoByVersion(v)` 读档位参数（装配层做派生并发预算用）；
`ScryptMemEstimate()` 返回真实内存。

## mockkdf 测试子包

`pkg/cryptox/shardseal/mockkdf`：`RegisterMockAlgorithm()` 注册 `shardseal/aes-256-gcm-mock`
（AlgoVersion=4、KDF 域 `shardseal/v1-mock`）。派生 = HKDF-SHA256 轻量（~µs），加密仍走真实
AES-GCM——组装正确性（key/salt/格式传递、密文往返）真实验证，仅派生轻量化。**仅供测试/开发，
生产装配不得调用**。

## 构建与测试

根 module（go.work 内），直接：

```bash
go build ./pkg/cryptox/shardseal/...
go test -count=1 -race ./pkg/cryptox/shardseal/...   # 真实三档 roundtrip + 跨档 fail-closed
go test -count=1 -race ./pkg/cryptox/shardseal/mockkdf/...
```

依赖：`golang.org/x/crypto`（scrypt）。（`make build` / `make test` / `make lint` 亦覆盖。）

> 未来方向与预留（压缩 / 内容寻址去重 / Reed-Solomon / 逐 blocklet 随机访问接线）见
> `docs/secret-volume.md §8`。
