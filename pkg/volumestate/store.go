// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package volumestate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// stateDirName 是受管 staging 的落点（与容量账本 `<root>/meta/quota/<卷>` 同约定）。
const stateDirName = "state"

// snapshotFile 是目录快照文件名。
const snapshotFile = "entries.json"

// maxSnapshotBytes 是单个快照的读取上限（防被注入超大 JSON 的 OOM DoS）。
const maxSnapshotBytes = 8 << 20

// Store 是受管 staging 落盘区：`<base>/<卷名>/<FS 路径>/entries.json`。
//
// 为何镜像目录结构而非扁平 hash：① 卷删除可整树回收（`RemoveVolume`）；
// ② 祖先失效可整子树删除（无需读-改-写）；③ 运维可直接看懂。
// FS 路径段由卷层保证安全（无 `..`/盘符），本层再归一一次（纵深防御）。
type Store struct {
	base     string // <storage_root>/meta
	log      *slog.Logger
	maxBytes int64 // 0 = 不落盘（仅内存）
}

// NewStore 构造落盘区。base 为 `<storage_root>`（内部拼 `meta/state`）。
// maxBytes<=0 ⇒ 不落盘（缓存仅内存，零磁盘占用）。
func NewStore(storageRoot string, maxBytes int64, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{log: log, maxBytes: maxBytes}
	if maxBytes > 0 && strings.TrimSpace(storageRoot) != "" {
		// 不在此扫描清理 *.tmp：单文件快照写入是「WriteFile(tmp) + Rename」，崩溃残留的 tmp
		// 会被**同名下次保存覆盖**；卷删除时整目录回收（RemoveVolume）。避免启动期递归
		// 遍历（无谓 I/O 且引入符号链接 TOCTOU 面）。
		s.base = filepath.Join(storageRoot, "meta", stateDirName)
	}
	return s
}

// Enabled 报告是否落盘。
func (s *Store) Enabled() bool { return s != nil && s.base != "" && s.maxBytes > 0 }

// pathFor 返回某卷某目录的快照绝对路径。
func (s *Store) pathFor(vol, fsPath string) (string, error) {
	if !s.Enabled() {
		return "", errors.New("volumestate: 未启用落盘")
	}
	if vol == "" || strings.ContainsAny(vol, `/\`) || vol == "." || vol == ".." {
		return "", fmt.Errorf("volumestate: 非法卷名 %q", vol)
	}
	rel := normalizePath(fsPath)
	parts := []string{s.base, vol}
	if rel != "" {
		for seg := range strings.SplitSeq(rel, "/") {
			if seg == "" || seg == "." || seg == ".." {
				return "", fmt.Errorf("volumestate: 非法路径段 %q", seg)
			}
			parts = append(parts, seg)
		}
	}
	return filepath.Join(append(parts, snapshotFile)...), nil
}

// Load 读取快照；缺失/损坏 ⇒ (nil, false)（调用方回源）。
func (s *Store) Load(vol, fsPath string) (*DirSnapshot, bool) {
	p, err := s.pathFor(vol, fsPath)
	if err != nil {
		return nil, false
	}
	f, oerr := os.Open(p)
	if oerr != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	data, rerr := io.ReadAll(io.LimitReader(f, maxSnapshotBytes+1))
	if rerr != nil || int64(len(data)) > maxSnapshotBytes {
		return nil, false
	}
	var snap DirSnapshot
	if jerr := json.Unmarshal(data, &snap); jerr != nil {
		return nil, false
	}
	if snap.Volume != vol { // 归属校验：串卷快照一律丢弃（fail-closed 到回源）
		return nil, false
	}
	return &snap, true
}

// Save 原子写快照（tmp + rename）。失败仅日志（快照是加速层，不是权威）。
func (s *Store) Save(vol, fsPath string, snap *DirSnapshot) {
	p, err := s.pathFor(vol, fsPath)
	if err != nil || snap == nil {
		return
	}
	data, merr := json.Marshal(snap)
	if merr != nil {
		return
	}
	if int64(len(data)) > maxSnapshotBytes {
		return // 超大目录不落盘（内存 LRU 仍生效）
	}
	if derr := os.MkdirAll(filepath.Dir(p), 0o755); derr != nil {
		s.warn("卷状态快照建目录失败", "path", p, "error", derr)
		return
	}
	tmp := p + ".tmp"
	if werr := os.WriteFile(tmp, data, 0o644); werr != nil {
		s.warn("卷状态快照写入失败", "path", tmp, "error", werr)
		return
	}
	if rerr := os.Rename(tmp, p); rerr != nil {
		_ = os.Remove(tmp)
		s.warn("卷状态快照 rename 失败", "path", p, "error", rerr)
	}
}

// Remove 删除单个目录快照（及空父目录）。best-effort。
func (s *Store) Remove(vol, fsPath string) {
	p, err := s.pathFor(vol, fsPath)
	if err != nil {
		return
	}
	_ = os.Remove(p)
	// 回收空目录（到卷级别为止；失败无所谓）。
	for dir := filepath.Dir(p); strings.HasPrefix(dir, s.base); dir = filepath.Dir(dir) {
		if dir == s.base || os.Remove(dir) != nil {
			return
		}
	}
}

// RemoveAll 清空全部落盘快照（对应 `POST /api/volumes/refresh` 无卷名的「全部卷」形态）。
func (s *Store) RemoveAll() {
	if !s.Enabled() {
		return
	}
	_ = os.RemoveAll(s.base)
}

// RemoveVolume 整卷回收（卷删除/卸载时调用）。
func (s *Store) RemoveVolume(vol string) {
	if !s.Enabled() || vol == "" {
		return
	}
	_ = os.RemoveAll(filepath.Join(s.base, vol))
}

func (s *Store) warn(msg string, args ...any) {
	if s.log != nil {
		s.log.Warn(msg, args...)
	}
}
