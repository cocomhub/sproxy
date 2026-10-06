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
	var indexer interface {
		KeyframeOffsets(req shardseal.KeyframeRequest) ([]int64, error)
	} = ffprobe.Indexer{}
	// R2-P1：ffprobe 内部用 context.Background()（无 ctx 参数），不随调用方取消——
	// 用户取消任务后 ffprobe 子进程仍跑完（最长 5min），下载槽/配额释放被推迟。
	// 用 goroutine + select 包裹：外层 ctx 取消即中止等待（子进程由 ffprobe 内部
	// timeout 兜底，进程残留由其自有超时清理；此处保证调用方不阻塞）。
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
		return &integrity.Report{OK: false, Reason: "video: 校验被取消（任务取消/删除）"}, nil
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
