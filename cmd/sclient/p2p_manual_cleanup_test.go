// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cli"
)

// TestManualSignaler_Cleanup_DeletesOwnFile 验证 Cleanup 删除本侧写出的残留 SDP 文件
// （对端已消费即文件被读删 → 已不存在 → 跳过；文件仍在 → 删除）。
func TestManualSignaler_Cleanup_DeletesOwnFile(t *testing.T) {
	dir := t.TempDir()
	offerFile := filepath.Join(dir, "offer.sdp")
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
	sig := newManualSignaler(offerFile, filepath.Join(dir, "answer.sdp"), ios)

	if err := sig.SendOffer("peer", `{"type":"offer","sdp":"v=0\r\n..."}`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(offerFile); err != nil {
		t.Fatalf("offer 文件应存在: %v", err)
	}

	// 场景 A：对端已消费（readSDP 读删）→ Cleanup 无副作用（不重新创建文件）。
	// 真实模拟"对端已读走 offer 文件"，而非直接 Cleanup（原测试注释与行为不符，
	// 断言也是空转）。I68：强断言——Cleanup 后文件必须仍不存在。
	if _, err := readSDP(offerFile, "offer", ios); err != nil {
		t.Fatalf("模拟对端读删 offer 失败: %v", err)
	}
	if _, err := os.Stat(offerFile); !os.IsNotExist(err) {
		t.Fatalf("对端读删后 offer 文件应不存在: %v", err)
	}
	sig.Cleanup()
	if _, err := os.Stat(offerFile); !os.IsNotExist(err) {
		t.Fatalf("Cleanup 对已消费文件不应有副作用（不得重新创建）: %v", err)
	}

	// 场景 B：文件仍在（对端未消费/打洞中途退出）→ Cleanup 删除
	if err := sig.SendOffer("peer", `{"type":"offer","sdp":"v=0\r\n...again"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(offerFile); err != nil {
		t.Fatalf("第二次 offer 应写入: %v", err)
	}
	sig.Cleanup()
	if _, err := os.Stat(offerFile); err == nil {
		t.Fatalf("Cleanup 应删除残留的 offer 文件")
	} else if !os.IsNotExist(err) {
		t.Fatalf("统计文件失败: %v", err)
	}
}

// TestManualSignaler_Cleanup_DeletesAnswerFile 验证 listen 侧（只写 answer、不读 offer 之外的文件）
// 打洞失败/退出时 Cleanup 删除残留 answer 文件——回归 SendAnswer 未记录 writtenFile 的 bug
// （原实现只记 offer，导致 listen 侧 a.sdp 打洞失败后遗留）。
func TestManualSignaler_Cleanup_DeletesAnswerFile(t *testing.T) {
	dir := t.TempDir()
	answerFile := filepath.Join(dir, "answer.sdp")
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
	// listen 侧：offer 由对端拷来已读删，本侧只 SendAnswer 写 answer
	sig := newManualSignaler(filepath.Join(dir, "offer.sdp"), answerFile, ios)

	if err := sig.SendAnswer("peer", `{"type":"answer","sdp":"v=0\r\n..."}`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(answerFile); err != nil {
		t.Fatalf("answer 文件应存在: %v", err)
	}

	// 打洞失败/连接退出 → Cleanup 应删除本侧写出的 answer 文件
	sig.Cleanup()
	if _, err := os.Stat(answerFile); err == nil {
		t.Fatalf("Cleanup 应删除残留的 answer 文件")
	} else if !os.IsNotExist(err) {
		t.Fatalf("统计文件失败: %v", err)
	}
}

// TestManualSignaler_Cleanup_NoSideEffects_NeverSent 验证从未写出任何 SDP 时 Cleanup 是无副作用 no-op。
func TestManualSignaler_Cleanup_NoSideEffects_NeverSent(t *testing.T) {
	dir := t.TempDir()
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
	sig := newManualSignaler(filepath.Join(dir, "offer.sdp"), filepath.Join(dir, "answer.sdp"), ios)

	// 未调用任何 Send* → writtenFile 为空 → Cleanup 不动任何文件
	sig.Cleanup()
	// I70：断言无副作用 = offer/answer 文件均未创建（死测试转有效断言）。
	if _, err := os.Stat(filepath.Join(dir, "offer.sdp")); !os.IsNotExist(err) {
		t.Fatalf("从未发送时 Cleanup 不应创建 offer 文件: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "answer.sdp")); !os.IsNotExist(err) {
		t.Fatalf("从未发送时 Cleanup 不应创建 answer 文件: %v", err)
	}
}

// TestManualSignaler_Cleanup_FileOverwritten 验证对端已重新写入同名文件时
// Cleanup 不删除（内容不再是我们写的那份，防误删新 offer）。
func TestManualSignaler_Cleanup_FileOverwritten(t *testing.T) {
	dir := t.TempDir()
	offerFile := filepath.Join(dir, "offer.sdp")
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
	sig := newManualSignaler(offerFile, filepath.Join(dir, "answer.sdp"), ios)

	if err := sig.SendOffer("peer", `{"type":"offer","sdp":"v=0\r\n...old"}`); err != nil {
		t.Fatal(err)
	}
	// 模拟对端把新 offer 覆盖同名文件
	newOld := `{"type":"offer","sdp":"v=0\r\n...new"}`
	if err := os.WriteFile(offerFile, []byte(newOld), 0o600); err != nil {
		t.Fatal(err)
	}

	sig.Cleanup()
	data, err := os.ReadFile(offerFile)
	if err != nil {
		t.Fatalf("Cleanup 不应删除被改写的新 offer: %v", err)
	}
	if string(data) != newOld {
		t.Fatalf("Cleanup 不应改写文件内容: %q", string(data))
	}
}
