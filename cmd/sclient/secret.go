// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/scrypt"
	"golang.org/x/term"
)

// secret 命令族：secret 卷管理（创建随机/双口令派生 / 列表 / 导出 / 删除 / 导入）。
//
// 领域逻辑全部在 pkg/client（CreateSecret / ListSecrets / ExportSecret / DeleteSecret）
// 与 pkg/volume/secrets（DerivePassphraseSecret）；本文件只做 flag 解析 + 交互输入 +
// 调用 + IO 展示（cmd 薄逻辑）。
//
// 安全约束（用户 2026-10-04 裁定）：
//   - 双口令**仅在本地**派生（x/term.ReadPassword 交互读取，绝不出现在 argv/bash 历史；
//     原始口令永不上线）——只把派生结果上传服务端。
//   - 随机 secret 由服务端生成（客户端不参与，更安全），响应返回 secret 值供立即备份。
//   - secret 经专用 /api/secrets（secrets 是 ExternalBackend，通用 /upload?volume= 不命中）。

// errSecretAborted 是交互输入空/EOF 时返回的错误（仿 errLoginNotConfirmed 语义：
// 无输入即中止，绝不把「未确认」当成功）。
var errSecretAborted = errors.New("操作已中止：未收到有效输入")

// NewCmdSecret 创建 secret 命令族。
func NewCmdSecret(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "secret 卷管理（创建随机/双口令派生 / 列表 / 导出 / 删除 / 导入）",
	}
	cmd.AddCommand(newCmdSecretCreate(factory, ios, cfgSvc))
	cmd.AddCommand(newCmdSecretList(factory, ios, cfgSvc))
	cmd.AddCommand(newCmdSecretExport(factory, ios, cfgSvc))
	cmd.AddCommand(newCmdSecretImport(factory, ios, cfgSvc))
	cmd.AddCommand(newCmdSecretDelete(factory, ios, cfgSvc))
	return cmd
}

// ---- secret import ----

// newCmdSecretImport 导入 secret：从本地文件读取（明文 hex 或口令加密信封）上传到
// secrets 卷。明文文件每行一个 secret 值；加密信封（secret export 默认产物）需交互
// 输入解密口令后上传。文件 = 普通文件直觉；服务端只校验格式后落盘。
func newCmdSecretImport(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	var nameFlag string
	cmd := &cobra.Command{
		Use:   "import <file>",
		Short: "导入 secret（明文 hex 文件或口令加密信封）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, rerr := os.ReadFile(args[0])
			if rerr != nil {
				ios.WriteErrLine("读导入文件失败: %v", rerr)
				return fmt.Errorf("读导入文件失败: %w", rerr)
			}
			name := resolveImportName(nameFlag, args[0])
			if name == "" {
				ios.WriteErrLine("secret 名不能为空（用 --name 指定）")
				return errSecretAborted
			}
			value, err := decodeImportValue(ios, data)
			if err != nil {
				return err
			}
			svc, cerr := newSecretDirectClient(cmd, factory, cfgSvc)
			if cerr != nil {
				ios.WriteErrLine(errFmtInitClientPrint, cerr)
				return fmt.Errorf(errFmtInitClient, cerr)
			}
			res, err := svc.CreateSecret(cmd.Context(), client.SecretCreateOptions{
				Name: name, Mode: "import", Value: value, Origin: "import",
			})
			if err != nil {
				ios.WriteErrLine("导入 secret %q 失败: %v", name, err)
				return fmt.Errorf("导入 secret %q 失败: %w", name, err)
			}
			ios.WriteOutLine("已导入 secret %q", res.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&nameFlag, "name", "", "目标 secret 名（默认取文件 basename，去 .secret 后缀）")
	return cmd
}

// resolveImportName 决定导入目标名：--name 显式优先，否则取文件 basename 去 .secret 后缀。
func resolveImportName(nameFlag, file string) string {
	name := strings.TrimSpace(nameFlag)
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(file), ".secret")
	}
	return strings.TrimSpace(name)
}

