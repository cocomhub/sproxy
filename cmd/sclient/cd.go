// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrg/xdg"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// ---- 工厂函数 ----

// NewCmdCd 创建独立的 cd 命令工厂函数，使用 state.State 替代全局 currentDir。
func NewCmdCd(st *state.State, ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "cd [path]",
		Short: "切换当前目录",
		Long: `切换当前操作目录，后续 upload/download/list/delete 等命令将以此目录为基准。
		cd 带参数时进入指定子目录，无参数时打印当前目录。
		cd / 回到根目录，cd .. 返回上级目录。`,
		Args: cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			if len(args) == 0 {
				printCurrentDirPath(st, ios)
				return
			}
			path := args[0]
			// 卷 URI（`<卷名>:/<路径>`；`<卷名>:` = 卷根）：切换工作目录**并切到该卷**。
			// URI 里的路径是**该卷根起算的绝对路径**（不叠加原工作目录）。
			if vol, inner, ok := splitVolumeURI(path); ok {
				st.CurrentVolume = vol
				st.CurrentDir = ""
				// URI 路径已含前导斜杠（卷根绝对）⇒ 状态内目录不带前导斜杠（既有约定）。
				inner = strings.TrimPrefix(inner, "/")
				switch {
				case inner == "" || inner == ".":
				case cdApplySpecialPath(st, inner):
					// `..`/`.` 已按卷内语义处理（卷根下 `..` = 原地）
				default:
					cdApplyNormalPath(st, ios, inner)
				}
				saveCurrentVolumeValue(st.CurrentVolume)
				saveCurrentDirValue(st.CurrentDir)
				return
			}
			// 前导 `/` = **本地卷**根（用户口径 2026-10-13）：显式离开当前卷 + 从根起算。
			if strings.HasPrefix(path, "/") {
				st.CurrentVolume = ""
				st.CurrentDir = ""
				saveCurrentVolumeValue("")
				// 状态内目录**不带**前导斜杠（与既有约定一致）：去掉后按「从根起算」拼接。
				path = strings.TrimPrefix(path, "/")
			}
			if cdApplySpecialPath(st, path) {
				saveCurrentVolumeValue(st.CurrentVolume)
				return
			}
			cdApplyNormalPath(st, ios, path)
			saveCurrentVolumeValue(st.CurrentVolume)
		},
	}
}

// printCurrentDirPath 打印当前工作目录（`<卷名>:/<目录>`；本地卷省略卷名 ⇒ `/目录`）。
func printCurrentDirPath(st *state.State, ios cli.IOStreams) {
	ios.WriteOutLine("%s", st.Location())
}

// cdApplySpecialPath 处理 cd 的特殊路径（/、.、..）；返回 true 表示已处理（调用方直接返回）。
func cdApplySpecialPath(st *state.State, path string) bool {
	switch path {
	case "/":
		st.CurrentDir = ""
		saveCurrentDirValue(st.CurrentDir)
	case ".":
		return true
	case "..":
		if st.CurrentDir == "" {
			return true
		}
		parts := strings.Split(st.CurrentDir, "/")
		if len(parts) <= 1 {
			st.CurrentDir = ""
		} else {
			st.CurrentDir = strings.Join(parts[:len(parts)-1], "/")
		}
		saveCurrentDirValue(st.CurrentDir)
		return true
	default:
		return false
	}
	return true
}

// cdApplyNormalPath 处理普通相对路径：拼接当前目录、清理、拒绝越界后写入目标目录。
func cdApplyNormalPath(st *state.State, ios cli.IOStreams, path string) {
	newDir := path
	if st.CurrentDir != "" {
		newDir = st.CurrentDir + "/" + path
	}
	cleaned := filepath.ToSlash(filepath.Clean(newDir))
	if cleaned == "." {
		cleaned = ""
	}
	if strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, "../") {
		ios.WriteErrLine("无效的路径")
		return
	}
	st.CurrentDir = cleaned
	saveCurrentDirValue(st.CurrentDir)
}

// NewCmdPwd 创建独立的 pwd 命令工厂函数。
func NewCmdPwd(st *state.State, ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "pwd",
		Short: "打印当前目录（含所在卷：`<卷名>:/<目录>`）",
		Run: func(cmd *cobra.Command, args []string) {
			ios.WriteOutLine("%s", st.Location())
		},
	}
}

// NewCmdMkdir 创建独立的 mkdir 命令工厂函数。
func NewCmdMkdir(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	return &cobra.Command{
		Use:   "mkdir <dirname>",
		Short: "在服务端创建目录",
		Long:  "在服务端上传目录下创建指定子目录。路径相对当前目录 (cd)，支持绝对路径 (/开头)。",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 卷上下文（URI / 当前工作目录所在卷 ⇒ --volume；用户口径 2026-10-13）。
			if err := applyVolumeContext(cmd, st, args); err != nil {
				return err
			}
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			dirname, err := st.ResolveRemotePath(args[0])
			if err != nil {
				ios.WriteErrLine("无效的路径: %v", err)
				return fmt.Errorf(errFmtInvalidPath, err)
			}
			if err := svc.Mkdir(cmd.Context(), dirname); err != nil {
				ios.WriteErrLine("创建目录失败: %v", err)
				return fmt.Errorf(errFmtMkdirFailed, err)
			}
			fmt.Fprintf(ios.Out, "目录已创建: %s\n", dirname)
			return nil
		},
	}
}

