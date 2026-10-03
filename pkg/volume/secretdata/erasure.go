// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// XOR 奇偶纠错（k-of-k+1，纯 XOR、纯 stdlib；任务 9d）。Options.Erasure=true 时写路径为
// ≥2 个数据分块生成奇偶校验段（meta.Parity 记录映射）；读路径某分块缺失（底层读失败）
// → 用 parity ^ 其余分块恢复其明文再解密。数学：parity = XOR 全部数据分块明文（按 max
// 长补零对齐）；任一分块丢失 = parity ^ 其余分块（XOR 恒等式），按该分块真实长度截断。
// parity 段自身按统一 blob 格式加密落盘（独立分块文件），有 secret 即可独立解密。

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"path"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// writeErasureParity 在写路径为 out 的 ≥2 个数据分块生成 XOR parity 段：
//  1. 从整文件明文按 meta 分块偏移切出各分块明文；
//  2. 各分块按 max 长补零对齐 → XORParity 得 parity 明文（maxLen 长）；
//  3. 单块加密 parity 明文 → 独立分块 blob，上传到底层数据目录；
//  4. 记录 meta.Parity（FileName/PlainLen/EncSize/ChunkCount），并把上传路径追加到 uploaded。
//
// 须在 encryptMetaBlob 之前调用（meta 需含 parity 引用）。非纠错 / 单分块文件跳过。
//
// 段类型偏差（审查 Minor-3）：parity blob 的 blocklet 用 **Data 类型**而非
// BlockletTypeParity(0x13)——validateBlocklets 拒绝与已用区间重叠的非数据段，单个 0x13
// 段会与非空数据区间冲突；parity 语义由 meta.Parity 承载（等价、不误读），0x13 常量保持
// 预留槽位供未来演化。
func (s *SecretdataFS) writeErasureParity(ctx context.Context, container string, out *shardseal.EncryptionResult, data []byte, mtime int64, uploaded *[]string) error {
	plains, err := chunkPlainTexts(data, out.Meta.Chunks)
	if err != nil {
		return err
	}
	parity, maxLen, perr := parityPlainOf(plains)
	if perr != nil {
		return fmt.Errorf("secretdata: parity 计算失败: %w", perr)
	}
	enc, blob, berr := s.encryptContentSingle(parity, "parity")
	if berr != nil {
		return fmt.Errorf("secretdata: parity 分块加密失败: %w", berr)
	}
	p := path.Join(container, enc.ChunkNames[0])
	if werr := s.inner.WriteFile(ctx, p, bytes.NewReader(blob), int64(len(blob)), s.blobMTime(mtime)); werr != nil {
		return fmt.Errorf("secretdata: 上传 parity %s 失败: %w", enc.ChunkNames[0], werr)
	}
	*uploaded = append(*uploaded, p)
	out.Meta.Parity = &shardseal.ParityInfo{
		FileName:   enc.ChunkNames[0],
		PlainLen:   maxLen,
		EncSize:    int64(len(blob)),
		ChunkCount: len(out.Meta.Chunks),
	}
	return nil
}

// chunkPlaintexts 从整文件明文按 meta 各分块 Offset/OrigSize 切出分块明文（顺序与
// meta.Chunks 一致）。越界 fail-closed。
func chunkPlainTexts(data []byte, chunks []shardseal.ChunkInfo) ([][]byte, error) {
	out := make([][]byte, 0, len(chunks))
	for _, c := range chunks {
		if c.Offset < 0 || c.Offset+c.OrigSize > int64(len(data)) {
			return nil, fmt.Errorf("secretdata: 分块 %s 区间 [%d,%d) 越出明文 %d",
				c.FileName, c.Offset, c.Offset+c.OrigSize, len(data))
		}
		out = append(out, data[c.Offset:c.Offset+c.OrigSize])
	}
	return out, nil
}

// parityPlainOf 计算 k 个数据分块明文的 XOR parity（k-of-k+1）。各块按 max 长左对齐补零
// 对齐后 XOR → parity（maxLen 长）。len(blocks)<2 不做纠错。返回 (parity, maxLen)。
func parityPlainOf(blocks [][]byte) ([]byte, int64, error) {
	if len(blocks) < 2 {
		return nil, 0, fmt.Errorf("secretdata: 需 ≥2 个数据分块才可生成 parity（got %d）", len(blocks))
	}
	maxL := 0
	for _, b := range blocks {
		if len(b) > maxL {
			maxL = len(b)
		}
	}
	if maxL == 0 {
		return nil, 0, fmt.Errorf("secretdata: 数据分块为空，无法生成 parity")
	}
	padded := make([][]byte, len(blocks))
	for i, b := range blocks {
		p := make([]byte, maxL)
		copy(p, b)
		padded[i] = p
	}
	parity, err := shardseal.XORParity(padded...)
	if err != nil {
		return nil, 0, err
	}
	return parity, int64(maxL), nil
}

