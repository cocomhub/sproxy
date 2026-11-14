<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# tun/tap 内核 VPN（虚拟子网接入）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-vpn-tuntap.md` 已于 2026-11-14 归档。
> CLI 用法见 `docs/cli.md` §mesh（`mesh up`）。本文件只保留**设计意图 / 取舍 / 外部契约 /
> 仍有效的判断**。

## 背景 / 目标
`mesh up` 是用户态 SOCKS5（无内核特权）；`mesh connect` 已支持虚拟 IP 寻址（`mesh.VipTable` 数据源 =
hub 权威节点列表 `ListHubNodes` 的 `VirtualIP`）。目标：tun/tap 内核虚拟网卡，整网段透明路由（IP 包按
目标 VIP 选路，无需逐连接转发），虚拟 IP 分配复用 hub 权威 VipTable。**长期 + 平台分片**（Linux tun /
Windows wintun）。

## 定案与取舍（仍有效）
1. **默认形态零回归**：`mesh up` 不带 `--tun` = 用户态 SOCKS5（复用 socks 命令的 mesh 路由，无特权）；
   `--tun` 是附加 flag，`mesh up` 既有路径零改动。
2. **禁静默降级**：`PlatformProbe` 失败 / `Open` EPERM / VIP 未分配 ⇒ **明确报错**（提示 sudo/管理员、
   wintun 驱动缺失），**不静默回落 SOCKS5**。
3. **VipTable 不分叉**：复用 `mesh connect` 同一 hub 权威数据源与同冲突 fail-closed 语义（不另造虚拟 IP 表）。
4. **平台 build tag 隔离**：`pkg/vpn` 平台实现经 build tag（`device_linux.go` / `device_windows.go`），
   未支持平台不参与构建、不引入编译依赖。
5. **单包失败不崩循环**：单包 dial 失败 ⇒ 丢包 + 计数日志，与 `meshForwardListen` 的 per-conn 失败处理同构。
6. **真设备 e2e 不冒充已验证**：CI 无特权环境 ⇒ 标注后续片（特权环境）单独验证。

## 外部契约 / 踩坑
- MTU 建议 1400（防 VPN 隧道内分片）；与实际隧道不符仅 Warn + 文档建议值，不强制。
- 特权要求、驱动依赖、wintun 许可证全部文档化（长期特性明示）。
- `TUNDevice` 接口：`Open(name)` / `SetMTU(int)` / `Addr()`；`Router.Serve(ctx)` 主循环 = 读 IP 包 →
  解析 dst → `vipTable.NodeByAddr` → dial 建链 → 写包。

## 落地指针
- `pkg/vpn/`：`device.go`（`TUNDevice` / `PlatformProbe`）、`device_linux.go` / `device_windows.go` /
  `device_default.go` / `platform_unsupported.go`（平台隔离）、`router.go`（`NewRouter` / `Router`）。
- `cmd/sclient/mesh_up.go`：`newCmdMeshUp`（默认 SOCKS5 / `--tun` 装配，`--vip` / `--auto-vip`；
  `--tun` 装配 fail-closed）。
- 测试：`pkg/vpn/device_test.go`、`cmd/sclient/mesh_up_tun_test.go`。
