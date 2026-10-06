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
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// ErrFFprobeMissing 是哨兵错误：PATH 中找不到 ffprobe（无 ffmpeg 环境，调用方回落）。
var ErrFFprobeMissing = errors.New("ffprobe: 未找到 ffprobe 可执行文件（需安装 ffmpeg）")

// ErrFFprobeOutputLimit 是哨兵错误：ffprobe 输出 JSON 超 maxFFprobeOutputBytes 上限。
// 语义与 ErrFFprobeMissing 同级——**环境/资源限制 ≠ 文件损坏**：合法视频帧数异常多时
// 校验无能力完成，调用方应视为「无校验器可用」放行（不误判 damaged）。视频校验器
// （ext/video）与 ErrFFprobeMissing 同分支放行。
var ErrFFprobeOutputLimit = errors.New("ffprobe: 输出超上限（帧数异常多）")

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
//
// **Path/Reader 双模式（评审 I1 修复，2026-10-04）**：ffprobe 需要可 seek 输入——
// stdin（-i pipe:0）不可 seek，对 moov 在尾部的大文件会降级/失败。有真实路径时
// 必须传 `-i <path>`（ffprobe 直接读文件，可 seek 最优）；无路径（secretdata 写路径
// 持内存明文）才退 stdin。Path 字段非空时优先用路径，Reader 字段仅作无路径兜底。
type ffprobeRunner interface {
	// Run 执行 ffprobe，返回 stdout 字节。Path 非空 → -i <path>；否则 → -i pipe:0 + stdin。
	Run(ctx context.Context, path string, file io.Reader) ([]byte, error)
}

// realRunner 生产实现：exec ffprobe，stdin 传文件流，stdout 写临时文件（避免大文件
// JSON 输出无界占内存——评审实测 4.6GB 视频关键帧 JSON 可达 MB 级，且超大文件可能更多）。
//
// **不依赖解码器（2026-10-04 真实文件实测决策）**：用 `-show_packets`（纯 demux 层读容器
// packet，不解码）替代 `-skip_frame nokey -show_frames`（依赖解码器判断关键帧）——
// 实测 4.6GB 损伤 MP4 上 skip_frame 因 h264 解码中断只解析前 14293 帧，show_packets 完整
// 解析 24958 帧（与 go-mp4 一致）。关键帧判定：packet.flags 首字符 'K'（MP4 由 stss 表、
// MKV 由 SimpleBlock keyframe flag、TS 由容器标志给出，均在 demux 层，无需解码）。
type realRunner struct{}

