// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build ignore

// 说明性占位：未支持平台（!linux && !windows）的 platformProbe 与
// newTUNDevice 默认实现集中放在 device_default.go（本文件 build 约束
// 相同会重复定义，故用 ignore 排除——保留文件仅为记录该结论）。
//
// 平台覆盖矩阵：
//   - Linux（device_linux.go）：platformProbe + newTUNDevice（tun 骨架，P1）；
//   - Windows（device_windows.go）：platformProbe + newTUNDevice（wintun 标注，P1）；
//   - 其他平台（device_default.go）：platformProbe + newTUNDevice（fail-closed stub）。
package vpn
