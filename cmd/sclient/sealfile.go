// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// sealfile 本地文件加解密命令族：使用与 sproxy secretdata 卷**完全相同的加解密能力**
// （同一写内核 secretdata.NewFS().WriteFile），实现「本地文件加密 → 加密产物作为
// secretdata 卷加载到 sproxy → 播放器 Range 直接播放」全流程。
//
// 对齐机制：encrypt 直接复用 secretdata.NewFS 写路径（杜绝复制逻辑），产物格式自描述
// （meta 内嵌算法版本/分块策略/整文件 SHA-256），服务器加载后不区分本地产物/服务器产物。
// 视频关键帧分块与 sproxy 同款：media.RegisterKeyframeBackends 共享注册（go-mp4 精确 +
// ffprobe 通配兜底），缺省按文件名自动选型（同 blockletPolicyFor）。
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	"github.com/cocomhub/sproxy/pkg/media"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/units/sizex"
	"github.com/cocomhub/sproxy/pkg/volume/secretdata"
	"github.com/spf13/cobra"
)

// sealfile 配置段键名（S1192：注册与读取共享常量，防拼写漂移）。
const (
	sealfileKeyKey        = "sealfile.key"
	sealfileAlgorithmKey  = "sealfile.algorithm"
	sealfileBlockPolicy   = "sealfile.block_policy"
	sealfileOutputDirKey  = "sealfile.out_dir"
	sealfileOwnerKey      = "sealfile.owner"
	sealfileFlagKey       = "key"
	sealfileFlagOutputDir = "output-dir"
	sealfileFlagAlgo      = "algorithm"
	sealfileFlagBlock     = "block-policy"
	sealfileFlagVideoKF   = "video-keyframe"
	sealfileFlagFixed     = "fixed"
	sealfileFlagOwner     = "owner"
	sealfileOutputSubdir  = "sealed"
	// sealfileDefaultOwner 是加密产物布局前缀的默认 owner：对齐服务器键空间
	// （卷 FSPath 恒 owner 前缀，未认证请求 owner=anonymous）。
	sealfileDefaultOwner = "anonymous"
)

// errSealfileKeyMissing 是密钥缺失哨兵错误（三子命令共用文案）。
var errSealfileKeyMissing = errors.New("未提供密钥：--key 需 64 hex 字面量或 secrets 卷密钥文件路径（sclient secret export --plain 产物）")

// sealfileCfg 是 sealfile 配置段结构（config.yaml 顶层 `sealfile:` 段；CLI flag > 配置 > 默认）。
// 字段与 contextcfg.Sealfile 对齐（yaml/mapstructure 蛇形键）。
type sealfileCfg struct {
	Key         string         `mapstructure:"key" yaml:"key"`
	Algorithm   string         `mapstructure:"algorithm" yaml:"algorithm"`
	BlockPolicy map[string]any `mapstructure:"block_policy" yaml:"block_policy"`
	OutDir      string         `mapstructure:"out_dir" yaml:"out_dir"`
	Owner       string         `mapstructure:"owner" yaml:"owner"`
}

// NewCmdSealfile 创建 sealfile 命令族（本地加解密，不连服务端）。
func NewCmdSealfile(ios cli.IOStreams, cfgSvc SealfileConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sealfile",
		Short: "本地文件加解密（与 sproxy secretdata 卷同内核；加密产物可作卷加载）",
		Long: `sealfile 使用与 sproxy secretdata 卷完全相同的加解密能力在本地加密文件。
		加密产物格式自描述，可直接作为 secretdata 卷根加载到 sproxy（type: secretdata +
		extra.secret_url 指向同一密钥），播放器 Range 直接播放。
		视频文件缺省按扩展名自动关键帧分块（MP4 走 go-mp4，其它走 ffprobe 兜底，与
		sproxy 完全同款）；--video-keyframe/--fixed 可显式覆盖。`,
	}
	cmd.AddCommand(newCmdSealfileEncrypt(ios, cfgSvc))
	cmd.AddCommand(newCmdSealfileDecrypt(ios, cfgSvc))
	cmd.AddCommand(newCmdSealfileInfo(ios, cfgSvc))
	return cmd
}

