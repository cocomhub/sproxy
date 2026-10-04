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
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ErrFFprobeMissing 是哨兵错误：PATH 中找不到 ffprobe（无 ffmpeg 环境，调用方回落）。
var ErrFFprobeMissing = errors.New("ffprobe: 未找到 ffprobe 可执行文件（需安装 ffmpeg）")

// onFragmentedMP4 是 fMP4 检测统计 hook（atomic.Pointer 保证读写无数据竞争——装配期
// SetOnFragmentedMP4 与运行期 notify 可跨 goroutine 并发，显式内存屏障）。
// ffprobe 对 fMP4 是**正常解析**（不降级），此统计供真实场景量化 fMP4 使用量（多数环境
// 装 ffmpeg 走 ffprobe，fMP4 统计主要在此侧），为「是否切 mp4ff」决策提供数据。
var onFragmentedMP4 atomic.Pointer[func()]

// SetOnFragmentedMP4 注入 fMP4 统计 hook（装配层调用 server.Metrics.RecordKeyframeFragmented；
// nil = 清除，不记录，零回归）。
func SetOnFragmentedMP4(fn func()) {
	if fn == nil {
		onFragmentedMP4.Store(nil)
		return
	}
	onFragmentedMP4.Store(&fn)
}

// notifyFragmentedMP4 触发 fMP4 统计 hook（nil 安全）。
func notifyFragmentedMP4() {
	if p := onFragmentedMP4.Load(); p != nil {
		(*p)()
	}
}

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
	// **fMP4 统计日志（2026-10-04）**：ffprobe 对 fMP4 是正常解析（能拿到关键帧），
	// 与 go-mp4 的降级不同——此处记录是供**真实场景量化 fMP4 使用量**（多数环境装
	// ffmpeg 走 ffprobe，fMP4 统计主要在此侧），为后续「是否切 mp4ff」决策提供数据。
	// 命中 moof/mvex 打 info 日志（不改变结果、不降级），避免误报用 box 结构扫描。
	// **fMP4 统计日志（2026-10-04）**：ffprobe 对 fMP4 是正常解析（能拿到关键帧），
	// 与 go-mp4 的降级不同——此处记录是供**真实场景量化 fMP4 使用量**（多数环境装
	// ffmpeg 走 ffprobe，fMP4 统计主要在此侧），为后续「是否切 mp4ff」决策提供数据。
	// 命中 moof/mvex 打 info 日志（不改变结果、不降级），避免误报用 box 结构扫描。
	if detectFragmentedMP4(&readerAtReader{r: r, size: fileSize}) {
		slog.Info("ffprobe: 检测到 fMP4（moof/mvex，无全局 stss）——记录 fMP4 使用量供 mp4ff 切换评估",
			"file_size", fileSize)
		notifyFragmentedMP4() // metric 统计（装配层注入；atomic 读，nil 安全）
	}
	return parseKeyframes(out)
}

// detectFragmentedMP4 用 MP4 box 结构扫描检测 fMP4 特征（moof/mvex）：读顶层 box 头
// （[4B size][4B type]），显式偏移跳到下一个 box（size 0=到 EOF、1=64 位扩展），并检查
// moov 内容内是否含 mvex。仅扫前 maxScanBoxes 个 box 防恶意超大（扫描失败/非 box 结构
// 返回 false——统计日志宁可漏报不可误报）。
//
// **实现要点**：所有读取走显式偏移（readerAt 的 ReadAt），不共享 reader 状态——mvexInside
// 从 moov payload 偏移读、不消费主扫描偏移（此前共享 io.Reader 导致 seek 错位漏检）。
func detectFragmentedMP4(r io.ReaderAt) bool {
	const maxScanBoxes = 32
	off := int64(0)
	for range maxScanBoxes {
		size, typ, ok := readMP4BoxHeader(r, off)
		if !ok || size < 8 {
			return false
		}
		if typ == "moof" {
			return true
		}
		if typ == "moov" && mvexInside(r, off+8, size) {
			return true
		}
		next, skip, ok := mp4BoxAdvance(r, off, size)
		if !ok || skip {
			return false
		}
		off = next
	}
	return false
}

