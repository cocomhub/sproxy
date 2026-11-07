// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// tempdir_gate_test.go 是「测试临时目录必须走统一入口」的门禁（R53）。
//
// 背景（2026-10-31 实测）：%TEMP% 累积了 1,910 个本仓测试目录（archive-volume-* 802、
// nested-secret-tmp* 648 等）。两类原因：① Windows 上测试内 os.RemoveAll 因句柄未释放
// 失败且错误被 `_ =` 吞掉；② 显式 TempDir 的契约是「调用方所有」，无人清理。
//
// 口径：`*_test.go` 中**不得**直接调用 `os.MkdirTemp` / `ioutil.TempDir`，必须用
// `testutil.MkdirTemp(t, prefix)`（测试内重试 + 进程级兜底）或 `t.TempDir()`。
// 例外只允许「没有 *testing.T 的构造路径」与 benchmark（已在 allowlist 逐条说明）。

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// tempDirAllowlist 是允许裸调 os.MkdirTemp 的测试文件（module 相对路径）→ 理由。
var tempDirAllowlist = map[string]string{
	"pkg/server/volume_wrapper_nested_test.go": "测试用 backend builder 无 *testing.T；已经 testutil.TrackTempDir 登记，TestMain 统一清理",
	"cmd/sproxy/secret_register_test.go":       "同上（reattach builder）",
	"pkg/server/benchmark_test.go":             "benchmark 专用 tmpfs 根 + tb.Cleanup（非 *testing.T）",
	"pkg/tunnel/xfer/ext/quic/bench_test.go":   "子 module benchmark，已有 defer os.RemoveAll",
	"test/e2e_relay_test.go":                   "e2e 二进制目录由包级 sync.Once 共享，已 testutil.TrackTempDir + TestMain 清理",
}

// TestTempDirGate 门禁：测试文件不得裸调 os.MkdirTemp/ioutil.TempDir（见文件头）。
func TestTempDirGate(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	var offenders []string
	for _, dir := range []string{"pkg", "cmd", "internal", "test"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if _, ok := tempDirAllowlist[rel]; ok {
				return nil
			}
			if strings.HasSuffix(rel, "tempdir_gate_test.go") {
				return nil // 本门禁自身含检测字面量
			}
			data, ferr := os.ReadFile(path)
			if ferr != nil {
				return nil
			}
			for i, line := range strings.Split(string(data), "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "//") {
					continue // 注释里提到不算违规
				}
				if strings.Contains(trimmed, "os.MkdirTemp(") || strings.Contains(trimmed, "ioutil.TempDir(") {
					offenders = append(offenders, rel+":"+strconv.Itoa(i+1))
				}
			}
			return nil
		})
	}
	if len(offenders) > 0 {
		t.Fatalf("测试文件裸调 os.MkdirTemp/ioutil.TempDir（请改用 testutil.MkdirTemp 或 t.TempDir）：\n%s",
			strings.Join(offenders, "\n"))
	}
}
