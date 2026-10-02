// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// NewCmdVolume 创建 volume 子命令：用户自有卷管理（create/list/delete）。
//
// 与服务端用户卷 API（POST/GET/DELETE /api/volumes/user）对应：每个 sproxy 用户可
// 管理自己的网盘盘（仅外部类型：baidupcs 等已注册 backend）。
//
//	volume create <name> --type baidupcs --extra '{"bduss":"..."}' [--capacity 100GiB]
//	volume list
//	volume delete <name>
func NewCmdVolume(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "volume",
		Short: "管理用户自有卷与卷间操作",
		Long: `管理当前用户的网盘盘（用户自有卷，仅外部类型）与卷间操作（copy/move/rebalance）。

用户卷是每个 sproxy 用户独立管理的网盘盘（经服务端 /api/volumes/user API）：
  create <name>  创建用户卷（--type 后端类型 + --extra 类型特有配置）
  list           列出我的用户卷
  delete <name>  删除用户卷（被活跃同步任务引用时拒绝）

卷间操作（多卷）：
  copy <file> --to-volume <v>  跨卷复制文件（保留源）
  move <file> --to-volume <v>  跨卷移动文件
  rebalance --from-volume <a> --to-volume <b>  卷再平衡（异步）`,
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newCmdVolumeCreate(factory, ios))
	cmd.AddCommand(newCmdVolumeList(factory, ios))
	cmd.AddCommand(newCmdVolumeDelete(factory, ios))
	cmd.AddCommand(newCmdVolumeCopy(factory, ios, st))
	cmd.AddCommand(newCmdVolumeMove(factory, ios, st))
	cmd.AddCommand(newCmdVolumeRebalance(factory, ios))
	return cmd
}

func newCmdVolumeCreate(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	var (
		typ      string
		extraRaw string
		capacity string
	)
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "创建用户自有卷",
		Long: `创建当前用户的网盘盘（仅外部类型）。

--type 指定后端类型（已注册 backend：baidupcs|webdav，未来自动扩展）；--extra 为类型特有
配置 JSON（如 baidupcs 的 {"bduss":"...","baidu_root":"/disk1"}、webdav 的
{"url":"https://nextcloud.example/remote.php/dav/files/user"}）；--capacity 可选容量上限
（人类可读大小如 "100GiB"，缺省 0 = 不限制）。`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return volumeCreateRunE(cmd, factory, ios, &typ, &extraRaw, &capacity)
		},
	}
	cmd.Flags().StringVar(&typ, "type", "", "卷后端类型（已注册 backend：baidupcs|webdav）")
	cmd.Flags().StringVar(&extraRaw, "extra", "", "类型特有配置 JSON（如 {\"bduss\":\"...\"}）")
	cmd.Flags().StringVar(&capacity, "capacity", "", "容量上限（如 100GiB；0/缺省 = 不限制）")
	return cmd
}

// volumeCreateRunE 执行 volume create 命令主体（newCmdVolumeCreate 的 RunE 抽出）。
func volumeCreateRunE(cmd *cobra.Command, factory clientfactory.Factory, ios cli.IOStreams, typ, extraRaw, capacity *string) error {
	name := args0(cmd)
	if *typ == "" {
		return fmt.Errorf("--type 必填（已注册 backend：baidupcs|webdav）")
	}
	extra, err := volumeExtraMap(*extraRaw)
	if err != nil {
		return err
	}
	capBytes, err := volumeCapacity(*capacity)
	if err != nil {
		return err
	}
	svc, err := factory.NewClient(cmd)
	if err != nil {
		ios.WriteErrLine(errFmtInitClientPrint, err)
		return fmt.Errorf(errFmtInitClient, err)
	}
	if err := svc.CreateUserVolume(cmd.Context(), name, *typ, capBytes, extra); err != nil {
		ios.WriteErrLine("创建用户卷失败: %v", err)
		return fmt.Errorf("创建用户卷失败: %w", err)
	}
	ios.WriteOutLine("创建成功: %s（type=%s）", name, *typ)
	return nil
}

// args0 返回命令第一个位置参数（volume create <name>）。
func args0(cmd *cobra.Command) string {
	args := cmd.Flags().Args()
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

// volumeExtraMap 解析 --extra JSON（对象语义；非对象/非法 → fail-closed 报错）。
func volumeExtraMap(extraRaw string) (map[string]any, error) {
	if extraRaw == "" {
		return nil, nil
	}
	extra := map[string]any{}
	if err := json.Unmarshal([]byte(extraRaw), &extra); err != nil {
		return nil, fmt.Errorf("--extra 必须是合法 JSON 对象: %w", err)
	}
	return extra, nil
}

// volumeCapacity 解析 --capacity（人类可读大小；缺省 0 = 不限制）。
func volumeCapacity(capacity string) (int64, error) {
	if capacity == "" {
		return 0, nil
	}
	c, err := size.ParseSize(capacity)
	if err != nil {
		return 0, fmt.Errorf("--capacity 非法: %w", err)
	}
	return c, nil
}

func newCmdVolumeList(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出我的用户卷",
		Long:  "列出当前用户的全部用户自有卷（name/type/capacity）。",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			vols, err := svc.UserVolumes(cmd.Context())
			if err != nil {
				ios.WriteErrLine("获取用户卷列表失败: %v", err)
				return fmt.Errorf("获取用户卷列表失败: %w", err)
			}
			printUserVolumes(ios.Out, cmd, vols)
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "以 JSON 输出")
	return cmd
}

func newCmdVolumeDelete(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "删除用户自有卷",
		Long:  "删除当前用户的网盘盘。被活跃同步任务引用时服务端返回 409（需先取消任务）。",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			if err := svc.DeleteUserVolume(cmd.Context(), args[0]); err != nil {
				ios.WriteErrLine("删除用户卷失败: %v", err)
				return fmt.Errorf("删除用户卷失败: %w", err)
			}
			ios.WriteOutLine("删除成功: %s", args[0])
			return nil
		},
	}
	return cmd
}

// printUserVolumes 按 --json 决定输出：JSON 直接输出 volumes 数组；文本人类可读表格。
func printUserVolumes(w io.Writer, cmd *cobra.Command, vols []client.UserVolume) {
	useJSON, _ := cmd.Flags().GetBool("json")
	if useJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"volumes": vols})
		return
	}
	if len(vols) == 0 {
		fmt.Fprintln(w, "无用户卷")
		return
	}
	fmt.Fprintf(w, "%-16s  %-10s  %-14s\n", "名称", "类型", "容量")
	for _, v := range vols {
		capTxt := "不限"
		if v.Capacity > 0 {
			capTxt = client.FormatByte(float64(v.Capacity))
		}
		fmt.Fprintf(w, "%-16s  %-10s  %-14s\n", v.Name, v.Type, capTxt)
	}
}