// readMP4BoxHeader 读 MP4 box 头（[4B size][4B type]），返回 size 与 type。
func readMP4BoxHeader(r io.ReaderAt, off int64) (size uint64, typ string, ok bool) {
	var hdr [8]byte
	if _, err := r.ReadAt(hdr[:], off); err != nil {
		return 0, "", false
	}
	size = uint64(hdr[0])<<24 | uint64(hdr[1])<<16 | uint64(hdr[2])<<8 | uint64(hdr[3])
	return size, string(hdr[4:8]), true
}

// mp4BoxAdvance 计算下一个 box 的偏移。返回 (nextOff, stop, ok)：size==0 表示 box 延伸到
// 文件尾（stop=true，扫描结束）；size==1 表示 64 位扩展（读取扩展字段作为真实 size）；
// 否则 nextOff = off + size。解析失败 ok=false。
func mp4BoxAdvance(r io.ReaderAt, off int64, size uint64) (nextOff int64, stop bool, ok bool) {
	if size == 0 { // size 0 = box 延伸到文件尾（最后一个 box）。
		return 0, true, false
	}
	if size == 1 { // 64 位扩展 size：读取扩展字段。
		var ext [8]byte
		if _, err := r.ReadAt(ext[:], off+8); err != nil {
			return 0, false, false
		}
		size = uint64(ext[0])<<56 | uint64(ext[1])<<48 | uint64(ext[2])<<40 | uint64(ext[3])<<32 |
			uint64(ext[4])<<24 | uint64(ext[5])<<16 | uint64(ext[6])<<8 | uint64(ext[7])
	}
	return off + int64(size), false, true
}

// mvexInside 在 moov box 的 payload（从 payloadOff 起、boxSize-8 字节）中按 box 结构扫描
// mvex 子 box（fMP4 的 mvex 声明通常在 moov 内）。box 边界 8 字节对齐（[4B size][4B type]）
// ——用结构扫描而非 4 字节盲扫（盲扫会因 size 字段恰好含 "mvex" 字节序列误报，或错位漏检）。
// 仅读前 maxMvexScanBytes 字节（moov 通常很小；过大则放弃，统计宁可漏报）。
func mvexInside(r io.ReaderAt, payloadOff int64, boxSize uint64) bool {
	const maxMvexScanBytes = 4 << 20 // 4MiB 上限（moov 通常远小于此）
	limit := min(int64(boxSize-8), maxMvexScanBytes)
	if limit <= 0 {
		return false
	}
	buf := make([]byte, limit)
	if _, err := r.ReadAt(buf, payloadOff); err != nil && err != io.EOF {
		return false
	}
	// 在缓冲内容中按 box 结构扫描：每个子 box [4B size][4B type]，type == "mvex" 即命中。
	off := 0
	for off+8 <= len(buf) {
		size := uint64(buf[off])<<24 | uint64(buf[off+1])<<16 | uint64(buf[off+2])<<8 | uint64(buf[off+3])
		typ := string(buf[off+4 : off+8])
		if typ == "mvex" {
			return true
		}
		if size < 8 {
			return false // 非法 box size（统计宁可漏报，避免误判）
		}
		if size > uint64(len(buf)-off) {
			return false // box 越出扫描上限（moov 后续未读完，mvex 通常靠前）
		}
		off += int(size)
	}
	return false
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

// ReadAt 实现 io.ReaderAt（供 fMP4 检测显式偏移扫描；detectFragmentedMP4 用 ReadAt
// 而非共享 reader 状态，避免 seek 错位）。
func (r *readerAtReader) ReadAt(p []byte, off int64) (int, error) {
	return r.r.ReadAt(p, off)
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
