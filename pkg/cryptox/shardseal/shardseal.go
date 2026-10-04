// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package shardseal 实现随机分块 + AES-256-GCM + scrypt 派生的自描述加密分块算法
// （设计 docs/designs/2026-10-01-secret-volume.md）。对外 API：EncryptShards /
// DecryptFile；命名含三段 16hex 校验和截断 + 随机段，meta 含全 stat + 每分块 stat。
package shardseal

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// DefaultBlockPolicy 返回默认分块策略（设计 §4.1：1MB-200MB 随机；blocklet 细分 64KB-4MB
// fixed——块内定长 blocklet，支持视频关键帧随机访问只下载/解密目标段）。
func DefaultBlockPolicy() BlockPolicy {
	return BlockPolicy{
		Mode: "random", Min: 1 << 20, Max: 200 << 20,
		BlockletMode: "fixed", BlockletMin: 64 << 10, BlockletMax: 4 << 20,
	}
}

// Planner 由 BlockPolicy 构造双层规划器（块 BlockPlanner + 块内 blocklet 规划器）。
// 块模式 "random"/"" → RandomPlanner；blocklet 模式 "fixed"/"" → FixedBlockletPlanner，
// "video-keyframe" 预留（未实现）。任一模式未知/预留 → 返回 (nil, nil)，调用方 fail-closed。
func (p BlockPolicy) Planner() (BlockPlanner, BlockletPlanner) {
	bp := p.blockPlanner()
	lp := p.blockletPlanner()
	if bp == nil || lp == nil {
		return nil, nil
	}
	return bp, lp
}

// blockPlanner 构造块规划器（仅 random/空；未知 fail-closed nil）。
func (p BlockPolicy) blockPlanner() BlockPlanner {
	if p.Mode == "" || p.Mode == "random" {
		minSize, maxSize := p.Min, p.Max
		if minSize <= 0 {
			minSize = 1
		}
		if maxSize < minSize {
			maxSize = minSize
		}
		return &RandomPlanner{Min: minSize, Max: maxSize}
	}
	return nil
}

// blockletPlanner 构造 blocklet 规划器（"fixed"/空 默认；"video-keyframe" 经 BlockPolicy
// 内已注入的 Indexer 构造；未知 fail-closed）。默认区间 64KB-4MB，若未配置则以默认补齐。
func (p BlockPolicy) blockletPlanner() BlockletPlanner {
	mode := p.BlockletMode
	if mode == "" {
		mode = "fixed"
	}
	switch mode {
	case "fixed":
		mn, mx := p.BlockletMin, p.BlockletMax
		if mn <= 0 {
			mn = 64 << 10
		}
		if mx < mn {
			mx = mn
		}
		return &FixedBlockletPlanner{Min: mn, Max: mx}
	case "video-keyframe":
		// Indexer 由装配层（secretdata 写路径）按文件类型经 ResolveBlockletMode 注入
		// policy；未注入（无 keyframe 提供者注册）→ fail-closed nil（调用方报错）。
		if p.Indexer == nil {
			return nil
		}
		mn, mx := p.BlockletMin, p.BlockletMax
		if mn <= 0 {
			mn = 64 << 10
		}
		if mx < mn {
			mx = mn
		}
		return &VideoKeyframeBlockletPlanner{Min: mn, Max: mx, Indexer: p.Indexer}
	default:
		return nil
	}
}

