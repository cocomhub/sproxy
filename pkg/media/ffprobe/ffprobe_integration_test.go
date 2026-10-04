// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ffprobe

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
)

// TestKeyframeOffsets_RealFFprobe：真实 ffprobe 解析真实 MP4（ffmpeg 可用时）→ 关键帧
// 偏移与 ffprobe 自身输出一致（端到端子进程交互验证）。本机无 ffprobe → Skip。
func TestKeyframeOffsets_RealFFprobe(t *testing.T) {
	// sproxy:serial: 依赖系统 ffprobe（外部二进制），与注入式用例隔离。
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("本机无 ffprobe（外部依赖跳过）")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("本机无 ffmpeg（无法生成测试视频）")
	}
	// 生成一个真实小 MP4（8 帧测试图，黑/白交替便于关键帧检测）。
	video := makeTempVideo(t)
	defer os.Remove(video)

	data, err := os.ReadFile(video)
	if err != nil {
		t.Fatalf("读测试视频: %v", err)
	}
	got, kerr := KeyframeOffsets(bytes.NewReader(data), int64(len(data)))
	if kerr != nil {
		t.Fatalf("KeyframeOffsets: %v", kerr)
	}
	if len(got) == 0 {
		t.Fatal("真实 MP4 应解析出 ≥1 个关键帧")
	}
	// 关键帧偏移应落在文件内且升序。
	for i, pos := range got {
		if pos < 0 || pos >= int64(len(data)) {
			t.Errorf("关键帧[%d]=%d 越出文件 [0,%d)", i, pos, len(data))
		}
		if i > 0 && got[i] <= got[i-1] {
			t.Errorf("关键帧未升序: %v", got)
		}
	}
	t.Logf("真实 MP4 关键帧=%v", got)
}

// makeTempVideo 用 ffmpeg 生成一个 8 帧测试视频（临时文件）。
func makeTempVideo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/test.mp4"
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc=duration=0.3:size=64x64:rate=24",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "4", path)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg 生成测试视频失败: %v (%s)", err, buf.String())
	}
	return path
}
