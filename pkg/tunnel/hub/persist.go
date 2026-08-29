// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// NodeSnap 是持久化快照中单个节点的离线表示。
// Mux 字段（在线连接）不持久化——重启后节点需重新建立连接。
type NodeSnap struct {
	ID         NodeID    `json:"id"`
	Mesh       string    `json:"mesh,omitempty"`
	Addr       string    `json:"addr,omitempty"`
	Secret     string    `json:"secret,omitempty"`
	RealNodeID string    `json:"real_node_id,omitempty"`
	Connected  time.Time `json:"connected"`
	Services   []Service `json:"services,omitempty"`
}

// MessageSnap 是一个 peer 的信令收件箱快照。
type MessageSnap struct {
	Peer string      `json:"peer"`
	Msgs []SignalMsg `json:"msgs"`
}

// Snapshot 是 Hub 状态的完整快照：节点注册 + 信令收件箱。
type Snapshot struct {
	Nodes    []NodeSnap    `json:"nodes,omitempty"`
	Messages []MessageSnap `json:"messages,omitempty"`
}

// snapshotFn 生成一次快照；返回 nil 表示无可持久化变化。
type snapshotFn func() *Snapshot

// Persister 将 Hub 状态快照原子写入单个 JSON 文件，并以去抖方式合并高频变更。
//
// 并发语义：变更入口 Schedule 把最新的 snapshotFn 排入 pending 并启动/重置去抖周期，
// 到期后只执行**最后一次**排队的 snapshotFn（中间变更合并为一落盘）。Flush 同步
// 执行 pending（进程优雅停服前调用，确保最后一次变更不丢失）。线程安全。
//
// 注：本实现允许多个排队的 closure 逐个执行（非严格跳过），去抖合并的是
// "多次 Schedule 中最后一次"——每次 timer 触发只执行当时的 pending。高扇出场景
// （多个连接同时注册）由 200ms 窗口自然合并。
type Persister struct {
	path string
	// mu 保护文件写侧并发（Save 唯一入口）与 pending/timer 状态。
	mu sync.Mutex

	// debounce 是去抖窗口：变更密集时合并落盘。0 表示立即。
	debounce time.Duration

	// logger 用于记录损坏文件等非致命告警；nil 时回退 slog.Default()。
	logger *slog.Logger

	pending *snapshotFn // 当前排队待落盘的最新闭包；nil 表示无变更（受 mu 保护）
	timer   *time.Timer // 激活中的去抖计时器；非 nil 表示已调度（受 mu 保护）
}

// NewPersister 创建指向 path 的持久化器。
func NewPersister(path string) *Persister {
	return &Persister{path: path, debounce: 200 * time.Millisecond, logger: slog.Default()}
}

// Load 读取快照文件并解码。
//   - 文件不存在（未持久化过）→ 返回空快照、无错误；
//   - 文件存在但损坏/非法 JSON → 记录 warn、返回空快照、无错误（hub 启动
//     不因持久化文件损坏而失败，也不 panic）；
//   - 其余 I/O 错误（如权限不足）→ 返回 error，由调用方决定是否中止。
func (p *Persister) Load() (*Snapshot, error) {
	raw, err := os.ReadFile(p.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Snapshot{}, nil
		}
		return nil, err
	}
	snap := &Snapshot{}
	if err := json.Unmarshal(raw, snap); err != nil {
		logger := p.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("hub 持久化文件损坏，忽略并启动为空状态", "path", p.path, "error", err)
		return &Snapshot{}, nil
	}
	return snap, nil
}

// Save 原子写快照到 p.path：先写同目录临时文件再 rename（不出现半写文件）。
// 父目录不存在时返回 error。
func (p *Persister) Save(snap *Snapshot) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	dir := filepath.Dir(p.path)
	tmp, err := os.CreateTemp(dir, filepath.Base(p.path)+".tmp-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // 失败路径清理；成功后 rename 使条目失效，Remove 报错无害。

	if err := json.NewEncoder(tmp).Encode(snap); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, p.path)
}

// Schedule 排队一次快照并在去抖窗口后异步落盘。fn 返回 nil 表示无可持久化变化。
// 多次调用只会让**最后一次**的 fn 在窗口到期后执行（合并），避免注册/信令风暴
// 反复落盘。线程安全。
func (p *Persister) Schedule(fn snapshotFn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending = &fn
	if p.timer == nil {
		delay := p.debounce
		t := time.AfterFunc(delay, func() {
			p.mu.Lock()
			fnPtr := p.pending
			p.pending = nil
			p.timer = nil
			p.mu.Unlock()
			if fnPtr == nil {
				return
			}
			fn := *fnPtr
			if snap := fn(); snap != nil {
				_ = p.Save(snap)
			}
		})
		p.timer = t
	} else {
		// timer 已在跑：更新 pending 并重置窗口覆盖最新变更。
		_ = p.timer.Reset(p.debounce)
	}
}

// Flush 同步执行当前排队的快照（若存在）。用于进程优雅停服前确保状态不丢失；
// 无 pending 时是 no-op。返回是否实际落盘。
func (p *Persister) Flush(curr *Snapshot) bool {
	p.mu.Lock()
	fnPtr := p.pending
	p.pending = nil
	t := p.timer
	p.timer = nil
	p.mu.Unlock()
	if t != nil {
		t.Stop()
	}
	if curr == nil && fnPtr == nil {
		return false
	}
	if curr != nil {
		_ = p.Save(curr)
		return true
	}
	fn := *fnPtr
	if snap := fn(); snap != nil {
		_ = p.Save(snap)
		return true
	}
	return false
}

// FlushFn 同步执行给定 snapshotFn 并落盘（curr nil 语义）。用于服务端在信令
// 变更后立即持久化当前收件箱状态。返回是否落盘。
func (p *Persister) FlushFn(fn snapshotFn) bool {
	p.mu.Lock()
	p.pending = nil
	t := p.timer
	p.timer = nil
	p.mu.Unlock()
	if t != nil {
		t.Stop()
	}
	if snap := fn(); snap != nil {
		_ = p.Save(snap)
		return true
	}
	return false
}