// sealfileFlags 汇总 sealfile 子命令的 flag 选项（encrypt/decrypt/info 共用）。
type sealfileFlags struct {
	key         string
	algorithm   string
	blockPolicy string
	videoKey    bool
	fixed       bool
	outputDir   string
	owner       string
}

// addSealfileFlags 注册共用 flag 并绑定到 sealfileFlags。
func addSealfileFlags(cmd *cobra.Command, f *sealfileFlags) {
	cmd.Flags().StringVar(&f.key, sealfileFlagKey, "", "加密密钥：64 hex 字面量或密钥文件路径（secrets 卷密钥文件，64 hex 文本）")
	cmd.Flags().StringVar(&f.algorithm, sealfileFlagAlgo, "", "加密算法（默认 "+shardseal.AlgorithmName+"）")
	cmd.Flags().StringVar(&f.blockPolicy, sealfileFlagBlock, "", `分块策略 JSON（如 {"mode":"random","min":"1MiB","max":"200MiB"}；缺省默认 random 1MiB-200MiB）`)
	cmd.Flags().BoolVar(&f.videoKey, sealfileFlagVideoKF, false, "强制视频关键帧分块（即使非视频扩展名）")
	cmd.Flags().BoolVar(&f.fixed, sealfileFlagFixed, false, "强制定长 blocklet 分块（关闭视频关键帧自动选型）")
	cmd.Flags().StringVar(&f.outputDir, sealfileFlagOutputDir, "", "输出目录（encrypt 产物目录 / decrypt 还原目录；缺省默认值）")
	cmd.Flags().StringVar(&f.owner, sealfileFlagOwner, "", "逻辑 owner（加密产物按 <owner>/user/ 布局，对齐服务器键空间；默认 "+sealfileDefaultOwner+"）")
}

// resolveSealfileKey 解析密钥：优先 flag，回落配置。两种形态：
//   - **文件路径**：读文件内容原样作为 secret 字节（trim 尾部换行）——与 sproxy
//     secrets 卷 datakey 口径完全一致（secret_url 解析即文件内容字节，不要求 hex）；
//   - **64 hex 字面量**：hex 解码为 32 字节。
//
// 错误输入 fail-closed。
func resolveSealfileKey(cmd *cobra.Command, f *sealfileFlags, cfgSvc SealfileConfigProvider) ([]byte, error) {
	keyStr := f.key
	if keyStr == "" && !cmd.Flags().Changed(sealfileFlagKey) {
		keyStr = sealfileKeyFromConfig(cfgSvc)
	}
	if keyStr == "" {
		return nil, errSealfileKeyMissing
	}
	return sealfileParseKey(keyStr)
}

// sealfileKeyFromConfig 读配置 sealfile.key（回落值；无配置/失败返回 ""）。
func sealfileKeyFromConfig(cfgSvc SealfileConfigProvider) string {
	var cfg sealfileCfg
	if cfgSvc != nil {
		if err := cfgSvc.LoadSealfileConfig(&cfg); err == nil {
			return cfg.Key
		}
	}
	return ""
}

// sealfileParseKey 解析密钥串：文件路径（内容原样字节作 secret，trim 尾部换行）
// 优先，否则 64 hex 字面量解码。非法输入 fail-closed。
func sealfileParseKey(keyStr string) ([]byte, error) {
	if st, err := os.Stat(keyStr); err == nil && !st.IsDir() {
		data, rerr := os.ReadFile(keyStr)
		if rerr != nil {
			return nil, fmt.Errorf("读取密钥文件失败: %w", rerr)
		}
		secret := bytes.TrimRight(data, "\r\n")
		if len(secret) == 0 {
			return nil, fmt.Errorf("密钥文件为空: %s", keyStr)
		}
		return secret, nil
	}
	// 字面量：64 hex（32 字节）。
	lit := strings.TrimSpace(keyStr)
	if len(lit) != 64 {
		return nil, fmt.Errorf("密钥字面量须为 64 hex（32 字节），实际 %d 字符；或传 secrets 卷密钥文件路径", len(lit))
	}
	raw, err := hex.DecodeString(strings.ToLower(lit))
	if err != nil {
		return nil, fmt.Errorf("密钥字面量非法（须为 64 小写 hex）: %w", err)
	}
	return raw, nil
}

