// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// execRunner 是外部命令执行抽象（**结构体字段注入**，不用包级可变量做 seam）。
type execRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// FFProbe 是基于 ffprobe 的 MediaProber 实现。
//
// 行为约定（对消费方重要）：
//   - **尽力而为**：ffprobe 不存在 / 超时 / 输出无法解析 ⇒ 返回错误，调用方应忽略并留空；
//   - 解析失败**不得**让调用链整体失败；
//   - 每实例独立，可安全并发使用（不带可变状态）。
type FFProbe struct {
	bin string     // 可执行文件（默认 "ffprobe"）
	run execRunner // 可注入（测试用）
}

// NewFFProbe 创建 ffprobe 探测器；bin 为空时用 "ffprobe"（走 PATH）。
func NewFFProbe(bin string) *FFProbe {
	if bin == "" {
		bin = "ffprobe"
	}
	return &FFProbe{bin: bin, run: defaultExecRunner}
}

// WithRunner 注入自定义 runner（仅供测试构造使用；生产不调用）。
func (p *FFProbe) WithRunner(fn execRunner) *FFProbe {
	if fn != nil {
		p.run = fn
	}
	return p
}

// Probe 实现 MediaProber：对 url 探测首个视频流的宽高。
func (p *FFProbe) Probe(ctx context.Context, url string) (ProbeResult, error) {
	if strings.TrimSpace(url) == "" {
		return ProbeResult{}, fmt.Errorf("ffprobe: empty url")
	}
	bin := p.bin
	if bin == "" {
		bin = "ffprobe"
	}
	run := p.run
	if run == nil {
		run = defaultExecRunner
	}
	out, err := run(ctx, bin,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-of", "json",
		url,
	)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("ffprobe: %w", err)
	}
	return parseFFProbeSize(out)
}

// ffprobeSizeJSON 是 ffprobe `-of json` 的最小反序列化结构。
type ffprobeSizeJSON struct {
	Streams []struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"streams"`
}

// parseFFProbeSize 从 ffprobe JSON 输出解析首个视频流的宽高（纯函数，便于单测）。
func parseFFProbeSize(out []byte) (ProbeResult, error) {
	var v ffprobeSizeJSON
	if err := json.Unmarshal(out, &v); err != nil {
		return ProbeResult{}, fmt.Errorf("ffprobe: parse json: %w", err)
	}
	if len(v.Streams) == 0 {
		return ProbeResult{}, fmt.Errorf("ffprobe: no video stream")
	}
	s := v.Streams[0]
	if s.Width <= 0 || s.Height <= 0 {
		return ProbeResult{}, fmt.Errorf("ffprobe: invalid size %s", strconv.Itoa(s.Width)+"x"+strconv.Itoa(s.Height))
	}
	return ProbeResult{Width: s.Width, Height: s.Height}, nil
}

// defaultExecRunner 用 os/exec 执行外部命令（生产路径）。
func defaultExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}
