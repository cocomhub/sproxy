// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"fmt"
	"strings"
)

// AlgorithmName 是算法标识（写进 meta.algorithm），默认 = **standard 档**（KDF 强度档位
// N=2^14）。装配 `Options.Algorithm` 传该名（或空）解析到 standard；要切换档位传
// "shardseal/aes-256-gcm-high"（N=2^17，保守）或 "shardseal/aes-256-gcm-low"（N=2^12，
// 测试/低配）。各档为不同 AlgoVersion + KDF 域，跨档 fail-closed。
const AlgorithmName = "shardseal/aes-256-gcm"

// metaVersion 是 meta 结构版本。
const metaVersion = 1

// OriginalInfo 是原始文件的全 stat（设计 §5：全 stat + 每分块 stat，支持旧卷还原时
// 恢复元信息、无需重下载）。
type OriginalInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// SHA256 是原始文件整文件 SHA-256（64 hex）。解密后可做全量校验。
	SHA256    string `json:"sha256"`
	MTime     string `json:"mtime,omitempty"`
	CTime     string `json:"ctime,omitempty"`
	Mode      uint32 `json:"mode,omitempty"`
	MediaType string `json:"media_type,omitempty"`
}

// BlockErrorMsg 是块/blocklet 的解析失败描述（写入 meta 失败信息列表；全密文内，
// 磁盘无明文）。
type BlockErrorMsg struct {
	Code string `json:"code,omitempty"`
	Msg  string `json:"msg,omitempty"`
}

// ChunkInfo 是单个分块的 stat（每块原始/加密名称与校验和、大小、nonce）。
type ChunkInfo struct {
	Index int `json:"index"`
	// FileName 是加密分块文件名（三段 16hex 命名）。
	FileName string `json:"file_name"`
	// Offset 是分块在原始文件中的字节偏移（0-based；随机访问定位用）。
	Offset int64 `json:"offset"`
	// OrigSize 是块的原始明文大小。
	OrigSize int64 `json:"orig_size"`
	// OrigSHA256 是块原始内容 SHA-256 前 16 hex。
	OrigSHA256 string `json:"orig_sha256"`
	// EncSize 是块的密文大小。
	EncSize int64 `json:"enc_size"`
	// EncSHA256 是块密文内容 SHA-256 前 16 hex（可校验密文完整）。
	EncSHA256 string `json:"enc_sha256"`
	// Nonce 是块 nonce（base64；当前块内联 nonce，字段为向后兼容/审计）。
	Nonce string `json:"nonce,omitempty"`
	// Blocklets 是块内 blocklet 索引（随机访问定位/只解目标 blocklet 段）。连续覆盖
	// [Offset, Offset+OrigSize)，validateMeta 校验。
	Blocklets []BlockletInfo `json:"blocklets"`
	// Failures 是**文件级共享**的解析失败信息列表（视频关键帧等；planner 生命周期失败
	// 记录冗余到每个块——任一块都含失败全貌，解密/审计不依赖特定块）。omitempty——
	// 旧 meta 无此字段。
	Failures []BlockErrorMsg `json:"failures,omitempty"`
}

// BlockletInfo 是单个 blocklet 的索引 stat（随机访问加速：按目标 offset 定位段）。
// ChunkInfo.Offset/OrigSize 保留作块级入口；blocklet 粒度索引供随机读取只下载/解密
// 含目标范围的 blocklet 段。
type BlockletInfo struct {
	// Offset 是 blocklet 在原始文件中的字节偏移（0-based，绝对）。
	Offset int64 `json:"offset"`
	// Size 是 blocklet 原始明文大小。
	Size int64 `json:"size"`
	// EncSize 是 blocklet 密文段大小（含段头+nonce+tag，定位段长用）。
	EncSize int64 `json:"enc_size"`
	// OrigSHA256 是 blocklet 原始内容 SHA-256 前 16 hex（审计/校验用）。
	OrigSHA256 string `json:"orig_sha256"`
	// EncOffset 是 blocklet 密文在 blob 内的起始偏移（nonce 起点；随机访问跳读用）。
	EncOffset int64 `json:"enc_offset"`
	// Used 标记该 blocklet 段是否已使用（false = padding 空闲段，供打包替换复用）。
	Used bool `json:"used,omitempty"`
	// Type 是 blocklet 段类型字节（与 blob 内一致；Data=0x01 / Padding=0x02 / Extra=0x03）。
	Type byte `json:"type,omitempty"`
	// Failures 预留槽位（blocklet 级失败，当前不产出——文件级失败冗余在 ChunkInfo.Failures；
	// 保留供未来单段级诊断）。omitempty。
	Failures []BlockErrorMsg `json:"failures,omitempty"`
}

