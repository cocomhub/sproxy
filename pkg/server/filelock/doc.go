// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package filelock 提供跨进程互斥的文件锁原语（Lock/Unlock）。
//
// **不是选主语义**：多节点写面唯一化走 pkg/leader 的 LeaderElector 抽象
// （含租约/续租/ErrLeaseLost/WriteGuard）。本包只负责「多进程对同一共享
// 文件做 read-modify-write 时互斥」，锁哪个文件、以什么为 key 由调用方决定
// （例如限流计数文件、带宽字节预算文件）。
//
// **进程内互斥不在此提供**：Unix flock 对同进程内同一文件的重复加锁可重入
// （不互斥），调用方需自行用进程内锁串行化。
//
// Windows 无 flock 等价实现（LockFileEx 需 CGO），Lock/Unlock 在 Windows 上
// 是空操作：跨进程互斥降级为调用方的进程内互斥 + 文件系统原子性，尽力而为。
package filelock