// recoverMissingPlaintext 用 XOR parity 恢复缺失分块 ci 的明文（长度 == ci.OrigSize）。
// 过程：解密 parity 明文（maxLen 长）→ 读其余分块 blob 并解出明文（各左对齐补零到
// maxLen）→ RecoverFromParity(parity ^ 其余) → 按 ci.OrigSize 截断。其余分块任一缺失
// （k-of-k+1 只容忍 1 块丢失）或解密失败即 fail-closed。
func (s *SecretdataFS) recoverMissingPlaintext(ctx context.Context, e *metaEntry, ci shardseal.ChunkInfo) ([]byte, error) {
	p := e.meta.Parity
	if p == nil {
		return nil, fmt.Errorf("secretdata: 文件 %q 无 parity 记录，无法恢复分块 %s", e.meta.Original.Name, ci.FileName)
	}
	if int64(ci.OrigSize) > p.PlainLen {
		return nil, fmt.Errorf("secretdata: 分块 %s OrigSize=%d 越出 parity PlainLen=%d", ci.FileName, ci.OrigSize, p.PlainLen)
	}
	parBlob, err := readBlob(ctx, s.inner, path.Join(s.dataSeg(e), p.FileName))
	if err != nil {
		return nil, fmt.Errorf("secretdata: 读 parity %s 失败: %w", p.FileName, err)
	}
	parPlain, derr := shardseal.DecryptChunkStandalone(s.secret, parBlob)
	if derr != nil {
		return nil, fmt.Errorf("secretdata: 解密 parity %s 失败: %w", p.FileName, derr)
	}
	others := make([][]byte, 0, len(e.meta.Chunks)-1)
	for _, oci := range e.meta.Chunks {
		if oci.Index == ci.Index {
			continue
		}
		b, oerr := s.readChunkBlob(ctx, e, oci)
		if oerr != nil {
			return nil, fmt.Errorf("secretdata: 恢复需其余分块齐全（%s 缺失）: %w", oci.FileName, oerr)
		}
		opl, perr := shardseal.DecryptChunkStandalone(s.secret, b)
		if perr != nil {
			return nil, fmt.Errorf("secretdata: 解密分块 %s 失败: %w", oci.FileName, perr)
		}
		padded := make([]byte, p.PlainLen)
		copy(padded, opl)
		others = append(others, padded)
	}
	rec, rerr := shardseal.RecoverFromParity(parPlain, others, ci.Index)
	if rerr != nil {
		return nil, rerr
	}
	return rec[:ci.OrigSize], nil
}

// readChunkBlobOrErase 读取分块 ci 的 blob；底层缺失且纠错启用 → 恢复明文后用**文件级
// key/salt** 重加密为单 blocklet blob（DecryptFile 走统一 blob 解密，blob 内 salt==meta.Salt
// 一致性校验可过；长度 == ci.OrigSize、整文件 SHA-256 全量校验仍有效）。
func (s *SecretdataFS) readChunkBlobOrErase(ctx context.Context, e *metaEntry, ci shardseal.ChunkInfo) ([]byte, error) {
	blob, err := s.readChunkBlob(ctx, e, ci)
	if err == nil {
		return blob, nil
	}
	if !s.opts.Erasure || e.meta.Parity == nil {
		return nil, err
	}
	plain, perr := s.recoverMissingPlaintext(ctx, e, ci)
	if perr != nil {
		return nil, fmt.Errorf("secretdata: 读分块 %s 失败且纠错恢复失败: %w", ci.FileName, perr)
	}
	key, salt, kerr := s.fileLevelKey(e)
	if kerr != nil {
		return nil, kerr
	}
	blob, cerr := shardseal.EncryptChunkStandalone(key, salt, plain, e.meta.AlgoVersion)
	if cerr != nil {
		return nil, fmt.Errorf("secretdata: 重加密恢复分块 %s 失败: %w", ci.FileName, cerr)
	}
	return blob, nil
}

// fileLevelKey 返回条目的文件级派生 key 与盐（meta.Salt base64 解码 + DeriveKey）。
func (s *SecretdataFS) fileLevelKey(e *metaEntry) ([]byte, []byte, error) {
	if e == nil || e.meta == nil {
		return nil, nil, fmt.Errorf("secretdata: 无效条目，无法派生文件密钥")
	}
	sb, serr := base64.StdEncoding.DecodeString(e.meta.Salt)
	if serr != nil || len(sb) != shardseal.SaltLen {
		return nil, nil, fmt.Errorf("secretdata: 文件 meta salt 解码失败: %v", serr)
	}
	key, kerr := shardseal.DeriveKey(s.secret, sb, e.meta.AlgoVersion)
	if kerr != nil {
		return nil, nil, fmt.Errorf("secretdata: 派生文件密钥失败: %w", kerr)
	}
	return key, sb, nil
}

