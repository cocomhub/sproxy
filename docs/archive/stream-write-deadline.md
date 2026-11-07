# 长时流式响应被 server_timeouts.write 掐断（2026-11-07）

## 现象（线上复盘）

`sclient download <大文件>` 反复失败，每次失败前日志固定形如：

```
level=WARN msg=下载中断，准备续传重试 attempt=1 offset=3342336 error="写入文件失败: decrypt stream: read chunk: unexpected EOF"
...
sclient: 下载失败: 下载失败（已保留断点；再次执行同一命令会自动续传）: 写入文件失败: decrypt stream: read chunk: unexpected EOF
```

关键观察（定位用的三点）：

1. **不同文件的首错偏移一致**（多个文件都是 `offset=3342336` 等），说明与文件内容无关；
2. **并发下载几乎同时失败**（相隔数秒），说明触发点是**共享的时间性事件**，不是数据损坏；
3. 偏移恒为 64 KiB 的整数倍（`3342336 = 51 × 65536`），说明断在隧道加密分块边界。

## 根因

`http.Server.WriteTimeout` 是**整个响应**的挂钟上限（`net/http` 在 handler 开始前对连接置
`now + WriteTimeout` 的写截止时间）。服务端配置（`config.example.yaml` 示例值
`server_timeouts.write: 30s`）下，任何**响应写出超过 30s** 的请求都会被服务端单方面关闭：

- 大文件下载（弱网尤其）必然 > 30s；
- `POST /tunnel` 是**长时会话**，其外层响应同样受该超时约束 —— 一条隧道上多个并发下载
  会在同一个 30s 边界集体断流（正是观察 2）。

客户端侧超时（`client.WithNoClientTimeout` / `tunnel.WithNoTimeout`，已在前序修复）只约束
客户端自己的 `http.Client.Timeout`，**管不到服务端**。所以服务端掐断后，隧道客户端解密流
只读到半个帧 → `decrypt stream: read chunk: unexpected EOF`（即原报错）。

续传本身是默认开启且有效的（偏移确实在推进），但每 30s 被服务端切断一次、
每次只推进「带宽 × 30s」，弱网/并发下永远下不完。

## 修复

新增 `pkg/server.WithSlidingWriteDeadline(next, grace)`：把 WriteTimeout 的语义从
「**整响应挂钟**」改为「**单次写入的停滞窗口**」——每次 `Write/WriteHeader/Flush` 都把
底层连接的写截止时间推后 `grace`。

- 持续有写进展 ⇒ 响应可以远超 WriteTimeout（大文件下载、`/tunnel`、SSE、S3/WebDAV GET）；
- `grace` 内无任何写进展（客户端停滞/黑洞/半开连接）⇒ 仍被切断（保留停滞保护）。

装配点：`cmd/sproxy.createHTTPServer` 用 `server.WithSlidingWriteDeadline(handler, cfg.ServerTimeouts.Write)`
包装主 HTTP handler（`Write<=0` 即默认无超时 ⇒ 原样返回，零行为变化）。这样单点覆盖
srvMux 上的全部流式路由。

实现要点：

- 经 `http.ResponseController.SetWriteDeadline`（Go 1.20+）触达底层连接；不支持该能力的
  ResponseWriter 返回 `ErrNotSupported`，忽略即可（退化为原行为）。
- 包装器**显式实现/透传** `Unwrap` / `Flush` / `Hijack`：`auditStatusRecorder`、隧道、
  SSE 都依赖这些能力（漏掉任何一个都会让隧道/中继/流式刷新失效）。
- **有意不实现 `io.ReaderFrom`**：`sendfile` 式单次长写无法在中途推后截止时间，会退化为
  「一个长写」而被掐断；不实现即让 `io.Copy` 落回 32 KiB 分块 `Write`，与逐块推后对齐。

## 证据

- 单测 `pkg/server/write_deadline_test.go`：`WriteTimeout=250ms` + 10×50ms 分步流式响应。
  控制组（不包装）客户端读到半数即 `unexpected EOF`；包装组完整送达。变异（让
  `WithSlidingWriteDeadline` 直接返回 next）⇒ 必红。
- 装配门禁 `cmd/sproxy/write_deadline_wiring_test.go`：断言 `createHTTPServer` 确实挂了
  该包装（`Write>0` 时必须调用 `SetWriteDeadline`；`Write=0` 时不得调用）。变异去掉装配 ⇒ 红。
- **真实端到端**（`D:/dlresume-e2e/wd_e2e.py`）：真实 `sproxy` + 真实 `sclient`，
  `server_timeouts.write: 1s` + 下载带宽 2 MiB/s + 64 MiB 文件 ⇒ 服务端写出耗时 ~32s。

  | 版本 | 结果 |
  |---|---|
  | master（无滑动写截止） | **下载失败**（rc=1），日志含 `decrypt stream: read chunk: unexpected EOF` 与 5 次「下载中断，准备续传重试」 |
  | 修复版 | **完整下载**（rc=0，SHA-256 一致，32.7s），**零**中断记录 |

## 仍有效判断 / 后续

- 客户端续传（恒开启，无 `--resume` flag）+ sidecar 的设计不变；本次只修服务端切断源，
  并把失败文案从「可用 `--resume` 续传」（误导用户手动加 flag）改为「再次执行同一命令会自动续传」。
- `net/http` 在 `Hijack()` 时会 `rwc.SetDeadline(time.Time{})`（已核对 Go 1.27 源码），
  故中继流（`POST /api/relay/stream`，hijack 后自行泵送）**不受** WriteTimeout 影响，本次
  无需改动。
- 若链路中间还有反向代理（nginx/网关）配置了 `proxy_read_timeout` 之类的**总时长**上限，
  那是另一处独立切断源，本修复无法覆盖，需要在部署侧对齐（通常同样应配成空闲/停滞语义）。
