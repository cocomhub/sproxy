// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// block_write.go 实现跨 FS 块级增量同步的服务端写会话（roadmap 4.3 P2 v2）：
// A 侧引擎对远端目标做块级差异写时，经写面链路调本会话：open（预分配 + 临时文件）→
// write（offset 写）→ close（mtime + 原子 rename 覆盖）。失败/超时 → 丢弃 tmp
// （引擎回退整文件复制——WriteFile 原子路径）。

// blockSession 是一次块级写会话（临时文件 + 锁 + 偏移游标）。
type blockSession struct {
	root   *storage.Root
	tmpRel string   // 同目录临时文件（原子 rename 覆盖用）
	dstRel string   // 目标 rel
	size   int64    // 最终大小（预分配）
	mtime  int64    // 完成时设置（UnixNano；0 = 不设置）
	f      *os.File // 临时文件句柄（write 时使用）
	mu     sync.Mutex
	closed bool
}

// blockSessions 是活跃块写会话表（block_id → session）。
var blockSessions = struct {
	sync.Mutex
	m map[string]*blockSession
}{m: map[string]*blockSession{}}

// blockSessionTTL 是会话超时（未 close 自动清理；防孤儿 tmp）。
const blockSessionTTL = 10 * time.Minute

// OpenBlockWrite 打开块级写会话：目标同目录创建临时文件（O_EXCL）+ 预分配 size。
// 返回会话 id（写/关引用）。目标不存在也允许（块级覆盖新文件——engine 语义）。
func (s *Service) OpenBlockWrite(ctx context.Context, owner, rel, volume string, size, mtime int64) (string, error) {
	tnt := s.rt.volumeTenant(volume, owner)
	if tnt == nil || tnt.Root() == nil {
		return "", &HTTPError{Status: 404, Message: "卷不存在"}
	}
	root := tnt.Root()
	norm, err := pathguard.ValidateFilePath(rel)
	if err != nil {
		return "", &HTTPError{Status: 400, Message: "非法路径"}
	}
	// authorize 的 rel 是 user 桶内相对路径；OpenFile 需 root/user/<rel>（卷根含桶布局）。
	norm = filepath.ToSlash(filepath.Join("user", norm))
	tmpRel := norm + ".block-tmp"
	// 父目录创建（同 WriteFile 语义：中间目录自动建）。
	dirRel := filepath.ToSlash(filepath.Dir(tmpRel))
	if dirRel != "." {
		if merr := root.MkdirAll(dirRel, 0o755); merr != nil {
			return "", fmt.Errorf("块写父目录: %w", merr)
		}
	}
	tf, err := root.OpenFile(tmpRel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("块写会话打开临时文件: %w", err)
	}
	// 预分配（稀疏文件语义：写多少占多少）。
	if size > 0 {
		if err := tf.Truncate(size); err != nil {
			_ = tf.Close()
			_ = root.Remove(tmpRel)
			return "", fmt.Errorf("块写预分配: %w", err)
		}
	}
	sess := &blockSession{root: root, tmpRel: tmpRel, dstRel: norm, size: size, mtime: mtime, f: tf}
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	id := hex.EncodeToString(buf)
	blockSessions.Lock()
	blockSessions.m[id] = sess
	blockSessions.Unlock()
	// 超时自动清理（孤儿 tmp 防护）。
	time.AfterFunc(blockSessionTTL, func() {
		blockSessions.Lock()
		cur, ok := blockSessions.m[id]
		blockSessions.Unlock()
		if ok && cur == sess {
			_ = sess.abort()
		}
	})
	return id, nil
}

// WriteBlock 往会话写一个差异块（offset 写；越界校验）。
func (s *Service) WriteBlock(ctx context.Context, id string, offset int64, data []byte) error {
	blockSessions.Lock()
	sess, ok := blockSessions.m[id]
	blockSessions.Unlock()
	if !ok {
		return &HTTPError{Status: 404, Message: "块写会话不存在或已过期"}
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.closed {
		return &HTTPError{Status: 409, Message: "块写会话已关闭"}
	}
	if offset < 0 || offset+int64(len(data)) > sess.size {
		return &HTTPError{Status: 400, Message: "块写偏移越界"}
	}
	if _, err := sess.f.WriteAt(data, offset); err != nil {
		return fmt.Errorf("块写失败: %w", err)
	}
	return nil
}

// CloseBlockWrite 完成会话：设 mtime + 原子 rename 覆盖目标（失败丢弃 tmp）。
func (s *Service) CloseBlockWrite(ctx context.Context, id string) error {
	blockSessions.Lock()
	sess, ok := blockSessions.m[id]
	delete(blockSessions.m, id)
	blockSessions.Unlock()
	if !ok {
		return &HTTPError{Status: 404, Message: "块写会话不存在或已过期"}
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.closed {
		return nil
	}
	sess.closed = true
	if err := sess.f.Sync(); err != nil {
		_ = sess.f.Close()
		_ = sess.root.Remove(sess.tmpRel)
		return fmt.Errorf("块写 fsync: %w", err)
	}
	if err := sess.f.Close(); err != nil {
		_ = sess.root.Remove(sess.tmpRel)
		return fmt.Errorf("块写关闭: %w", err)
	}
	// mtime 由调用方（引擎）在完成时显式处理；此处不设置（保持 write_ops 单实现门禁）。
	_ = sess.mtime // 保留字段（未来 WriteFile 语义扩展）
	// 原子 rename 覆盖目标（storage.Root.AtomicRename 保持替换语义）。
	if err := sess.root.AtomicRename(sess.tmpRel, sess.dstRel); err != nil {
		_ = sess.root.Remove(sess.tmpRel)
		return fmt.Errorf("块写原子落位: %w", err)
	}
	return nil
}

// abort 放弃会话（超时/错误）：关句柄 + 删 tmp。
func (sess *blockSession) abort() error {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.closed {
		return nil
	}
	sess.closed = true
	_ = sess.f.Close()
	return sess.root.Remove(sess.tmpRel)
}
