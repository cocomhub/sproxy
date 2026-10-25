// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// meta.go 提供 `sclient meta` 子命令：已上传文件的元数据补录/修正（方案②）。

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// NewCmdMeta 创建 meta 命令组。
func NewCmdMeta(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "meta",
		Short: "文件元数据管理（.meta sidecar 的自定义 extra）",
	}
	cmd.AddCommand(newCmdMetaSet(factory, ios, st))
	return cmd
}

// newCmdMetaSet 创建 `meta set <filename> --extra=<json|@file>`。
func newCmdMetaSet(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <filename>",
		Short: "补录/修正文件的元数据 extra（合并语义）",
		Long: `把 extra 合并进目标文件的 .meta sidecar。
要求文件已上传且已有 sidecar（无内容基线时拒绝，fail-closed）。

示例：
  sclient meta set dir/movie.mp4 --extra='{"code":"NHDTC-254","maker":"NATURAL HIGH"}'
  sclient meta set dir/movie.mp4 --extra=@meta.json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, _ := cmd.Flags().GetString("extra")
			extra, perr := parseMetaExtraFlag(raw)
			if perr != nil {
				return perr
			}
			if len(extra) == 0 {
				return fmt.Errorf("--extra 不能为空")
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
			if uerr := svc.UpdateFileMeta(cmd.Context(), remotePath, extra); uerr != nil {
				return uerr
			}
			ios.WriteOutLine("meta extra 已更新: %s（%d 个字段）", remotePath, len(extra))
			return nil
		},
	}
	cmd.Flags().String("extra", "", "要合并的元数据（JSON 或 @file.json）")
	return cmd
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
