// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// NewCmdCloudArchive 创建 cloud-download archive 子命令的工厂函数。
//
// 转存目标（可选）用**卷 URI 路径** `--target <卷名>:/<路径>`（cd/mv 同规约）——不引入
// volume flag：URI 里的卷名由客户端归一为请求体的 target_volume/target_path，协议零改动。
// 未带卷名的纯路径回落当前工作目录所在卷（`cd <卷名>:/…` 设定）。
func NewCmdCloudArchive(factory clientfactory.Factory, ios cli.IOStreams, st *state.State, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "archive <task-id> [task-id...]",
		Short: "打包云端下载已完成的任务文件",
		Long: `将指定已完成云端下载任务的文件打包为 tar.gz。

支持单个或多个任务 ID。使用 --name 指定归档文件名。
使用 --target 指定转存目标（卷 URI 路径 <卷名>:/<路径>）时，打包后转存到该外部卷，
不落服务端 archive 桶：

  sclient cloud-download archive <task-id> --target vault:/backups/a.tar.gz
  sclient cloud-download archive <task-id> --target disk2:/            # 目录（默认文件名）
  cd vault:/backups && sclient cloud-download archive <task-id> --target a.tar.gz`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			return runCloudArchive(cmd, args, svc, ios, st)
		},
	}

	cmd.Flags().String("name", "", "归档文件名（默认自动生成）")
	cmd.Flags().String("target", "", "转存目标（卷 URI 路径 <卷名>:/<路径>；缺省落服务端 archive 桶）")
	cmd.Flags().Bool("encrypt", false, "转存时流式加密（tar.aes；需服务端 archive.key_file；仅与 --target 同用）")
	cmd.Flags().String("cipher", "", "加密算法（默认 aes-256-gcm；仅与 --encrypt/--target 同用）")
	cmd.Flags().String("compress", "", "压缩算法（gzip 默认 / zstd / brotli；仅与 --target 同用）")

	return cmd
}

// runCloudArchive 执行归档（单/批）；抽离自 RunE 控制 gocognit。
func runCloudArchive(cmd *cobra.Command, args []string, svc *client.FileClient, ios cli.IOStreams, st *state.State) error {
	archiveName, _ := cmd.Flags().GetString("name")
	if archiveName == "" {
		// 客户端要求非空名称，未指定时自动生成
		archiveName = fmt.Sprintf("cloud-archive-%d.tar.gz", time.Now().Unix())
	}
	target, terr := resolveArchiveTargetFlag(cmd, st)
	if terr != nil {
		ios.WriteErrLine("%v", terr)
		return terr
	}
	opts, oerr := archivePreOpts(cmd, target)
	if oerr != nil {
		ios.WriteErrLine("%v", oerr)
		return oerr
	}

	if len(args) == 1 {
		result, err := svc.ArchiveCloudTask(cmd.Context(), args[0], archiveName, opts...)
		if err != nil {
			return fmt.Errorf("归档失败: %w", err)
		}
		ios.WriteOutLine("归档完成: %s (%d bytes)", result.File, result.Size)
		return nil
	}
	result, err := svc.ArchiveCloudTasks(cmd.Context(), args, archiveName, opts...)
	if err != nil {
		return fmt.Errorf("归档失败: %w", err)
	}
	// SkippedCount>0 时提示部分跳过（服务端批量归档会跳过无效/未完成任务）
	if result.SkippedCount > 0 {
		ios.WriteOutLine("归档完成: %s (%d bytes, %d files, 跳过 %d 个任务)",
			result.File, result.Size, result.TaskCount, result.SkippedCount)
		return nil
	}
	ios.WriteOutLine("归档完成: %s (%d bytes, %d files)", result.File, result.Size, result.TaskCount)
	return nil
}

// resolveArchiveTargetFlag 读取 --target 并归一卷 URI 路径。
func resolveArchiveTargetFlag(cmd *cobra.Command, st *state.State) (*client.TransferSpec, error) {
	raw, _ := cmd.Flags().GetString("target")
	return resolveArchiveTarget(st, raw)
}

// archiveTargetOpts 把转存目标转为 SDK option（nil → 无 option，落 archive 桶）。
func archiveTargetOpts(t *client.TransferSpec) []client.CloudArchiveOption {
	if t == nil {
		return nil
	}
	return []client.CloudArchiveOption{client.WithCloudArchiveTarget(t)}
}

// archivePreOpts 组装归档预处理 option（--encrypt/--cipher/--compress）；仅与 --target 同用
// （无 target 时本地 fail-fast，与服务端 400 一致，防静默忽略）。
func archivePreOpts(cmd *cobra.Command, target *client.TransferSpec) ([]client.CloudArchiveOption, error) {
	encrypt, _ := cmd.Flags().GetBool("encrypt")
	cipher, _ := cmd.Flags().GetString("cipher")
	compress, _ := cmd.Flags().GetString("compress")
	if (encrypt || cipher != "" || compress != "") && target == nil {
		return nil, fmt.Errorf("--encrypt/--cipher/--compress 仅与 --target（转存外部卷）同用")
	}
	// --cipher 未配 --encrypt 时无任何效果（服务端也不加密）——本地 fail-fast，防静默忽略。
	if cipher != "" && !encrypt {
		return nil, fmt.Errorf("--cipher 仅与 --encrypt 同用（单独指定不加密，已拒绝以避免误信已加密）")
	}
	opts := archiveTargetOpts(target)
	if encrypt {
		opts = append(opts, client.WithCloudArchiveEncrypt(cipher))
	}
	if compress != "" {
		opts = append(opts, client.WithCloudArchiveCompression(compress))
	}
	return opts, nil
}

// resolveArchiveTarget 把 `--target` 的卷 URI 路径归一为转存目标（volume+path）。
//   - `<卷名>:/<路径>` → 该卷 + 路径（`<卷名>:` / `:<卷名>/` = 卷根，path 空 = 服务端默认 archive/<名>）；
//     卷 URI 里的路径是**卷根起算的绝对路径**（与 splitVolumeURI 口径一致）。
//   - 纯路径（无卷 URI 前缀）→ 当前工作目录所在卷；路径按 CLI 统一约定
//     （state.ResolveRemotePath，与 cp/mv/delete/cd 同语义）**拼当前目录**，前导 "/" 则
//     绕过当前目录、`..` 拒绝；无当前卷 → 报错（请显式给卷 URI）。
//   - 空 → nil（不转存，落 archive 桶）。
//
// 与 cd/mv/cp 共用 splitVolumeURI（`:/` 形式，`https://` / `C:\` 不误判）。
func resolveArchiveTarget(st *state.State, raw string) (*client.TransferSpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if vol, path, ok := splitVolumeURI(raw); ok {
		if path == "/" {
			path = "" // 卷根：服务端用默认 archive/<归档名>
		}
		return &client.TransferSpec{Volume: vol, Path: path}, nil
	}
	if st == nil || st.CurrentVolume == "" {
		return nil, fmt.Errorf("转存目标须为卷 URI 路径（<卷名>:/<路径>），或先 `cd <卷名>:/` 指定当前卷")
	}
	// 与 cp/mv/delete 同语义：相对路径拼当前目录，前导 "/" 绕过，`..` 拒绝。
	rel, err := st.ResolveRemotePath(raw)
	if err != nil {
		return nil, fmt.Errorf("无效的转存目标路径: %w", err)
	}
	return &client.TransferSpec{Volume: st.CurrentVolume, Path: rel}, nil
}