// BlockPolicy 是 meta 内记录的分块策略（设计 §2.2）。
type BlockPolicy struct {
	Mode string `json:"mode"`
	Min  int64  `json:"min"`
	Max  int64  `json:"max"`
	// BlockletMode 是块内结构细分模式：默认 "fixed"（定长 blocklet）；"video-keyframe"
	// 为视频关键帧边界规划（装配层按文件类型经 ResolveBlockletMode 选型注入）。
	BlockletMode string `json:"blocklet_mode,omitempty"`
	// BlockletMin/BlockletMax 是 blocklet 大小区间（默认 64KB-4MB）。
	BlockletMin int64 `json:"blocklet_min,omitempty"`
	BlockletMax int64 `json:"blocklet_max,omitempty"`
	// Indexer 是关键帧解析器（装配期注入，json:"-" 不入 meta——解析器不可序列化且
	// 解密路径不需要它；仅写路径构造 VideoKeyframeBlockletPlanner 用）。
	Indexer KeyframeIndexer `json:"-"`
}

// Meta 是文件级元数据（JSON 编解码）。
type Meta struct {
	Version   int    `json:"version"`
	Algorithm string `json:"algorithm"`
	// AlgoVersion 是加密算法版本（json algo_version）。版本不明文进 blob，仅存于
	// meta（加密落盘 = 密文内），供解密选派生域。validateMeta 要求版本已知注册
	// （无旧 blob 需兼容，未知/缺失即 fail-closed）。
	AlgoVersion AlgoVersion  `json:"algo_version"`
	KDF         string       `json:"kdf"`
	SecretURL   string       `json:"secret_url,omitempty"`
	Salt        string       `json:"salt"`
	Original    OriginalInfo `json:"original"`
	Chunks      []ChunkInfo  `json:"chunks"`
	// BlockPolicy 是生成时的分块策略（还原不依赖；旧卷读取审计用）。
	Block BlockPolicy `json:"block_policy"`

	// Keyframes 是全文件关键帧绝对字节偏移（升序；seek 时间戳→关键帧反向索引，本
	// 任务预留字段不实现消费；omitempty，旧 meta 无此字段）。
	Keyframes []int64 `json:"keyframes,omitempty"`

	// ---- 压缩（横向能力，§13.2 落位说明：非编号审计项；加密/压缩解耦，
	// 改压缩算法不升级加密版本）----
	// 以下字段全部密文内、omitempty，**仅预留**：本任务不写值、不实现逻辑（压缩/去重/
	// 乐观锁/GC 等语义在任务 9c 及后续）。空值（零值）的旧 meta 可正常解密加载。

	// Compressed 标记该文件是否已压缩（true 时按 Compression 解压）。
	Compressed bool `json:"compressed,omitempty"`
	// Compression 是压缩算法标识（"none"/"zstd"；RegisterCompression 独立注册表，
	// 改压缩算法无需升级加密算法版本——压缩在密文内、明文面不可见）。
	Compression string `json:"compression,omitempty"`

	// ---- 密钥池 / 轮换（用户：卷级数据互操作，同 secret 卷可互读）----
	// KeyID 是派生所用 secret 的池内标识（派生时按 KeyID 从 secret 池选 secret，
	// 支持多 secret 轮换互读，不重加密）。
	KeyID string `json:"key_id,omitempty"`

	// ---- 扩展元数据（用户：文件名/大小/权限/备注/kv/原始校验和进密文，降外部 meta 依赖）----
	// Extra 是任意扩展键值（如 media_type、ACL、tag；map[]byte 值，密文内）。
	Extra map[string][]byte `json:"extra,omitempty"`

	// ---- 审计 / 溯源（§13.2 审计项 5：溯源）+ 访问计数（审计项 10）----
	WriterID    string `json:"writer_id,omitempty"`    // 写入者指纹（PikPak 下载来源等）
	SourceURL   string `json:"source_url,omitempty"`   // 溯源（下载来源 URL）
	AccessCount int64  `json:"access_count,omitempty"` // 访问计数（热数据统计/成本）
	LastAccess  string `json:"last_access,omitempty"`  // 最近访问时间（RFC3339）

	// ---- 版本 / 乐观锁（§13.2 审计项 2：乐观锁）+ 版本保留（审计项 6）+ 版本时钟（审计项 12）----
	BaseVersion int64  `json:"base_version,omitempty"` // 乐观锁 CAS 版本（多进程写前校验）
	VersionSeq  int64  `json:"version_seq,omitempty"`  // 版本保留序号（覆盖写保留 N 个旧版本）
	Supersedes  string `json:"supersedes,omitempty"`   // 被本版本取代的版本标识（版本链）
	VClock      string `json:"vclock,omitempty"`       // 版本时钟（防跨时区/时钟漂移覆盖误判）

	// ---- 去重（§13.2 审计项 1：去重引用；块级内容寻址）----
	RefCount int64 `json:"ref_count,omitempty"` // 块引用计数（去重共享；>1 表示被多文件引用）

	// ---- 删除 / 墓碑（§13.2 审计项 3：墓碑 + GC）----
	Deleted      bool   `json:"deleted,omitempty"`       // 删除墓碑（loadIndex 跳过；GC 清理）；本字段现为格式预留（Delete 即时物理删、不再写墓碑）
	ExportedFrom string `json:"exported_from,omitempty"` // 备份/导出溯源（来源卷/任务）

	// ---- 安全（§13.2 审计项 11：meta 独立签名）----
	Signature string `json:"signature,omitempty"` // meta HMAC（HKDF 子域派生签名密钥，防 meta 被替换）

	// ---- 纠错（任务 9d：XOR parity，k-of-k+1，纯 stdlib）----
	// Parity 是 XOR 奇偶校验段引用（Options.Erasure=true 写入时产出）。parity 明文 =
	// 各数据分块明文按 maxOrigSize 补零对齐后逐字节 XOR（XOR 恒等式：任一分块丢失 →
	// 用 parity ^ 其余分块恢复）。parity 段按统一 blob 格式加密落盘（独立分块文件），
	// 引用记录于此。值为 nil 表示未启用纠错（默认，旧 meta 兼容）。
	Parity *ParityInfo `json:"parity,omitempty"`
}

