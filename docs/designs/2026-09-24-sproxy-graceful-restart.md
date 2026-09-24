# sproxy 优雅重启（USR2）设计

> 批次：D-1（11.6-②）｜状态：草案｜日期：2026-09-24

## 背景 / 目标
- 现状（cmd/sproxy/root.go）：`runSignalHandler` 仅处理 SIGHUP（软配置热载，`handleSighup` 已声明 addr/storage_root/owner_quotas/tls.enabled 等硬配置需重启）/ SIGTERM/SIGINT/SIGQUIT（停服，`handleSignalShutdown`）；**无重启语义**。
- 目标：`kill -USR2` → 新进程接管监听（同端口）→ 旧进程 drain（复用 `handleSignalShutdown` 优雅关闭）→ 退出；已建连接不中断，停机窗口趋近于 0。
- 平台：Linux/macOS（Unix）。Windows 无 SIGUSR2 投递且 `os/exec.ExtraFiles` 不支持跨进程套接字继承 → 文档声明不支持（信号永不投递，行为零变化）。

## 方案选型
- 主方案：**监听器 fd 继承（nginx 风格）**——旧进程把已绑定 TCP listener 经 `ExtraFiles` 传给子进程（fd 3），子进程用 `net.FileListener` 重建同一监听。同端口、无内核依赖；TLS 语义不变（继承的是裸 TCP listener，TLS 握手在新进程 `ServeTLS` 内完成）。
- 备选：SO_REUSEPORT（`ListenConfig.Control` + golang.org/x/sys，Linux/macOS 可用）——两进程各自 bind 同端口内核负载均衡；缺点：旧进程也要改绑（动既有启动路径）、多监听场景复杂。作后续增强，不在一期。
- Windows：不支持（见上）。

## 组件与接口（新增，均在 cmd/sproxy/）
1. `parseRestartEnv()`：读 `SPROXY_INHERIT_FD`（int；子进程启动时存在 = 继承模式）。
2. `inheritListener(addr) (net.Listener, error)`：`SPROXY_INHERIT_FD` 存在 → `net.FileListener(os.NewFile(uintptr(fd), "http"))`；否则回退 `net.Listen("tcp", addr)`（现逻辑原样）。
3. `startRestartChild(ln net.Listener, logger) (*exec.Cmd, error)`：`os.Executable()` + `os.Args[1:]` 原样重启；env 追加 `SPROXY_INHERIT_FD=3`；`cmd.ExtraFiles = []*os.File{lnFile}`；子进程 stdout/stderr 接日志。
4. `waitRestartReady(ctx, addr, timeout) error`：按 `writeBackActualAddr` 后的实际地址轮询 `GET http://127.0.0.1:<port>/readyz`（200=就绪；503/拒绝=未就绪，间隔 200ms）。
5. `handleSignalRestart(cancel, s, h, logger, cfg)`：重启编排（见数据流）。
6. `runSignalHandler` 新增分支：`sig == syscall.SIGUSR2 → handleSignalRestart(...); return`。

## 数据流
1. `kill -USR2 <pid>` → runSignalHandler 收到 USR2。
2. 启动路径改造：startPlainListener/startTLSListener 把绑定成功的 `ln` 存入包级 `restartListener atomic.Value`（旁路监听如 hub TCP/QUIC/gRPC/xfer 不参与继承——新进程会自行重建，接受短暂重建窗口）。
3. handleSignalRestart：`startRestartChild(ln)` → 成功则 `waitRestartReady`（重启超时默认 `max(60s, shutdownTimeout)`）。
4. 就绪 → 复用 `handleSignalShutdown(cancel, s, h)`（drain 语义与 SIGTERM 完全一致：cancel → s.Shutdown → h.Close）→ 返回 → runServer 退出码 0。
5. 新进程侧：启动见 `SPROXY_INHERIT_FD` → 跳过 `net.Listen`（避免 EADDRINUSE）→ Serve 同端口 → `/readyz` 就绪后被旧进程探到。
6. 交接窗口内旧进程继续 serve；新进程就绪后旧进程 drain——两进程各自服务各自连接，无共享内存态。

## 错误处理
- spawn 失败（exec 错误）→ Error 日志，**中止重启**，旧进程继续服务（fail-safe 不自杀）。
- 子进程启动即崩 / readyz 一直不就绪 → 超时 → Error 日志 + best-effort SIGTERM 子进程，旧进程继续服务。
- drain 超时：沿用 handleSignalShutdown 既有 30s 默认 + Error 日志（现状语义不变）。
- 继承 fd 非法（指向非监听 fd）→ FileListener 失败 → fail-fast 拒绝启动（避免误接管）。

## 测试 + 变异点
- 单测（信号注入）：`testSignalCh` 注入 USR2 → 断言走 handleSignalRestart 而非 handleSignalShutdown（mock spawn 注入点）。**变异**：USR2 误走 shutdown → 红。
- 单测（编排）：mock 子进程（httptest 模拟新进程 readyz）→ 断言旧进程在子进程就绪前不 drain。**变异**：删就绪等待直接 drain → 红。
- 集成：真实 listener 经 ExtraFiles 传 helper 子进程 → 断言 FileListener 可 accept 且地址一致。**变异**：子进程回退 net.Listen → EADDRINUSE → 红。
- 超时中止：readyz 永不就绪 → 断言旧进程存活未 drain。**变异**：超时仍 drain → 红。
- 零回归：既有 SIGTERM/SIGHUP 测试保持全绿。

## 片划分
- P1：parseRestartEnv + inheritListener + 启动路径接缝（restartListener 存 ln）+ FileListener 集成测试。
- P2：startRestartChild + waitRestartReady + handleSignalRestart 编排 + 单测（mock spawn/ready）。
- P3：runSignalHandler USR2 分支 + 真实二进制端到端（USR2 → 新进程接管 → 旧进程退出 → 期间 HTTP 请求不断）+ 信号表文档。

## 风险与零回归保证
- 零回归：USR2 为新增信号（此前无人发送）；SIGTERM/SIGHUP 路径代码不动；无 `SPROXY_INHERIT_FD` env 时启动路径完全不变。
- 双进程短暂共存：存储层文件锁为 per-fd 建议锁不互斥；重叠期写语义不变，但新进程看不到旧进程内存态（分块上传会话等）——记录为已知限制，建议低峰期重启；11.11 写面唯一后窗口内写面由单写主约束。
- TLS：新进程独立跑 certmgr，auto_tls 自签证书会重新生成 → 客户端若 pin 证书指纹会失效；文档提示生产配置 tls.cert_file/key_file 固定证书。
- Windows：信号不投递 + ExtraFiles 不可用 → 特性天然关闭（Unix-only 文件加 build tag），行为零变化。
