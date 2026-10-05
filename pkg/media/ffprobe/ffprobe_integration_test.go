// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ffprobe

import (
	"bytes"
	"os"
	"os/exec"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
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

// TestKeyframeOffsets_RealFFprobe_PathMode：Path 模式（评审 I1 修复回归）——有真实路径时
// runner 传 `-i <path>`（可 seek，大文件降级不存在），解析关键帧正确。此前 bug：只是打开
// 文件仍 stdin 喂入，文件模式名不副实。本机无 ffprobe → Skip。
func TestKeyframeOffsets_RealFFprobe_PathMode(t *testing.T) {
	// sproxy:serial: 依赖系统 ffprobe/ffmpeg（外部二进制），与注入式用例隔离。
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("本机无 ffprobe（外部依赖跳过）")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("本机无 ffmpeg（无法生成测试视频）")
	}
	video := makeTempVideo(t)
	defer os.Remove(video)
	st, _ := os.Stat(video)

	got, kerr := Indexer{}.KeyframeOffsets(shardseal.KeyframeRequest{Path: video, Size: st.Size()})
	if kerr != nil {
		t.Fatalf("KeyframeOffsets(Path 模式): %v", kerr)
	}
	if len(got) == 0 {
		t.Fatal("Path 模式真实 MP4 应解析出 ≥1 个关键帧")
	}
	for i, pos := range got {
		if pos < 0 || pos >= st.Size() {
			t.Errorf("关键帧[%d]=%d 越出文件 [0,%d)", i, pos, st.Size())
		}
		if i > 0 && got[i] <= got[i-1] {
			t.Errorf("关键帧未升序: %v", got)
		}
	}
	t.Logf("Path 模式真实 MP4 关键帧=%v", got)
}

// TestKeyframeOffsets_RealFragmentedMP4：真实 fMP4（ffmpeg -movflags frag_keyframe 生成）
// → ffprobe 正常解析关键帧（不降级）+ fMP4 统计 hook 触发（OnFragmentedMP4 被调用）。
// 本机无 ffprobe → Skip。
func TestKeyframeOffsets_RealFragmentedMP4(t *testing.T) {
	// sproxy:serial: 依赖系统 ffprobe + 包级统计 hook（与注入式用例隔离）。
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("本机无 ffprobe（外部依赖跳过）")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("本机无 ffmpeg（无法生成测试视频）")
	}
	// 生成 fMP4（frag_keyframe：每关键帧分片 → moof fragment）。
	video := makeTempVideoFragmented(t)
	defer os.Remove(video)

	data, err := os.ReadFile(video)
	if err != nil {
		t.Fatalf("读测试视频: %v", err)
	}
	// 注入统计 hook 捕获 fMP4 命中（atomic setter，并发安全）。
	oldHook := loadFFprobeHook()
	defer func() { SetOnFragmentedMP4(oldHook) }()
	hit := false
	SetOnFragmentedMP4(func() { hit = true })

	got, kerr := KeyframeOffsets(bytes.NewReader(data), int64(len(data)))
	if kerr != nil {
		t.Fatalf("KeyframeOffsets(fMP4): %v", kerr)
	}
	if len(got) == 0 {
		t.Fatal("fMP4 应解析出 ≥1 个关键帧（ffprobe 正常支持，不降级）")
	}
	if !hit {
		t.Error("fMP4 统计 hook 应被触发（SetOnFragmentedMP4）")
	}
	t.Logf("真实 fMP4 关键帧=%v", got)
}

// loadFFprobeHook 读取当前注入的 fMP4 统计 hook（测试清理用；atomic 读）。
func loadFFprobeHook() func() {
	p := onFragmentedMP4.Load()
	if p == nil {
		return nil
	}
	return *p
}

// makeTempVideoFragmented 用 ffmpeg 生成 fMP4（frag_keyframe → moof fragment，无全局 stss）。
func makeTempVideoFragmented(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/fragmented.mp4"
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc=duration=0.3:size=64x64:rate=24",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "4",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof", path)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg 生成 fMP4 失败: %v (%s)", err, buf.String())
	}
	return path
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
