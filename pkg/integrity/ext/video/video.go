// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package video

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	"github.com/cocomhub/sproxy/pkg/integrity"
	"github.com/cocomhub/sproxy/pkg/media/ffprobe"
)

// testSeamIndexer 是 KeyframeOffsets 索引器的注入缝（生产 nil → 用 ffprobe.Indexer{}；
// 测试替换以模拟缺 ffprobe/取消/超时，避免真实 ffprobe 依赖）。测试通过
// setTestSeamIndexer（持锁）替换；生产 Check 只读它并做**局部回落**——绝不写全局
// （并发首次校验对 nil 的懒加载写会产生数据竞争）。KeyframeOffsets 无状态，共享安全。
var testSeamIndexer interface {
	KeyframeOffsets(req shardseal.KeyframeRequest) ([]int64, error)
}

// testSeamMu 保护 testSeamIndexer 的替换（测试专用；生产只读 + 局部回落不竞争）。
var testSeamMu sync.RWMutex

// setTestSeamIndexer 是测试替换注入缝的入口（持写锁）；测试 t.Cleanup 恢复原值。
func setTestSeamIndexer(idx interface {
	KeyframeOffsets(req shardseal.KeyframeRequest) ([]int64, error)
}) {
	testSeamMu.Lock()
	defer testSeamMu.Unlock()
	testSeamIndexer = idx
}

// init 把 VideoChecker 装配进 pkg/integrity 包级默认注册表（插件模式：cmd/sproxy
// 空白导入触发本 init；重复注册同 Kind 由 Registry fail-fast panic——见 Register）。
func init() {
	integrity.Register("video/*", func() integrity.Checker { return VideoChecker{} })
}

// VideoChecker 校验视频语义可用性：ffprobe 可解析容器且含视频流关键帧 → OK。
// 复用 pkg/media/ffprobe（go-mp4 只认 ISO-BMFF；ffprobe 覆盖全部容器 MP4/MKV/WebM/TS/
// AVI/FLV…）。ffprobe 缺失或解析失败 → OK=false（内容异常或环境不含 ffmpeg——下游
// 按任务配置放行/标记；本校验器不阻断未知环境，仅报告语义结果）。
type VideoChecker struct{}

// EstimateMem 预估一次 Check 的峰值内存（保守高估，防低估 OOM）：
//   - ffprobe 子进程常驻 ~64MiB（OS 管理，不占 Go 堆）；
//   - **关键**：ffprobe 输出（-show_packets JSON）经 os.ReadFile 全读进 Go 堆再
//     json.Unmarshal（parseKeyframes 需完整数组）——JSON 大小随视频**帧数**增长
//     （低码率高帧率视频同字节数帧数可高 1-2 个数量级），**与文件字节数无固定比例**；
//     实测 4.6GB/24958 帧 ≈ 2.5MB JSON（~0.5MB/GB 样本密度）。
//   - 估算：JSON 按 size×1MiB/2GB 折算（0.5MB/GB 样本密度，非上浮）+ 进程 64MiB；
//     注意 <2GiB 文件整数除法后 jsonEst=0（预留仅 64MiB）——低码率高帧率小文件是残余
//     低估窗口；但读入端已由 maxFFprobeOutputBytes（256MiB）硬封顶（超限 fail-closed
//     放行），实际占用最坏 = 64MiB 进程 + 256MiB JSON（配额信号量按实际读入约束，非
//     估算）。文件本身由 ffprobe 子进程流式读（OS 页缓存，不进 Go 堆）——不计。
func (VideoChecker) EstimateMem(path string, size int64) int64 {
	if size <= 0 {
		if fi, err := os.Stat(path); err == nil {
			size = fi.Size()
		}
	}
	jsonEst := min(
		// 0.5MB/GB 样本密度折算（1MiB per 2GiB）
		size/(2<<30)*(1<<20),
		// JSON 上限 256MiB（与 maxFFprobeOutputBytes 对齐）
		256<<20)
	return 64<<20 + jsonEst
}

// Kind 返回类型标识（注册键，全局唯一）。
func (VideoChecker) Kind() string { return "video/*" }

