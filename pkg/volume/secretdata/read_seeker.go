// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/cocomhub/sproxy/pkg/audit"
	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// read_seeker.go 实现 secretdata 的**按需解密读句柄**（2026-10-18 架构级根治 Step 2）。
//
// 为什么：旧 `openRead` 先把**全部分块密文**复制进 `temp/view-*`，再解密整份明文到
// `temp/decrypt-*` ⇒ 峰值临时盘 ≈ 2× 文件、明文落盘、读侧无大小上限（大文件批量路径爆盘 ✗）。
// Step 1 已去掉密文 staging（2× → 1×）；本文件把明文临时文件也去掉 ⇒ **temp → 0**。
//
// 设计要点：
//   - **按需解密**：只解密「当前读位置所在的 blocklet」并缓存（内存上界 = 单 blocklet，默认
//     64KB–4MB；legacy meta 无 blocklet 索引时退化为单块，已在文档登记）。复用既有段级解密
//     `readChunkRangeBytes`（底层实现 `RangeReader` 时只拉目标段的密文 ⇒ 最小下载；缺失块由
//     erasure parity 恢复的可用性也一并继承）。
//   - **可 seek**（消费侧强要求 `io.ReadSeeker`：`pkg/files/read.go` 断言 +
//     `pkg/files/meta/verify.go` 逐块 `Seek` 校验）⇒ 不牺牲可用性。
//   - **整文件 SHA-256**：仅在**从头顺序读完**时于 EOF 校验（`meta.Original.SHA256`）。
//     这是相对旧行为的**语义变化**（旧行为是「首字节之前」已全量校验），已在设计文档与
//     §9 明确；逐 blocklet 的 GCM 认证**始终**生效（每个被服务的字节都已认证），且下载路径
//     消费侧 `meta.VerifyReadSeeker` 另有逐块校验层 ⇒ 端到端保证不减。任何 seek 使该顺序校验
//     失效（不猜测重排后的整文件一致性）。
//   - **不持长期句柄**：每次按需读取都短持有底层句柄（不锁卷内文件，Windows 删除不受阻）。
//
// 并发：一个句柄由单个消费者使用（不做内部加锁）；多个消费者各自 `OpenRead` 得到独立句柄。
type fileReader struct {
	s *SecretdataFS
	// ctx 是 open 时的请求 ctx：按需解密要经它做**审计 decrypt span**（与旧 range 读路径同口径）
	// 并让底层读可被取消。`io.Reader` 接口无法逐次传 ctx ⇒ 必须随句柄保存（containedctx 门禁
	// 例外，理由即此）。
	ctx context.Context //nolint:containedctx // 见上：io.Reader 无法逐次传 ctx（审计/取消语义）
	e   *metaEntry

	key  []byte // 文件级派生密钥（每次 open 派生一次）
	salt []byte

	size     int64 // 逻辑文件大小（meta.Original.Size）
	pos      int64 // 当前逻辑读位置
	limitEnd int64 // 本句柄可读上界（范围视图用；全文件句柄 = size）

	// 单 blocklet 明文缓存：buf 覆盖 [bufOff, bufOff+len(buf))。
	bufOff int64
	buf    []byte

	// 整文件顺序校验（仅「从 0 起、未 seek」的顺序读生效）。
	full      hash.Hash
	seq       bool
	verifyErr error

	closed bool
}

var (
	_ io.ReadSeeker = (*fileReader)(nil)
	_ io.Closer     = (*fileReader)(nil)
)

// openReader 打开逻辑路径的按需解密读句柄（不落任何临时文件）。
func (s *SecretdataFS) openReader(ctx context.Context, rel string) (*fileReader, error) {
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
	key, salt, kerr := s.fileLevelKey(e) // 每文件派生一次（与旧读路径同口径）
	if kerr != nil {
		return nil, kerr
	}
	size := e.meta.Original.Size
	return &fileReader{
		s: s, ctx: ctx, e: e, key: key, salt: salt,
		size: size, limitEnd: size,
		full: sha256.New(),
		seq:  true, // 从 0 起且未 seek ⇒ 顺序读，EOF 时校验整文件
	}, nil
}

// Read 按需解密并返回 [pos, pos+len(p)) 的明文（最多到 limitEnd）。
func (r *fileReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, os.ErrClosed
	}
	if r.verifyErr != nil {
		return 0, r.verifyErr // 校验失败的错误持续返回（不静默变回 EOF）
	}
	if r.pos >= r.limitEnd {
		if err := r.finishSequential(); err != nil {
			return 0, err
		}
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if err := r.ensureCached(); err != nil {
		return 0, err
	}
	avail := r.bufOff + int64(len(r.buf)) - r.pos
	if avail <= 0 {
		return 0, fmt.Errorf("secretdata: 读缓存未覆盖偏移 %d（内部错误）", r.pos)
	}
	n := min(min(int64(len(p)), avail), r.limitEnd-r.pos)
	copy(p, r.buf[r.pos-r.bufOff:r.pos-r.bufOff+n])
	r.pos += n
	if r.seq {
		r.full.Write(p[:n]) // 顺序读：累加整文件 SHA-256（EOF 校验）
	}
	return int(n), nil
}

