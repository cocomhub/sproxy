# Benchmark job 反复超时的根因：夹具把 runner 磁盘带宽写进了计时路径（2026-09-15）

适用范围：sproxy 的 `make bench` / CI `Benchmark` job 及其所有 benchmark 夹具。
本文记录**已实测取证**的结论，避免把「runner 磁盘抖动」误当成代码回归（或反过来）。

## TL;DR

- `pkg/client` 的 benchmark 夹具（`newMockServerBench` 的 `/upload` handler）原来把每个 payload
  `os.Create` 到 `b.TempDir()`（= runner 系统盘 /tmp），每趟 CI 写 **≈2.5 GiB**、且**每次迭代都用新文件名**。
- 当 runner 的脏页回写带宽被同宿主机挤到 ~0.15 MB/s 时，单次 1 MiB 上传从 **5 ms → 6.49 s**、
  4 MiB 从 **15 ms → 29.38 s**；benchmark 每个 count 会跑 ~180–290 次 × 5 个 count ⇒ 单个 benchmark
  需要 20+ 分钟 ⇒ `timeout-minutes: 6` 必然把 job 掐断（现象：`client.test` 被 `Terminate orphan process`）。
- 判据是**耗时与写入字节数成正比**（1 MiB→6.49 s、4 MiB→29.38 s，速率都落在 140–180 KB/s），
  且**只读**的 `BenchmarkDownload` 在同一时间窗内仍是 3.8 ms/op（CPU / loopback 网络都正常）。
- 修复方向：**benchmark 夹具不得把被测 payload 写进 runner 磁盘**。`pkg/client` 的 mock 现在只做
  「流式哈希 + 丢弃」，并用 `TestMockBenchUploadHandler_DoesNotPersistPayload` 钉住（夹具写盘即红）。

## 1. 取证（3 个真超时 job，全部卡在 `pkg/client`）

| job id | job 时长 | 被杀的孤儿进程 | 卡住位置 |
| --- | --- | --- | --- |
| `104347597755` | 7m03s | `client.test` | `BenchmarkUpload`（1 MiB） |
| `104343040072` | 8m16s | `client.test` | `BenchmarkUpload_4MB_Regular`（4 MiB） |
| `104290453568` | 8m16s | `client.test` | `BenchmarkUpload` 第 1 个 count |

日志尾部形态固定：`##[error]The operation was canceled.` → `Terminate orphan process: pid (…)(client.test)`，
**没有 panic / goroutine dump**（job 级 timeout 直接掐断，无自诊断）。

把日志里 `[trace …] POST /upload <dur>` 全量解析后的形态：

| job | 阶段 | 单次 op | 折算写带宽 |
| --- | --- | --- | --- |
| `104347597755` | 1 MiB | 前 123 次 ~8 ms，之后**恒定 6.447–6.495 s** | 161 KB/s |
| `104343040072` | 4 MiB | 前 4 个 count ~17 ms，第 5 个 count 起**恒定 29.33–29.38 s** | 139 KB/s |
| `104290453568` | 1 MiB | count#1 内 5.0–6.0 s（末尾 629/420 ms 说明在恢复） | ~180–350 KB/s |
| 正常（run `34961709042`） | 1 MiB / 4 MiB | 5–6.5 ms / 15–17 ms | 190–270 MB/s |

- **方差极小（±50 ms）+ 与字节数成正比** ⇒ 排除「固定网络/连接超时」（那会与字节数无关）。
- **同一 run 内只读 benchmark 不受影响**（`104290453568` 中慢的 count 结束后 `BenchmarkDownload`
  立刻回到 3.8 ms/op）⇒ 排除 CPU/loopback 争抢。
- **可恢复**：`104290453568` 的 count#1 之后物理恢复（约 4.7 min 的慢窗口），说明是**环境带宽塌陷**而非死锁。

## 2. 每次 CI 到底写了多少（按成功 run `34961709042` 的 n 统计）

| 包 / benchmark | 迭代数 | payload | 写入量 |
| --- | --- | --- | --- |
| `pkg/client` BenchmarkUpload | 100+253+256+274+277 = 1160 | 1 MiB | **1.13 GiB** |
| `pkg/client` BenchmarkUpload_4MB_Regular | 72+72+69+69+72 = 354 | 4 MiB | **1.38 GiB** |
| `pkg/client` BenchmarkListFiles | 5×~2000 | 100×~10 B（setup） | 可忽略 |
| `pkg/server` 4 个 benchmark | — | — | **0**（见 §4：3 个 401 秒失败、1 个静默空跑） |

⇒ `pkg/client` 一侧 ~2.5 GiB / run 全是**夹具的伪副作用**（benchmark 本体只测客户端装配 + HTTP 往返）。

## 3. 机制（为什么是「先快后慢的悬崖」）