// readChunkRangeBytes 读取分块 ci 在 [offset, end) 内的明文（块级 blocklet 合并解密）。
// 底层 blob 缺失且纠错启用 → 恢复整块明文后直接裁切 [max(offset,ci.Offset), min(end,
// ci.Offset+ci.OrigSize))（re-encrypt 的 blob 其 blocklet 布局与 meta 索引不同，故经
// 明文裁切而非 blob 合并，保证随机读取正确）。
//
// 底层支持 RangeReader 时：只下载含目标区间的 blocklet 段密文（+ 块头部 salt 一次），
// 逐段独立解密——视频关键帧随机访问省去整块下载（边缓冲边播）。无 RangeReader → 整块
// io.ReadAll 回退（零回归）。
func (s *SecretdataFS) readChunkRangeBytes(ctx context.Context, e *metaEntry, keyBytes, salt []byte, ci shardseal.ChunkInfo, offset, end int64) ([]byte, error) {
	if rr := syncpkg.AssertRangeReader(s.inner); rr != nil {
		seg, ok, rerr := s.readChunkRangeBySegments(ctx, rr, e, keyBytes, salt, ci, offset, end)
		if rerr != nil {
			return nil, rerr
		}
		if ok {
			return seg, nil
		}
	}
	blob, berr := s.readChunkBlob(ctx, e, ci)
	if berr == nil {
		return s.decryptChunkRangeBlob(keyBytes, salt, ci, offset, end, blob)
	}
	if !s.opts.Erasure || e.meta.Parity == nil {
		return nil, berr
	}
	plain, perr := s.recoverMissingPlaintext(ctx, e, ci)
	if perr != nil {
		return nil, perr
	}
	cl := max(offset, ci.Offset)
	cr := end
	if ofc := ci.Offset + ci.OrigSize; cr > ofc {
		cr = ofc
	}
	if cl >= cr {
		return nil, nil
	}
	return plain[cl-ci.Offset : cr-ci.Offset], nil
}

// readChunkRangeBySegments 用 RangeReader 按 blocklet 段局部读取：只拉含目标区间的段的
// 密文 [EncOffset, EncOffset+EncSize) + 块头部 salt 一次（防块被替换），逐段独立解密。
// 任一 Range 读取失败 → 返回 (nil, true, err)（fail-closed，不静默回退整块——底层明确
// 支持 Range 却失败应暴露）；底层不实现 RangeReader 时调用方已前置断言，此处恒 ok=true。
func (s *SecretdataFS) readChunkRangeBySegments(ctx context.Context, rr syncpkg.RangeReader, e *metaEntry, keyBytes, salt []byte, ci shardseal.ChunkInfo, offset, end int64) ([]byte, bool, error) {
	segPath := path.Join(s.dataSeg(e), ci.FileName)
	// 拉块头部 salt（[R 128B][8B 长][salt 32B] → salt 位于 blSaltOff 起 32B）。
	head, ok, err := syncpkg.RangeReadAll(rr, ctx, segPath, shardseal.BlockSaltOffset(), shardseal.SaltLen)
	if err != nil {
		return nil, true, err
	}
	if !ok {
		return nil, false, nil // 防御：调用方已断言，此处不应发生。
	}
	if verr := shardseal.VerifyBlockSalt(head, salt); verr != nil {
		return nil, true, verr
	}
	var out []byte
	for _, bl := range ci.Blocklets {
		if !bl.Used || bl.Offset >= end || bl.Offset+bl.Size <= offset {
			continue
		}
		seg, ok, serr := syncpkg.RangeReadAll(rr, ctx, segPath, bl.EncOffset, bl.EncSize)
		if serr != nil {
			return nil, true, serr
		}
		if !ok {
			return nil, false, nil
		}
		plain, derr := shardseal.DecryptBlockletSegmentStandalone(keyBytes, seg, bl, ci.Offset)
		if derr != nil {
			return nil, true, fmt.Errorf("secretdata: 解密 blocklet（offset=%d）失败: %w", bl.Offset, derr)
		}
		// 裁切到 [offset, end)∩blocklet。
		segStart := max(offset, bl.Offset)
		segEnd := end
		if blEnd := bl.Offset + bl.Size; segEnd > blEnd {
			segEnd = blEnd
		}
		out = append(out, plain[segStart-bl.Offset:segEnd-bl.Offset]...)
	}
	return out, true, nil
}

// decryptChunkRangeBlob 按 meta blocklet 索引合并解密分块 blob 在 [offset,end)∩块 的明文
// （原 rangeReadBytes 的 blocklet 合并逻辑，抽方法控制认知复杂度）。
func (s *SecretdataFS) decryptChunkRangeBlob(keyBytes, salt []byte, ci shardseal.ChunkInfo, offset, end int64, blob []byte) ([]byte, error) {
	var out []byte
	for _, bl := range ci.Blocklets {
		if !bl.Used || bl.Offset >= end || bl.Offset+bl.Size <= offset {
			continue
		}
		seg, derr := decryptRangeBlocklet(keyBytes, salt, blob, ci.Offset, bl, offset, end)
		if derr != nil {
			return nil, derr
		}
		out = append(out, seg...)
	}
	return out, nil
}