// Run 执行 ffprobe。**Path 模式（评审 I1 修复，2026-10-04）**：path 非空时用
// `-i <path>`（ffprobe 直接读文件，可 seek——大文件降级不存在）；path 空才用
// stdin（`-i pipe:0` + file）。stdout 写临时文件（避免大文件 JSON 无界占内存）。
func (realRunner) Run(ctx context.Context, path string, file io.Reader) ([]byte, error) {
	bin, lerr := exec.LookPath("ffprobe")
	if lerr != nil {
		return nil, ErrFFprobeMissing
	}
	args := []string{
		"-v", "error",
		"-select_streams", "v:0",
		"-show_packets",
		"-show_entries", "packet=flags,pos",
		"-of", "json",
	}
	if path != "" {
		args = append(args, "-i", path)
	} else {
		args = append(args, "-i", "pipe:0")
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if path == "" {
		cmd.Stdin = file
	}
	// stdout 写临时文件（os.CreateTemp）而非 bytes.Buffer：ffprobe 输出随视频帧数增长，
	// 大文件可能产生数 MB 至数百 MB JSON——Buffer 无界占内存，临时文件可控（读回后删除）。
	outTmp, terr := os.CreateTemp("", "sproxy-ffprobe-*.json")
	if terr != nil {
		return nil, fmt.Errorf("ffprobe: 创建临时输出文件失败: %w", terr)
	}
	defer os.Remove(outTmp.Name())
	cmd.Stdout = outTmp
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		outTmp.Close()
		return nil, fmt.Errorf("ffprobe 执行失败: %w", runErr)
	}
	// M5-I2：ffprobe 输出 JSON 大小 ∝ 帧数（低码率高帧率视频同字节数帧数可高 1-2 个
	// 数量级），无界 ReadFile 会把数百 MB JSON 全读进 Go 堆——估算（EstimateMem 按
	// 字节折算）在低码率场景严重低估，配额信号量被实际占用突破 → OOM。先 Stat 探测
	// 输出大小（在 Close 前），超上限 fail-closed（解析失败 → 调用方按「无校验器可用」
	// 放行，不误判 damaged）；同时避免超大 JSON 撑爆内存。
	if fi, ferr := outTmp.Stat(); ferr != nil {
		outTmp.Close()
		return nil, fmt.Errorf("ffprobe: stat 输出文件失败: %w", ferr)
	} else if fi.Size() > maxFFprobeOutputBytes {
		outTmp.Close()
		return nil, fmt.Errorf("%w（size=%d）", ErrFFprobeOutputLimit, fi.Size())
	}
	if serr := outTmp.Close(); serr != nil {
		return nil, fmt.Errorf("ffprobe: 关闭输出文件失败: %w", serr)
	}
	out, rerr := os.ReadFile(outTmp.Name())
	if rerr != nil {
		return nil, fmt.Errorf("ffprobe: 读输出文件失败: %w", rerr)
	}
	return out, nil
}

// maxFFprobeOutputBytes 是 ffprobe 输出 JSON 的字节上限（低码率高帧率视频防撑爆内存；
// 超限 fail-closed 解析失败 → 校验放行，不误判 damaged）。与 VideoChecker.EstimateMem
// 的 JSON 封顶对齐（256MiB）。
const maxFFprobeOutputBytes = int64(256 << 20)

// Indexer 是 shardseal.KeyframeIndexer 的装配实例（ffprobe 子进程解析器）。
type Indexer struct{}

// KeyframeOffsets 实现 shardseal.KeyframeIndexer（req 单一模式，2026-10-04）：ffprobe
// 解析关键帧（I 帧）文件绝对偏移。流程：有 Path 走文件路径（ffprobe 子进程直接读文件，
// 避免 stdin 不可 seek 对大文件降级——4.6GB 实测文件路径 3.9s/24958 帧、stdin 0 帧）；
// 无 Path 用 stdin（Reader 流）。升序返回。任意失败（无 ffprobe / 非零退出 / JSON 非法 /
// 截断）返回「已解析部分 + err」，不 panic（不可信输入隔离见 recover）。
//
// **seek 输入原则（2026-10-04 用户裁定：最简单=最高效，禁隐藏高代价行为）**：
// 有 Path（调用方能提供真实文件路径，如 EncryptShards 文件变体）→ 文件路径模式
// （ffprobe 直接读，可 seek，最优）；无 Path（secretdata 写路径持内存明文）→ stdin
// 流（接受 stdin 不可 seek 对大文件降级，**绝不落临时文件复制数据**）。
func (Indexer) KeyframeOffsets(req shardseal.KeyframeRequest) ([]int64, error) {
	if req.Path != "" {
		return keyframeOffsetsPath(realRunner{}, req.Path, req.Size)
	}
	return KeyframeOffsets(req.Reader, req.Size)
}

// KeyframeOffsets 解析视频容器的关键帧字节偏移（Reader 流，stdin 模式）。
func KeyframeOffsets(r io.ReaderAt, fileSize int64) ([]int64, error) {
	return keyframeOffsetsWithRunner(realRunner{}, r, fileSize)
}