// resolveSealfileAlgorithm 解析算法名：flag > 配置 > 默认（NewFS 对未知算法 fail-fast）。
func resolveSealfileAlgorithm(cmd *cobra.Command, f *sealfileFlags, cfgSvc SealfileConfigProvider) string {
	if f.algorithm != "" {
		return f.algorithm
	}
	var cfg sealfileCfg
	if cfgSvc != nil {
		if err := cfgSvc.LoadSealfileConfig(&cfg); err == nil && cfg.Algorithm != "" {
			return cfg.Algorithm
		}
	}
	return ""
}

// resolveSealfileBlockPolicy 解析分块策略：flag JSON > 配置 block_policy > 默认。
// min/max 接受人类可读字节（"1MiB"，经 sizex.ParseSize）。
// blocklet_mode 由 --video-keyframe/--fixed 显式覆盖（优先级最高）。
func resolveSealfileBlockPolicy(cmd *cobra.Command, f *sealfileFlags, cfgSvc SealfileConfigProvider) (shardseal.BlockPolicy, error) {
	bp := shardseal.DefaultBlockPolicy()
	bp.BlockletMode = "" // 缺省 = 自动按扩展名选型（同 sproxy）
	var m map[string]any
	if f.blockPolicy != "" {
		if err := jsonUnmarshal(f.blockPolicy, &m); err != nil {
			return bp, fmt.Errorf("block-policy JSON 解析失败: %w", err)
		}
	} else {
		var cfg sealfileCfg
		if cfgSvc != nil {
			if err := cfgSvc.LoadSealfileConfig(&cfg); err == nil && len(cfg.BlockPolicy) > 0 {
				m = cfg.BlockPolicy
			}
		}
	}
	if err := applySealfileBlockPolicyMap(&bp, m); err != nil {
		return bp, err
	}
	// 显式 blocklet 覆盖优先级最高（--video-keyframe > --fixed；两者互斥）。
	switch {
	case f.videoKey && f.fixed:
		return bp, errors.New("--video-keyframe 与 --fixed 互斥")
	case f.videoKey:
		bp.BlockletMode = shardseal.BlockletModeVideoKeyframe
	case f.fixed:
		bp.BlockletMode = shardseal.BlockletModeFixed
	}
	return bp, nil
}

// applySealfileBlockPolicyMap 把分块策略 map（mode/min/max；来自 flag JSON 或配置
// block_policy 段）应用到策略。min/max 支持人类可读字节（"1MiB"）；未知键忽略。
func applySealfileBlockPolicyMap(bp *shardseal.BlockPolicy, m map[string]any) error {
	if v, ok := m["mode"].(string); ok && v != "" {
		bp.Mode = v
	}
	if v, ok := m["min"].(string); ok && v != "" {
		n, err := sizexParse(v)
		if err != nil {
			return fmt.Errorf("block-policy.min 解析失败: %w", err)
		}
		bp.Min = n
	}
	if v, ok := m["max"].(string); ok && v != "" {
		n, err := sizexParse(v)
		if err != nil {
			return fmt.Errorf("block-policy.max 解析失败: %w", err)
		}
		bp.Max = n
	}
	return nil
}

// resolveSealfileOutputDir 解析输出目录：flag > 配置 out_dir > 默认（当前目录/sealed）。
// encrypt 产物与 decrypt 还原共用：encrypt 输出到 <out>/<原始名>（卷根布局）；
// decrypt 还原到 <out>/<文件名>。
func resolveSealfileOutputDir(cmd *cobra.Command, f *sealfileFlags, cfgSvc SealfileConfigProvider) (string, error) {
	out := f.outputDir
	if out == "" {
		var cfg sealfileCfg
		if cfgSvc != nil {
			if err := cfgSvc.LoadSealfileConfig(&cfg); err == nil && cfg.OutDir != "" {
				out = cfg.OutDir
			}
		}
	}
	if out == "" {
		out = filepath.Join(".", sealfileOutputSubdir)
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return "", fmt.Errorf("解析输出目录失败: %w", err)
	}
	return abs, nil
}