// NewCmdRmdir 创建独立的 rmdir 命令工厂函数。
func NewCmdRmdir(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rmdir <dirname>",
		Short: "删除服务端目录",
		Long:  "删除服务端上传目录下的指定目录（含所有内容）。路径相对当前目录。\n使用 --force 跳过确认提示。",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 卷上下文（URI / 当前工作目录所在卷 ⇒ --volume；用户口径 2026-10-13）。
			if err := applyVolumeContext(cmd, st, args); err != nil {
				return err
			}
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			dirname, err := st.ResolveRemotePath(args[0])
			if err != nil {
				ios.WriteErrLine("无效的路径: %v", err)
				return fmt.Errorf(errFmtInvalidPath, err)
			}

			// confirmed = 「用户已确认删除」。服务端 rmdir **恒**要求 force=true（最后一道闸门），
			// 故此处的确认必须真的翻成 force=true 传给服务端——此前 CLI 从不发送该参数 ⇒
			// `rmdir`（含 --force）恒 400「请使用 ?force=true 确认删除」（外部卷更早被 501 掩盖）。
			force, _ := cmd.Flags().GetBool("force")
			confirmed, cerr := confirmRmdir(cmd, svc, ios, dirname, force)
			if cerr != nil {
				return cerr
			}
			if !confirmed {
				return nil // 用户取消（交互回答非 y）
			}

			if err := svc.Rmdir(cmd.Context(), dirname, confirmed); err != nil {
				ios.WriteErrLine("删除目录失败: %v", err)
				return fmt.Errorf("删除目录失败: %w", err)
			}
			fmt.Fprintf(ios.Out, "目录已删除: %s\n", dirname)
			return nil
		},
	}
	cmd.Flags().Bool("force", false, "跳过非空确认提示")
	return cmd
}

// ---- XDG 缓存持久化 ----

const cacheDirName = "sproxy"
const cacheFile = "current_dir"
const cacheFileVolume = "current_volume"

// saveCurrentDirValue 将指定目录持久化到 XDG 缓存目录。
func saveCurrentDirValue(dir string) {
	cachePath, err := xdg.CacheFile(filepath.Join(cacheDirName, cacheFile))
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(cachePath), 0755)
	_ = os.WriteFile(cachePath, []byte(dir), 0644)
}

// saveCurrentVolumeValue 持久化当前工作目录所在卷（空串也写：显式回到本地卷要覆盖旧值）。
func saveCurrentVolumeValue(vol string) {
	cachePath, err := xdg.CacheFile(filepath.Join(cacheDirName, cacheFileVolume))
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(cachePath), 0755)
	_ = os.WriteFile(cachePath, []byte(vol), 0644)
}

// loadCurrentVolume 从 XDG 缓存目录加载当前工作目录所在卷（无缓存 = 本地卷）。
func loadCurrentVolume() string {
	cachePath, err := xdg.CacheFile(filepath.Join(cacheDirName, cacheFileVolume))
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// loadCurrentDir 从 XDG 缓存目录加载当前目录。
func loadCurrentDir() string {
	cachePath, err := xdg.CacheFile(filepath.Join(cacheDirName, cacheFile))
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// confirmRmdir 决定「本次删除是否已获用户确认」（= 发给服务端的 force=true）：
//   - `--force`：直接确认；
//   - 空目录（列目录成功且无条目）：无需确认（与历史交互语义一致）；
//   - 非空目录：交互确认，回答非 y/yes → (false, nil)（调用方按取消处理，**不删**）；
//   - 列目录失败且未 `--force`：**不猜** → 返回错误（绝不静默递归删用户内容）。
func confirmRmdir(cmd *cobra.Command, svc *client.FileClient, ios cli.IOStreams, dirname string, force bool) (bool, error) {
	if force {
		return true, nil
	}
	entries, listErr := svc.List(cmd.Context(), dirname)
	if listErr != nil {
		fmt.Fprintf(ios.ErrOut, "无法确认目录内容（%v），请使用 --force 确认删除\n", listErr)
		return false, fmt.Errorf("无法确认目录内容: %w", listErr)
	}
	if len(entries) == 0 {
		return true, nil
	}
	fmt.Fprintf(ios.ErrOut, "警告: 目录 '%s' 包含 %d 个条目，非空删除将清除所有内容\n", dirname, len(entries))
	fmt.Fprint(ios.ErrOut, "确认删除? (y/N): ")
	answer, _ := bufio.NewReader(ios.In).ReadString('\n')
	switch strings.TrimSpace(strings.ToLower(answer)) {
	case "y", "yes":
		return true, nil
	}
	fmt.Fprintln(ios.Out, "已取消")
	return false, nil
}