// keyframeOffsetsPath 解析视频容器的关键帧字节偏移（文件路径模式——ffprobe 子进程直接
// 读文件，可 seek，最优路径）。
func keyframeOffsetsPath(rr ffprobeRunner, path string, fileSize int64) ([]int64, error) {
	// path 模式由 runner 直接 `-i <path>`，无需打开文件流（I1 修复：此前只是打开后仍
	// stdin 喂入，ffprobe 从未收到真实路径，文件模式名不副实）。
	return keyframeOffsetsWithRunnerPath(rr, path, fileSize)
}

// keyframeOffsetsWithRunnerPath 是 Path 模式的注入变体（真实文件端到端测试用）。
func keyframeOffsetsWithRunnerPath(rr ffprobeRunner, path string, fileSize int64) (offs []int64, err error) {
	if path == "" {
		return nil, fmt.Errorf("ffprobe: Path 模式路径为空")
	}
	if fileSize <= 0 {
		return nil, fmt.Errorf("ffprobe: 非法文件大小 %d", fileSize)
	}
	ctx, cancel := context.WithTimeout(context.Background(), ffprobeTimeout(fileSize))
	defer cancel()
	out, runErr := rr.Run(ctx, path, nil)
	if runErr != nil {
		return nil, runErr
	}
	return parseKeyframes(out)
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
	ctx, cancel := context.WithTimeout(context.Background(), ffprobeTimeout(fileSize))
	defer cancel()
	out, runErr := rr.Run(ctx, "", &readerAtReader{r: r, size: fileSize})
	if runErr != nil {
		return nil, runErr
	}
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

// ffprobeTimeout 按文件大小自适应子进程超时（评审 I-1/M-2 修复）：小文件 30s 起，每 GB
// 加 15s——真实 4.6GB 文件 ffprobe 实测 ~5.7s 完成，30s+4×15=90s 余量充足，不会误杀
// 正常工作；防恶意超大文件挂死仍有界。此前固定 10s 对 4G+ 大文件必误杀（静默降级 fixed）。
func ffprobeTimeout(fileSize int64) time.Duration {
	const base = 30 * time.Second
	const perGB = 15 * time.Second
	gb := max(fileSize>>30, 0)
	// 上限 5 分钟（超大视频防御），下限 30s。
	return min(base+time.Duration(gb)*perGB, 5*time.Minute)
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

// ffprobeOutput 是 ffprobe -show_packets -of json 的输出结构（packet 层，不解码）。
type ffprobeOutput struct {
	Packets []ffprobePacket `json:"packets"`
}

// ffprobePacket 是单个容器 packet 条目：flags 首字符 'K' = 关键帧（MP4 由 stss 表、
// MKV 由 SimpleBlock keyframe flag、TS 由容器标志给出，均在 demux 层）；pos 为文件内偏移。
type ffprobePacket struct {
	Flags string          `json:"flags"`
	Pos   json.RawMessage `json:"pos"`
}

// parsePktPos 解析 pos RawMessage（JSON number 或 string）。
func parsePktPos(raw json.RawMessage) (int64, error) {
	s := strings.TrimSpace(string(raw))
	s = strings.Trim(s, `"`)
	return strconv.ParseInt(s, 10, 64)
}

// parseKeyframes 从 ffprobe -show_packets JSON 提取关键帧 pos（升序；去重）。
// 关键帧判定：packet.flags 首字符 'K'（AV_PKT_FLAG_KEY，demux 层给出，不依赖解码器）。
// 输出缺帧/字段（截断或非视频）→ 空结果 + err（降级由 shardseal 决策）。
func parseKeyframes(raw []byte) ([]int64, error) {
	var out ffprobeOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("ffprobe: 输出 JSON 解析失败: %w", err)
	}
	var keys []int64
	seen := map[int64]bool{}
	for _, p := range out.Packets {
		if len(p.Flags) == 0 || p.Flags[0] != 'K' || len(p.Pos) == 0 {
			continue
		}
		pos, perr := parsePktPos(p.Pos)
		if perr != nil || pos < 0 {
			continue // 无效 pos 跳过（截断/异常 packet）
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
