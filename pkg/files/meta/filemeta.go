// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package meta 提供文件配套元数据（FileMeta）——可信卷（Trusted Volume）核心：
// 卷提供文件 meta 接口后成为可信卷，上传/下载按总哈希 + 分块哈希逐分片校验。
//
// FileMeta 是**明文通用版**元数据（隐藏、占配额），字段与加密卷 shardseal.Meta
// 对齐（校验/分块/扩展/版本），可互转。Extra 用 map[string]any（用户裁定：都是 JSON
// 序列化，[]byte 无优势且无法存 int/string 等结构化扩展信息）。
package meta

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// metaVersion 是 FileMeta 结构版本。
const metaVersion = 1

// ChunkMeta 是单个分块的校验元信息（偏移 + 大小 + sha256 + md5 双算法）。
type ChunkMeta struct {
	Index  int    `json:"index"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	MD5    string `json:"md5,omitempty"`
}

// FileMeta 是文件的配套元数据（隐藏、占配额）。字段与 shardseal.Meta 对齐
// （Original/Chunk/Extra/版本），去加密专属字段，供普通本地卷/外部卷使用。
type FileMeta struct {
	Version     int            `json:"version"`
	Size        int64          `json:"size"`
	TotalSHA256 string         `json:"total_sha256"`
	TotalMD5    string         `json:"total_md5,omitempty"`
	ChunkSize   int64          `json:"chunk_size"`
	Chunks      []ChunkMeta    `json:"chunks"`
	Name        string         `json:"name,omitempty"` // 裸 basename
	MTime       string         `json:"mtime,omitempty"`
	CTime       string         `json:"ctime,omitempty"` // 创建时间
	MediaType   string         `json:"media_type,omitempty"`
	Extra       map[string]any `json:"extra,omitempty"` // 自定义扩展信息（创建人/email/校验信息等任意数据）
	BaseVersion int64          `json:"base_version,omitempty"`
	Signature   string         `json:"signature,omitempty"` // meta HMAC
}

// Validate 校验 meta 字段完备性（fail-closed：残缺即不可信）。
// 校验依据与 shardseal.validateMeta 对齐：Version/Size/TotalSHA256/分块非空/分块
// 覆盖 [0, Size) 连续无空洞。
func Validate(m *FileMeta) error {
	if err := validateHeader(m); err != nil {
		return err
	}
	return validateChunkCoverage(m)
}

// validateHeader 校验 meta 头部字段（nil/版本/size/总哈希/分块大小/非空分块）。
func validateHeader(m *FileMeta) error {
	if m == nil {
		return fmt.Errorf("meta: FileMeta 为 nil")
	}
	if m.Version != metaVersion {
		return fmt.Errorf("meta: 未知版本 %d", m.Version)
	}
	if m.Size < 0 {
		return fmt.Errorf("meta: size 非法 %d", m.Size)
	}
	if m.TotalSHA256 == "" {
		return fmt.Errorf("meta: 整文件 SHA-256 缺失（完整性校验不可用，fail-closed）")
	}
	if m.ChunkSize <= 0 {
		return fmt.Errorf("meta: 分块大小非法 %d", m.ChunkSize)
	}
	if len(m.Chunks) == 0 {
		return fmt.Errorf("meta: 无分块")
	}
	return nil
}

// validateChunkCoverage 校验分块连续覆盖 [0, Size)：首块 Offset=0、逐块相接、
// 末块覆盖到 Size；逐块 index/offset/size/sha256 完备。
func validateChunkCoverage(m *FileMeta) error {
	cur := int64(0)
	for i, c := range m.Chunks {
		if c.Index != i {
			return fmt.Errorf("meta: 分块 %d index %d 不连续", i, c.Index)
		}
		if c.Offset != cur {
			return fmt.Errorf("meta: 分块 %d 偏移 %d，期望 %d（不连续）", i, c.Offset, cur)
		}
		if c.Size <= 0 {
			return fmt.Errorf("meta: 分块 %d 大小非法 %d", i, c.Size)
		}
		if c.SHA256 == "" {
			return fmt.Errorf("meta: 分块 %d 缺失 SHA-256", i)
		}
		cur += c.Size
	}
	if cur != m.Size {
		return fmt.Errorf("meta: 分块覆盖 [0,%d)，期望 [0,%d)", cur, m.Size)
	}
	return nil
}

// Marshal 序列化为 JSON（紧凑，供落盘/传输）。
func Marshal(m *FileMeta) ([]byte, error) {
	return json.Marshal(m)
}

// Unmarshal 反序列化 + 校验（fail-closed）。
func Unmarshal(data []byte) (*FileMeta, error) {
	var m FileMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("meta: 解析失败: %w", err)
	}
	if err := Validate(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ChunkSizeForSize 按总文件大小返回分块大小（用户裁定：大文件分块大一点，6G 家常便饭）。
//
//	<16MiB           → 1MiB
//	16MiB ~ 256MiB   → 4MiB
//	256MiB ~ 1GiB    → 8MiB
//	1GiB ~ 4GiB      → 16MiB
//	>4GiB            → 32MiB（6G ≈ 192 块，逐分片校验/流量成本可控）
func ChunkSizeForSize(size int64) int64 {
	const miB = 1 << 20
	switch {
	case size <= 0:
		return miB
	case size < 16*miB:
		return 1 * miB
	case size < 256*miB:
		return 4 * miB
	case size < 1<<30: // 1GiB
		return 8 * miB
	case size < 4<<30: // 4GiB
		return 16 * miB
	default:
		return 32 * miB
	}
}

// nowRFC3339 供 CTime/MTime 使用（测试可注入可变时钟）。
var nowRFC3339 = func() string { return time.Now().UTC().Format(time.RFC3339) }

// sanitizeName 取裸 basename（去路径分隔符，防 meta.Name 注入）。
func sanitizeName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// FromFile 计算本地文件的完整 FileMeta（双算法整文件 + 分块）。
// chunkSize <= 0 时按 ChunkSizeForSize 自适应。mtime/ctime 取自文件 stat。
func FromFile(path string, chunkSize int64, extra map[string]any) (*FileMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("meta: 打开 %s: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("meta: stat %s: %w", path, err)
	}
	c, err := NewCalculator(fi.Size(), chunkSize)
	if err != nil {
		return nil, err
	}
	// 分块读取：按 chunkSize 分段读，逐块累计 sha256/md5（一次性两遍哈希齐算）。
	if _, err := c.ReadFrom(f); err != nil {
		return nil, fmt.Errorf("meta: 计算 %s: %w", path, err)
	}
	m := c.Finish()
	m.Name = sanitizeName(fi.Name())
	m.MTime = fi.ModTime().UTC().Format(time.RFC3339)
	m.CTime = nowRFC3339()
	m.Extra = extra
	return m, nil
}
