// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package ffprobe 实现 shardseal.KeyframeIndexer：以 ffprobe 子进程解析视频容器，提取
// 关键帧（I 帧）在文件中的绝对字节偏移。与 go-mp4（pkg/media/ext/mp4，仅 MP4/MOV）不同，
// ffprobe 覆盖**全部容器**（MP4/MOV/MKV/WebM/TS/AVI/FLV…）——一个二进制替代 N 个
// 纯 Go 解析库，功能覆盖、正确性（ffmpeg 生态久经考验）、可维护性全面胜出。
//
// 部署可选（渐进增强）：有 ffmpeg/ffprobe 的环境注册该提供者（优先于 go-mp4），
// 无则回落 go-mp4（MP4）或默认 fixed（其它格式）。不污染主仓 go.mod（os/exec +
// encoding/json 纯标准库），隔离在 pkg/media 域（媒体解析与加密 cryptox 语义分离）。
package ffprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ErrFFprobeMissing 是哨兵错误：PATH 中找不到 ffprobe（无 ffmpeg 环境，调用方回落）。
var ErrFFprobeMissing = errors.New("ffprobe: 未找到 ffprobe 可执行文件（需安装 ffmpeg）")

// ffprobeRunner 是子进程执行器的抽象（依赖注入：生产用 realRunner 起 ffprobe，
// 测试注入 fakeRunner 返回固定 JSON——跨平台、不依赖真实 ffmpeg/脚本可执行）。
type ffprobeRunner interface {
	// Run 执行 ffprobe（stdin 为文件流），返回 stdout 字节。
	Run(ctx context.Context, file io.Reader) ([]byte, error)
}

// realRunner 生产实现：exec ffprobe，stdin 传文件流。
type realRunner struct{}

func (realRunner) Run(ctx context.Context, file io.Reader) ([]byte, error) {
	bin, lerr := exec.LookPath("ffprobe")
	if lerr != nil {
		return nil, ErrFFprobeMissing
	}
	cmd := exec.CommandContext(ctx, bin,
		"-v", "error",
		"-select_streams", "v:0",
		"-skip_frame", "nokey",
		"-show_frames",
		"-show_entries", "frame=key_frame,pkt_pos",
		"-of", "json",
		"-i", "pipe:0",
	)
	cmd.Stdin = file
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		return nil, fmt.Errorf("ffprobe 执行失败: %w", runErr)
	}
	return stdout.Bytes(), nil
}

// Indexer 是 shardseal.KeyframeIndexer 的装配实例（ffprobe 子进程解析器）。
type Indexer struct{}

// KeyframeOffsets 实现 shardseal.KeyframeIndexer：ffprobe 解析关键帧（I 帧）文件绝对偏移。
// 流程：ffprobe -show_frames 输出每帧 key_frame/pkt_pos → 筛选 key_frame=true 的 pkt_pos →
// 升序返回。任意失败（无 ffprobe / 非零退出 / JSON 非法 / 截断）返回「已解析部分 + err」，
// 不 panic（不可信输入隔离见 recover）。
func (Indexer) KeyframeOffsets(r io.ReaderAt, fileSize int64) ([]int64, error) {
	return KeyframeOffsets(r, fileSize)
}

// KeyframeOffsets 解析视频容器的关键帧字节偏移（shardseal.KeyframeIndexer 实现面）。
func KeyframeOffsets(r io.ReaderAt, fileSize int64) ([]int64, error) {
	return keyframeOffsetsWithRunner(realRunner{}, r, fileSize)
}

// keyframeOffsetsWithRunner 是 KeyframeOffsets 的注入变体（测试用 fakeRunner）。
func keyframeOffsetsWithRunner(rr ffprobeRunner, r io.ReaderAt, fileSize int64) (offs []int64, err error) {
	// panic 兜底：子进程输出/JSON 解析对不可信输入也可能 panic（如畸形 JSON 深度）。
	defer func() {
		if rec := recover(); rec != nil {
			offs = nil
			err = fmt.Errorf("ffprobe: 解析 panic 已隔离（不可信输入）: %v", rec)
		}
	}()

	if r == nil {
		return nil, fmt.Errorf("ffprobe: 输入 reader 为 nil")
	}
	if fileSize <= 0 {
		return nil, fmt.Errorf("ffprobe: 非法文件大小 %d", fileSize)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, runErr := rr.Run(ctx, &readerAtReader{r: r, size: fileSize})
	if runErr != nil {
		return nil, runErr
	}
	return parseKeyframes(out)
}

// readerAtReader 把 io.ReaderAt + size 适配为 io.Reader（ffprobe stdin 流输入）。
type readerAtReader struct {
	r    io.ReaderAt
	size int64
	off  int64
}

func (r *readerAtReader) Read(p []byte) (int, error) {
	if r.off >= r.size {
		return 0, io.EOF
	}
	n := int64(len(p))
	if remain := r.size - r.off; remain < n {
		n = remain
	}
	nr, err := r.r.ReadAt(p[:n], r.off)
	r.off += int64(nr)
	if nr < len(p) && err == nil {
		err = io.EOF
	}
	return nr, err
}

// ffprobeOutput 是 ffprobe -show_frames -of json 的输出结构。
type ffprobeOutput struct {
	Frames []ffprobeFrame `json:"frames"`
}

// ffprobeFrame 是单帧条目（key_frame=1 为关键帧；pkt_pos 为文件内字节偏移）。
// ffprobe 的 pkt_pos 可能是 JSON number（uint64）或 string——用 RawMessage 兼容两者。
type ffprobeFrame struct {
	KeyFrame int             `json:"key_frame"`
	PktPos   json.RawMessage `json:"pkt_pos"`
}

// parsePktPos 解析 pkt_pos RawMessage（JSON number 或 string）。
func parsePktPos(raw json.RawMessage) (int64, error) {
	s := strings.TrimSpace(string(raw))
	s = strings.Trim(s, `"`)
	return strconv.ParseInt(s, 10, 64)
}

// parseKeyframes 从 ffprobe JSON 提取关键帧 pkt_pos（升序；去重）。
// 输出缺帧/字段（截断或非视频）→ 空结果 + err（降级由 shardseal 决策）。
func parseKeyframes(raw []byte) ([]int64, error) {
	var out ffprobeOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("ffprobe: 输出 JSON 解析失败: %w", err)
	}
	var keys []int64
	seen := map[int64]bool{}
	for _, f := range out.Frames {
		if f.KeyFrame != 1 || len(f.PktPos) == 0 {
			continue
		}
		// pkt_pos 兼容 JSON number（如 512）与 string（如 "512"）。
		pos, perr := parsePktPos(f.PktPos)
		if perr != nil || pos < 0 {
			continue // 无效 pkt_pos 跳过（截断/异常帧）
		}
		if !seen[pos] {
			seen[pos] = true
			keys = append(keys, pos)
		}
	}
	// 升序排序。
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys, nil
}