// ParityInfo 是 XOR 奇偶校验段的元信息（Meta.Parity）。parity 段是 k 个数据块的逐字节
// XOR（k-of-k+1），自身按统一 blob 格式加密（同分块 blob，前端 [R][8B 密文流总长][salt]
// [boot][数据段][index]），因此调用方有 secret 即可独立解密。首段类型保留 BlockletTypeParity
// 槽位（0x13）；数据块与 parity 的映射 = 全部 ChunkInfo 条目（除 parity 自身外）。
type ParityInfo struct {
	// FileName 是 parity 段的加密分块文件名（独立于各数据分块，落盘在数据目录）。
	FileName string `json:"file_name"`
	// PlainLen 是 parity 明文的原始长度 = max(各数据分块 OrigSize)（对齐长度，供恢复截断）。
	PlainLen int64 `json:"plain_len"`
	// EncSize 是 parity 段密文大小（包含段头/索引等 blob 开销）。
	EncSize int64 `json:"enc_size"`
	// ChunkCount 是参与 XOR 的数据分块数（k），须 == len(Chunks)。
	ChunkCount int `json:"chunk_count"`
}

// EncryptionResult 是 EncryptShards 的产物：分块文件名 + 最终 meta blob + meta 文件名 + meta 内容。
type EncryptionResult struct {
	// ChunkNames 是加密分块文件名列表（写入 outDir）。
	ChunkNames []string
	// MetaName 是 meta 文件名（写入 outDir）。三段哈希锚定 MetaBlob：首段 =
	// hash16(metaJSON)、中段 = 原始总校验和前 16、末段 = hash16(MetaBlob)。
	MetaName string
	// MetaBlob 是最终可上传的加密 meta（含 padding），与落盘文件一致。
	MetaBlob []byte
	// Meta 是完整 meta（含全 stat + 每块 stat）。
	Meta *Meta
}