// EncryptShards 把本地文件加密为分块 + meta，返回分块与 meta 信息。
// srcFile：原始文件路径；outDir：加密分块输出目录；secret：密钥；policy：分块策略；
// padTarget：meta 加密 padding 目标（整块落盘总长，0=不 padding；secretdata 卷传
// min_block_size 附近值）；v：算法版本（写路径由装配层按 Options.Algorithm 解析，
// 不明文进 blob、仅经 KDF 派生域影响 key）。流程：读全文件 → 分块 → 每块按 blocklet
// 细分加密（块 blob：boot+段序列+index，见 crypto.go）→ 写分块文件 → 生成 meta（全
// stat + 每块 stat + blocklet 索引）→ meta 明文整体加密到 padTarget 并落盘 → 返回包含
// 最终 MetaBlob 的产物。磁盘上不出现明文 meta JSON（含文件名/size/sha256）），meta 名
// 三段真实补齐并锚定最终 blob（首段=明文哈希，中段=总校验和，末段=MetaBlob 哈希）。
func EncryptShards(srcFile, outDir string, secret []byte, policy BlockPolicy, padTarget int, v AlgoVersion) (*EncryptionResult, error) {
	src, err := os.Open(srcFile)
	if err != nil {
		return nil, fmt.Errorf("shardseal: 打开源文件 %s 失败: %w", srcFile, err)
	}
	defer src.Close()

	data, err := io.ReadAll(src)
	if err != nil {
		return nil, fmt.Errorf("shardseal: 读源文件失败: %w", err)
	}
	st, err := os.Stat(srcFile)
	if err != nil {
		return nil, fmt.Errorf("shardseal: stat 源文件失败: %w", err)
	}
	return encryptShards(data, srcMeta{name: filepath.Base(srcFile), size: st.Size(), mtime: st.ModTime(), mode: uint32(st.Mode().Perm())}, outDir, secret, policy, padTarget, v)
}

// EncryptShardsBytes 把已读入内存的明文加密为分块 + meta（EncryptShards 的内存变体）。
// 供 secretdata 写路径直接传入已持有的明文（避免「写临时源 → 再 io.ReadAll」的二次整读，
// 峰值从 2× 文件降到 1×；Imp-2 部分修复）。origName 为逻辑文件名（meta.original.name，
// 与临时源变体行为一致——临时源以 sanitizeName 命名、EncryptShards 取 filepath.Base）。
// 语义与 EncryptShards 相同（统一落盘格式、meta 名三段锚定最终 blob）。
func EncryptShardsBytes(data []byte, origName, outDir string, secret []byte, policy BlockPolicy, padTarget int, v AlgoVersion) (*EncryptionResult, error) {
	return encryptShards(data, srcMeta{name: origName, size: int64(len(data)), mtime: time.Now(), mode: 0o600}, outDir, secret, policy, padTarget, v)
}

// srcMeta 是一次加密的源文件逻辑元数据（EncryptShards 文件变体取 os.Stat、内存变体取
// len/调用方；S107 收敛：origName/size/mtime/mode 四个来源参数归组）。
type srcMeta struct {
	name  string
	size  int64
	mtime time.Time
	mode  uint32
}

// encryptShards 是 EncryptShards 系列的核心（共享实现，控制认知复杂度 #727）。
// data 为整文件明文；src 为逻辑元数据（来源变体提供：文件变体取 os.Stat，内存变体取
// len/调用方）。
func encryptShards(data []byte, src srcMeta, outDir string, secret []byte, policy BlockPolicy, padTarget int, v AlgoVersion) (*EncryptionResult, error) {
	blocksPlanner, blockletsPlanner := policy.Planner()
	if blocksPlanner == nil || blockletsPlanner == nil {
		return nil, fmt.Errorf("shardseal: 未知分块策略 %q 或 blocklet 模式 %q", policy.Mode, policy.BlockletMode)
	}
	blocks, err := blocksPlanner.Plan(int64(len(data)))
	if err != nil {
		return nil, err
	}

	salt, err := newSalt()
	if err != nil {
		return nil, err
	}
	key, err := deriveKey(secret, salt, v)
	if err != nil {
		return nil, err
	}

	totalHex, err := hash16(data)
	if err != nil {
		return nil, err
	}

	res := &EncryptionResult{Meta: &Meta{
		Version:     metaVersion,
		Algorithm:   algorithmName(v),
		AlgoVersion: v,
		KDF:         "scrypt",
		Salt:        toBase64(salt),
		Original: OriginalInfo{
			Name:      src.name,
			Size:      src.size,
			SHA256:    sha256Hex64(data),
			MTime:     src.mtime.UTC().Format(time.RFC3339Nano),
			Mode:      src.mode,
			MediaType: mediaType(src.name),
		},
		Block: policy,
	}}

	names, chunkInfos, cerr := encryptWriteChunks(&chunkPlan{blocks: blocks, blp: blockletsPlanner}, data, key, salt, totalHex, outDir, v)
	if cerr != nil {
		return nil, cerr
	}
	res.ChunkNames = names
	res.Meta.Chunks = chunkInfos
	// **fMP4/关键帧解析失败的文件名日志（2026-10-04）**：解析器（indexer）层无文件名，
	// 此处（有 src.name）在 planner 有失败记录时补一条带文件名的 warn——metric 已精确统计
	// fMP4 计数（sproxy_keyframe_fmp4_total），日志定位具体文件供后续人工确认（两者互补：
	// 计数看趋势、日志看明细）。
	if rep, ok := blockletsPlanner.(failureReporter); ok {
		if fails := rep.Failures(); len(fails) > 0 {
			slog.Warn("shardseal: 视频关键帧解析失败，blocklet 降级 fixed（fMP4/截断等）",
				"file", src.name, "size", src.size, "failures", len(fails))
		}
	}

	metaName, merr := encryptWriteMeta(res, key, salt, totalHex, outDir, padTarget)
	if merr != nil {
		return nil, merr
	}
	res.MetaName = metaName
	return res, nil
}