// decodeImportValue 解开导入文件内容为 64-hex：加密信封（secretDecryptExport 可解）需
// 交互输入口令；明文 hex 直接透传（服务端最终校验）。空输入 fail-closed。
//
// 非 tty 环境（stdin 非终端）下 `secretReadPassphrase` 拒绝读取 → pass 为空：若文件形态
// 是加密信封（非 64-hex），**不会**把它当明文 hex 透传（那只会让服务端报「hex 校验失败」，
// 掩盖真实原因）——显式提示需 tty 才能解密；明文 hex 则正常透传。
func decodeImportValue(ios cli.IOStreams, data []byte) (string, error) {
	raw := strings.TrimSpace(string(data))
	if raw == "" {
		ios.WriteErrLine("导入文件为空")
		return "", errSecretAborted
	}
	isPlainHex := isSecretHexLocal(raw)
	// 加密信封（secret export 产物，非 64-hex）需交互读口令解开；明文 hex 直接透传。
	pass, perr := secretReadPassphrase(ios, "导入解密口令（无口令直接回车跳过，明文导入）: ")
	if perr != nil && !errors.Is(perr, errSecretAborted) {
		return "", perr
	}
	if pass == "" {
		if !isPlainHex {
			// 非 tty 下口令读取被拒（pass==""）且文件不是明文 hex → 是加密信封。
			// 明确提示需终端环境解密，避免把密文当 hex 透传后服务端报费解的 400。
			ios.WriteErrLine("导入文件是口令加密信封（非明文 hex）：需在终端环境输入解密口令")
			return "", errSecretAborted
		}
		return raw, nil
	}
	plain, derr := secretDecryptExport(pass, data)
	if derr != nil {
		ios.WriteErrLine("解密导入文件失败: %v", derr)
		return "", fmt.Errorf("解密导入文件失败: %w", derr)
	}
	return strings.TrimSpace(string(plain)), nil
}

// isSecretHexLocal 判定字符串是否为 64 位小写 hex（导入文件形态探测，与服务端
// isSecretHex 同规则；CLI 侧不 import secrets 包防子 module 循环依赖）。
func isSecretHexLocal(v string) bool {
	if len(v) != 64 {
		return false
	}
	for _, c := range v {
		if c < '0' || c > '9' && c < 'a' || c > 'f' {
			return false
		}
	}
	return true
}

// newSecretDirectClient 构建直连客户端（secret 端点走主 mux authMiddleware，需签名
// actor；隧道内层不注入 actor → 必须直连，与 trust 同模式）。
func newSecretDirectClient(cmd *cobra.Command, factory clientfactory.Factory, cfgSvc ConfigProvider) (*client.FileClient, error) {
	return newTrustDirectClient(cmd, factory, cfgSvc)
}

// ---- secret create ----

// newCmdSecretCreate 创建 secret：无 --passphrase → 服务端生成随机（返回值供备份）；
// 有 --passphrase → 本地交互读两口令后双口令派生上传（origin=passphrase）。
func newCmdSecretCreate(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	var passphrase bool
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "创建 secret（默认服务端随机生成；--passphrase 本地双口令派生）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			if name == "" {
				ios.WriteErrLine(msgSecretNameEmpty)
				return errSecretAborted
			}
			svc, cerr := newSecretDirectClient(cmd, factory, cfgSvc)
			if cerr != nil {
				ios.WriteErrLine(errFmtInitClientPrint, cerr)
				return fmt.Errorf(errFmtInitClient, cerr)
			}
			if !passphrase {
				// 服务端生成随机；响应返回 secret 值供立即备份。
				res, rerr := svc.CreateRandomSecret(cmd.Context(), name)
				if rerr != nil {
					ios.WriteErrLine("创建 secret 失败: %v", rerr)
					return fmt.Errorf("创建 secret 失败: %w", rerr)
				}
				ios.WriteOutLine("已创建随机 secret %q（请立即备份，仅本次显示）：", res.Name)
				ios.WriteOutLine("%s", res.Value)
				return nil
			}
			// 本地双口令派生：交互读两口令（不进 bash 历史，不回显）。
			passA, err := secretReadPassphrase(ios, "口令 A（不显示）: ")
			if err != nil {
				return err
			}
			passB, err := secretReadPassphrase(ios, "口令 B（不显示，作秘密盐）: ")
			if err != nil {
				return err
			}
			res, err := svc.CreatePassphraseSecret(cmd.Context(), name, passA, passB)
			if err != nil {
				ios.WriteErrLine("创建双口令 secret 失败: %v", err)
				return fmt.Errorf("创建双口令 secret 失败: %w", err)
			}
			ios.WriteOutLine("已创建双口令 secret %q（origin=passphrase；重建=重输两口令+固定 high 档）", res.Name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&passphrase, "passphrase", false, "本地双口令派生（两口令交互输入，绝不上传）")
	return cmd
}