// ensureCached 保证当前 pos 落在已解密的缓存区间内（缓存 = 含 pos 的单个 blocklet；
// legacy meta 无 blocklet 索引或 blocklet 不可用时退化为整块）。
func (r *fileReader) ensureCached() error {
	if r.pos >= r.bufOff && r.pos < r.bufOff+int64(len(r.buf)) {
		return nil
	}
	ci, ok := r.chunkAt(r.pos)
	if !ok {
		return fmt.Errorf("secretdata: 偏移 %d 无对应分块（meta 不一致）", r.pos)
	}
	start, end := ci.Offset, ci.Offset+ci.OrigSize
	if bl, bok := blockletAt(ci, r.pos); bok {
		start, end = bl.Offset, bl.Offset+bl.Size
	}
	// 审计：decrypt 阶段 span（与旧 range 读路径同口径；无 ctx logger 时 no-op）。
	decSpan := audit.From(r.ctx).Begin("decrypt")
	plain, err := r.s.readChunkRangeBytes(r.ctx, r.e, r.key, r.salt, ci, start, end)
	if err != nil {
		decSpan.Fail(err)
		return err
	}
	decSpan.End("bytes", int64(len(plain)))
	r.buf, r.bufOff = plain, start
	return nil
}

// chunkAt 返回含 pos 的分块（meta.Chunks 按 Offset 升序，二分定位）。
func (r *fileReader) chunkAt(pos int64) (shardseal.ChunkInfo, bool) {
	chunks := r.e.meta.Chunks
	i := sort.Search(len(chunks), func(i int) bool {
		return chunks[i].Offset+chunks[i].OrigSize > pos
	})
	if i >= len(chunks) {
		return shardseal.ChunkInfo{}, false
	}
	return chunks[i], true
}

// blockletAt 返回含 pos 的 blocklet；无 blocklet 索引或该 blocklet 标记为未使用
// （`Used=false`，例如纠错冗余覆盖之外）时返回 false ⇒ 调用方退化为整块解密（正确性优先）。
func blockletAt(ci shardseal.ChunkInfo, pos int64) (shardseal.BlockletInfo, bool) {
	bls := ci.Blocklets
	i := sort.Search(len(bls), func(i int) bool {
		return bls[i].Offset+bls[i].Size > pos
	})
	if i >= len(bls) {
		return shardseal.BlockletInfo{}, false
	}
	bl := bls[i]
	if !bl.Used || bl.Size <= 0 {
		return shardseal.BlockletInfo{}, false
	}
	return bl, true
}

// Seek 实现 io.Seeker（绝对/相对/从末尾；允许 seek 到 EOF 之后，与 *os.File 一致）。
// 任何改变位置的 seek 都会使「整文件顺序校验」失效（不猜测重排后的整文件一致性）。
func (r *fileReader) Seek(offset int64, whence int) (int64, error) {
	if r.closed {
		return 0, os.ErrClosed
	}
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.pos + offset
	case io.SeekEnd:
		abs = r.size + offset
	default:
		return 0, fmt.Errorf("secretdata: 非法 whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("secretdata: 非法负偏移 %d（whence=%d）", abs, whence)
	}
	if abs != r.pos {
		r.seq = false // 位置变化 ⇒ 不再是「从 0 起的顺序读」
	}
	r.pos = abs
	return abs, nil
}

// finishSequential 在「从 0 起顺序读完」时校验整文件 SHA-256（只校验一次）。
func (r *fileReader) finishSequential() error {
	if !r.seq || r.full == nil {
		return nil
	}
	r.seq = false
	want := r.e.meta.Original.SHA256
	if want == "" {
		return nil
	}
	if hex.EncodeToString(r.full.Sum(nil)) != want {
		r.verifyErr = fmt.Errorf("%w（sha256 不匹配）", shardseal.ErrIntegrityMismatch)
		return r.verifyErr
	}
	return nil
}

// Close 释放缓存（无临时文件可清理——这正是本设计的要点）。
func (r *fileReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.buf = nil
	r.full = nil
	return nil
}

// rangeReadCloser 是 `OpenRangeRead` 的返回类型：**只暴露 io.ReadCloser**（有界区间）。
// 刻意不暴露 Seek：区间视图的 seek 语义（相对区间还是绝对文件偏移）容易误用，而既有
// 调用方（RangeSeeker / DAV 元数据面）只需要有界流。
type rangeReadCloser struct{ r *fileReader }

func (rr *rangeReadCloser) Read(p []byte) (int, error) { return rr.r.Read(p) }
func (rr *rangeReadCloser) Close() error               { return rr.r.Close() }
