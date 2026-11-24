// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// batchEntry 是批量清单中的一行：`url<TAB>转存路径<TAB>期望checksum(可空)`。
type batchEntry struct {
	URL    string
	Path   string
	Expect string
}

// parseBatchPlan 解析批量清单（跳过空行与 # 注释；列以 TAB 分隔，Path/Expect 可省略）。
func parseBatchPlan(r *bufio.Scanner) ([]batchEntry, error) {
	var out []batchEntry
	for r.Scan() {
		line := strings.TrimSpace(r.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		e := batchEntry{URL: strings.TrimSpace(cols[0])}
		if len(cols) > 1 {
			e.Path = strings.TrimSpace(cols[1])
		}
		if len(cols) > 2 {
			e.Expect = strings.TrimSpace(cols[2])
		}
		if e.URL == "" {
			continue
		}
		out = append(out, e)
	}
	return out, r.Err()
}

// NewCmdCloudDownloadBatch 创建 `cloud-download batch`：按清单批量提交并汇总结果。
//
// 清单格式（TSV，`#` 开头为注释）：
//
//	url<TAB>转存目标路径<TAB>期望checksum(可空)
//
// 语义：`--continue-on-error` 下单条失败不影响其余；默认**等待全部任务终态**后输出汇总
// （成功/失败/超时 + 每个失败任务的错误类别），便于"修 24 个文件"这类批量作业一次跑完。
func NewCmdCloudDownloadBatch(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	var file string
	var transferVolume string
	var transferOverwrite bool
	var concurrency int
	var wait bool
	var timeout time.Duration
	var interval time.Duration
	cmd := &cobra.Command{SilenceUsage: true, SilenceErrors: true,
		Use:   "batch",
		Short: "按清单批量提交云端下载并汇总结果",
		Args:  cobra.NoArgs,
	}
	cmd.Flags().StringVar(&file, "file", "", "清单文件（TSV：url<TAB>转存路径<TAB>期望checksum）")
	cmd.Flags().StringVar(&transferVolume, "transfer-volume", "", "转存目标卷名（清单给了转存路径时**必填**）")
	cmd.Flags().BoolVar(&transferOverwrite, "transfer-overwrite", false,
		"允许覆盖已存在的目标（修复受损文件时必需；默认 false = 目标唯一语义）")
	cmd.Flags().IntVar(&concurrency, "concurrency", 2, "并发提交数（建议 2–3，避免源限流）")
	cmd.Flags().BoolVar(&wait, "wait", true, "等待全部任务终态后汇总")
	cmd.Flags().DurationVar(&timeout, "timeout", 6*time.Hour, "整体等待超时")
	cmd.Flags().DurationVar(&interval, "interval", 15*time.Second, "轮询间隔")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if file == "" {
			return fmt.Errorf("--file 必填")
		}
		f, oerr := os.Open(file)
		if oerr != nil {
			return oerr
		}
		defer f.Close()
		entries, perr := parseBatchPlan(bufio.NewScanner(f))
		if perr != nil {
			return perr
		}
		if len(entries) == 0 {
			return fmt.Errorf("清单为空（每行形如 url<TAB>转存路径<TAB>期望checksum）")
		}
		c, cerr := factory.NewClient(cmd)
		if cerr != nil {
			return cerr
		}
		if transferVolume == "" && hasBatchPath(entries) {
			return fmt.Errorf("--transfer-volume 必填（清单含转存路径时必须有目标卷）")
		}
		ids := submitBatch(cmd.Context(), c, entries, transferVolume, transferOverwrite, concurrency, ios)
		if !wait {
			ios.WriteOutLine("已提交 %d 个任务（--wait=false，不等待）", len(ids))
			return nil
		}
		return summarizeBatch(cmd.Context(), c, ids, timeout, interval, ios)
	}
	return cmd
}

// submitBatch 并发提交（有界并发），返回任务 ID。单条失败只打印、不中断（continue-on-error）。
func submitBatch(ctx context.Context, c *client.FileClient, entries []batchEntry,
	transferVolume string, transferOverwrite bool, concurrency int, ios cli.IOStreams,
) []string {
	if concurrency <= 0 {
		concurrency = 2
	}
	var mu sync.Mutex
	var ids []string
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func(e batchEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			opts := []client.CloudDownloadOption{client.WithCloudDownloadSave(true)}
			if e.Path != "" {
				// Volume 必须显式给出：空 Volume 的 TransferSpec 服务端会判无效（2026-11-24 修正）。
				opts = append(opts, client.WithCloudDownloadTransfer(&client.TransferSpec{
					Volume: transferVolume, Path: e.Path, Overwrite: transferOverwrite}))
			}
			if e.Expect != "" {
				opts = append(opts, client.WithCloudDownloadExpectChecksum(e.Expect))
			}
			task, err := c.CloudDownload(ctx, e.URL, opts...)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				ios.WriteErrLine("提交失败 %s: %v", e.URL, err)
				return
			}
			ids = append(ids, task.ID)
		}(e)
	}
	wg.Wait()
	return ids
}

// summarizeBatch 轮询到终态并输出汇总（成功/失败/未完成 + 失败类别）。
func summarizeBatch(ctx context.Context, c *client.FileClient, ids []string, timeout, interval time.Duration, ios cli.IOStreams) error {
	pending := make(map[string]bool, len(ids))
	for _, id := range ids {
		pending[id] = true
	}
	ok := 0
	failed := map[string]string{}
	deadline := time.Now().Add(timeout)
	for len(pending) > 0 && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		ok += pollBatchOnce(ctx, c, pending, failed)
	}
	reportBatch(ios, len(ids), ok, failed, pending)
	if len(failed) > 0 || len(pending) > 0 {
		return fmt.Errorf("批量任务未全部成功（失败 %d，未完成 %d）", len(failed), len(pending))
	}
	return nil
}

// pollBatchOnce 轮询一轮：把已终态的任务从 pending 移除，返回本轮完成数。
func pollBatchOnce(ctx context.Context, c *client.FileClient, pending map[string]bool, failed map[string]string) int {
	done := 0
	for id := range pending {
		t, err := c.GetCloudTask(ctx, id)
		if err != nil {
			continue
		}
		switch t.Status {
		case "completed":
			done++
			delete(pending, id)
		case "failed", "cancelled":
			class := ""
			if t.Diagnostics != nil {
				class = t.Diagnostics.LastErrorClass
			}
			failed[id] = fmt.Sprintf("%s (%s)", t.Status, class)
			delete(pending, id)
		}
	}
	return done
}

// reportBatch 输出批量汇总。
func reportBatch(ios cli.IOStreams, total, ok int, failed map[string]string, pending map[string]bool) {
	ios.WriteOutLine("批量汇总：提交 %d / 成功 %d / 失败 %d / 未完成 %d",
		total, ok, len(failed), len(pending))
	for id, why := range failed {
		ios.WriteErrLine("  失败 %s: %s", id, why)
	}
	for id := range pending {
		ios.WriteErrLine("  未完成 %s（超时，可用 cloud-download list 查看/retry）", id)
	}
}

// hasBatchPath 判断清单里是否出现转存路径（用于 --transfer-volume 必填校验）。
func hasBatchPath(entries []batchEntry) bool {
	for _, e := range entries {
		if e.Path != "" {
			return true
		}
	}
	return false
}
