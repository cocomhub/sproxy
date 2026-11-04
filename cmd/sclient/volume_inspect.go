// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// volume_inspect.go 提供「真实存储审查」CLI（设计
// docs/designs/2026-11-04-secretdata-storage-inspection.md §5.3–§5.5）。
//
//	volume trace   <逻辑路径> [--volume <v>] [--disk] [--json]
//	volume explain <真实路径> [--volume <v>] [--disk] [--json]
//
// 退出码：0 一致；3 发现差异（仅 --disk）；1 参数/服务端错误。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/spf13/cobra"
)

// errInspectDiff 表示「审查成功但发现差异」（进程退出码 3）。
var errInspectDiff = errors.New("发现存储差异")

const (
	flagInspectVolume = "volume"
	flagInspectDisk   = "disk"
	flagInspectJSON   = "json"
)

func newCmdVolumeTrace(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trace <逻辑路径>",
		Short: "显示逻辑文件/目录在真实存储上的落点（容器/meta/分块/parity）",
		Long: `显示逻辑路径在真实存储上的完整落点：容器链、meta blob、每个内容分块
（含 blocklet 数）、parity 段与去重池引用。

  --disk  同时对比真实磁盘，输出差异（缺失/孤儿/大小不符），发现差异时退出码 3。
  --json  输出稳定 JSON（schema_version=1）。`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			volName, rel, err := resolveInspectTarget(cmd, args[0], st, true)
			if err != nil {
				return err
			}
			disk, _ := cmd.Flags().GetBool(flagInspectDisk)
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return fmt.Errorf(errFmtInitClient, err)
			}
			tr, err := svc.VolumeTrace(cmd.Context(), volName, rel, disk)
			if err != nil {
				return err
			}
			jsonOut, _ := cmd.Flags().GetBool(flagInspectJSON)
			if jsonOut {
				return writeInspectJSON(ios.Out, tr)
			}
			writeTraceText(ios.Out, tr)
			if disk && len(tr.Anomalies) > 0 {
				return errInspectDiff
			}
			return nil
		},
	}
	cmd.Flags().String(flagInspectVolume, "", "卷名（缺省 = 路径内的 <卷>: 前缀或当前卷上下文）")
	cmd.Flags().Bool(flagInspectDisk, false, "对比真实磁盘（慢；输出差异码）")
	cmd.Flags().Bool(flagInspectJSON, false, "输出 JSON")
	return cmd
}

func newCmdVolumeExplain(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "explain <真实路径>",
		Short: "解释真实存储上的文件/目录用途与逻辑归属",
		Long: `解释卷根相对真实路径（容器目录或单个 blob）的用途：
container / dir_meta / file_meta / chunk / parity / unknown，并给出逻辑归属
（哪个文件、第几个分块、哪个逻辑目录）与判定依据。

  --disk  额外对比索引声明（缺失 blob 等），发现差异时退出码 3。`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			volName, realRel, err := resolveInspectTarget(cmd, args[0], st, false)
			if err != nil {
				return err
			}
			disk, _ := cmd.Flags().GetBool(flagInspectDisk)
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return fmt.Errorf(errFmtInitClient, err)
			}
			ex, err := svc.VolumeExplain(cmd.Context(), volName, realRel, disk)
			if err != nil {
				return err
			}
			jsonOut, _ := cmd.Flags().GetBool(flagInspectJSON)
			if jsonOut {
				return writeInspectJSON(ios.Out, ex)
			}
			writeExplainText(ios.Out, ex)
			if len(ex.Anomalies) > 0 {
				return errInspectDiff
			}
			return nil
		},
	}
	cmd.Flags().String(flagInspectVolume, "", "卷名（缺省 = 路径内的 <卷>: 前缀或当前卷上下文）")
	cmd.Flags().Bool(flagInspectDisk, false, "对比索引声明与磁盘（输出差异码）")
	cmd.Flags().Bool(flagInspectJSON, false, "输出 JSON")
	return cmd
}

// resolveInspectTarget 解析卷名与路径：
//   - `--volume` 优先；其次 `<卷>:<路径>` 前缀；最后当前卷上下文；
//   - trace 的逻辑路径走当前目录解析（相对路径）；explain 的真实路径原样传递。
func resolveInspectTarget(cmd *cobra.Command, arg string, st *state.State, logical bool) (string, string, error) {
	volName, _ := cmd.Flags().GetString(flagInspectVolume)
	target := arg
	// 前缀总是剥掉（即使 --volume 指定了目标卷，`vol:/a` 也不该把 `vol:` 当路径一部分）；
	// 卷名以 --volume 为准。
	if v, rest, ok := splitVolumeURI(arg); ok {
		target = rest
		if volName == "" {
			volName = v
		}
	}
	if volName == "" && st != nil {
		volName = st.CurrentVolume
	}
	if volName == "" {
		return "", "", fmt.Errorf("未指定卷：用 --volume 或 `<卷>:<路径>`（如 data:/a/b.txt）")
	}
	if logical {
		if st == nil {
			return volName, strings.TrimPrefix(target, "/"), nil
		}
		resolved, err := st.ResolveRemotePath(target)
		if err != nil {
			return "", "", err
		}
		return volName, resolved, nil
	}
	// 真实路径是「卷根相对」：卷 URI 的前导斜杠只是绝对语义标记，服务端要求相对形态。
	return volName, strings.TrimPrefix(target, "/"), nil
}