// secretReadPassphrase 用 x/term.ReadPassword 无回显读取口令（不从 argv/stdio 落
// bash 历史）。非终端（stdin 非 tty）且 ios.In 为空 → 中止（fail-closed，不静默空口令）。
func secretReadPassphrase(ios cli.IOStreams, prompt string) (string, error) {
	fmt.Fprintf(ios.ErrOut, "%s", prompt)
	var fd int
	f, ok := ios.In.(*os.File)
	if ok {
		fd = int(f.Fd())
	} else {
		ios.WriteErrLine("（非终端环境，口令输入不可用，已中止）")
		return "", errSecretAborted
	}
	// 非 tty（管道/重定向）：拒绝读取（口令不落历史由 tty 不回显保证；管道明文即泄漏）。
	if !term.IsTerminal(fd) {
		ios.WriteErrLine("（stdin 非终端，口令输入拒绝——避免明文管道泄漏，已中止）")
		return "", errSecretAborted
	}
	pw, err := term.ReadPassword(fd)
	fmt.Fprintln(ios.ErrOut)
	if err != nil {
		if errors.Is(err, io.EOF) {
			ios.WriteErrLine("（无输入，已中止）")
			return "", errSecretAborted
		}
		return "", fmt.Errorf("读取口令失败: %w", err)
	}
	pass := strings.TrimSpace(string(pw))
	if pass == "" {
		ios.WriteErrLine("（空口令已拒绝）")
		return "", errSecretAborted
	}
	return pass, nil
}

// ---- secret list ----

// newCmdSecretList 列出卷内全部 secret 名。
func newCmdSecretList(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出 secret 卷内全部 secret 名",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := newSecretDirectClient(cmd, factory, cfgSvc)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			names, err := svc.ListSecrets(cmd.Context())
			if err != nil {
				ios.WriteErrLine("获取 secret 列表失败: %v", err)
				return fmt.Errorf("获取 secret 列表失败: %w", err)
			}
			if len(names) == 0 {
				ios.WriteOutLine("（暂无 secret）")
				return nil
			}
			for _, n := range names {
				ios.WriteOutLine("%s", n)
			}
			return nil
		},
	}
}

// ---- secret export ----

// newCmdSecretExport 导出 secret 内容：默认口令加密写文件（本地派生密钥 + AES-GCM），
// 可选 --plain 明文写文件（用户自保管）。
func newCmdSecretExport(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	var outPath string
	var plain bool
	cmd := &cobra.Command{
		Use:   "export <name>",
		Short: "导出 secret（默认口令加密写文件；--plain 明文）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			if name == "" {
				ios.WriteErrLine(msgSecretNameEmpty)
				return errSecretAborted
			}
			svc, cerr := newSecretDirectClient(cmd, factory, cfgSvc)
			if cerr != nil {
				ios.WriteErrLine(errFmtInitClientPrint, cerr)
				return fmt.Errorf(errFmtInitClient, cerr)
			}
			val, verr := svc.ExportSecret(cmd.Context(), name)
			if verr != nil {
				ios.WriteErrLine("导出 secret %q 失败: %v", name, verr)
				return fmt.Errorf("导出 secret %q 失败: %w", name, verr)
			}
			return runSecretExport(ios, svc, name, val, outPath, plain)
		},
	}
	cmd.Flags().StringVar(&outPath, "out", "", "输出文件路径（默认 <name>.secret）")
	cmd.Flags().BoolVar(&plain, "plain", false, "明文导出（不自保管风险由用户承担）")
	return cmd
}