// encryptWriteMeta 把 meta 明文 JSON 整体加密到 padTarget 落盘并返回 meta 文件名
// （EncryptShards 的 meta 处理，抽方法控制认知复杂度 #727 gocognit=15）。meta 明文 =
// [4B jsonLen][metaJSON][rand padding 到 padTarget]，整体加密为统一格式
// [R][8B 密文长][salt][nonce][ct+tag]。meta 名三段真实并锚定最终 blob：首段 = meta
// 明文哈希前 16，中段 = 原始总校验和前 16，末段 = MetaBlob 哈希前 16——磁盘上不出
// 现明文 meta JSON。
func encryptWriteMeta(res *EncryptionResult, key, salt []byte, totalHex, outDir string, padTarget int) (string, error) {
	metaJSON, err := json.Marshal(res.Meta)
	if err != nil {
		return "", fmt.Errorf("shardseal: meta 序列化失败: %w", err)
	}
	metaOrigHex, err := hash16(metaJSON)
	if err != nil {
		return "", err
	}
	metaBlob, err := encryptMetaJSON(key, salt, metaJSON, padTarget)
	if err != nil {
		return "", fmt.Errorf("shardseal: meta 加密失败: %w", err)
	}
	res.MetaBlob = metaBlob
	metaEncHex, err := hash16(metaBlob)
	if err != nil {
		return "", err
	}
	metaName := MetaName(metaOrigHex, totalHex, metaEncHex)
	if err := os.WriteFile(filepath.Join(outDir, metaName), metaBlob, 0o600); err != nil {
		return "", fmt.Errorf("shardseal: 写 meta %s 失败: %w", metaName, err)
	}
	return metaName, nil
}

// chunkPlan 是 encryptShards 的分块规划结果（块 + blocklet 规划器；S107 收敛：blocks/blp
// 两个规划参数归组）。由 policy.Planner() 一次性产出。
type chunkPlan struct {
	blocks []Block
	blp    BlockletPlanner
}

// failureReporter 是可选接口：blocklet 规划器携带解析失败记录（video-keyframe 模式）。
// 非 nil 时失败记入每个 ChunkInfo.Failures（全密文 meta 内）。
type failureReporter interface {
	Failures() []BlockErrorMsg
}

