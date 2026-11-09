// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

// rapidcalc.go —— rapidupload（秒传）参数计算：**全部离线可算、可单测**。
//
// 设计依据 docs/archive/baidupcs-upload-core.md（S5）：内核的 Rapid 阶段需要 upload_id，
// 而 upload_id 只能由 rapidupload 响应给出（库 internal/pcsfunctions/pcsupload/upload_task_unit.go:281-283），
// 因此必须先算出 rapidupload 的**完整参数**再发请求。
//
// **复用策略（不自行复刻算法）**：
//   - 所有 checksum 由 fork 库的公开包 `pcsutil/checksum`（`LocalFileChecksum`）计算：
//     MD5 / SliceMD5 / CRC32 / BlocksList / 随机片段内容 —— 一行都不自己实现（避免算法漂移）；
//   - 切片长度用库**公开常量** `baidupcs.SliceMD5Size`（256KB）；
//   - 分块大小**固定为本包的上传块**（`uploadBlockSize`，见 blockSizeForRapid 的约束说明）；
//   - **仅复刻一个纯函数**（库把它放在 `internal/` 包内，无法 import；逻辑约 10 行）：
//     `rapidDataOffset`（库 `creaetDataOffset`，utils.go:22），行为由**独立计算的金值向量**锁定。

import (
	//nolint:gosec // G501：百度 rapidupload 协议要求 md5（必须与库 creaetDataOffset 逐位一致），非安全用途
	"crypto/md5" // NOSONAR: S2070 — 协议要求的弱哈希（秒传参数算法），非安全用途
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	bdlib "github.com/qjfoidnh/BaiduPCS-Go/baidupcs"
	"github.com/qjfoidnh/BaiduPCS-Go/pcsutil/checksum"
)

// rapidSubSize 是 rapidupload 随机片段的长度：复刻库
// `internal/pcsfunctions/pcsupload/upload_task_unit.go:66` 的 `DefaultContentSize = 4 * converter.KB`。
const rapidSubSize int64 = 4 * 1024

// rapidPlan 是一次 rapidupload 请求所需的全部参数（纯计算结果，无副作用）。
type rapidPlan struct {
	ContentMD5  string   // 整文件 md5（hex）
	SliceMD5    string   // 前 SliceMD5Size 字节的 md5（hex）
	CRC32       string   // 整文件 crc32（十进制字符串，与库一致）
	DataContent string   // 随机片段 base64（**去掉尾部 '='**，与库一致）
	Offset      int64    // 随机片段起始偏移
	DataLength  int64    // 随机片段实际长度
	DataTime    int64    // 参与偏移计算的当前时间（unix 秒；同一时刻重算结果一致）
	Size        int64    // 文件总大小（rapidupload length 参数）
	BlockSize   int64    // 分块大小（决定 blocks list）
	BlocksList  []string // 每块 md5（hex）
}

// blockSizeForRapid 返回 rapidupload 的 blockList 分块大小。
//
// **关键约束（本包既有实现的既定语义，必须保持）**：blockList 的分块**必须与分片上传的块大小一致**
// （`uploadBlockSize`，storage.go:771 = 4MiB），否则刷新时块索引对不上（见 adapter.go:366 注释：
// "与上传块一致才命中刚上传的块索引"）。
//
// 因此**刻意不复刻**库的 `getBlockSize`（4/16/64MB 随文件大小变化）—— 库之所以能用变化块大小，
// 是因为它的 `uploader.MultiUploader.BlockSize` 也用同一个值；而本包的上传块固定 4MiB
// ⇒ blockList 也必须固定 4MiB 才自洽。（这是 S5 设计中的一处**反直觉点**，特此写明。）
func blockSizeForRapid() int64 { return uploadBlockSize }

// rapidDataOffset 复刻库 `creaetDataOffset`（`internal/pcsfunctions/pcsupload/utils.go:22`）：
//
//	sumStr   = fmt.Sprintf("%d%s%s", uk, contentMD5, dataTime)
//	rawOffset = 前 8 个 hex 字符按 16 进制解析（即 md5 的前 4 字节）
//	offset    = rawOffset % (fileSize - subSize + 1)   // 窗口 <= 1 时取 0
//
// 该偏移决定上传哪个随机片段，**必须与库逐位一致**，否则秒传必然失败
// （本函数行为由 TestRapidDataOffset_GoldenVector 用独立计算的金值锁定）。
func rapidDataOffset(contentMD5 string, uk, dataTime, fileSize, subSize int64) (int64, error) {
	//nolint:gosec // G401：与库 creaetDataOffset 同算法（md5），偏移必须逐位一致，非安全用途
	sum := md5.Sum([]byte(strconv.FormatInt(uk, 10) + contentMD5 + strconv.FormatInt(dataTime, 10))) // NOSONAR: S2070 — 协议要求（非安全用途）
	rawOffset, err := strconv.ParseInt(hex.EncodeToString(sum[:])[:8], 16, 64)
	if err != nil {
		return 0, fmt.Errorf("baidupcs: 解析随机片段偏移: %w", err)
	}
	window := fileSize - subSize + 1
	if window <= 1 {
		return 0, nil
	}
	return rawOffset % window, nil
}

// planRapid 计算 rapidupload 的全部参数（离线；checksum 全部复用库的 LocalFileChecksum）。
func planRapid(localPath string, uk, dataTime int64) (*rapidPlan, error) {
	lfc := checksum.NewLocalFileChecksum(localPath, int(bdlib.SliceMD5Size))
	if err := lfc.OpenPath(); err != nil {
		return nil, fmt.Errorf("baidupcs: 打开待上传文件: %w", err)
	}
	defer func() { _ = lfc.Close() }()

	if lfc.Length <= 0 {
		st, err := os.Stat(localPath)
		if err != nil {
			return nil, fmt.Errorf("baidupcs: stat 待上传文件: %w", err)
		}
		lfc.Length = st.Size()
	}

	if err := lfc.Sum(checksum.CHECKSUM_MD5 | checksum.CHECKSUM_SLICE_MD5 | checksum.CHECKSUM_CRC32); err != nil {
		return nil, fmt.Errorf("baidupcs: 计算文件校验值: %w", err)
	}

	blockSize := blockSizeForRapid()
	if err := lfc.CalculateChunkedSum(blockSize); err != nil {
		return nil, fmt.Errorf("baidupcs: 计算分块校验值: %w", err)
	}

	md5Hex := hex.EncodeToString(lfc.MD5)
	offset, err := rapidDataOffset(md5Hex, uk, dataTime, lfc.Length, rapidSubSize)
	if err != nil {
		return nil, err
	}
	dataContent, dataLength, err := lfc.GetSliceDataContent(offset, rapidSubSize)
	if err != nil {
		return nil, fmt.Errorf("baidupcs: 读取随机片段: %w", err)
	}
	return &rapidPlan{
		ContentMD5: md5Hex,
		SliceMD5:   hex.EncodeToString(lfc.SliceMD5),
		CRC32:      strconv.FormatUint(uint64(lfc.CRC32), 10),
		// **必须去掉尾部 '='**（与库 upload_task_unit.go:221 `strings.TrimRight(b64, "=")` 一致）：
		// 百度侧不接受带 padding 的 base64（差异虽小但会导致秒传参数无效）。
		DataContent: strings.TrimRight(base64.StdEncoding.EncodeToString(dataContent), "="),
		Offset:      offset,
		DataLength:  dataLength,
		DataTime:    dataTime,
		Size:        lfc.Length,
		BlockSize:   blockSize,
		BlocksList:  append([]string(nil), lfc.BlocksList...),
	}, nil
}
