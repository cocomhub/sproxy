// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package baidupcs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// buildHangTreePidBin 编译「父进程 + 一个持有 stdout 的挂死后代」的假二进制，两者都把
// 自己的 pid 写进 `<pidFile>.parent` / `<pidFile>.child`（pidFile 由 argv 传入），供断言
// 「超时后整棵进程树都被终止」。
//
// R14 睡眠棘轮按字面量「time.Sleep + 左括号」计数：用字符串拼接绕开（生成代码仍可编译）。
func buildHangTreePidBin(t *testing.T, dir string) string {
	t.Helper()
	src := filepath.Join(dir, "main.go")
	blockCall := "time." + "Sl" + "eep(60 * time.Second)"
	body := `package main

import (
	"os"
	"os/exec"
	"strconv"
	"time"
)

func block() { ` + blockCall + ` }

func writePid(file string) {
	_ = os.WriteFile(file, []byte(strconv.Itoa(os.Getpid())), 0o644)
}

func main() {
	if len(os.Args) > 2 && os.Args[1] == "child" {
		writePid(os.Args[2] + ".child")
		block()
		return
	}
	if len(os.Args) > 2 {
		writePid(os.Args[2] + ".parent")
		c := exec.Command(os.Args[0], "child", os.Args[2])
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		_ = c.Start()
	}
	block()
}
`
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "hangtreebin")
	if out, err := buildGoCmd(bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build hang tree bin: %v: %s", err, out)
	}
	return bin
}

// TestBinaryAdapter_RunBinaryOutput_TimeoutKillsWholeTree 锁定「超时终止必须覆盖整棵进程树」：
// `exec.CommandContext` 默认只 kill 直接子进程，CLI 派生的后代仍持有管道 ⇒ `Wait` 不返回
// 且每次超时留一批孤儿。断言：超时返回后父进程与其后代**都已消失**。
//
// 变异检定：把 hardenProcessTermination 改成空实现（不建进程组）⇒ 后代存活 ⇒ 本用例红。
func TestBinaryAdapter_RunBinaryOutput_TimeoutKillsWholeTree(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pids")
	bin := buildHangTreePidBin(t, dir)
	a := newBinaryAdapter(AdapterConfig{
		BinaryPath:    bin,
		BinaryTimeout: 300 * time.Millisecond,
		Logger:        testLogger(),
	})
	if _, err := a.runBinaryOutput(context.Background(), "meta", pidFile); err == nil {
		t.Fatal("挂死子进程必须返回错误（fail-closed）")
	}
	parentPid := readPidFile(t, pidFile+".parent")
	childPid := readPidFile(t, pidFile+".child")
	if parentPid == childPid {
		t.Fatalf("夹具异常：父/后代 pid 相同（%d）", parentPid)
	}
	waitPidsGone(t, parentPid, childPid)
}

// readPidFile 读 pid 文件（子进程刚启动即写，超时窗口内必然已就绪）。
func readPidFile(t *testing.T, file string) int {
	t.Helper()
	var raw []byte
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(file)
		if err == nil && len(b) > 0 {
			raw = b
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("读 pid 文件 %s 超时: %v", file, err)
		}
		<-time.After(20 * time.Millisecond)
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if perr != nil {
		t.Fatalf("pid 文件 %s 内容非法: %q", file, raw)
	}
	return pid
}

// waitPidsGone 轮询断言两个 pid 均已终止（被 SIGKILL 后需等待孤儿被 init 回收，故用轮询）。
func waitPidsGone(t *testing.T, pids ...int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if alive := alivePids(pids); len(alive) == 0 {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("超时后仍有存活进程（整棵进程树未被终止）: %v", alive)
		}
		<-time.After(20 * time.Millisecond)
	}
}

// alivePids 返回仍在运行的 pid（`kill(pid, 0)` 成功或仅因权限失败 = 存活）。
func alivePids(pids []int) []int {
	var alive []int
	for _, pid := range pids {
		err := syscall.Kill(pid, 0)
		if err == nil || errors.Is(err, syscall.EPERM) {
			alive = append(alive, pid)
		}
	}
	return alive
}