// Matches 按扩展名族判定归属：.mp4/.mkv/.webm/.mov/.ts/.avi/.flv/.wmv（不区分大小写）。
func (VideoChecker) Matches(name string) bool {
	lower := strings.ToLower(name)
	for _, ext := range []string{".mp4", ".mkv", ".webm", ".mov", ".ts", ".avi", ".flv", ".wmv"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// Check 用 ffprobe 解析 path 指向的视频：KeyframeOffsets 无 err 且返回 ≥1 关键帧
// （有视频流）→ OK=true；否则（无 ffprobe / 容器非法 / 无视频流 / 文件打开失败）
// → OK=false。文件打开失败（路径不存在等）→ error（校验执行错误，非语义判定）。
func (VideoChecker) Check(ctx context.Context, path string, size int64) (*integrity.Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("video check open %s: %w", path, err)
	}
	defer f.Close()
	if size <= 0 {
		if fi, statErr := f.Stat(); statErr != nil {
			return nil, statErr
		} else {
			size = fi.Size()
		}
	}
	// testSeamIndexer 是测试注入缝（生产 nil → 局部回落 ffprobe.Indexer{}；测试经
	// setTestSeamIndexer 持锁替换）。**只读 + 局部回落**，绝不写全局——并发首次校验
	// 对 nil 的懒加载写会构成数据竞争（实测 -race 命中）。
	testSeamMu.RLock()
	seam := testSeamIndexer
	testSeamMu.RUnlock()
	indexer := seam
	if indexer == nil {
		indexer = ffprobe.Indexer{}
	}
	// ffprobe 必须**阻塞等待结果**（用户裁定 2026-10-06）：校验结果必须可得才能决定
	// 后续流程（放行/重下/标记 damaged）——非阻塞会丢失校验语义（取消即 OK:false 被当
	// 语义异常）。子进程由 ffprobe 内部 timeout 兜底（30s~5min 按文件大小），进程不会
	// 无限残留。外层 ctx（任务取消/删除）取消时中止等待：ffprobe 走 context.Background
	// 无法被中断，但 goroutine+select 保证调用方不悬挂——取消任务后的 ffprobe 残留在
	// 其内部 timeout 后自行结束（配额/槽位释放最多推迟 timeout 时长，可接受）。
	//
	// 此前「取消即返回 OK:false」是错误的：调用方把 OK:false 当语义异常累计，导致用户
	// 取消任务被误判为文件损坏（errIntegrityFail 重下/标记 damaged）。
	type offsRes struct {
		offs []int64
		err  error
	}
	resCh := make(chan offsRes, 1)
	go func() {
		offs, kerr := indexer.KeyframeOffsets(shardseal.KeyframeRequest{Path: path, Size: size})
		resCh <- offsRes{offs: offs, err: kerr}
	}()
	var offs []int64
	var kerr error
	select {
	case <-ctx.Done():
		// 任务取消/删除：校验无意义，返回「中止」哨兵——调用方识别后按取消处理，
		// 不得当语义异常累计。ffprobe 残留由内部 timeout 兜底。
		return nil, fmt.Errorf("video check cancelled: %w", ctx.Err())
	case r := <-resCh:
		offs, kerr = r.offs, r.err
	}
	if kerr != nil {
		// R5-I1：环境缺 ffprobe（ErrFFprobeMissing）≠ 文件损坏——视为通过（无校验器
		// 可用，与 plan §5「无校验器 → 视为通过」对齐），避免无 ffmpeg 部署对每个
		// 视频误判 damaged。**输出超上限（ErrFFprobeOutputLimit）与执行/IO 失败
		// （ErrFFprobeExec）同级放行**：合法视频帧数异常多 / 慢盘超时 / 临时文件失败时
		// 校验无能力完成（环境/资源限制 ≠ 文件损坏），不误判 damaged。
		// 其他解析失败（容器非法/无视频流）→ OK=false（真异常）。
		if errors.Is(kerr, ffprobe.ErrFFprobeMissing) ||
			errors.Is(kerr, ffprobe.ErrFFprobeOutputLimit) ||
			errors.Is(kerr, ffprobe.ErrFFprobeExec) {
			return &integrity.Report{OK: true, Reason: "video: ffprobe 无法解析（未安装/输出超限/执行失败），跳过语义校验"}, nil
		}
		return &integrity.Report{OK: false, Reason: fmt.Sprintf("video: ffprobe 解析失败: %v", kerr)}, nil
	}
	if len(offs) == 0 {
		// M3-I1：仅音频轨的合法容器（如音频以 .mp4 命名，m4a 常见）——-select_streams v:0
		// 无视频流输出空 → OK=false 会把合法音频误判 damaged/重下。有内容但非视频 ≠ 损坏：
		// 返回「跳过」Reason（OK=true），与「未知类型无校验器放行」同语义。
		return &integrity.Report{OK: true, Reason: "video: 容器无视频流（仅音频或空流），跳过语义校验"}, nil
	}
	return &integrity.Report{OK: true}, nil
}
