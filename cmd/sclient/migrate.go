// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/migrate"
	"github.com/spf13/cobra"
)

// NewCmdMigrate 创建 migrate 向导命令：单机 → 多卷 → 联邦的自动化迁移（roadmap 11.7-⑩）。
//
//	子命令：
//	  export         导出源机文件到本地目录（清单 + files/ 相对路径保持）
//	  import         从本地目录导入目标机（幂等/冲突分类/校验复核）
//	  mirror-config  由 manifest + 目标机卷列表生成 volumes[].mirror_to /
//	                 sync_remotes / federation 片段 YAML
//
// 交互与纯脚本双模式：交互提示走 stdin 扫描（--yes 跳过）；纯逻辑在 pkg/migrate
// （Exporter/Importer/MirrorConfig），本命令只做 CLI 装配与摘要打印。
func NewCmdMigrate(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate <export|import|mirror-config>",
		Short: "迁移向导：单机到多卷/联邦自动化迁移",
		Long: `迁移向导（roadmap 11.7-⑩）：单机 → 多卷 → 联邦的自动化迁移。

	子命令：
	  migrate export <out>         导出源机文件到本地目录（manifest.json + files/）
	  migrate import <in>          从本地目录导入目标机（幂等/冲突分类/校验复核）
	  migrate mirror-config <in>   生成镜像/同步/联邦 YAML 片段（--print 或 --write）

	通用 flags：
	  --yes              跳过交互确认（纯脚本模式）
	  --export-only      只导出不导入（--yes 语义）
	  --import-only      只导入不导出
	  --no-verify        跳过导入后校验（慎用，默认校验）
	  --ignore-errors    单文件失败跳过继续（默认快速失败）
	  --out <dir>        导出目录
	  --in <dir>         导入目录
	  --print            打印生成的 YAML 片段（mirror-config 默认）
	  --write <path>     把 YAML 片段写入文件`,
		Example: `  sclient migrate export ./mig
  sclient migrate import ./mig --yes
  sclient migrate mirror-config ./mig --print
  sclient migrate mirror-config ./mig --write /etc/sproxy/migrate.yaml`,
		Args: cobra.MinimumNArgs(1),
	}
	cmd.AddCommand(newCmdMigrateExport(factory, ios))
	cmd.AddCommand(newCmdMigrateImport(factory, ios))
	cmd.AddCommand(newCmdMigrateMirrorConfig(factory, ios))
	cmd.PersistentFlags().Bool("yes", false, "跳过交互确认（纯脚本模式）")
	cmd.PersistentFlags().Bool("ignore-errors", false, "单文件失败跳过继续（默认快速失败）")
	cmd.PersistentFlags().Bool("no-verify", false, "跳过导入后校验（慎用，默认校验）")
	return cmd
}

// newCmdMigrateExport 创建 migrate export 子命令。
func newCmdMigrateExport(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export <out>",
		Short: "导出源机文件到本地目录",
		Long: `递归列出源机全部文件（含子目录）→ 下载到 <out>/files/（相对路径保持）
→ 逐文件校验 SHA-256 → 原子写 <out>/manifest.json。

	<out> 为目标目录（自动创建）；导出的清单 + 文件可用于跨机迁移（import）。`,
		Example: `  sclient migrate export ./mig
  sclient migrate export ./mig --yes`,
		Args: cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			outDir, dirErr := migrateOutDir(cmd, args)
			if dirErr != nil {
				return dirErr
			}
			yes, _ := cmd.Flags().GetBool("yes")
			if !yes {
				if cErr := confirmMigration(ios.In, ios.ErrOut, "导出源机全部文件到 "+outDir+"？(y/N) "); cErr != nil {
					return cErr
				}
			}
			ex := &migrate.Exporter{Client: svc, OutDir: outDir}
			m, err := ex.Export(cmd.Context())
			if err != nil {
				ios.WriteErrLine("导出失败: %v", err)
				return fmt.Errorf("导出失败: %w", err)
			}
			var total int64
			for _, f := range m.Files {
				total += f.Size
			}
			ios.WriteOutLine("导出完成: %d 文件 / %d 字节 → %s（清单 manifest.json）", len(m.Files), total, outDir)
			return nil
		},
	}
	cmd.Flags().String("out", "", "导出目录（也可作为第一参数 <out> 传入）")
	return cmd
}

// newCmdMigrateImport 创建 migrate import 子命令。
func newCmdMigrateImport(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import <in>",
		Short: "从本地目录导入目标机",
		Long: `读 <in>/manifest.json（schema/checksum 自检，损坏/不符拒绝）→ 逐文件上传
→ 目标 stat/checksum 复核。同名同 checksum 跳过（幂等）；不同 → CONFLICT。

	默认快速失败（单文件失败即中止）；--ignore-errors 跳过继续；
	--no-verify 跳过导入后复核（慎用）。`,
		Example: `  sclient migrate import ./mig --yes
  sclient migrate import ./mig --ignore-errors`,
		Args: cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			inDir, dirErr := migrateInDir(cmd, args)
			if dirErr != nil {
				return dirErr
			}
			yes, _ := cmd.Flags().GetBool("yes")
			if !yes {
				if cErr := confirmMigration(ios.In, ios.ErrOut, "从 "+inDir+" 导入目标机？(y/N) "); cErr != nil {
					return cErr
				}
			}
			im := &migrate.Importer{
				Client:       svc,
				InDir:        inDir,
				IgnoreErrors: migrateIgnoreErrors(cmd),
			}
			sum, err := im.Import(cmd.Context())
			printImportSummary(ios.Out, sum)
			if err != nil {
				ios.WriteErrLine("导入未完成: %v", err)
				if errors.Is(err, migrate.ErrConflict) {
					return fmt.Errorf("%w（同名文件 checksum 不一致；处置冲突后重跑幂等收敛）", err)
				}
				return err
			}
			return nil
		},
	}
	cmd.Flags().String("in", "", "导入目录（也可作为第一参数 <in> 传入）")
	return cmd
}