// resolveSealfileOwner 解析逻辑 owner：flag > 配置 sealfile.owner > 默认 "anonymous"。
// owner 决定加密产物布局前缀 <owner>/user/——与服务器键空间一致（卷 Location.FSPath
// 恒 owner 前缀），加密产物作为卷加载后能被对应 owner 的请求直接命中。fail-closed：
// 空/点段/含路径分隔符（防 path.Join 穿越污染 rel）。
func resolveSealfileOwner(cmd *cobra.Command, f *sealfileFlags, cfgSvc SealfileConfigProvider) (string, error) {
	owner := f.owner
	if owner == "" {
		var cfg sealfileCfg
		if cfgSvc != nil {
			if err := cfgSvc.LoadSealfileConfig(&cfg); err == nil && cfg.Owner != "" {
				owner = cfg.Owner
			}
		}
	}
	if owner == "" {
		return sealfileDefaultOwner, nil
	}
	owner = strings.TrimSpace(owner)
	if owner == "" || owner == "." || owner == ".." ||
		strings.ContainsAny(owner, `/\`) || strings.Contains(owner, "\x00") {
		return "", fmt.Errorf("非法 owner %q：须为非空、不含路径分隔符的段名", owner)
	}
	return owner, nil
}

// sealfileRel 构造卷内键：<owner>/user/<短名>（服务器键空间；卷 FSPath 恒 owner 前缀，
// user 桶 + 相对路径）。encrypt/decrypt/info 共用，保证三端布局一致。
func sealfileRel(owner, shortName string) string {
	return path.Join(owner, "user", shortName)
}

// resolveSealfileReadName 解析卷内读取名（decrypt 用；info 枚举走同一布局）：位置参数
// 2 显式指定短名，否则枚举 <owner>/user/ 取唯一文件。返回完整 rel 与短名（decrypt
// 还原目标用短名，不保留 owner/user 层级）。
func resolveSealfileReadName(cmd *cobra.Command, fs *secretdata.SecretdataFS, ctx context.Context, owner string, args []string) (rel, shortName string, err error) {
	if len(args) >= 2 {
		shortName = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(args[1])), "/")
		if shortName == "" || shortName == "." {
			return "", "", fmt.Errorf("卷内文件名非法: %s", args[1])
		}
		return sealfileRel(owner, shortName), shortName, nil
	}
	entries, lerr := fs.ListDir(ctx, path.Join(owner, "user"))
	if lerr != nil {
		return "", "", fmt.Errorf("列出卷内文件失败: %w", lerr)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir {
			files = append(files, e.Name)
		}
	}
	if len(files) != 1 {
		return "", "", fmt.Errorf("卷内 %s/user 含 %d 个文件，请用第二参数或 -o 指定要还原的文件", owner, len(files))
	}
	return sealfileRel(owner, files[0]), files[0], nil
}

// newSealfileFS 构造 secretdata FS（写/读共用）：底层 NewLocalFS(输出目录) + NewFS。
// 构造即触发 loadIndex（空目录 = 空卷）。
func newSealfileFS(outDir string, secret []byte, algo string, bp shardseal.BlockPolicy) (*secretdata.SecretdataFS, error) {
	inner := syncpkg.NewLocalFS(outDir, nil)
	fs, err := secretdata.NewFS(inner, secretdata.Options{
		Secret:    secret,
		Algorithm: algo,
		Block:     bp,
		TempDir:   filepath.Join(outDir, ".sealfile-tmp"),
	})
	if err != nil {
		return nil, err
	}
	return fs, nil
}

// newCmdSealfileEncrypt 加密：读源文件 → secretdata.WriteFile（与 sproxy 上传同一条
// 写路径）。video-keyframe 自动选型由 SecretdataFS.blockletPolicyFor 内部完成。
func newCmdSealfileEncrypt(ios cli.IOStreams, cfgSvc SealfileConfigProvider) *cobra.Command {
	var f sealfileFlags
	cmd := &cobra.Command{
		Use:   "encrypt <file>",
		Short: "加密文件到输出目录（secretdata 卷布局；视频自动关键帧分块）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 共享关键帧注册（Once 幂等；go-mp4 精确 + ffprobe 通配兜底，同 sproxy）。
			media.RegisterKeyframeBackends()
			return runSealfileEncrypt(ios, cmd, &f, cfgSvc, args[0])
		},
	}
	addSealfileFlags(cmd, &f)
	return cmd
}

// runSealfileEncrypt 执行加密：resolve 参数 → newSealfileFS → WriteFile → 打印结果。
func runSealfileEncrypt(ios cli.IOStreams, cmd *cobra.Command, f *sealfileFlags, cfgSvc SealfileConfigProvider, src string) error {
	st, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("读取源文件失败: %w", err)
	}
	if st.IsDir() {
		return fmt.Errorf("源须为文件（目录加密暂不支持）: %s", src)
	}
	secret, kerr := resolveSealfileKey(cmd, f, cfgSvc)
	if kerr != nil {
		ios.WriteErrLine("%v", kerr)
		return kerr
	}
	algo := resolveSealfileAlgorithm(cmd, f, cfgSvc)
	bp, berr := resolveSealfileBlockPolicy(cmd, f, cfgSvc)
	if berr != nil {
		ios.WriteErrLine("%v", berr)
		return berr
	}
	owner, oerr := resolveSealfileOwner(cmd, f, cfgSvc)
	if oerr != nil {
		ios.WriteErrLine("%v", oerr)
		return oerr
	}
	outDir, oerr := resolveSealfileOutputDir(cmd, f, cfgSvc)
	if oerr != nil {
		return oerr
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return fmt.Errorf("创建输出目录失败: %w", err)
	}

	fs, nerr := newSealfileFS(outDir, secret, algo, bp)
	if nerr != nil {
		return fmt.Errorf("初始化加密卷失败: %w", nerr)
	}
	rel := sealfileRel(owner, filepath.Base(src))
	rc, oerr := os.Open(src)
	if oerr != nil {
		return fmt.Errorf("打开源文件失败: %w", oerr)
	}
	defer rc.Close()
	werr := fs.WriteFile(cmd.Context(), syncpkg.LogicalKey{}, rel, rc, st.Size(), st.ModTime().UnixNano())
	if werr != nil {
		return fmt.Errorf("加密写入失败: %w", werr)
	}
	// 输出块模式（关键帧分块是否生效的可观测性）。
	mode, merr := fs.BlockletMode(cmd.Context(), rel)
	if merr != nil {
		mode = "(未知)"
	}
	ios.WriteOutLine("已加密 %s → %s（blocklet 模式: %s）", src, filepath.Join(outDir, filepath.FromSlash(rel)), mode)
	ios.WriteOutLine("加载到 sproxy：volume type: secretdata，extra.secret_url 指向本密钥；播放器直接 Range 播放")
	return nil
}

// newCmdSealfileDecrypt 还原：OpenRead 整文件解密（含 meta.Original.SHA256 全量校验，
// fail-closed），写还原文件。**仅注册 -o/--out（还原文件路径）**——decrypt 的输入是
// 卷目录（位置参数），不复用加密用的 --output-dir（避免混淆）。
func newCmdSealfileDecrypt(ios cli.IOStreams, cfgSvc SealfileConfigProvider) *cobra.Command {
	var f sealfileFlags
	// decrypt 输入是卷目录（位置参数），不复用 encrypt 的 --output-dir：仅注册
	// key/algorithm/block-policy/video-keyframe/fixed（addSealfileFlags 含 output-dir，
	// 故这里手动注册除 output-dir 外的子集，避免 --out 与 --output-dir 双 flag 混淆）。
	cmd := &cobra.Command{
		Use:   "decrypt <输出目录> [卷内文件名] --key <同上> [-o 还原文件]",
		Short: "整文件解密还原（含 SHA-256 完整性校验）",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			media.RegisterKeyframeBackends()
			return runSealfileDecrypt(ios, cmd, &f, cfgSvc, args)
		},
	}
	cmd.Flags().StringVar(&f.key, sealfileFlagKey, "", "加密密钥：64 hex 字面量或密钥文件路径（secrets 卷密钥文件）")
	cmd.Flags().StringVar(&f.algorithm, sealfileFlagAlgo, "", "加密算法（默认 "+shardseal.AlgorithmName+"）")
	cmd.Flags().StringVar(&f.blockPolicy, sealfileFlagBlock, "", `分块策略 JSON（如 {"mode":"random","min":"1MiB","max":"200MiB"}）`)
	cmd.Flags().BoolVar(&f.videoKey, sealfileFlagVideoKF, false, "强制视频关键帧分块")
	cmd.Flags().BoolVar(&f.fixed, sealfileFlagFixed, false, "强制定长 blocklet 分块")
	cmd.Flags().StringVar(&f.owner, sealfileFlagOwner, "", "逻辑 owner（卷内读取按 <owner>/user/ 定位；默认 "+sealfileDefaultOwner+"）")
	// 仅 decrypt 额外注册 -o/--output（还原文件路径；与 root 持久 -o/--output 同名，
	// 本地优先——archive/backup 既有同模式）。名字必须用 "output"（与 root 同名使
	// cobra 合并持久 flag 时跳过继承项，避免 pflag 同 shorthand 冲突 panic）。
	cmd.Flags().StringVarP(&f.outputDir, "output", "o", "", "还原文件路径（缺省：卷内唯一文件名）")
	return cmd
}

// runSealfileDecrypt 执行解密：resolve 参数 → OpenRead 整文件还原（含 SHA-256 校验）。
func runSealfileDecrypt(ios cli.IOStreams, cmd *cobra.Command, f *sealfileFlags, cfgSvc SealfileConfigProvider, args []string) error {
	outDir, aerr := filepath.Abs(args[0])
	if aerr != nil {
		return fmt.Errorf("解析输出目录失败: %w", aerr)
	}
	ctx := cmd.Context()
	// 解密共用 key/algorithm/block-policy 解析（flag > 配置 > 默认）。
	secret, kerr := resolveSealfileKey(cmd, f, cfgSvc)
	if kerr != nil {
		ios.WriteErrLine("%v", kerr)
		return kerr
	}
	algo := resolveSealfileAlgorithm(cmd, f, cfgSvc)
	bp, berr := resolveSealfileBlockPolicy(cmd, f, cfgSvc)
	if berr != nil {
		ios.WriteErrLine("%v", berr)
		return berr
	}
	owner, oerr := resolveSealfileOwner(cmd, f, cfgSvc)
	if oerr != nil {
		ios.WriteErrLine("%v", oerr)
		return oerr
	}
	fs, nerr := newSealfileFS(outDir, secret, algo, bp)
	if nerr != nil {
		return fmt.Errorf("打开加密卷失败: %w", nerr)
	}
	// 卷内读取名（**短名**，不含 owner/user 前缀；完整 rel 由 sealfileRel 统一
	// 拼接）：位置参数 2（显式指定）> 卷内唯一文件（缺省，枚举 <owner>/user/）。
	rel, shortName, rerr := resolveSealfileReadName(cmd, fs, ctx, owner, args)
	if rerr != nil {
		return rerr
	}
	// 还原目标：-o 指定；缺省 <卷目录的父目录>/<短名>（不保留 owner/user 层级，
	// 还原即原始文件名；避免输出进卷目录干扰加载——卷目录是加密容器布局）。
	dst := f.outputDir
	if dst == "" {
		dst = filepath.Join(filepath.Dir(outDir), filepath.FromSlash(shortName))
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("创建还原目录失败: %w", err)
	}
	rc, rerr := fs.OpenRead(ctx, rel)
	if rerr != nil {
		return fmt.Errorf("解密读取失败: %w", rerr)
	}
	defer rc.Close()
	out, werr := os.Create(filepath.Clean(dst))
	if werr != nil {
		return fmt.Errorf("创建还原文件失败: %w", werr)
	}
	defer out.Close()
	if _, cerr := io.Copy(out, rc); cerr != nil {
		return fmt.Errorf("写还原文件失败: %w", cerr)
	}
	// SHA-256 校验在 OpenRead 内部（shardseal.DecryptFile 对 meta.Original.SHA256
	// 全量比对，不一致 fail-closed 并删除残file）；此处报告校验结果。
	fm, ferr := fs.FileMeta(ctx, rel)
	if ferr != nil {
		return fmt.Errorf("读取校验元信息失败: %w", ferr)
	}
	ios.WriteOutLine("已还原 %s → %s（sha256 校验通过: %s）", outDir, dst, fm.TotalSHA256)
	return nil
}

// newCmdSealfileInfo 查看加密卷/文件信息：文件列表 + FileMeta（大小/sha256/分块数）+
// blocklet 模式（断言关键帧分块生效）。
func newCmdSealfileInfo(ios cli.IOStreams, cfgSvc SealfileConfigProvider) *cobra.Command {
	var f sealfileFlags
	cmd := &cobra.Command{
		Use:   "info <输出目录> [文件名] --key <同上>",
		Short: "查看加密卷信息（blocklet 模式/分块/sha256）",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			media.RegisterKeyframeBackends()
			return runSealfileInfo(ios, cmd, &f, cfgSvc, args)
		},
	}
	addSealfileFlags(cmd, &f)
	return cmd
}

// runSealfileInfo 执行查看：resolve 参数 → 枚举/单文件打印（FileMeta + blocklet 模式）。
func runSealfileInfo(ios cli.IOStreams, cmd *cobra.Command, f *sealfileFlags, cfgSvc SealfileConfigProvider, args []string) error {
	outDir, aerr := filepath.Abs(args[0])
	if aerr != nil {
		return fmt.Errorf("解析输出目录失败: %w", aerr)
	}
	ctx := cmd.Context()
	secret, kerr := resolveSealfileKey(cmd, f, cfgSvc)
	if kerr != nil {
		ios.WriteErrLine("%v", kerr)
		return kerr
	}
	algo := resolveSealfileAlgorithm(cmd, f, cfgSvc)
	bp, berr := resolveSealfileBlockPolicy(cmd, f, cfgSvc)
	if berr != nil {
		ios.WriteErrLine("%v", berr)
		return berr
	}
	owner, oerr := resolveSealfileOwner(cmd, f, cfgSvc)
	if oerr != nil {
		ios.WriteErrLine("%v", oerr)
		return oerr
	}
	fs, nerr := newSealfileFS(outDir, secret, algo, bp)
	if nerr != nil {
		return fmt.Errorf("打开加密卷失败: %w", nerr)
	}
	// 位置参数 2（短名）显式指定单文件；否则枚举 <owner>/user/ 全部文件。
	if len(args) >= 2 {
		short := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(args[1])), "/")
		return printSealfileInfo(ios, fs, ctx, sealfileRel(owner, short))
	}
	entries, lerr := fs.ListDir(ctx, path.Join(owner, "user"))
	if lerr != nil {
		return fmt.Errorf("列出卷内文件失败: %w", lerr)
	}
	for _, e := range entries {
		if e.IsDir {
			continue
		}
		if err := printSealfileInfo(ios, fs, ctx, sealfileRel(owner, e.Name)); err != nil {
			return err
		}
	}
	return nil
}

// printSealfileInfo 打印单文件信息（FileMeta + BlockletMode）。
func printSealfileInfo(ios cli.IOStreams, fs *secretdata.SecretdataFS, ctx context.Context, rel string) error {
	fm, err := fs.FileMeta(ctx, rel)
	if err != nil {
		return fmt.Errorf("读取文件元信息失败: %w", err)
	}
	mode, merr := fs.BlockletMode(ctx, rel)
	if merr != nil {
		mode = "(未知)"
	}
	ios.WriteOutLine("文件: %s", rel)
	ios.WriteOutLine("  大小: %d 字节", fm.Size)
	ios.WriteOutLine("  sha256: %s", fm.TotalSHA256)
	ios.WriteOutLine("  分块数: %d", len(fm.Chunks))
	ios.WriteOutLine("  blocklet 模式: %s", mode)
	if mode == shardseal.BlockletModeVideoKeyframe {
		ios.WriteOutLine("  （视频关键帧分块已生效——播放器 seek 直达关键帧）")
	}
	return nil
}

// ---- 配置读取辅助（sealfile 配置段） ----

// jsonUnmarshal 反序列化 JSON 字符串到 out（block-policy flag 用）。
func jsonUnmarshal(s string, out any) error {
	return json.Unmarshal([]byte(s), out)
}

// sizexParse 解析人类可读字节大小（"1MiB"→1048576；纯数字=字节）。
func sizexParse(s string) (int64, error) {
	return sizex.ParseSize(s)
}
