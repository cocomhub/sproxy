// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// NewCmdMeta 创建 meta 命令：查询文件元信息（含版本历史摘要）。
func NewCmdMeta(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "meta <filename>",
		Short: "查询文件元信息（含版本历史摘要）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 卷 URI 寻址（`<卷名>:/<路径>` ⇒ --volume + 纯路径；用户要求 2026-10-13）。
			if err := applyVolumeContext(cmd, st, args); err != nil {
				return err
			}
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}
			filename, err := st.ResolveRemotePathOrErr(args[0])
			if err != nil {
				return err
			}
			info, err := svc.Stat(cmd.Context(), filename)
			if err != nil {
				return fmt.Errorf("获取文件信息失败: %w", err)
			}
			fm := buildFormatterWithWriter(ios.Out, cmd)
			fm.PrintStat(info, filename)
			// 元数据 extra（KV）：读失败不阻断主查询（Stat 已成功），仅提示。
			extra, xerr := svc.GetFileMeta(cmd.Context(), filename)
			if xerr == nil {
				fm.PrintMetaExtra(extra)
			} else {
				ios.WriteErrLine("（读取 meta extra 失败: %v）", xerr)
			}
			return nil
		},
	}
	// 文件版本管理子命令
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "文件版本管理",
	}
	versionCmd.AddCommand(NewCmdMetaVersionList(factory, ios, st))
	versionCmd.AddCommand(NewCmdMetaVersionRestore(factory, ios, st))
	versionCmd.AddCommand(NewCmdMetaVersionDelete(factory, ios, st))
	cmd.AddCommand(versionCmd)
	// extra 补录/修正（方案②：已上传文件的元数据）
	cmd.AddCommand(newCmdMetaSet(factory, ios, st))
	return cmd
}

func NewCmdMetaVersionList(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	return &cobra.Command{
		Use:   "list <filename>",
		Short: "列出文件版本历史",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 卷 URI 寻址（`<卷名>:/<路径>` ⇒ --volume + 纯路径；用户要求 2026-10-13）。
			if err := applyVolumeContext(cmd, st, args); err != nil {
				return err
			}
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}
			filename, err := st.ResolveRemotePathOrErr(args[0])
			if err != nil {
				return err
			}
			versions, err := svc.ListVersions(cmd.Context(), filename)
			if err != nil {
				return err
			}
			fm := buildFormatterWithWriter(ios.Out, cmd)
			fm.PrintVersionList(filename, versions)
			return nil
		},
	}
}

func NewCmdMetaVersionRestore(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	return &cobra.Command{
		Use:   "restore <filename> <version_id>",
		Short: "恢复文件到指定版本",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 卷 URI 寻址（`<卷名>:/<路径>` ⇒ --volume + 纯路径；用户要求 2026-10-13）。
			if err := applyVolumeContext(cmd, st, args); err != nil {
				return err
			}
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}
			filename, err := st.ResolveRemotePathOrErr(args[0])
			if err != nil {
				return err
			}
			v, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				return fmt.Errorf("版本 ID 必须是数字: %w", err)
			}
			if err := svc.RestoreVersion(cmd.Context(), filename, v); err != nil {
				return err
			}
			fmt.Fprintf(ios.Out, "已恢复文件 '%s' 到版本 %s\n", filename, args[1])
			return nil
		},
	}
}

func NewCmdMetaVersionDelete(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <filename> <version_id>",
		Short: "删除文件的指定版本",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 卷 URI 寻址（`<卷名>:/<路径>` ⇒ --volume + 纯路径；用户要求 2026-10-13）。
			if err := applyVolumeContext(cmd, st, args); err != nil {
				return err
			}
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}
			filename, err := st.ResolveRemotePathOrErr(args[0])
			if err != nil {
				return err
			}
			v, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				return fmt.Errorf("版本 ID 必须是数字: %w", err)
			}
			if err := svc.DeleteVersion(cmd.Context(), filename, v); err != nil {
				return err
			}
			fmt.Fprintf(ios.Out, "已删除文件 '%s' 的版本 %s\n", filename, args[1])
			return nil
		},
	}
}