// encryptWriteChunks 逐块加密并写盘，返回分块文件名与 ChunkInfo（EncryptShards 的
// 分块处理，抽方法控制认知复杂度 #727 gocognit=15）。Index 为 0 基顺序号；每块按
// blocklet 规划细分并记录 BlockletInfo（随机访问索引）。
func encryptWriteChunks(plan *chunkPlan, data []byte, key, salt []byte, totalHex, outDir string, v AlgoVersion) ([]string, []ChunkInfo, error) {
	var names []string
	var chunks []ChunkInfo
	for _, b := range plan.blocks {
		chunk := data[b.Offset : b.Offset+b.Size]
		blocklets, perr := plan.blp.PlanBlocklets(bytes.NewReader(data), int64(len(data)), b.Offset, b.Size)
		if perr != nil {
			return nil, nil, perr
		}
		enc, entries, cerr := encryptBlocklets(key, salt, blocklets, chunk, v)
		if cerr != nil {
			return nil, nil, cerr
		}
		origBlockHex, _ := hash16(chunk)
		encBlockHex, _ := hash16(enc)
		name := ChunkName(origBlockHex, totalHex, encBlockHex)
		if werr := os.WriteFile(filepath.Join(outDir, name), enc, 0o600); werr != nil {
			return nil, nil, fmt.Errorf("shardseal: 写分块 %s 失败: %w", name, werr)
		}
		var blInfos []BlockletInfo
		for i, bl := range blocklets {
			e := entries[i] // 与 blocklets 同序
			if blockletType(bl) == BlockletTypeError {
				// 错误段：诊断信息留在 blob 密文内（全密文、GCM 认证），**不写入
				// meta.Blocklets**——validateChunkBlocklets 要求 blocklet 连续覆盖
				// [Offset, Offset+OrigSize)，错误段 Offset=块末会把覆盖推超界。
				// 失败列表经 planner.Failures() 记入 ChunkInfo.Failures（见下）。
				continue
			}
			segment := chunk[bl.Offset-b.Offset : bl.Offset-b.Offset+bl.Size]
			segHex, _ := hash16(segment)
			blInfos = append(blInfos, BlockletInfo{
				Offset:     bl.Offset,
				Size:       bl.Size,
				EncOffset:  int64(e.EncOffset),
				EncSize:    e.EncSize,
				OrigSHA256: segHex,
				Used:       !bl.Padding,
				Type:       byte(blockletType(bl)),
			})
		}
		names = append(names, name)
		ci := ChunkInfo{
			Index:      len(chunks),
			FileName:   name,
			Offset:     b.Offset,
			OrigSize:   int64(len(chunk)),
			OrigSHA256: origBlockHex,
			EncSize:    int64(len(enc)),
			EncSHA256:  encBlockHex,
			Blocklets:  blInfos,
		}
		// 解析失败信息是**文件级**共享的（planner 生命周期内失败记录，非单块专属）；因
		// 任一块的错误段 JSON 都含全部失败，meta 选择在**每个块**冗余记录同一列表——保证
		// 解密/审计任一块都能看到失败全貌（读取不依赖特定块）。语义注释见 ChunkInfo.Failures。
		if rep, ok := plan.blp.(failureReporter); ok {
			ci.Failures = rep.Failures()
		}
		chunks = append(chunks, ci)
	}
	return names, chunks, nil
}

// DecryptFile 用 meta + 分块还原原始文件到 dstFile。
// meta：meta 内容；chunkDir：分块所在目录；dstFile：还原目标；secret：密钥。
func DecryptFile(meta *Meta, chunkDir, dstFile string, secret []byte) error {
	if err := validateMeta(meta); err != nil {
		return err
	}
	// 每文件只派生一次 key（meta.Salt 是文件级盐；逐块重复 scrypt 在 N=2^17 下
	// 不可接受——S5344 提升参数后派生开销放大，整文件一次派生保持解密线性）。
	// 派生版本由 meta.algo_version 决定（validateMeta 已确保版本注册已知）。
	salt, err := decodeSalt(meta)
	if err != nil {
		return err
	}
	key, err := deriveKey(secret, salt, meta.AlgoVersion)
	if err != nil {
		return err
	}
	mode := os.FileMode(meta.Original.Mode)
	if mode == 0 {
		mode = 0o644
	}
	f, err := os.OpenFile(dstFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("shardseal: 创建还原文件 %s 失败: %w", dstFile, err)
	}
	full := sha256.New() // 还原同时累加整文件 SHA-256，用于 meta.Original.SHA256 全量校验
	if err := writeDecryptedChunks(f, meta, chunkDir, key, salt, full); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("shardseal: 关闭还原文件失败: %w", err)
	}
	// 设计 §5：meta.Original.SHA256 供解密后全量校验。只按等长逐块比对
	// 不够——等长交换/重排分块会静默产出错内容。此处对重组明文做整文件 SHA-256，
	// 与 meta 不一致即判失败（fail-closed，并删除残file）。
	if want := meta.Original.SHA256; want != "" {
		if hex.EncodeToString(full.Sum(nil)) != want {
			_ = os.Remove(dstFile)
			return fmt.Errorf("shardseal: 还原内容完整性校验失败（sha256 不匹配）")
		}
	}
	return nil
}