// writeInspectJSON 输出稳定 JSON（缩进便于人读，字段序按结构体声明）。
func writeInspectJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// writeTraceText 渲染正向树（设计 §5.4）。
func writeTraceText(w io.Writer, tr *volume.PathTrace) {
	fmt.Fprintf(w, "%s   %s", displayRel(tr.Rel), humanSize(tr.Size))
	if tr.MTime > 0 {
		fmt.Fprintf(w, "  mtime=%s", time.Unix(0, tr.MTime).Format("2006-01-02 15:04"))
	}
	fmt.Fprintln(w)
	if tr.Meta != nil {
		chain := strings.Split(tr.Meta.RealPath, "/")
		depth := 0
		for i := 0; i < len(chain)-1; i++ {
			label := "容器 " + chain[i]
			if i == 0 {
				label = "卷根容器 " + chain[i]
			}
			fmt.Fprintf(w, "%s└─ %s\n", strings.Repeat("   ", depth), label)
			depth++
		}
		fmt.Fprintf(w, "%s├─ %s %s  %s\n", strings.Repeat("   ", depth), metaLabel(tr.Kind), chain[len(chain)-1], humanSize(tr.Meta.Bytes))
	}
	for _, c := range tr.Chunks {
		fmt.Fprintf(w, "   ├─ 分块#%d %s  %s  明文 %s  (blocklets=%d)\n",
			c.Index, path.Base(c.Blob.RealPath), humanSize(c.Blob.Bytes), humanSize(c.Blob.Plain), c.Blocklets)
	}
	if tr.Parity != nil {
		fmt.Fprintf(w, "   └─ parity  %s  %s\n", path.Base(tr.Parity.RealPath), humanSize(tr.Parity.Bytes))
	}
	if tr.Dedup != nil {
		fmt.Fprintf(w, "   去重池: %s（引用 %d 个文件；分块共享，非本文件容器）\n", tr.Dedup.Container, tr.Dedup.Refs)
	}
	writeAnomalies(w, tr.Anomalies)
}

func metaLabel(kind string) string {
	if kind == "dir" {
		return "目录meta"
	}
	return "文件meta"
}

// writeExplainText 渲染反向解释（设计 §5.5）。
func writeExplainText(w io.Writer, ex *volume.RealExplanation) {
	fmt.Fprintf(w, "真实路径 %s\n", ex.RealPath)
	fmt.Fprintf(w, "用途     %s\n", ex.Kind)
	if ex.Owner != nil {
		fmt.Fprintf(w, "归属     %s  (%s)%s\n", ex.Owner.Rel, ex.Owner.Role, ownerSuffix(ex.Owner))
	}
	if ex.Container != nil {
		fmt.Fprintln(w, "容器     "+ex.Container.RealPath+"  逻辑目录 "+displayDir(ex.Container.LogicalDir)+containerSuffix(ex.Container))
	}
	for _, ev := range ex.Evidence {
		fmt.Fprintf(w, "依据     %s\n", ev)
	}
	if len(ex.Entries) > 0 {
		fmt.Fprintln(w, "条目")
		for _, e := range ex.Entries {
			fmt.Fprintln(w, renderRealEntry(e))
		}
	}
	writeAnomalies(w, ex.Anomalies)
}

// displayDir 空逻辑目录 = 卷根。
func displayDir(dir string) string {
	if dir == "" {
		return "(卷根)"
	}
	return dir
}

// ownerSuffix 渲染归属补充信息（分块序号 / parity）。
func ownerSuffix(o *volume.LogicalRef) string {
	switch {
	case o == nil:
		return ""
	case o.Parity:
		return " (parity)"
	case o.ChunkIndex != nil:
		return fmt.Sprintf(" (分块#%d)", *o.ChunkIndex)
	default:
		return ""
	}
}

// containerSuffix 渲染容器 dir_id/parent 补充信息。
func containerSuffix(c *volume.ContainerRef) string {
	if c == nil || c.DirID == "" {
		return ""
	}
	if c.ParentID == "" {
		return " (dir_id=" + c.DirID + ")"
	}
	return fmt.Sprintf(" (dir_id=%s, parent=%s)", c.DirID, c.ParentID)
}

// renderRealEntry 渲染容器内单条目的用途行。
func renderRealEntry(e volume.RealEntry) string {
	owner := ""
	if e.Owner != nil {
		owner = "→ " + e.Owner.Rel + ownerSuffix(e.Owner)
	}
	note := ""
	if e.Note != "" {
		note = "  " + e.Note
	}
	return fmt.Sprintf("  %-9s %s  %s  %s%s", e.Kind, e.Name, humanSize(e.Bytes), owner, note)
}

// writeAnomalies 输出差异码（只读审计，不构成删除建议）。
func writeAnomalies(w io.Writer, list []volume.Anomaly) {
	if len(list) == 0 {
		return
	}
	fmt.Fprintln(w, "差异（只读审计，不构成删除建议；删除请走 GC）")
	for _, a := range list {
		if a.RealPath != "" {
			fmt.Fprintf(w, "  [%s] %s  %s\n", a.Code, a.RealPath, a.Detail)
		} else {
			fmt.Fprintf(w, "  [%s] %s\n", a.Code, a.Detail)
		}
	}
}

func displayRel(rel string) string {
	if rel == "" {
		return "/"
	}
	return rel
}

// humanSize 是审查输出的字节格式化（与 pkg/client 的 FormatByte 同口径）。
func humanSize(n int64) string {
	return formatBytes(n)
}
