// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/cocomhub/buildinfo"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/selfupdate"
	"github.com/spf13/cobra"
)

// NewCmdUpgrade 创建 upgrade 命令：从 GitHub Releases 拉取最新（或 --to 指定）
// 版本，SHA-256 校验后解包替换自身二进制。
//
// 数据流：Latest()/ByTag() → CompareVersions → FindAsset(GOOS/GOARCH) →
// checksums.txt 取期望 SHA-256 → DownloadAndVerify（CDN 直链，不耗 API 配额）→
// ExtractBinary → SwapBinary（原子替换；Windows 两段式兜底）→ 提示重启。
//
// upgradeDeps 是 upgrade 命令的可注入依赖（测试用；生产默认走 os.Executable /
// buildinfo 包级变量）。用**构造注入**而非包级 seam，避免并行测试写同一变量产生
// 数据竞争（R18：全部测试默认 t.Parallel）。
type upgradeDeps struct {
	version func() string
	exec    func() (string, error)
}

func NewCmdUpgrade(ios cli.IOStreams) *cobra.Command {
	return newUpgradeCmd(ios, upgradeDeps{})
}

// newUpgradeCmd 是 NewCmdUpgrade 的内部实现（deps 可为空，空时用生产默认）。
func newUpgradeCmd(ios cli.IOStreams, deps upgradeDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "自更新：从 GitHub Releases 拉取最新版本并替换自身二进制",
		Long: `从 GitHub Releases 获取最新（或 --to 指定）版本发布，校验 SHA-256 后
解包替换当前 sclient 二进制。--check 只查询不安装；--force 跳过版本比较
（当前版本为快照/脏构建时必用）。`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			check, _ := cmd.Flags().GetBool("check")
			to, _ := cmd.Flags().GetString("to")
			force, _ := cmd.Flags().GetBool("force")
			apiBase, _ := cmd.Flags().GetString("api-base")
			useJSON, _ := cmd.Flags().GetBool("json")

			current := deps.currentVersion()
			c := selfupdate.New(apiBase)
			if releaseBase, _ := cmd.Flags().GetString("release-base"); releaseBase != "" {
				c.ReleaseBase = releaseBase
			}
			ctx := cmd.Context()

			// --to 指定版本；否则取最新。
			var rel *selfupdate.Release
			var err error
			if to != "" {
				rel, err = c.ByTag(ctx, selfupdate.NormalizeVersion(to))
			} else {
				rel, err = c.Latest(ctx)
			}
			if err != nil {
				return err
			}

			latest := rel.TagName
			cmp, cmpErr := selfupdate.CompareVersions(current, latest)
			if check {
				return printUpgradeCheck(ios, current, latest, cmp, cmpErr, useJSON)
			}

			// 完整升级：已最新且无 --force → 提示 exit 0。
			if cmpErr == nil && cmp >= 0 && !force {
				if useJSON {
					return printUpgradeJSON(ios, upgradeResult{
						Current: current, Latest: latest,
						UpdateAvailable: false, Action: "up-to-date",
					})
				}
				ios.WriteOutLine("已是最新版本 %s", latest)
				return nil
			}
			if cmpErr != nil && !force {
				if useJSON {
					return printUpgradeJSON(ios, upgradeResult{
						Current: current, Latest: latest,
						UpdateAvailable: true, Action: "check",
					})
				}
				ios.WriteOutLine("当前版本 %s 无法判定（快照/脏构建），使用 --force 强制升级", current)
				return nil
			}

			return runUpgrade(ctx, deps, ios, c, rel, current, useJSON)
		},
	}

	cmd.Flags().Bool("check", false, "只检查最新版本，不安装")
	cmd.Flags().String("to", "", "指定目标版本（默认最新）")
	cmd.Flags().Bool("force", false, "跳过版本比较强制升级（当前版本为快照/脏构建时必用）")
	cmd.Flags().String("api-base", selfupdate.DefaultAPIBase, "GitHub API 仓库基址（隐藏 flag，测试注入用）")
	_ = cmd.Flags().MarkHidden("api-base")
	cmd.Flags().String("release-base", selfupdate.DefaultReleaseURL, "Release 页面/CDN 下载基址（隐藏 flag，测试注入用）")
	_ = cmd.Flags().MarkHidden("release-base")
	return cmd
}

