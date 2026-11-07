// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package baidupcs

// behavior_probe5_e2e_test.go —— 【Put 阶段定位探针 P7】把 `Storage.Put` 的耗时**归因到阶段**
// （暂存 staging / 上传+复核 / 可见性等待），并采集库路径自己的 warn/info 日志。
//
// 前序实测：P1 用**库 adapter** 往**已存在目录**写 9 字节文件，150s ctx 超时仍未完成。
// 本探针把该现象拆开：
//
//	(a) `PutFromStagedPath`（跳过暂存，只测 上传+复核）
//	(b) `Put`（含暂存）
//	⇒ 两者之差 = 暂存耗时；并把库路径日志打出来，判断是否卡在「可见性等待」。
//
// 运行（WSL）：
//
//	cd pkg/volume/ext/baidupcs && GOTOOLCHAIN=local GOWORK=off go test -tags=e2e \
//	  -run TestE2E_PutPhaseProbe -count=1 -v -timeout 15m
import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"
)

// logCapture 收集库路径日志（判断耗时落在哪个阶段）。
type logCapture struct {
	mu    sync.Mutex
	lines []string
	cap   int
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cap == 0 {
		l.cap = 200
	}
	if len(l.lines) < l.cap {
		l.lines = append(l.lines, strings.TrimSpace(string(p)))
	}
	return len(p), nil
}

func (l *logCapture) dump(t *testing.T, tag string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		t.Logf("[P7 %s 日志] %s", tag, s)
	}
}

// TestE2E_PutPhaseProbe 见文件头。
func TestE2E_PutPhaseProbe(t *testing.T) {
	// sproxy:serial: 真实网盘 e2e 探针（共享远端资源 + 限流），必须串行执行
	bduss := e2eBDUSSFromConfig(t)
	if bduss == "" {
		t.Skip("BDUSS 为空")
	}
	if _, ok := e2eBinaryAvailable(); !ok {
		t.Skip("BaiduPCS-Go 二进制不在 PATH") // 仅用于 CLI 预建目录（铺夹具）
	}
	env := newProbeEnv(t)
	ctx := context.Background()
	uniq := time.Now().UnixNano()

	// 用**库 adapter**（复现 P1 的场景）；日志写入 capture。
	capture := &logCapture{}
	logger := slog.New(slog.NewTextHandler(capture, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cl, err := NewClient(bduss, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	libSt, err := NewStorage(StorageConfig{Root: path.Join(env.root, fmt.Sprintf("p7-%d", uniq)),
		BDUSS: bduss, TempDir: t.TempDir(), Adapter: newLibraryAdapter(cl, logger), Logger: logger})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	// 预建目标目录（用 CLI；已存在目录场景 ⇒ 排除"新目录树"因素）
	env.mkdirRemote(ctx, path.Join(libSt.root, "d"))

	// 本地夹具文件（1KB 随机性内容，避免秒传命中带来的干扰）
	local := path.Join(t.TempDir(), "p7.bin")
	if werr := os.WriteFile(local, bytes.Repeat([]byte("p7"), 512), 0o600); werr != nil {
		t.Fatal(werr)
	}

	// (a) PutFromStagedPath：跳过暂存，只测「上传 + 复核（含可见性等待）」
	{
		pctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		start := time.Now()
		_, aErr := libSt.PutFromStagedPath(pctx, "d/a.bin", local, int64(1024), nil)
		elapsed := time.Since(start)
		cancel()
		t.Logf("[P7] (a) PutFromStagedPath(已存在目录) err=%v 耗时=%.2fs", aErr, elapsed.Seconds())
		capture.dump(t, "(a)")
	}

	// (b) Put：含暂存
	{
		pctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		start := time.Now()
		_, bErr := libSt.Put(pctx, "d/b.bin", bytes.NewReader(bytes.Repeat([]byte("p7b"), 512)))
		elapsed := time.Since(start)
		cancel()
		t.Logf("[P7] (b) Put(已存在目录) err=%v 耗时=%.2fs", bErr, elapsed.Seconds())
		capture.dump(t, "(b)")
	}

	// (c) 对照：同一目录再写第 3 个文件（此时目录/连接已"热"）
	{
		pctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		start := time.Now()
		_, cErr := libSt.Put(pctx, "d/c.bin", bytes.NewReader(bytes.Repeat([]byte("p7c"), 512)))
		elapsed := time.Since(start)
		cancel()
		t.Logf("[P7] (c) Put 第 3 个文件（热态）err=%v 耗时=%.2fs", cErr, elapsed.Seconds())
	}

	t.Logf("[P7] 结论：以上三项耗时即为阶段归因依据（暂存=Put−PutFromStagedPath）")
}

// TestE2E_PutProdPathProbe 对照探针：用**生产装配**（binary CLI 优先 + 库兜底）写小文件，
// 与 P7（纯库 adapter）对比 —— 判断"库上传 ~90s 且不响应 ctx"是否影响真实生产路径。
func TestE2E_PutProdPathProbe(t *testing.T) {
	// sproxy:serial: 真实网盘 e2e 探针（共享远端资源 + 限流），必须串行执行
	env := newProbeEnv(t) // env.prod = NewStorage{BinaryPath: ...}（生产装配）
	ctx := context.Background()
	env.mkdirRemote(ctx, env.remoteFor("p8"))
	for _, name := range []string{"a.bin", "b.bin"} {
		pctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		start := time.Now()
		_, perr := env.prod.Put(pctx, "p8/"+name, bytes.NewReader(bytes.Repeat([]byte("p8"), 512)))
		elapsed := time.Since(start)
		cancel()
		t.Logf("[P8] 生产装配 Put %s err=%v 耗时=%.2fs", name, perr, elapsed.Seconds())
	}
}