1. 写入先进 page cache（~200 MB/s），dirty 页不断累积；
2. 触到 `vm.dirty_ratio`（16 GB runner 约 3.2 GiB，与 `104343040072` 的 2.1+1.1 GiB 命中点吻合）后，
   写者被限速到**设备真实回写带宽**；共享 runner 上该带宽可低至 ~0.15 MB/s；
3. 于是一次 1 MiB 上传要 6.5 s、4 MiB 要 29 s，且**只要还在写就一直慢**；
4. benchmark 的 N 按「~1 s/op」自适应，在此速率下每 count 仍需数十分钟 ⇒ 必然撞 job timeout。

## 4. 顺带发现（同一次取证，同一个 PR 修复）

1. **`pkg/server` 的 3/4 个 benchmark 在 CI 里是红的、却被 job 绿遮住**：
   `BenchmarkUpload` / `BenchmarkDownload` / `BenchmarkConcurrentUploads` 全部
   `upload #0 failed: status=401`（`auth: 未配置任何凭据且不允许无认证访问 … allow_insecure_loopback=false`），
   而 `BenchmarkChunkedUpload` 因为**不校验状态码**而在全 401 下「静默通过」（数值无意义），
   期间刷出 **8469 行 WARN**。
   - 更深一层：`benchServer` 是**手搓 `Handlers`**，连 `globalRoot`/`tenants`/`checksumStores` 都没装
     ⇒ 把 401 兜底打开后下一个错误是 **400「无效的文件路径」**。这两个错都源于同一个病根：
     **夹具副本与生产装配（`RegisterRoutes`）长期脱节**，且夹具出事时没有任何用例会红。
2. **`make bench` 吞掉失败**：`go test … | tee build/bench/output.txt` 的管道退出码取自 `tee`
   （POSIX sh 没有 `pipefail`），所以 `go test` 的 `FAIL … exit status 1` 不会让 job 变红
   （`FAIL pkg/server 17.965s` 就发生在**成功**的 run 里）。
3. **`pkg/telemetry` 的 span 缩进无限增长**：`slogTracer.depth` 只 `++` 从不 `--` ⇒ 每请求缩进 +2 空格，
   单行最多 4.3 KB 空白；成功 run 的 job 日志 **26 MB 中 94% 是这些空白**（16 200 行 span 日志）。

## 5. 修复与防回归

- 已做（`perf(bench)`，PR #283）：`pkg/client/benchmark_test.go` 的 mock `/upload` 改为
  「`io.Copy(sha256.New(), f)` + 丢弃」，保留 checksum 校验语义；新增
  `TestMockBenchUploadHandler_DoesNotPersistPayload`（校验 200 通过 + checksum 不符仍 400 + **目录必须为空**）。
- 已做（`fix(bench)`，同批）：
  - `pkg/server/benchmark_test.go` 的夹具改为**走 `RegisterRoutes` 生产装配**（与 `newTestServer`
    同一入口）+ 丢弃式 logger ⇒ 401 与 400 一并消失，也不再刷 8469 行 WARN；
  - 存储根改走 `benchStorageRoot`（Linux = `/dev/shm` tmpfs，其它平台回退 `tb.TempDir()`）——
    `b.TempDir()` 是**每个 count 重建**（实测），所以单 count 峰值 ~0.9 GiB 而不是累加 5 份，tmpfs 完全够；
  - 4 个 benchmark 全部补上状态码/`success` 断言（chunked 不再可能全 401 静默通过）；
  - `make bench` 改为「把 go test 退出码写文件 → 读完再 exit」的**纯 POSIX** 写法（dash 没有 `pipefail`，
    也不用改 `SHELL`）；实测旧写法 `false | tee` 退出 0、新写法退出 1；
  - 新增 `TestBenchServerAllowsLoopbackUpload` / `TestBenchServerWithChunkedAllowsLoopbackFlow` /
    `TestBenchStorageRootSelection`，把「夹具真的能跑」钉进 `make test`。
- 待做（telemetry 片）：`end()` 里释放 `t.depth`，并加「顺序 span 缩进不增长」的单测。

## 6. 复现与验证

```bash
# 判据：benchmark 期间是否有磁盘副作用（夹具写盘会让目录非空 → 单测红）
go test -count=1 -run TestMockBenchUploadHandler_DoesNotPersistPayload ./pkg/client/

# 计时变化（本机对比基线用；CI 数字见 §2 表）
go test -bench='BenchmarkUpload$' -benchmem -count=1 -run=^$ ./pkg/client/
```

CI 侧：正常 `Benchmark` job 约 **214–247 s**（p50≈230 s，`timeout-minutes: 6` 余量 ~60%），
近 60 次 run 中 ≥360 s 的有 **9 次（15%）**——本修复后该尾部应显著收窄；若仍有慢 run，
先看是不是**别的** benchmark 又开始写盘（本文件 §1 的判据可直接复用）。