// newCmdMigrateMirrorConfig 创建 migrate mirror-config 子命令。
func newCmdMigrateMirrorConfig(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mirror-config <in>",
		Short: "生成镜像/同步/联邦 YAML 片段",
		Long: `读 <in>/manifest.json + 目标机卷列表（GET /api/volumes）→ 生成
volumes[].mirror_to（多卷冗余）/ sync_remotes（源机为同步远端）/
federation.peers（源机为联邦对端）片段 YAML。

	默认打印（--print）；--write <path> 落盘。合并进目标机配置后重启生效。`,
		Example: `  sclient migrate mirror-config ./mig --print
  sclient migrate mirror-config ./mig --write /etc/sproxy/migrate.yaml`,
		Args: cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			inDir, err := migrateInDir(cmd, args)
			if err != nil {
				return err
			}
			m, err := migrate.ReadFile(filepath.Join(inDir, "manifest.json"))
			if err != nil {
				ios.WriteErrLine("读清单失败: %v", err)
				return fmt.Errorf("读清单失败: %w", err)
			}
			vols, err := svc.Volumes(cmd.Context())
			if err != nil {
				ios.WriteErrLine("获取目标机卷列表失败: %v", err)
				return fmt.Errorf("获取卷列表失败: %w", err)
			}
			ak, _ := cmd.Flags().GetString("access-key")
			sk, _ := cmd.Flags().GetString("access-key-secret")
			akid, _ := cmd.Flags().GetString("access-key-id")
			out, err := migrate.GenerateMirrorConfig(m, vols, migrate.MirrorOptions{
				TargetURL:       svc.ServerURL(),
				AccessKey:       ak,
				AccessKeySecret: sk,
				AccessKeyID:     akid,
			})
			if err != nil {
				return err
			}
			writePath, _ := cmd.Flags().GetString("write")
			if writePath != "" {
				if err := os.WriteFile(writePath, out, 0o644); err != nil {
					ios.WriteErrLine("写 YAML 失败: %v", err)
					return fmt.Errorf("写 YAML 失败: %w", err)
				}
				ios.WriteOutLine("已写入 %s（合并进目标机配置后重启生效）", writePath)
				return nil
			}
			_, _ = ios.Out.Write(out)
			return nil
		},
	}
	cmd.Flags().String("write", "", "把 YAML 片段写入文件（默认打印到 stdout）")
	cmd.Flags().Bool("print", false, "打印 YAML 片段到 stdout（默认行为，显式声明用）")
	cmd.Flags().String("access-key", "", "源机 SproxySig AccessKey（写入 sync_remotes/federation 凭据）")
	cmd.Flags().String("access-key-secret", "", "源机 SproxySig AccessKeySecret")
	cmd.Flags().String("access-key-id", "", "源机 SproxySig SK 条目 ID")
	return cmd
}

// migrateOutDir 解析导出目录（flag --out 优先于位置参数）。
func migrateOutDir(cmd *cobra.Command, args []string) (string, error) {
	out, _ := cmd.Flags().GetString("out")
	if out == "" && len(args) > 0 {
		out = args[0]
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("导出目录不能为空（migrate export <out> 或 --out <dir>）")
	}
	return filepath.Clean(out), nil
}

// migrateInDir 解析导入目录（flag --in 优先于位置参数）。
func migrateInDir(cmd *cobra.Command, args []string) (string, error) {
	in, _ := cmd.Flags().GetString("in")
	if in == "" && len(args) > 0 {
		in = args[0]
	}
	if strings.TrimSpace(in) == "" {
		return "", fmt.Errorf("导入目录不能为空（migrate import <in> 或 --in <dir>）")
	}
	return filepath.Clean(in), nil
}

// migrateIgnoreErrors 解析 --ignore-errors。
func migrateIgnoreErrors(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool("ignore-errors")
	return v
}

// confirmMigration 交互确认（stdin 扫描；EOF/否 → 取消）。
func confirmMigration(in io.Reader, errOut io.Writer, prompt string) error {
	fmt.Fprint(errOut, prompt)
	sc := bufio.NewScanner(in)
	if !sc.Scan() {
		return fmt.Errorf("已取消（无输入）")
	}
	answer := strings.ToLower(strings.TrimSpace(sc.Text()))
	if answer != "y" && answer != "yes" {
		return fmt.Errorf("已取消")
	}
	return nil
}

// printImportSummary 打印导入汇总（计数/字节/失败清单）。
func printImportSummary(out io.Writer, sum *migrate.ImportSummary) {
	fmt.Fprintf(out, "导入汇总: imported=%d skipped=%d conflict=%d failed=%d（%d 字节）\n",
		sum.Imported, sum.Skipped, sum.Conflict, sum.Failed, sum.ImportedBytes+sum.SkippedBytes)
	for _, f := range sum.Failures {
		fmt.Fprintf(out, "  FAILED %s: %s\n", f.Name, f.Err)
	}
}
