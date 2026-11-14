<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# FTP 外部后端（roadmap 12.2-1）— 归档

> **归档说明**：原文 `docs/designs/2026-09-27-ftp-backend.md`（#617 已实现后回补的设计）已于
> 2026-11-14 归档。配置键/默认值与用法见 `docs/config.md` §FTP 后端配置、`docs/api.md`
> §`GET /api/backends`（现行事实源）；本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 定位与目标
FTP 后端与 sftp/webdav/s3/baidupcs/federated 并列，是**第六个真实外部卷后端**（V3 通用卷模型）：
把任意 FTP 服务（NAS、企业遗留 FTP 服务器等）适配为 `pkg/sync.FS`（7 方法），经
`registry.RegisterBackend("ftp")` 可插拔接入系统盘（`volumes[] type=ftp`）与用户卷（运行时注册）。
**能力边界：只做客户端适配**（sproxy 是 FTP 的客户端），不提供 FTP 服务端。

## 核心设计决策（仍有效）
1. **并发模型：每实例独立控制连接，无共享连接池**。FTP 控制连接命令/响应严格串行配对，跨实例共享
   会串扰（与「禁共享 `http.DefaultClient`」同类要求）；单实例内控制命令经 `mu` 串行化；**数据连接
   （PASV）逐命令建立、不缓存**（FTP 无标准复用协议，逐命令建连最简单可靠）。
2. **只实现被动模式（PASV，RFC 959）**（`openData`）：客户端发 `PASV` → 解析 227 → 主动拨数据连接。
   **不实现主动模式（PORT）**——sproxy 是服务器侧客户端，通常处 NAT/防火墙后，被动模式是标准选择。
   - **PASV 回退策略**：服务端常回 `0.0.0.0`/`127.0.0.1`（NAT/网关简化实现）；此时若控制连接目标
     是真实非回环 IP，回退用**控制主机地址**——与 curl/lftp 同策略。
   - **EPSV 未实现**（IPv6 场景缺口，见片划分）。
3. **fail-closed 面**：`url` 强制 `ftp` scheme（`ftps://`/`sftp://` 直接构造失败，不静默明文回落）；
   无匿名回落（`password` 必填）；Close 后一切操作 fail-fast；未知 LIST 行类型报错（不猜）；
   卷不可达 ⇒ `Ping` 报错 ⇒ 卷状态 degraded 可观测（不假装 healthy）。
4. **路径契约对齐 `pkg/sync.FS`**：`ListDir` 返回条目的 `Path` 为**完整相对路径**（引擎递归/rename
   依赖）；`root` 归一后 `abs(rel)` 映射远端绝对路径。**路径穿越防御由上层把关**（ValidateFilePath 等），
   FTP 层 `abs` 只做拼接——这与本地卷存在差异面（见缺口 G2）。
5. **无自动重连**：连接中断（网络/服务端 421/超时）后 FTPFS 进入不可用态，后续操作报错；恢复依赖
   上层（同步任务失败 → SyncManager 重试 → 重新构造/查询卷）。与 sftp（无自动重连）同策略，属**已知边界**。
6. **超时分层**：拨号（控制连接）默认 10s；登录握手同拨号 deadline（登录后清除）；数据连接拨号固定
   30s；数据命令尾部响应（226）5s 有界读；**控制连接命令响应无超时**（对端挂死会阻塞该实例，
   但单实例模型不阻塞其它卷）。

## 外部契约（FTP 协议语义，改动需重新实证）
- 登录：`220` → `USER` → `331` → `PASS` → `230` → `TYPE I`（二进制；LIST/RETR/STOR 数据面）。
- 尾部响应消费**不能发 NOOP 代读**（NOOP 自身回复会滞留控制流，导致下一命令响应错位）。
- 错误码语义：Delete/MakeDir 的 `550`（不存在/已存在）→ **幂等 nil**；Stat 不存在 → `(nil,nil)`；
  `530`（被踢出）命令失败透传；非 2xx → 统一「被拒」上抛。
- 保留地址：分块/meta 命名与 FTP 无关；`mtime` 不可设置（FTP 无标准机制，同步引擎对 FTP 目标不保留 mtime）。

## 测试面与缺口（建议片）
- 已覆盖：注册/配置校验 fail-closed、Write→Read→Stat→ListDir→Delete 往返、建目录/改名、完整相对路径、
  Stat 缺失 `(nil,nil)`、Delete 缺失幂等、NOOP 探针、错密码构造失败。测试基建 `fakeFTPServer`（纯标准库
  内存 fake，只绑 127.0.0.1；已实现 EPSV 分支但客户端未用）。
- 缺口：G1 连接中断重连语义（P1）· G2 路径穿越防御（P1）· G3 FTPS 握手失败（P2）· G4 数据连接中断 426
  （P2）· G5 EPSV 优先（P2）· G6 PASV 回退（P2）· G7 mtime 语义（P3）。
- 片 2 候选：连接健壮性（G1/G4/G6）· FTPS（`ftps://` + `AUTH TLS`，需与真实 FTPS 服务 E2E）· EPSV/IPv6。

## 已知边界（如实记录，不承诺）
无自动重连；无 FTPS；无 EPSV/IPv6；控制连接命令响应无超时；`mtime` 不可设置。
