<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sproxy 优雅重启（SIGUSR2）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-sproxy-graceful-restart.md` 已于 2026-11-14 归档。
> 运维用法见 `docs/config.md` §优雅重启（SIGUSR2）。本文件只保留**设计意图 / 取舍 / 外部契约 /
> 仍有效的判断**。

## 背景
`runSignalHandler` 原有 SIGHUP（软配置热载）与 SIGTERM/SIGINT/SIGQUIT（停服），**无重启语义**。
目标：`kill -USR2` → 新进程接管监听（同端口）→ 旧进程 drain → 退出，已建连接不中断，停机窗口趋近 0。

## 方案选型（仍有效）
- **主方案 = 监听器 fd 继承（nginx 风格）**：旧进程把已绑定 TCP listener 经 `ExtraFiles` 传给子进程
  （fd 3），子进程 `net.FileListener` 重建同一监听。同端口、无内核依赖；**继承的是裸 TCP listener**，
  TLS 握手仍在新进程 `ServeTLS` 内完成（TLS 语义不变）。
- **备选 SO_REUSEPORT 留后续**：两进程各自 bind 同端口由内核负载均衡；缺点是要改旧进程既有启动路径、
  多监听场景复杂 ⇒ 不在一期（不在本期引入的风险）。
- **Windows 不支持**：无 SIGUSR2 投递且 `os/exec.ExtraFiles` 不支持跨进程套接字继承 ⇒ 特性天然关闭
  （Unix-only build tag），行为零变化。

## 定案与取舍（仍有效）
1. **fail-safe 优先于「一定重启成功」**：spawn 失败 / 子进程启动即崩 / `/readyz` 超时 ⇒ 记 Error 并
   **中止重启，旧进程继续服务**（不自杀）。重启是「尽力」，服务可用性是红线。
2. **就绪门（`/readyz`）是交接关键**：旧进程在**新进程就绪前不得 drain**（否则出现真空窗口）；
   `/readyz` 未就绪（503/拒绝）视为未就绪，间隔 200ms 轮询，超时默认 `max(60s, shutdownTimeout)`。
3. **drain 复用 `handleSignalShutdown`**：与 SIGTERM 语义完全一致（`cancel → s.Shutdown → h.Close`），
   不新造第二套关闭逻辑。
4. **旁路监听不继承**：hub TCP/QUIC/gRPC/xfer 等由新进程自行重建（接受短暂重建窗口）——只继承主 HTTP
   监听，避免多 fd 继承的复杂度。
5. **fd 非法 fail-fast**：继承 fd 指向非监听 fd ⇒ `FileListener` 失败 ⇒ 拒绝启动（避免误接管）。

## 外部契约 / 踩坑（仍有效）
- **双进程短暂共存期**：存储层文件锁为 per-fd 建议锁不互斥；重叠期写语义不变，但新进程**看不到旧进程
  内存态**（分块上传会话等）——已知限制，建议低峰期重启。
- **TLS 证书**：新进程独立跑 certmgr，`auto_tls` 自签证书会**重新生成** ⇒ 客户端 pin 证书指纹会失效；
  生产应固定 `tls.cert_file`/`tls.key_file`。
- **信号新增无回归**：USR2 此前无人发送；无 `SPROXY_INHERIT_FD` env 时启动路径完全不变。

## 落地指针
- `cmd/sproxy/graceful_restart_unix.go`：`inheritRestartFd` / `getRestartListener` / `startRestartChild` /
  `waitRestartReady` / `restartTimeoutFor` / `handleSignalRestart`（`restartEnvKey`=`SPROXY_INHERIT_FD`，
  `restartFd`=3）。
- `cmd/sproxy/restart_signal_unix.go`（`registerRestartSignalUnix` / `isRestartSignalUnix` /
  `inheritListener`）与 `restart_signal_windows.go`（恒关闭桩）、`restart_signal_stub.go`。
- 测试：`cmd/sproxy/graceful_restart_unix_test.go`。
