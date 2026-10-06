// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package video

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	"github.com/cocomhub/sproxy/pkg/integrity"
	"github.com/cocomhub/sproxy/pkg/media/ffprobe"
)

// testSeamIndexer 是 KeyframeOffsets 索引器的注入缝（生产 nil → 用 ffprobe.Indexer{}；
// 测试替换以模拟缺 ffprobe/取消/超时，避免真实 ffprobe 依赖）。可变全局变量仅测试
// 场景使用；生产路径不触碰（并发校验共享同一 Indexer{} 实例——KeyframeOffsets 无状态）。
var testSeamIndexer interface {
	KeyframeOffsets(req shardseal.KeyframeRequest) ([]int64, error)
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

// EstimateMem 预估一次 Check 的峰值内存：ffprobe 子进程输出写临时文件（不占内存），
// 进程自身约 64MB 常驻（保守固定估算）。子进程内存由 OS 管理不占用进程堆，但配额
// 治理按并发子进程数折算（MaxCheckMemBytes 控制同时 ffprobe 数量）。
func (VideoChecker) EstimateMem(path string, size int64) int64 {
	return 64 << 20 // 64MiB 固定估算（ffprobe 子进程）
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
	// testSeamIndexer 是测试注入缝（生产恒 ffprobe.Indexer{}；测试可替换 fakeRunner 模拟
	// 缺 ffprobe/取消/超时场景，见 video_test.go）。
	if testSeamIndexer == nil {
		testSeamIndexer = ffprobe.Indexer{}
	}
	indexer := testSeamIndexer
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
		// 视频误判 damaged。其他解析失败（容器非法/无视频流）→ OK=false（真异常）。
		if errors.Is(kerr, ffprobe.ErrFFprobeMissing) {
			return &integrity.Report{OK: true, Reason: "video: ffprobe 未安装，跳过语义校验"}, nil
		}
		return &integrity.Report{OK: false, Reason: fmt.Sprintf("video: ffprobe 解析失败: %v", kerr)}, nil
	}
	if len(offs) == 0 {
		return &integrity.Report{OK: false, Reason: "video: 容器无可解析关键帧（无视频流或空文件）"}, nil
	}
	return &integrity.Report{OK: true}, nil
}