// validateMeta 校验 meta 字段完备性（失败 = 无法还原）。
// 算法校验**经注册表**（parseAlgorithm 名字→版本，非硬编码 AlgorithmName）：新增算法
// 仅注册即生效，secretdata 零改动；并校验 m.AlgoVersion 与 m.Algorithm 是同一已注册
// 算法的映射一致（名字↔版本一致，杜绝自相矛盾的 meta）。
//
// **纵深加固（修复轮 M1，P3/P6 落地）**：
//   - 强制 `Original.SHA256` 非空——DecryptFile 的全文件完整性校验（`want != ""` 门控）依赖
//     它；空 SHA256 的 meta 会被 DecryptFile 跳过完整性检查（弱化防线），此处 fail-closed。
//   - 强制 `Original.Name` 为不含 `/`/`\` 的裸 basename——解密路径 `path.Join(dirPath, name)`
//     依赖裸名，非法名会导致索引键水平漂移（secretdata 写路径恒写裸名，故只拦异常 meta）。
//   - 强制分块 `FileName` 不含路径分隔符——分块名恒为三段 hex 裸名。
func validateMeta(m *Meta) error {
	if m == nil {
		return fmt.Errorf("shardseal: meta 为 nil")
	}
	algoVer, ok := parseAlgorithm(m.Algorithm)
	switch {
	case m.Version != metaVersion:
		return fmt.Errorf("shardseal: 未知 meta 版本 %d", m.Version)
	case !ok:
		return fmt.Errorf("shardseal: 未知算法 %q（未注册）", m.Algorithm)
	case m.AlgoVersion != algoVer:
		return fmt.Errorf("shardseal: 算法 %q 版本 %d，注册版本 %d（不一致）", m.Algorithm, m.AlgoVersion, algoVer)
	case m.Original.Name == "" || m.Original.Size < 0:
		return fmt.Errorf("shardseal: meta 原始信息缺失（name=%q size=%d）", m.Original.Name, m.Original.Size)
	case m.Original.SHA256 == "":
		return fmt.Errorf("shardseal: meta 原始整文件 SHA-256 缺失（完整性校验不可用，fail-closed）")
	case strings.Contains(m.Original.Name, "/") || strings.Contains(m.Original.Name, `\`):
		return fmt.Errorf("shardseal: meta 原始文件名非法（须为不含路径分隔符的裸名，name=%q）", m.Original.Name)
	case len(m.Chunks) == 0:
		return fmt.Errorf("shardseal: meta 无分块")
	default:
	}
	for _, c := range m.Chunks {
		if c.FileName == "" || c.OrigSize < 0 {
			return fmt.Errorf("shardseal: meta 分块信息缺失（index=%d）", c.Index)
		}
		if strings.Contains(c.FileName, "/") || strings.Contains(c.FileName, `\`) {
			return fmt.Errorf("shardseal: meta 分块 %d 文件名非法（须为不含路径分隔符的裸名，file=%q）", c.Index, c.FileName)
		}
		if err := validateChunkBlocklets(c); err != nil {
			return err
		}
	}
	return nil
}

// validateChunkBlocklets 校验单个分块的 blocklet 索引连续覆盖 [Offset, Offset+OrigSize)
// （fail-closed：缺失/不连续/越界即不可还原）。
func validateChunkBlocklets(c ChunkInfo) error {
	if len(c.Blocklets) == 0 {
		return fmt.Errorf("shardseal: 分块 %d 无 blocklet 索引（不可还原）", c.Index)
	}
	cur := c.Offset
	for i, bl := range c.Blocklets {
		if bl.Size <= 0 {
			return fmt.Errorf("shardseal: 分块 %d blocklet[%d] 大小非法 %d", c.Index, i, bl.Size)
		}
		if bl.Offset != cur {
			return fmt.Errorf("shardseal: 分块 %d blocklet[%d] 偏移 %d 不连续（期望 %d）", c.Index, i, bl.Offset, cur)
		}
		if bl.EncSize <= 0 {
			return fmt.Errorf("shardseal: 分块 %d blocklet[%d] enc_size 非法 %d", c.Index, i, bl.EncSize)
		}
		cur += bl.Size
	}
	if cur != c.Offset+c.OrigSize {
		return fmt.Errorf("shardseal: 分块 %d blocklet 覆盖 [%d,%d)，应 [%d,%d)", c.Index, c.Offset, cur, c.Offset, c.Offset+c.OrigSize)
	}
	return nil
}
