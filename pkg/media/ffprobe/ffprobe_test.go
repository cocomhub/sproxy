// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ffprobe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// fakeRunner 是注入式假 ffprobe 执行器：返回固定 JSON（或模拟失败），跨平台不依赖
// 真实 ffmpeg/脚本可执行性。
type fakeRunner struct {
	out     string // stdout（合法 JSON）
	failErr error  // 非 nil 时模拟子进程失败
}

func (f *fakeRunner) Run(_ context.Context, _ string, _ io.Reader) ([]byte, error) {
	if f.failErr != nil {
		return nil, f.failErr
	}
	return []byte(f.out), nil
}

// TestKeyframeOffsets_ValidJSON：ffprobe 返回合法关键帧 JSON（-show_packets 格式）→
// 解析出升序 pos（flags 首字符 'K' 为关键帧）。
func TestKeyframeOffsets_ValidJSON(t *testing.T) {
	t.Parallel()
	const jsonOut = `{
  "packets": [
    {"flags": "K__", "pos": 512},
    {"flags": "___", "pos": 1012},
    {"flags": "K__", "pos": 2400},
    {"flags": "___", "pos": 3000}
  ]
}`
	got, err := keyframeOffsetsWithRunner(&fakeRunner{out: jsonOut}, bytes.NewReader(nil), 4000)
	if err != nil {
		t.Fatalf("KeyframeOffsets: %v", err)
	}
	want := []int64{512, 2400}
	if len(got) != len(want) {
		t.Fatalf("关键帧=%v，应为 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("关键帧[%d]=%d，应为 %d", i, got[i], want[i])
		}
	}
}

// TestKeyframeOffsets_NoFrames：无关键帧帧 → 空结果（无关键帧）。
func TestKeyframeOffsets_NoFrames(t *testing.T) {
	t.Parallel()
	got, err := keyframeOffsetsWithRunner(&fakeRunner{out: `{"packets":[]}`}, bytes.NewReader(nil), 100)
	if err != nil {
		t.Fatalf("KeyframeOffsets: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("无关键帧应返回空，got %v", got)
	}
}

// TestKeyframeOffsets_RunnerFail：子进程失败（无 ffmpeg / 非零退出）→ 返回错误（不 panic）。
func TestKeyframeOffsets_RunnerFail(t *testing.T) {
	t.Parallel()
	_, err := keyframeOffsetsWithRunner(&fakeRunner{failErr: errors.New("exit status 1")}, bytes.NewReader(nil), 100)
	if err == nil {
		t.Error("子进程失败应报错")
	}
}

// TestKeyframeOffsets_MissingBin：runner 返回 ErrFFprobeMissing → 哨兵错误透传。
func TestKeyframeOffsets_MissingBin(t *testing.T) {
	t.Parallel()
	_, err := keyframeOffsetsWithRunner(&fakeRunner{failErr: ErrFFprobeMissing}, bytes.NewReader(nil), 100)
	if !errors.Is(err, ErrFFprobeMissing) {
		t.Fatalf("应透传 ErrFFprobeMissing，got: %v", err)
	}
}

// TestKeyframeOffsets_MalformedJSON：ffprobe 输出非法 JSON → 错误（不 panic）。
func TestKeyframeOffsets_MalformedJSON(t *testing.T) {
	t.Parallel()
	_, err := keyframeOffsetsWithRunner(&fakeRunner{out: "not-json{"}, bytes.NewReader(nil), 100)
	if err == nil {
		t.Error("非法 JSON 应报错")
	}
	if !strings.Contains(err.Error(), "JSON") {
		t.Errorf("错误应含 JSON 信息，got: %v", err)
	}
}

// TestKeyframeOffsets_NilInput：nil reader → 错误（fail-closed）。
func TestKeyframeOffsets_NilInput(t *testing.T) {
	t.Parallel()
	_, err := keyframeOffsetsWithRunner(&fakeRunner{out: `{"packets":[]}`}, nil, 100)
	if err == nil {
		t.Error("nil reader 应报错")
	}
}

// TestKeyframeOffsets_BadSize：非正文件大小 → 错误（fail-closed）。
func TestKeyframeOffsets_BadSize(t *testing.T) {
	t.Parallel()
	_, err := keyframeOffsetsWithRunner(&fakeRunner{out: `{"packets":[]}`}, bytes.NewReader(nil), 0)
	if err == nil {
		t.Error("非正文件大小应报错")
	}
}

// TestParseKeyframes_DuplicatesAndOrder：重复 pos 去重 + 乱序升序 + 非法 pos 跳过。
func TestParseKeyframes_DuplicatesAndOrder(t *testing.T) {
	t.Parallel()
	got, err := parseKeyframes([]byte(`{"packets":[
		{"flags":"K__","pos":"3000"},
		{"flags":"K__","pos":"512"},
		{"flags":"K__","pos":"512"},
		{"flags":"K__","pos":"-5"},
		{"flags":"K__","pos":"abc"}
	]}`))
	if err != nil {
		t.Fatalf("parseKeyframes: %v", err)
	}
	want := []int64{512, 3000}
	if len(got) != len(want) {
		t.Fatalf("关键帧=%v，应为 %v（重复去重 + 非法 pos 跳过）", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("关键帧[%d]=%d，应为 %d", i, got[i], want[i])
		}
	}
}
