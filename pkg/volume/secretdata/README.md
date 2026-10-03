# secretdata (pkg/volume/secretdata)

`secretdata` 卷是**透明加密封装卷（wrapper）**：写入自动分块加密、读取自动解密，对上层透明
（`sync.FS` wrap）。算法由 `pkg/cryptox/shardseal` 提供；密钥来自 `secrets://` 卷（`pkg/volume/secrets`）。
设计 `docs/designs/2026-10-01-secret-volume.md`；使用者文档 `docs/secret-volume.md`；未来方向见
`docs/secret-volume.md §8`。

## 能力

- **透明加解密**：`WriteFile` 分块加密落盘，`OpenRead` 解密还原；逻辑 Stat/ListDir 用 meta 内原始
  mtime/模式，不受底层 blob 形态影响。
- **底层全匿名布局**：无顶层 `data`/`meta` 目录；一个逻辑目录 = 一个随机命名容器目录（5-30
  `[a-z0-9]`），内含目录 meta（`@` 标记）、文件 meta（`-`/`_` 标记）、加密分块（无标记）。
- **目录解耦（父引用）**：目录 meta 只存 `{name, parent_dir_id}`；目录移动/改名 = 只重写被移动目录
  自己的引用，子树零改动；文件移动（basename 不变）= 物理复制自包含 blob（零改内容）。
- **即时物理删**：Delete 一次删完 meta + 全部分块（+ parity），不写墓碑——删即释放、重启不复现、
  正确性不依赖后台 GC。
- **GC 可选兜底**：`Options.GCInterval` 缺省 0 不启动后台；仅远程卷/多进程共享场景显式启用
  （`fs.GC()` 或配置 >0），作孤儿/墓碑兜底清理。
- **冗余（实验性）**：`Options.Erasure` 启用 XOR parity（k-of-k+1，纯 stdlib），任一分块丢失/损坏
  可由其余分块复原；未生产验证、默认关。
- **多 target 复制（实验性）**：`Options.Targets` 声明副本卷名，写路径复制自包含容器到全部 target、
  读主失败回退副本（当前仅多 local root）。
- **寻址**：装配后 `secretdata://<卷名>/<path>` 可被 registry 寻址；密钥寻址 `secrets://<卷>/<name>`。

## Options（装配层经 config `volumes[].extra` 解析）

| 字段 | 默认 | 说明 |
|------|------|------|
| `Secret` | 必填 | 密钥字节（装配层经 `secret_url` 从 secrets 卷读取注入） |
| `Algorithm` | `shardseal/aes-256-gcm` | KDF 档位（`-high` / `-low` 切换；跨档 fail-closed） |
| `Block` | random 1MiB-200MiB | 分块策略（blocklet 默认 fixed 64KB-4MB） |
| `TempDir` | 系统随机临时目录 | 本地临时空间（0700） |
| `MetaPadBytes` | `Block.Min` | meta pad 目标基准（`+rand()`，受 196B R 地板） |
| `PreserveMTime` | false | 默认 blob mtime 打散（0-48h 偏移）；true 才透传原始 |
| `Dedup` | false | **预留，装配层不接线**（整文件内容去重未来由独立子系统承接） |
| `GCInterval` | 0（禁用） | 后台孤儿/墓碑 GC 周期（可选维护工具） |
| `Erasure` | false | XOR parity 纠错（**实验性**，默认关） |
| `Targets` | 空 | 多 target 副本卷名（装配元数据；副本 FS 由装配层注入） |
| `MaxFileBytes` | 0（不限） | 单文件上限（读取全文前按 size 拦截，内存防护） |

字节大小字段统一 `pkg/units/sizex.ByteSize`（可读 `"1GiB"`/`"5GB"`/纯数字）。

## 用法（写入/读取）

```go
// 装配（cmd/sproxy/secret_register.go 仿 baidupcs）：构造底层 FS + Options
targetFS := syncpkg.NewLocalFS(root, nil)          // 底层卷根（多 target 用 NewFSMultiplicas）
opts := secretdata.Options{Secret: key, Algorithm: shardseal.AlgorithmName, ...}
fs, err := secretdata.NewBackend(ctx, v, targetFS, opts)  // 或 NewBackendMultiplicas

// 透明读写
fs.WriteFile(ctx, "dir/file.txt", reader, size, mode)
rc, _ := fs.OpenRead(ctx, "dir/file.txt")
fs.Delete(ctx, "dir/file.txt")
```

测试：`go test -count=1 -race ./pkg/volume/secretdata/...`（测试用 mockkdf 档 `shardseal/aes-256-gcm-mock`，
生产禁配）。`//go:build slow` 的 `secretdata_slow_test.go` 为真实 high 档内存一次性基准，不进常规测试。

> 未来方向与预留（压缩 / 流式 / 内容寻址去重 / 乐观锁多进程 CAS / CLI 接线 / Reed-Solomon 纠删码）
> 见 `docs/secret-volume.md §8`。
