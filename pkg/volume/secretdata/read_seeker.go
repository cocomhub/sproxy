// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// read_seeker.go 是读句柄的**薄装配层**（2026-10-18 重新整合，避免重复实现）。
//
// 架构判断（为什么这里几乎没有逻辑）：
//   - 随机读的**唯一原语**是 `SecretdataFS.OpenRangeRead`——它按 blocklet 索引定位、只拉目标
//     段的密文（底层实现 `RangeReader` 时）、并内置 erasure parity 恢复；**此前就存在**。
//   - 「`RangeReader` → `io.ReadSeeker`」的**通用适配器**是 `pkg/sync.RangeSeeker`——4MiB 窗口、
//     跨窗续读、seek 后重开区间、ctx 由闭包捕获（不存 struct 字段）；**此前就存在**。
//   - 因此 `OpenRead` 只需把二者接起来，再补两件**既有层没有**的事：
//     ① **整文件 SHA-256 校验**（旧 `openRead` 在服务首字节前全量校验，是既有保证，必须保留）；
//     ② **读侧 `MaxFileBytes` 上限**（此前只护写路径）。
//
// ⇒ 本文件**不含**任何 blocklet 定位/缓存/seek 实现（这些若再写一遍即为重复实现 ✗）。

// openReader 打开逻辑路径的读句柄：`sync.RangeSeeker`（复用既有适配器）+ 薄校验装饰器。
// 不落任何临时文件（temp → 0）。
func (s *SecretdataFS) openReader(ctx context.Context, rel string) (io.ReadCloser, error) {
	rel = strings.TrimPrefix(rel, "/")
	s.mu.RLock()
	e, ok := s.index[rel]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("secretdata: 文件 %q 不存在: %w", rel, os.ErrNotExist)
	}
	if e.meta == nil {
		return nil, fmt.Errorf("secretdata: %q 是目录", rel)
	}
	size := e.meta.Original.Size
	// 读侧上限（Step 3）：与写侧同一语义与哨兵（此前 MaxFileBytes 只护写路径）。0 = 不限。
	if s.opts.MaxFileBytes > 0 && size > int64(s.opts.MaxFileBytes) {
		return nil, fmt.Errorf("%w: 大小 %d，上限 %d", ErrMaxFileBytes, size, int64(s.opts.MaxFileBytes))
	}
	// 复用既有适配器：它经 s.OpenRangeRead（本卷唯一的按需随机读原语）实现 io.ReadSeeker。
	// 传入 s 自身：能力探测会下探到 SecretdataFS.OpenRangeRead（非装饰层回显）。
	rs, err := syncpkg.NewRangeSeeker(ctx, s, rel, size)
	if err != nil {
		return nil, fmt.Errorf("secretdata: 构造读句柄失败: %w", err)
	}
	return &verifyOnEOF{inner: rs, want: e.meta.Original.SHA256, full: sha256.New(), seq: true}, nil
}

// verifyOnEOF 是**薄**装饰器：在既有 `RangeSeeker` 之上补「从头顺序读完时校验整文件
// SHA-256」与「校验失败的错误持续返回」。
//
// 语义（相对旧行为的**唯一**变化，已在设计文档与 §9 说明）：旧 `openRead` 在**服务首字节之前**
// 已全量校验（因为它先把整份明文落盘再返回）；现在改为**读到 EOF 时**校验——逐 blocklet 的
// GCM 认证**始终**生效（每个被服务的字节都已认证），且下载路径消费侧 `meta.VerifyReadSeeker`
// 另有逐块校验层 ⇒ 端到端保证不减。任何**改变位置**的 seek 使该顺序校验失效（不猜测重排后的
// 整文件一致性）。
type verifyOnEOF struct {
	inner *syncpkg.RangeSeeker
	want  string
	full  hash.Hash
	seq   bool
	err   error
	done  bool
	// pos 是**本层视角**的当前逻辑位置；started 表示已经读过数据。
	// 用于区分「取大小后回到开头」（ServeContent 的固定姿势，不算破坏顺序读）与「中途跳转」。
	pos     int64
	started bool
}

var (
	_ io.ReadSeeker = (*verifyOnEOF)(nil)
	_ io.Closer     = (*verifyOnEOF)(nil)
)

func (v *verifyOnEOF) Read(p []byte) (int, error) {
	if v.err != nil {
		return 0, v.err // 校验失败持续返回（不静默变回 EOF）
	}
	// 真正开始顺序读之前若位置不在 0（随机读/Range 从中间开始）⇒ 整文件哈希不成立。
	if !v.started && v.pos != 0 {
		v.seq = false
	}
	n, err := v.inner.Read(p)
	if n > 0 {
		v.started = true
		v.pos += int64(n)
		if v.seq {
			v.full.Write(p[:n])
		}
	}
	if errors.Is(err, io.EOF) && v.seq && !v.done {
		v.done = true
		v.seq = false
		if v.want != "" && hex.EncodeToString(v.full.Sum(nil)) != v.want {
			v.err = fmt.Errorf("%w（sha256 不匹配）", shardseal.ErrIntegrityMismatch)
			return n, v.err
		}
	}
	return n, err
}

func (v *verifyOnEOF) Seek(offset int64, whence int) (int64, error) {
	pos, err := v.inner.Seek(offset, whence)
	if err != nil {
		return pos, err
	}
	// **只有「已经读过数据后再跳转」才失效**（P2 修复，2026-10-21）：此前任何 Seek 都清 seq ⇒
	// `http.ServeContent`/`pkg/sync` 的 Range 路径固定先 `Seek(0, SeekEnd)` 取大小、再
	// `Seek(0, SeekStart)` 回到开头，于是「EOF 整文件 SHA-256 校验」在这些消费方**恒不生效**
	// （静默丢失一层纵深校验）。读前取大小/回原位不破坏「从 0 起顺序读」，校验继续有效；
	// 真正从中间开始读（读前位置 ≠ 0）由 Read 内的判据关闭校验。
	if !v.done && v.started {
		v.seq = false
	}
	v.pos = pos
	return pos, err
}

func (v *verifyOnEOF) Close() error { return v.inner.Close() }

// cleanupStaleReadTemp 删除 temp 目录内**旧版本遗留的明文解密临时文件**（`decrypt-*`）。
//
// 为什么安全、且只删这一类：本步起读路径**按需解密、不落任何临时文件** ⇒ 任何 `decrypt-*`
// 都只可能是**升级前崩溃**留下的明文残留 ⇒ 启动时清除可避免明文长期驻留磁盘。写路径的临时物是
// **密文**（加密分块目录），前缀不同、不会被误删。
func cleanupStaleReadTemp(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return // 目录不可读/不存在：非致命（写路径会自行报错）
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "decrypt-") {
			continue
		}
		if rerr := os.Remove(filepath.Join(dir, e.Name())); rerr != nil {
			slog.Warn("secretdata: 清理陈旧明文解密临时文件失败（不影响正确性）",
				"file", e.Name(), "error", rerr)
		}
	}
}