// currentVersion 返回当前二进制版本（Makefile -X main.Version 注入）。
func (d upgradeDeps) currentVersion() string {
	if d.version != nil {
		return d.version()
	}
	info := buildinfo.Default()
	if info.Version == "" {
		return Version
	}
	return info.Version
}

// executablePath 返回当前二进制路径（测试注入 seam）。
func (d upgradeDeps) executable() (string, error) {
	if d.exec != nil {
		return d.exec()
	}
	return os.Executable()
}

// upgradeResult 是 upgrade 命令的 JSON 输出结构（--json）。
type upgradeResult struct {
	Current         string `json:"current"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"update_available"`
	Action          string `json:"action,omitempty"`
	Path            string `json:"path,omitempty"`
}

func printUpgradeJSON(ios cli.IOStreams, r upgradeResult) error {
	return json.NewEncoder(ios.Out).Encode(r)
}

// printUpgradeCheck 实现 --check：恒 exit 0（脚本解析 JSON 字段）。
func printUpgradeCheck(ios cli.IOStreams, current, latest string, cmp int, cmpErr error, useJSON bool) error {
	if useJSON {
		avail := true
		if cmpErr == nil && cmp >= 0 {
			avail = false
		}
		return printUpgradeJSON(ios, upgradeResult{
			Current: current, Latest: latest,
			UpdateAvailable: avail, Action: "check",
		})
	}
	if cmpErr != nil {
		ios.WriteOutLine("当前版本 %s 无法判定（快照/脏构建），最新版本 %s（--force 可强升）", current, latest)
		return nil
	}
	if cmp >= 0 {
		ios.WriteOutLine("已是最新版本 %s", latest)
		return nil
	}
	ios.WriteOutLine("发现新版本 %s（当前 %s），执行 `sclient upgrade` 升级", latest, current)
	return nil
}

// runUpgrade 执行完整升级：FindAsset → Checksums → DownloadAndVerify →
// ExtractBinary → SwapBinary → 打印新路径/版本。goos/goarch 可注入（测试
// 跨平台构造假归档，不依赖 runner 的平台）。
func runUpgrade(ctx context.Context, deps upgradeDeps, ios cli.IOStreams, c *selfupdate.Client, rel *selfupdate.Release, current string, useJSON bool) error {
	return runUpgradeFor(ctx, deps, ios, c, rel, current, useJSON, runtime.GOOS, runtime.GOARCH)
}

func runUpgradeFor(ctx context.Context, deps upgradeDeps, ios cli.IOStreams, c *selfupdate.Client, rel *selfupdate.Release, current string, useJSON bool, goos, goarch string) error {
	asset, err := selfupdate.FindAsset(rel, goos, goarch)
	if err != nil {
		return err
	}

	// 当前二进制路径（os.Executable）；临时文件放同目录保证跨设备 rename 原子性。
	target, err := deps.executable()
	if err != nil {
		return fmt.Errorf("无法定位当前二进制: %w", err)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("解析二进制路径失败: %w", err)
	}

	sums, err := c.Checksums(ctx, rel.TagName)
	if err != nil {
		return err
	}
	wantSHA, ok := selfupdate.LookupChecksum(sums, asset.Name)
	if !ok {
		return fmt.Errorf("checksums.txt 缺少资产 %s 的校验和", asset.Name)
	}

	// 下载到同目录临时文件（边下边算 SHA-256，fail-closed）。
	dir := filepath.Dir(target)
	dest := filepath.Join(dir, asset.Name)
	if err := c.DownloadAndVerify(ctx, asset.BrowserDownloadURL, dest, wantSHA); err != nil {
		return err
	}

	// 解包取 sclient 二进制（临时文件 + rename 原子写）。
	extracted := filepath.Join(dir, ".sclient.upgrade.new")
	if err := selfupdate.ExtractBinaryFromFile(dest, goos, extracted); err != nil {
		return err
	}

	// 原子替换自身。
	if err := selfupdate.SwapBinary(extracted, target); err != nil {
		return err
	}
	_ = os.Remove(dest) // 归档清理 best-effort

	if useJSON {
		return printUpgradeJSON(ios, upgradeResult{
			Current: current, Latest: rel.TagName,
			UpdateAvailable: true, Action: "upgraded", Path: target,
		})
	}
	ios.WriteOutLine("已升级到 %s（%s），请退出后重新运行 sclient", rel.TagName, target)
	return nil
}
