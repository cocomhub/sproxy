// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// fakeFactory 是最小 Factory 替身：把 NewClient 固定到指向 httptest 的 FileClient。
// 用「嵌入接口」只实现用到的方法（仓内既有测试替身约定）。
type fakeFactory struct {
	clientfactory.Factory
	base string
}

func (f fakeFactory) NewClient(*cobra.Command) (*client.FileClient, error) {
	return client.NewFileClient(f.base, client.WithInsecureTLS()), nil
}

func newTestIOS() (cli.IOStreams, *bytes.Buffer) {
	var out bytes.Buffer
	return cli.IOStreams{In: strings.NewReader(""), Out: &out, ErrOut: &out}, &out
}

// runPerf 经 **root 入口**执行 perf 子命令（贴近真实调用；cobra 的 Args/RunE/flag
// 解析在 parent 挂载后才完整生效，直接 Execute 子命令会漏掉部分校验）。
func runPerf(t *testing.T, base string, args ...string) (string, error) {
	t.Helper()
	ios, out := newTestIOS()
	rootCmd := &cobra.Command{Use: "sclient"}
	rootCmd.AddCommand(NewCmdPerf(fakeFactory{base: base}, ios))
	rootCmd.SetOut(out)
	rootCmd.SetErr(out)
	rootCmd.SetArgs(append([]string{"perf"}, args...))
	err := rootCmd.Execute()
	return out.String(), err
}

// TestCmdPerf_IsHidden 锁定**隐藏语义**（用户要求 2026-11-12：设计隐藏子命令保留 perf 能力）。
//
// 判据有决定性：既断言 cmd.Hidden==true，又断言挂到 root 后 `--help` 文本**不含** "perf"
// —— 只看其中一个都可能是"恰好没出现"而非真的隐藏。
func TestCmdPerf_IsHidden(t *testing.T) {
	t.Parallel()
	ios, _ := newTestIOS()
	cmd := NewCmdPerf(fakeFactory{}, ios)
	if !cmd.Hidden {
		t.Fatal("perf 必须是隐藏命令（Hidden=true）")
	}
	root := &cobra.Command{Use: "sclient"}
	root.AddCommand(cmd)
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("help 执行失败: %v", err)
	}
	if strings.Contains(buf.String(), "perf") {
		t.Fatalf("隐藏命令不得出现在 --help 中，实际输出:\n%s", buf.String())
	}
}

// TestCmdPerf_ProfileArgWhitelist profile 名必须受白名单约束（防任意路径注入）。
func TestCmdPerf_ProfileArgWhitelist(t *testing.T) {
	t.Parallel()
	if _, err := runPerf(t, "", "profile", "../../etc/passwd"); err == nil {
		t.Fatal("非白名单 profile 必须被拒绝（防路径注入）")
	}
}

// TestCmdPerf_MetricsHighlightsHeap 锁定 metrics 默认行为：只高亮 heap 相关指标。
func TestCmdPerf_MetricsHighlightsHeap(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("sproxy_requests_total 12\n" +
			"sproxy_heap_alloc_bytes 956598992\n" +
			"sproxy_heap_objects 8068273\n" +
			"sproxy_gc_cycles 43\n"))
	}))
	defer srv.Close()

	got, err := runPerf(t, srv.URL, "metrics")
	if err != nil {
		t.Fatalf("metrics 执行失败: %v\n输出:\n%s", err, got)
	}
	for _, want := range []string{"sproxy_heap_alloc_bytes", "sproxy_heap_objects", "sproxy_gc_cycles"} {
		if !strings.Contains(got, want) {
			t.Errorf("应高亮 %s，实际输出:\n%s", want, got)
		}
	}
	if strings.Contains(got, "sproxy_requests_total") {
		t.Errorf("默认不应输出非内存指标，实际输出:\n%s", got)
	}
}

// TestCmdPerf_ProfileErrHintsPprofFlag 锁定错误提示：服务端未开 pprof（404）时给出可执行建议。
func TestCmdPerf_ProfileErrHintsPprofFlag(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 page not found"))
	}))
	defer srv.Close()
	_, err := runPerf(t, srv.URL, "profile", "heap")
	if err == nil {
		t.Fatal("服务端未开 pprof（404）必须报错")
	}
	if !strings.Contains(err.Error(), "debug_pprof_enabled") {
		t.Errorf("错误信息应提示检查 debug_pprof_enabled，实际: %v", err)
	}
}