// newCmdMetaSet 创建 `meta set <filename> --extra=<json|@file>`。
func newCmdMetaSet(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <filename>",
		Short: "补录/修正文件的元数据 extra（合并语义）",
		Long: `把 extra 合并进目标文件的 .meta sidecar。
要求文件已上传且已有 sidecar（无内容基线时拒绝，fail-closed）。

示例：
  sclient meta set dir/movie.mp4 --extra='{"code":"MOVIE-001","maker":"STUDIO X"}'
  sclient meta set dir/movie.mp4 --extra=@meta.json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			extra, mode, perr := parseMetaSetArgs(cmd)
			if perr != nil {
				return perr
			}
			if err := applyStateVolume(cmd, st); err != nil {
				return err
			}
			svc, cerr := factory.NewClient(cmd)
			if cerr != nil {
				ios.WriteErrLine(errFmtInitClientPrint, cerr)
				return fmt.Errorf(errFmtInitClient, cerr)
			}
			remotePath, rerr := st.ResolveRemotePathOrErr(args[0])
			if rerr != nil {
				return rerr
			}
			if uerr := svc.UpdateFileMetaWithMode(cmd.Context(), remotePath, extra, client.MetaUpdateOptions{Mode: mode}); uerr != nil {
				return uerr
			}
			ios.WriteOutLine("meta extra 已%s: %s（%d 个字段）", modeVerb(mode), remotePath, len(extra))
			return nil
		},
	}
	cmd.Flags().String("extra", "", "要合并的元数据（JSON 或 @file.json）")
	cmd.Flags().Bool("replace", false, "整表替换（丢弃所有旧键；默认是合并/新增）")
	cmd.Flags().String("delete", "", "删除指定键（逗号分隔，如 code,maker）")
	return cmd
}

// parseMetaSetArgs 解析 meta set 的 --extra/--replace/--delete 为（extra, mode）。
// 三模式归一：--delete 优先（删键）；否则 --replace 整表替换；否则默认合并。
func parseMetaSetArgs(cmd *cobra.Command) (map[string]any, client.MetaUpdateMode, error) {
	raw, _ := cmd.Flags().GetString("extra")
	delRaw, _ := cmd.Flags().GetString("delete")
	replace, _ := cmd.Flags().GetBool("replace")
	extra, perr := parseMetaExtraFlag(raw)
	if perr != nil {
		return nil, "", perr
	}
	if strings.TrimSpace(delRaw) != "" {
		if len(extra) > 0 {
			return nil, "", fmt.Errorf("--delete 与 --extra 不能同时使用")
		}
		del := map[string]any{}
		for k := range strings.SplitSeq(delRaw, ",") {
			if key := strings.TrimSpace(k); key != "" {
				del[key] = nil // value 占位，服务端只取 key 集合
			}
		}
		if len(del) == 0 {
			return nil, "", fmt.Errorf("--delete 至少指定一个键")
		}
		return del, client.MetaUpdateDelete, nil
	}
	if len(extra) == 0 {
		return nil, "", fmt.Errorf("--extra 不能为空（或用 --delete=<keys> 删键）")
	}
	if replace {
		return extra, client.MetaUpdateReplace, nil
	}
	return extra, client.MetaUpdateMerge, nil
}

// modeVerb 返回更新模式的中文动词（CLI 输出文案）。
func modeVerb(m client.MetaUpdateMode) string {
	switch m {
	case client.MetaUpdateReplace:
		return "替换"
	case client.MetaUpdateDelete:
		return "删除"
	default:
		return "更新"
	}
}

// parseMetaExtraFlag 解析 --extra（JSON 或 @file.json）。
func parseMetaExtraFlag(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var payload []byte
	if rest, ok := strings.CutPrefix(raw, "@"); ok {
		b, err := os.ReadFile(rest)
		if err != nil {
			return nil, fmt.Errorf("读 %s: %w", rest, err)
		}
		payload = b
	} else {
		payload = []byte(raw)
	}
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w", err)
	}
	return obj, nil
}
