// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

// graceful_restart_unix.go 实现 sproxy 优雅重启（roadmap 11.6-②，kill -USR2）：
//
//   - 旧进程把已绑定 TCP listener 经 ExtraFiles 传给子进程（fd 3），子进程用
//     net.FileListener 重建同一监听（nginx 风格；同端口、无内核依赖，TLS 语义不变）。
//   - 子进程启动后轮询 /readyz（200 = 就绪）→ 旧进程复用 handleSignalShutdown
//     drain（语义与 SIGTERM 完全一致：cancel → s.Shutdown → h.Close）→ 退出。
//   - 失败安全：spawn 失败 / readyz 超时 → 记 Error 并中止重启，旧进程继续服务。
//
// 平台：Unix（Linux/macOS）。Windows 无 SIGUSR2 投递且 ExtraFiles 不支持跨进程
// 套接字继承 → 信号桩（restart_signal_windows.go）恒关闭，行为零变化。
// 信号注册/判定与 fd 继承的平台实现见 restart_signal_unix.go（!windows）。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
	"github.com/cocomhub/sproxy/pkg/server"
)

// restartEnvKey 是子进程继承监听 fd 的环境变量名（子进程启动路径读取）。
const restartEnvKey = "SPROXY_INHERIT_FD"

// restartFd 是 ExtraFiles 注入的 fd 序号（ExtraFiles[0] → fd 3）。
const restartFd = 3

// inheritRestartFd 读取 SPROXY_INHERIT_FD（子进程启动时存在 = 继承模式）。
// 缺失/非法（非正整数）返回 (0,false)。供 inheritListener（各平台实现）消费。
// 定义在本文件（Unix 编译）；Windows 编译时由 restart_signal_windows.go 桩避免引用。
func inheritRestartFd() (int, bool) {
	v := os.Getenv(restartEnvKey)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// getRestartListener 返回启动路径绑定的 HTTP listener（未绑定返回 nil）。
// 定义在本文件（USR2 优雅重启编排消费）；root.go 只负责 store。
func getRestartListener() net.Listener {
	ln := restartListener.Load()
	if ln == nil {
		return nil
	}
	return *ln
}

// startRestartChild 启动同命令行的子进程，把已绑定 listener 作为 fd 3 传给子进程
// （ExtraFiles[0]）。环境追加 SPROXY_INHERIT_FD=3。子进程 stdout/stderr 接当前进程。
func startRestartChild(ln net.Listener) (*exec.Cmd, error) {
	if ln == nil {
		return nil, errors.New("优雅重启：无已绑定 listener（启动未完成）")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("优雅重启：解析可执行路径失败: %w", err)
	}
	lnFile, ok := ln.(*net.TCPListener)
	if !ok {
		return nil, fmt.Errorf("优雅重启：listener 类型 %T 不支持继承（仅 TCP）", ln)
	}
	f, err := lnFile.File()
	if err != nil {
		return nil, fmt.Errorf("优雅重启：获取 listener 文件描述符失败: %w", err)
	}
	defer f.Close() // 子进程持有 dup 后的 fd；本进程保留原 listener 继续 serve

	// #nosec G702 -- 参数与可执行路径均来自本进程自身（os.Executable + os.Args），
	// 是运维可控的固定输入，非外部注入面；nginx 风格重启的既有形态。
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), restartEnvKey+"="+strconv.Itoa(restartFd))
	cmd.ExtraFiles = []*os.File{f}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd, nil
}

// waitRestartReady 轮询 GET http://127.0.0.1:<port>/readyz 直至 200（就绪）。
// 503/拒绝 = 未就绪；超时返回错误。
func waitRestartReady(ctx context.Context, addr string, timeout time.Duration) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("优雅重启：解析就绪探测地址 %q 失败: %w", addr, err)
	}
	url := "http://127.0.0.1:" + port + "/readyz"
	client := &http.Client{Transport: netutil.IsolatedTransport()}
	deadline := time.Now().Add(timeout)
	interval := 200 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return fmt.Errorf("优雅重启：上下文取消，中止就绪等待: %w", ctx.Err())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("优雅重启：子进程 %s 在 %s 内未就绪", url, timeout)
		}
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("优雅重启：上下文取消，中止就绪等待: %w", ctx.Err())
		case <-time.After(interval):
		}
	}
}

// restartTimeoutFor 计算重启就绪等待超时：max(60s, shutdownTimeout)。
func restartTimeoutFor(cfg *server.Config) time.Duration {
	to := 60 * time.Second
	if cfg != nil && cfg.ServerTimeouts.Shutdown > to {
		to = cfg.ServerTimeouts.Shutdown
	}
	return to
}

// restartSpawn 是子进程构造注入点（默认 startRestartChild）。
// 测试替换：避免把测试二进制自身递归 spawn（os.Executable 在 go test 下是
// 测试二进制，直接 startRestartChild 会无限递归跑测试）。
var restartSpawn = startRestartChild

// restartStart 是子进程启动注入点（默认 (*exec.Cmd).Start）。
// 测试替换：mock spawn 返回的 exec.Cmd 未实际启动，Start 需 no-op。
var restartStart = (*exec.Cmd).Start

// handleSignalRestart 执行优雅重启编排：
//  1. startRestartChild(restartListener) —— spawn 失败记 Error 并中止（fail-safe 不自杀）。
//  2. 成功则 waitRestartReady（就绪探针）—— 超时记 Error + best-effort SIGTERM 子进程，
//     旧进程继续服务。
//  3. 就绪 → 复用 handleSignalShutdown(cancel, s, h) drain → 返回（runServer 退出码 0）。
func handleSignalRestart(cancel context.CancelFunc, s *http.Server, h *server.Handlers, logger *slog.Logger, cfg *server.Config) {
	ln := getRestartListener()
	if ln == nil {
		logger.Error("优雅重启：无已绑定 listener（启动未完成），中止重启")
		return
	}
	cmd, err := restartSpawn(ln)
	if err != nil {
		logger.Error("优雅重启：启动子进程失败，中止重启（旧进程继续服务）", "error", err)
		return
	}
	if err := restartStart(cmd); err != nil {
		logger.Error("优雅重启：spawn 失败，中止重启（旧进程继续服务）", "error", err)
		return
	}
	readyCtx, readyCancel := context.WithCancel(context.Background())
	defer readyCancel()
	if err := waitRestartReady(readyCtx, ln.Addr().String(), restartTimeoutFor(cfg)); err != nil {
		logger.Error("优雅重启：子进程未就绪，回滚（旧进程继续服务）", "error", err)
		// best-effort 终止子进程（子进程可能仍在启动/未监听）。
		_ = cmd.Process.Signal(syscall.SIGTERM)
		return
	}
	logger.Info("优雅重启：新进程已就绪，旧进程开始 drain")
	handleSignalShutdown(cancel, s, h)
	_ = cmd.Wait() // 子进程接管后由自己退出（旧进程退出后子进程继续服务）
}
