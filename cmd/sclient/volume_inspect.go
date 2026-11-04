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
			if err := emitTraceResult(cmd, ios, tr); err != nil {
				return err
			}
			if inspectHasDiff(tr.Anomalies) {
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
			if err := emitExplainResult(cmd, ios, ex); err != nil {
				return err
			}
			if inspectHasDiff(ex.Anomalies) {
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

// emitTraceResult 输出正向结果（--json 或文本）。**两种模式共用同一退出码判定**：
// 此前 JSON 分支提前 return，导致同一份数据文本模式退 3、JSON 模式退 0（不一致）。
func emitTraceResult(cmd *cobra.Command, ios cli.IOStreams, tr *volume.PathTrace) error {
	if jsonOut, _ := cmd.Flags().GetBool(flagInspectJSON); jsonOut {
		return writeInspectJSON(ios.Out, tr)
	}
	writeTraceText(ios.Out, tr)
	return nil
}

// emitExplainResult 输出反向结果（--json 或文本）。
func emitExplainResult(cmd *cobra.Command, ios cli.IOStreams, ex *volume.RealExplanation) error {
	if jsonOut, _ := cmd.Flags().GetBool(flagInspectJSON); jsonOut {
		return writeInspectJSON(ios.Out, ex)
	}
	writeExplainText(ios.Out, ex)
	return nil
}

// inspectInformationalAnomalies 是**信息性**标记：不构成「存储差异」，不影响退出码。
//   - upload_in_progress：本进程在途上传（meta 未提交），是正常中间态；
//   - dedup_shared：内容位于去重池，是设计内的共享布局。
var inspectInformationalAnomalies = map[string]struct{}{
	volume.AnomalyUploadInProgress: {},
	volume.AnomalyDedupShared:      {},
}

// inspectHasDiff 报告是否存在**真实差异**（详见 volume.Anomaly 码表）。
//
// 语义：退出码 3 = 索引声明与磁盘事实不一致（或缺/多/大小不符/meta 不可读）。
// 上传中/去重共享不算差异——否则「上传中看一眼」就退 3，脚本无法区分。
func inspectHasDiff(anomalies []volume.Anomaly) bool {
	for _, a := range anomalies {
		if _, ok := inspectInformationalAnomalies[a.Code]; ok {
			continue
		}
		return true
	}
	return false
}

// writeInspectJSON 输出稳定 JSON（缩进便于人读，字段序按结构体声明）。
func writeInspectJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// writeTraceText 渲染正向树（设计 §5.4）。
//
// 每层显示**真实容器名**与其**逻辑目录名**（例如 `容器 kv… → kv`），
// 这样才能把「盘上的随机目录」和「用户认识的目录」对上。
func writeTraceText(w io.Writer, tr *volume.PathTrace) {
	head := displayRel(tr.Rel) + "   " + humanSize(tr.Size)
	if tr.MTime > 0 {
		head += "  mtime=" + time.Unix(0, tr.MTime).Format("2006-01-02 15:04")
	}
	if tr.Mode == "uploading" {
		fmt.Fprintln(w, head+"   上传中（meta 尚未提交）")
		depth := writeContainerRefs(w, tr.Containers)
		writeUploadProgress(w, tr.Upload)
		_ = depth
		writeAnomalies(w, tr.Anomalies)
		return
	}
	fmt.Fprintln(w, head)
	depth := 0
	if len(tr.Containers) > 0 {
		depth = writeContainerRefs(w, tr.Containers)
	} else if tr.Meta != nil {
		depth = writeContainerChain(w, tr.Meta.RealPath)
	}
	if tr.Meta != nil {
		fmt.Fprintln(w, indent(depth)+"├─ "+metaLabel(tr.Kind)+" "+path.Base(tr.Meta.RealPath)+"  "+humanSize(tr.Meta.Bytes))
	}
	for _, c := range tr.Chunks {
		line := fmt.Sprintf("├─ 分块#%d %s  %s  明文 %s  (blocklets=%d)",
			c.Index, path.Base(c.Blob.RealPath), humanSize(c.Blob.Bytes), humanSize(c.Blob.Plain), c.Blocklets)
		fmt.Fprintln(w, indent(depth)+line)
	}
	if tr.Parity != nil {
		fmt.Fprintln(w, indent(depth)+"└─ parity  "+path.Base(tr.Parity.RealPath)+"  "+humanSize(tr.Parity.Bytes))
	}
	if tr.Dedup != nil {
		fmt.Fprintf(w, "   去重池: %s（引用 %d 个文件；分块共享，非本文件容器）", tr.Dedup.Container, tr.Dedup.Refs)
		fmt.Fprintln(w)
	}
	writeAnomalies(w, tr.Anomalies)
}

// writeUploadProgress 渲染在途上传进度（已上传密文字节 ≈ 明文 + 每块开销）。
func writeUploadProgress(w io.Writer, up *volume.UploadProgress) {
	if up == nil {
		return
	}
	pct := ""
	if up.Total > 0 {
		v := min(up.Committed*100/up.Total, 100)
		pct = fmt.Sprintf(" (%.0f%%)", float64(v))
	}
	line := fmt.Sprintf("  已上传 %s / %s%s   已完成 %d 块",
		humanSize(up.Committed), humanSize(up.Total), pct, up.Chunks)
	if up.StartedAt > 0 {
		line += "   开始于 " + time.Unix(0, up.StartedAt).Format("15:04:05")
	}
	fmt.Fprintln(w, line)
	fmt.Fprintln(w, "  说明：目标 meta 未提交，索引中暂无该文件；提交后自动转为正常落点。")
}

// writeContainerRefs 输出带逻辑名的容器链，返回最深层层号。
func writeContainerRefs(w io.Writer, refs []volume.ContainerRef) int {
	depth := 0
	for i, c := range refs {
		label := "容器 " + path.Base(c.RealPath)
		if i == 0 {
			label = "卷根容器 " + path.Base(c.RealPath)
		}
		name := c.LogicalDir
		switch {
		case i == 0 && name == "":
			name = "(卷根)"
		case name == "":
			name = "(未索引)"
		}
		fmt.Fprintln(w, indent(depth)+"└─ "+label+"   → "+name)
		depth++
	}
	return depth
}

// writeContainerChain 在没有 Containers 数据时按真实路径兜底渲染链。
func writeContainerChain(w io.Writer, realPath string) int {
	chain := strings.Split(realPath, "/")
	depth := 0
	for i := 0; i < len(chain)-1; i++ {
		label := "容器 " + chain[i]
		if i == 0 {
			label = "卷根容器 " + chain[i]
		}
		fmt.Fprintln(w, indent(depth)+"└─ "+label)
		depth++
	}
	return depth
}

// indent 返回 depth 层的缩进（每层 3 空格，便于与 └─ 对齐）。
func indent(depth int) string { return strings.Repeat("   ", depth) }

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
	if e.Container != nil {
		owner = "→ 目录 " + displayDir(e.Container.LogicalDir)
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
