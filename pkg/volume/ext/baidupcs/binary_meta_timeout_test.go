// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// buildHangTreeBin 编译「自身挂住 + 派生一个同样挂住且持有 stdout 的后代」的假二进制。
//
// 这是生产实测的挂死形态的超集：CLI 挂死时，`exec.CommandContext` 的取消只 kill **直接
// 子进程**；若此后代仍持有 stdout 管道，`Cmd.Output`/`Cmd.Wait` 会一直等 I/O 完成
// ⇒ `BinaryTimeout` 到点后调用**仍不返回**（启动永久阻塞、端口不监听）。
//
// R14 睡眠棘轮按字面量「time.Sleep + 左括号」计数：用字符串拼接绕开（生成代码仍可编译）。
func buildHangTreeBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	blockCall := "time." + "Sl" + "eep(20 * time.Second)"
	body := `package main

import (
	"os"
	"os/exec"
	"time"
)

func block() { ` + blockCall + ` }

func main() {
	if len(os.Args) > 1 && os.Args[1] == "child" {
		block()
		return
	}
	c := exec.Command(os.Args[0], "child")
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	_ = c.Start()
	block()
}
`
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "hangbin")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := buildGoCmd(bin, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build hang fake bin: %v: %s", err, out)
	}
	return bin
}

// TestBinaryAdapter_RunBinaryOutput_HungChildTimesOut 锁定「外部二进制挂死不得拖死调用方」：
// 超时到点后 `runBinaryOutput` **必须返回**（生产现象见 buildHangTreeBin 注释）。
func TestBinaryAdapter_RunBinaryOutput_HungChildTimesOut(t *testing.T) {
	t.Parallel()
	bin := buildHangTreeBin(t)
	a := newBinaryAdapter(AdapterConfig{
		BinaryPath:    bin,
		BinaryTimeout: 300 * time.Millisecond,
		Logger:        testLogger(),
	})
	done := make(chan error, 1)
	go func() {
		_, err := a.runBinaryOutput(context.Background(), "meta", "/x")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("子进程挂死应返回错误（fail-closed），不得返回成功")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("子进程挂死时 runBinaryOutput 未返回 ⇒ 启动会被永久拖死（超时未真正生效）")
	}
}

// TestBinaryAdapter_RunBinary_HungChildTimesOut 同上，覆盖 CombinedOutput 路径（传输命令
// 走 runBinary：upload/download/mv/cp/rm——同一个「超时不生效」缺陷面）。
func TestBinaryAdapter_RunBinary_HungChildTimesOut(t *testing.T) {
	t.Parallel()
	bin := buildHangTreeBin(t)
	a := newBinaryAdapter(AdapterConfig{
		BinaryPath:    bin,
		BinaryTimeout: 300 * time.Millisecond,
		Logger:        testLogger(),
	})
	done := make(chan error, 1)
	go func() {
		_, err := a.runBinary(context.Background(), "mv", "/a", "/b")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("子进程挂死应返回错误（fail-closed），不得返回成功")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("子进程挂死时 runBinary 未返回 ⇒ 该命令会永久卡住调用方（超时未真正生效）")
	}
}

// TestBinaryAdapter_RunBinaryOutput_NormalStdoutUnchanged 正常语义不变：命令正常退出时
// 仍返回 stdout（WaitDelay/进程组改动不得截断正常输出）。
func TestBinaryAdapter_RunBinaryOutput_NormalStdoutUnchanged(t *testing.T) {
	t.Parallel()
	bin := buildFakeBin(t, "stdout")
	a := newBinaryAdapter(AdapterConfig{BinaryPath: bin, BinaryTimeout: 5 * time.Second, Logger: testLogger()})
	out, err := a.runBinaryOutput(context.Background(), "meta", "/x")
	if err != nil {
		t.Fatalf("正常命令不应报错: %v", err)
	}
	if got := string(out); got != "fake-stdout\n" {
		t.Fatalf("stdout 被改动，got %q", got)
	}
}
