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
	// mu 保护文件写侧并发（Save / saveSnapshotLocked 共用 writeFile 落盘）与
	// pending/timer 状态。快照生成 + 落盘在临界区内原子完成（I1）。
	mu sync.Mutex

	// debounce 是去抖窗口：变更密集时合并落盘。0 表示立即。
	debounce time.Duration

	// lg 用于记录损坏文件等非致命告警与落盘失败日志；nil 时回退 slog.Default()
	// （经 logger() 访问，日志行号指向真实调用点，不做方法封装转发）。
	lg *slog.Logger

	pending *snapshotFn // 当前排队待落盘的最新闭包；nil 表示无变更（受 mu 保护）
	timer   *time.Timer // 激活中的去抖计时器；非 nil 表示已调度（受 mu 保护）
}

// NewPersister 创建指向 path 的持久化器。
func NewPersister(path string) *Persister {
	return &Persister{path: path, debounce: 200 * time.Millisecond, lg: slog.Default()}
}

// Path 返回持久化文件路径（供服务层日志/诊断展示）。
func (p *Persister) Path() string { return p.path }

// logger 返回生效的 slog.Logger；lg 字段为 nil 时回落 slog.Default()。
// 所有日志调用点经此访问，保证日志不会因字段未初始化而 panic。
// 注意：不做「记录错误再转发」的封装——每个调用点直接用返回的 logger 打日志，
// 使 slog 的 source 行号指向真实错误发生处（而非本 helper），便于排障定位。
func (p *Persister) logger() *slog.Logger {
	if p.lg != nil {
		return p.lg
	}
	return slog.Default()
}

// maxSnapshotBytes 是持久化快照文件允许的最大字节数（M6）。
// 超出视为文件损坏（拒绝整体读入内存），避免攻击者/事故写入超大文件
// 导致启动时 OOM。
const maxSnapshotBytes = 64 << 20 // 64 MiB

// Load 读取快照文件并解码。
//   - 文件不存在（未持久化过）→ 返回空快照、无错误；
//   - 文件存在但损坏/非法 JSON，或超出 maxSnapshotBytes → 记录 warn、返回空快照、
//     无错误（hub 启动不因持久化文件损坏而失败，也不 panic）；
//   - 其余 I/O 错误（如权限不足）→ 返回 error，由调用方决定是否中止。
func (p *Persister) Load() (*Snapshot, error) {
	fi, err := os.Stat(p.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Snapshot{}, nil
		}
		return nil, err
	}
	if fi.Size() > maxSnapshotBytes {
		// 快速路径：stat 已知超出上限，不读入内存（防启动 OOM）。
		p.logger().Warn("hub 持久化文件超出大小上限，忽略并启动为空状态", "path", p.path, "size", fi.Size(), "max", maxSnapshotBytes)
		return &Snapshot{}, nil
	}
	raw, err := os.ReadFile(p.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Snapshot{}, nil
		}
		return nil, err
	}
	// 权威上限校验（M6/TOCTOU）：stat 之后文件可能被替换/膨胀，
	// 以实际读入长度为准——读入超过上限视为损坏，拒绝解码防 OOM。
	if len(raw) > maxSnapshotBytes {
		p.logger().Warn("hub 持久化文件实际大小超出上限，忽略并启动为空状态", "path", p.path, "size", len(raw), "max", maxSnapshotBytes)
		return &Snapshot{}, nil
	}
	snap := &Snapshot{}
	if err := json.Unmarshal(raw, snap); err != nil {
		p.logger().Warn("hub 持久化文件损坏，忽略并启动为空状态", "path", p.path, "error", err)
		return &Snapshot{}, nil
	}
	return snap, nil
}

// Save 原子写快照到 p.path：先写同目录临时文件再 rename（不出现半写文件）。
// 父目录不存在时返回 error。写失败记录 error 日志（不静默吞掉，I2）。
func (p *Persister) Save(snap *Snapshot) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.writeFile(snap); err != nil {
		p.logger().Error("persist: save failed", "path", p.path, "err", err)
		return err
	}
	return nil
}

// writeFile 假设调用方已持有 p.mu（Save / saveSnapshotLocked 均保证），执行原子写：
// 同目录临时文件 + fsync + rename。返回底层 I/O 错误，由调用方决定记录/上抛。
func (p *Persister) writeFile(snap *Snapshot) error {
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
	if err := os.Rename(tmpName, p.path); err != nil {
		return err
	}
	// M7：显式收紧为 0600——per-node secret 等敏感信息落盘不应被同机其他用户读取。
	// CreateTemp 在 Unix 已建 0600 文件，rename 保留权限；这里再显式 Chmod 一次以
	// 防御依赖 CreateTemp 具体实现（Windows 上 Chmod 只影响只读位，ACL 由系统继承，
	// 属于平台固有限制，文档注明）。
	if err := os.Chmod(p.path, 0o600); err != nil {
		// chmod 失败不视为持久化失败（文件已写入）；记录 warn 供排查权限问题。
		p.logger().Warn("persist: 设置快照文件权限 0600 失败", "path", p.path, "err", err)
	}
	return nil
}

// saveSnapshotLocked 在调用方已持有 p.mu 的前提下，先执行 fn() 生成快照再原子落盘。
// 快照生成与文件写入处于同一临界区，避免"旧快照覆盖新快照"的 lost-update 竞态
// （I1）：并发写者不可能在另一个写者的"读快照→写盘"之间穿插写入更新后再被旧快照
// 覆盖——每次落盘内容都反映获取 p.mu 时刻的最新状态。返回是否实际落盘。
func (p *Persister) saveSnapshotLocked(fn snapshotFn) error {
	snap := fn()
	if snap == nil {
		return nil
	}
	return p.writeFile(snap)
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
			// 快照生成与落盘都持有 p.mu（saveSnapshotLocked），保证与其它写者串行、
			// 不产生旧快照覆盖新快照的 lost-update（I1）。
			p.mu.Lock()
			defer p.mu.Unlock()
			fnPtr := p.pending
			p.pending = nil
			p.timer = nil
			if fnPtr != nil {
				if err := p.saveSnapshotLocked(*fnPtr); err != nil {
					p.logger().Error("persist: save failed", "path", p.path, "err", err)
				}
			}
		})
		p.timer = t
	} else {
		// timer 已在跑：更新 pending 并重置窗口覆盖最新变更。
		_ = p.timer.Reset(p.debounce)
	}
}

// Flush 同步执行当前排队的快照（若存在）。用于进程优雅停服前确保状态不丢失；
// 无 pending 且 curr 为 nil 时是 no-op（返回 nil）。返回落盘错误（I2/M8：不静默吞）。
// 快照生成与落盘在同一临界区（I1）。
func (p *Persister) Flush(curr *Snapshot) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	fnPtr := p.pending
	p.pending = nil
	t := p.timer
	p.timer = nil
	if t != nil {
		t.Stop()
	}
	if curr != nil {
		return p.writeFile(curr)
	}
	if fnPtr != nil {
		return p.saveSnapshotLocked(*fnPtr)
	}
	return nil
}

// FlushFn 同步执行给定 snapshotFn 并落盘（curr nil 语义）。用于服务端在信令
// 变更后立即持久化当前收件箱状态。返回落盘错误（M8：不再以 bool 掩盖失败）。
// 快照生成与落盘在同一临界区（I1）。
func (p *Persister) FlushFn(fn snapshotFn) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending = nil
	t := p.timer
	p.timer = nil
	if t != nil {
		t.Stop()
	}
	return p.saveSnapshotLocked(fn)
}