// runSecretExport 执行导出写文件（抽出 RunE 主路径降 gocognit；加密/明文分支 + 写盘）。
func runSecretExport(ios cli.IOStreams, svc *client.FileClient, name, val, outPath string, plain bool) error {
	content := []byte(val + "\n")
	if !plain {
		// 默认加密导出：本地口令派生密钥 + AES-GCM 信封（encrypting_crypto）。
		pass, perr := secretReadPassphrase(ios, "导出加密口令（不显示）: ")
		if perr != nil {
			return perr
		}
		enc, eerr := secretEncryptExport(pass, content)
		if eerr != nil {
			ios.WriteErrLine("导出加密失败: %v", eerr)
			return fmt.Errorf("导出加密失败: %w", eerr)
		}
		content = enc
	}
	if outPath == "" {
		outPath = name + ".secret"
	}
	if werr := os.WriteFile(outPath, content, 0o600); werr != nil {
		ios.WriteErrLine("写导出文件失败: %v", werr)
		return fmt.Errorf("写导出文件失败: %w", werr)
	}
	mode := "明文"
	if !plain {
		mode = "口令加密"
	}
	ios.WriteOutLine("已导出 secret %q（%s）到 %s", name, mode, outPath)
	return nil
}

// secretEncryptExport 用单一口令派生 32B 密钥并 AES-256-GCM 加密导出内容。
// 格式：nonce(12B) || ct（复用 accesskey.EncryptWithKey 信封）。解密：同一口令派生同
// 密钥 + accesskey.DecryptWithKey。
//
// **导出格式锁定 v1**（exportKDFSalt/N 常量，不回随 shardseal 档位）：导出/导入信封是
// 跨版本互操作面——shardseal 提档（passphrase 派生自动跟随 AlgoByVersion）时，导出
// 信封必须保持旧档位可解，否则历史备份无法导入。故此处**刻意不**复用
// shardseal.AlgoByVersion 单一事实源（该事实源用于**派生产物**，导出信封是另一用途，
// 参数一经发布即固化）。N=2^17 与双口令 high 档一致（导出强度不降）。
const (
	// msgSecretNameEmpty 是 create/import 共用错误文案（S1192 收敛）。
	msgSecretNameEmpty = "secret 名不能为空"

	exportKDFSalt = "sproxy-secret-export/v1"
	exportKDFN    = 1 << 17
	exportKDFR    = 8
	exportKDFP    = 1
)

// secretExportKey 由导出口令派生对称密钥（encrypt/decrypt 共用；S2053 收敛：
// KDF 盐是公开版本串非密钥，集中一处避免重复字面量）。
func secretExportKey(pass string) ([]byte, error) {
	return scrypt.Key([]byte(pass), []byte(exportKDFSalt), exportKDFN, exportKDFR, exportKDFP, 32)
}

func secretEncryptExport(pass string, plain []byte) ([]byte, error) {
	key, err := secretExportKey(pass)
	if err != nil {
		return nil, fmt.Errorf("导出口令派生失败: %w", err)
	}
	ct, err := accesskey.EncryptWithKey(key, plain)
	if err != nil {
		return nil, err
	}
	return ct, nil
}

// secretDecryptExport 解开 secretEncryptExport 产物（导入/恢复用）。
func secretDecryptExport(pass string, data []byte) ([]byte, error) {
	key, err := secretExportKey(pass)
	if err != nil {
		return nil, fmt.Errorf("导出口令派生失败: %w", err)
	}
	return accesskey.DecryptWithKey(key, data)
}

// ---- secret delete ----

// newCmdSecretDelete 删除 secret。
func newCmdSecretDelete(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "删除 secret（不存在报错）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			if name == "" {
				ios.WriteErrLine(msgSecretNameEmpty)
				return errSecretAborted
			}
			svc, err := newSecretDirectClient(cmd, factory, cfgSvc)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			if err := svc.DeleteSecret(cmd.Context(), name); err != nil {
				ios.WriteErrLine("删除 secret %q 失败: %v", name, err)
				return fmt.Errorf("删除 secret %q 失败: %w", name, err)
			}
			ios.WriteOutLine("已删除 secret %q", name)
			return nil
		},
	}
}