// decodeSalt 解码 meta 的文件级盐（meta.Salt 为 base64）。空/非法即失败（fail-closed）。
func decodeSalt(meta *Meta) ([]byte, error) {
	if meta == nil || meta.Salt == "" {
		return nil, fmt.Errorf("shardseal: meta 缺少 salt")
	}
	salt, err := base64.StdEncoding.DecodeString(meta.Salt)
	if err != nil {
		return nil, fmt.Errorf("shardseal: meta salt 解码失败: %w", err)
	}
	if len(salt) != SaltLen {
		return nil, fmt.Errorf("shardseal: meta salt 长度 %d，应为 %d", len(salt), SaltLen)
	}
	return salt, nil
}

// writeDecryptedChunks 逐块解密写入 f 并累加整文件 SHA-256（DecryptFile 的分块处理，
// 抽方法控制认知复杂度 #727 gocognit=15）。任何分块失败返回错误（f 由调用方 Close）。
// key 与 salt 是 DecryptFile 派生一次的文件密钥与文件级盐（decryptBlock 内做块内
// salt 一致性校验）。
func writeDecryptedChunks(f *os.File, meta *Meta, chunkDir string, key, salt []byte, full hash.Hash) error {
	for _, ci := range meta.Chunks {
		blob, err := os.ReadFile(filepath.Join(chunkDir, ci.FileName))
		if err != nil {
			return fmt.Errorf("shardseal: 读分块 %s 失败: %w", ci.FileName, err)
		}
		plain, err := decryptBlock(key, salt, blob)
		if err != nil {
			return err
		}
		if int64(len(plain)) != ci.OrigSize {
			return fmt.Errorf("shardseal: 分块 %s 解密长度 %d 不匹配 meta %d", ci.FileName, len(plain), ci.OrigSize)
		}
		if _, err := f.Write(plain); err != nil {
			return fmt.Errorf("shardseal: 写还原文件失败: %w", err)
		}
		full.Write(plain)
	}
	return nil
}

// MediaKindOf 依据文件名扩展名返回媒体**容器族**标识（blocklet 自动选型判据）。secretdata
// 写路径按此查注册表选型；**未注册的容器族 Kind 由注册表回落默认 fixed**（不「宣称支持实为
// 必败降级」）。
//
// **容器族口径（对抗评审 + 库选型评估定稿，2026-10-04）**：Kind 按容器族拆分（video/mp4、
// video/mkv、video/ts、video/avi…），当前仅 video/mp4 有解析器（go-mp4，ISO-BMFF box）——
// 其余容器族返回具体 Kind 但注册表未命中 → 回落 fixed。未来接入新解析器只需注册对应 Kind
// （ebml-go→video/mkv、go-astits→video/ts），无需改本函数。
func MediaKindOf(name string) string {
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	case ".mp4", ".mov":
		return "video/mp4" // ISO-BMFF：go-mp4 可解析（当前唯一注册）
	case ".mkv", ".webm":
		return "video/mkv" // EBML：无解析器注册 → 回落 fixed（未来 ebml-go）
	case ".ts":
		return "video/ts" // MPEG-TS：无解析器注册 → 回落 fixed（未来 go-astits）
	case ".avi":
		return "video/avi" // RIFF：无解析器注册 → 回落 fixed（未来手写 RIFF）
	default:
		return ""
	}
}

// mediaType 依据扩展名返回 media_type（meta 审计字段；未知返回空）。
func mediaType(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mp4", ".mkv", ".webm", ".mov", ".avi":
		return "video" + ext
	case ".jpg", ".jpeg", ".png", ".gif", ".webp":
		return "image" + ext
	case ".mp3", ".flac", ".wav":
		return "audio" + ext
	}
	return ""
}
