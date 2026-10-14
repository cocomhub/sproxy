// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	"github.com/spf13/cobra"
)

// sealfileCfgStub 实现 SealfileConfigProvider（测 resolveSealfile* 的配置回落）。
type sealfileCfgStub struct {
	cfg sealfileCfg
	err error
}

func (s *sealfileCfgStub) LoadSealfileConfig(out any) error {
	if s.err != nil {
		return s.err
	}
	*out.(*sealfileCfg) = s.cfg
	return nil
}

// newSealfileTestCmd 构造带 sealfile 共用 flag 的测试命令（resolve* 用 Changed 判定）。
// flag 值统一经 Set 注入（pflag 标准路径；直接改 f 字段会被 StringVar 的默认值覆盖）。
func newSealfileTestCmd(f *sealfileFlags, set map[string]string) *cobra.Command {
	cmd := &cobra.Command{Use: "sealfile-test"}
	addSealfileFlags(cmd, f)
	for k, v := range set {
		if err := cmd.Flags().Set(k, v); err != nil {
			panic("set flag " + k + ": " + err.Error())
		}
	}
	return cmd
}

func TestResolveSealfileKey_Literal64Hex(t *testing.T) {
	t.Parallel()
	var f sealfileFlags
	cmd := newSealfileTestCmd(&f, map[string]string{
		sealfileFlagKey: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	got, err := resolveSealfileKey(cmd, &f, nil)
	if err != nil {
		t.Fatalf("resolveSealfileKey: %v", err)
	}
	want, _ := hex.DecodeString("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if string(got) != string(want) {
		t.Fatalf("字面量解码错误: got %x", got)
	}
}

func TestResolveSealfileKey_KeyFileRawBytes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// 密钥文件内容原样作 secret 字节（trim 尾部换行）；不要求 hex 形态。
	raw := []byte("0123456789abcdef0123456789abcdef\n")
	keyFile := filepath.Join(dir, "datakey")
	if err := os.WriteFile(keyFile, raw, 0o600); err != nil {
		t.Fatalf("写密钥文件: %v", err)
	}
	var f sealfileFlags
	cmd := newSealfileTestCmd(&f, map[string]string{sealfileFlagKey: keyFile})
	got, err := resolveSealfileKey(cmd, &f, nil)
	if err != nil {
		t.Fatalf("resolveSealfileKey: %v", err)
	}
	if len(got) != 32 {
		t.Fatalf("密钥文件字节应 32（原样，非 hex 解码）, got %d", len(got))
	}
	if string(got) != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("密钥字节错误: %q", got)
	}
}

func TestResolveSealfileKey_Missing(t *testing.T) {
	t.Parallel()
	var f sealfileFlags
	cmd := newSealfileTestCmd(&f, nil)
	if _, err := resolveSealfileKey(cmd, &f, nil); !errors.Is(err, errSealfileKeyMissing) {
		t.Fatalf("空密钥应报 errSealfileKeyMissing, got %v", err)
	}
}

func TestResolveSealfileKey_ConfigFallback(t *testing.T) {
	t.Parallel()
	cfgSvc := &sealfileCfgStub{cfg: sealfileCfg{Key: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}
	var f sealfileFlags
	cmd := newSealfileTestCmd(&f, nil)
	got, err := resolveSealfileKey(cmd, &f, cfgSvc)
	if err != nil {
		t.Fatalf("resolveSealfileKey: %v", err)
	}
	if len(got) != 32 {
		t.Fatalf("配置回落密钥应 32B, got %d", len(got))
	}
}

func TestResolveSealfileKey_InvalidLiteralLength(t *testing.T) {
	t.Parallel()
	var f sealfileFlags
	cmd := newSealfileTestCmd(&f, map[string]string{sealfileFlagKey: "abcd"})
	if _, err := resolveSealfileKey(cmd, &f, nil); err == nil {
		t.Fatal("非法字面量长度应报错")
	}
}

func TestResolveSealfileKey_ConfigErrorPropagates(t *testing.T) {
	t.Parallel()
	cfgSvc := &sealfileCfgStub{err: errors.New("config load failed")}
	var f sealfileFlags
	cmd := newSealfileTestCmd(&f, nil)
	if _, err := resolveSealfileKey(cmd, &f, cfgSvc); !errors.Is(err, errSealfileKeyMissing) {
		t.Fatalf("配置失败应回落 errSealfileKeyMissing, got %v", err)
	}
}

func TestResolveSealfileAlgorithm(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		flag   string
		cfgAlg string
		want   string
	}{
		{name: "flag 优先", flag: "alg-x", cfgAlg: "alg-y", want: "alg-x"},
		{name: "配置回落", cfgAlg: "alg-y", want: "alg-y"},
		{name: "默认空", want: ""},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var f sealfileFlags
			set := map[string]string{}
			if tt.flag != "" {
				set[sealfileFlagAlgo] = tt.flag
			}
			cmd := newSealfileTestCmd(&f, set)
			cfgSvc := &sealfileCfgStub{cfg: sealfileCfg{Algorithm: tt.cfgAlg}}
			if got := resolveSealfileAlgorithm(cmd, &f, cfgSvc); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveSealfileBlockPolicy_Defaults(t *testing.T) {
	t.Parallel()
	var f sealfileFlags
	cmd := newSealfileTestCmd(&f, nil)
	bp, err := resolveSealfileBlockPolicy(cmd, &f, nil)
	if err != nil {
		t.Fatalf("resolveSealfileBlockPolicy: %v", err)
	}
	d := shardseal.DefaultBlockPolicy()
	if bp.Mode != d.Mode || bp.Min != d.Min || bp.Max != d.Max {
		t.Fatalf("默认策略应等于 DefaultBlockPolicy, got %+v", bp)
	}
	if bp.BlockletMode != "" {
		t.Fatalf("默认 BlockletMode 应为空（自动按扩展名选型）, got %q", bp.BlockletMode)
	}
}

func TestResolveSealfileBlockPolicy_JSON(t *testing.T) {
	t.Parallel()
	var f sealfileFlags
	cmd := newSealfileTestCmd(&f, map[string]string{
		sealfileFlagBlock: `{"mode":"fixed","min":"1MiB","max":"64MiB"}`,
	})
	bp, err := resolveSealfileBlockPolicy(cmd, &f, nil)
	if err != nil {
		t.Fatalf("resolveSealfileBlockPolicy: %v", err)
	}
	if bp.Mode != "fixed" {
		t.Fatalf("mode=%q, want fixed", bp.Mode)
	}
	if bp.Min != 1<<20 {
		t.Fatalf("min=%d, want %d", bp.Min, 1<<20)
	}
	if bp.Max != 64<<20 {
		t.Fatalf("max=%d, want %d", bp.Max, 64<<20)
	}
}

func TestResolveSealfileBlockPolicy_InvalidJSON(t *testing.T) {
	t.Parallel()
	var f sealfileFlags
	cmd := newSealfileTestCmd(&f, map[string]string{sealfileFlagBlock: `{not json`})
	if _, err := resolveSealfileBlockPolicy(cmd, &f, nil); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

func TestResolveSealfileBlockPolicy_InvalidMin(t *testing.T) {
	t.Parallel()
	var f sealfileFlags
	cmd := newSealfileTestCmd(&f, map[string]string{sealfileFlagBlock: `{"min":"not-a-size"}`})
	if _, err := resolveSealfileBlockPolicy(cmd, &f, nil); err == nil {
		t.Fatal("非法 min 应报错")
	}
}

func TestResolveSealfileBlockPolicy_ConfigFallback(t *testing.T) {
	t.Parallel()
	cfgSvc := &sealfileCfgStub{cfg: sealfileCfg{BlockPolicy: map[string]any{
		"mode": "random", "min": "2MiB", "max": "32MiB",
	}}}
	var f sealfileFlags
	cmd := newSealfileTestCmd(&f, nil)
	bp, err := resolveSealfileBlockPolicy(cmd, &f, cfgSvc)
	if err != nil {
		t.Fatalf("resolveSealfileBlockPolicy: %v", err)
	}
	if bp.Min != 2<<20 || bp.Max != 32<<20 {
		t.Fatalf("配置回落策略错误: %+v", bp)
	}
}

func TestResolveSealfileBlockPolicy_MutualExclusion(t *testing.T) {
	t.Parallel()
	var f sealfileFlags
	cmd := newSealfileTestCmd(&f, map[string]string{
		sealfileFlagVideoKF: "true",
		sealfileFlagFixed:   "true",
	})
	if _, err := resolveSealfileBlockPolicy(cmd, &f, nil); err == nil {
		t.Fatal("--video-keyframe 与 --fixed 互斥应报错")
	}
}

func TestResolveSealfileBlockPolicy_ExplicitModes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		key  string
		want string
	}{
		{name: "video-keyframe", key: sealfileFlagVideoKF, want: shardseal.BlockletModeVideoKeyframe},
		{name: "fixed", key: sealfileFlagFixed, want: shardseal.BlockletModeFixed},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var f sealfileFlags
			cmd := newSealfileTestCmd(&f, map[string]string{tt.key: "true"})
			bp, err := resolveSealfileBlockPolicy(cmd, &f, nil)
			if err != nil {
				t.Fatalf("resolveSealfileBlockPolicy: %v", err)
			}
			if bp.BlockletMode != tt.want {
				t.Fatalf("BlockletMode=%q, want %q", bp.BlockletMode, tt.want)
			}
		})
	}
}

func TestResolveSealfileOwner(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		flag    string
		cfgOwn  string
		want    string
		wantErr bool
	}{
		{name: "默认 anonymous", want: "anonymous"},
		{name: "flag 优先", flag: "bob", cfgOwn: "alice", want: "bob"},
		{name: "配置回落", cfgOwn: "alice", want: "alice"},
		{name: "含路径分隔符", flag: "a/b", wantErr: true},
		{name: "反斜杠", flag: `a\b`, wantErr: true},
		{name: "点段", flag: "..", wantErr: true},
		{name: "纯空白", flag: "  ", wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var f sealfileFlags
			set := map[string]string{}
			if tt.flag != "" {
				set[sealfileFlagOwner] = tt.flag
			}
			cmd := newSealfileTestCmd(&f, set)
			cfgSvc := &sealfileCfgStub{cfg: sealfileCfg{Owner: tt.cfgOwn}}
			got, err := resolveSealfileOwner(cmd, &f, cfgSvc)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("非法 owner %q 应报错, got %q", tt.flag, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveSealfileOwner: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSealfileRel(t *testing.T) {
	t.Parallel()
	tests := []struct{ owner, name, want string }{
		{owner: "test", name: "movie.mp4", want: "test/user/movie.mp4"},
		{owner: "anonymous", name: "a/b.txt", want: "anonymous/user/a/b.txt"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.owner+"/"+tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sealfileRel(tt.owner, tt.name); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveSealfileReadName 覆盖 decrypt 的短名解析（显式指定 + 缺省唯一枚举）。
// 用真实 SecretdataFS 构造（t.TempDir 隔离，可并行）。
func TestResolveSealfileReadName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	secret := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	fs, err := newSealfileFS(dir, secret, "", shardseal.DefaultBlockPolicy())
	if err != nil {
		t.Fatalf("newSealfileFS: %v", err)
	}
	content := strings.NewReader("read-name-test")
	if werr := fs.WriteFile(context.Background(), sealfileRel("test", "doc.bin"), content, 14, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	cmd := &cobra.Command{Use: "decrypt"}

	// 显式短名：解析为 <owner>/user/<短名>。
	rel, short, err := resolveSealfileReadName(cmd, fs, context.Background(), "test", []string{dir, "doc.bin"})
	if err != nil {
		t.Fatalf("resolveSealfileReadName 显式: %v", err)
	}
	if rel != "test/user/doc.bin" || short != "doc.bin" {
		t.Fatalf("显式解析错误: rel=%q short=%q", rel, short)
	}

	// 缺省枚举：卷内唯一文件取 <owner>/user/ 下唯一文件。
	rel2, short2, err := resolveSealfileReadName(cmd, fs, context.Background(), "test", []string{dir})
	if err != nil {
		t.Fatalf("resolveSealfileReadName 缺省: %v", err)
	}
	if rel2 != "test/user/doc.bin" || short2 != "doc.bin" {
		t.Fatalf("缺省枚举错误: rel=%q short=%q", rel2, short2)
	}
}

func TestResolveSealfileOutputDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// 显式 flag → Abs 一致。
	var f sealfileFlags
	cmd := newSealfileTestCmd(&f, map[string]string{sealfileFlagOutputDir: filepath.Join(dir, "out")})
	got, err := resolveSealfileOutputDir(cmd, &f, nil)
	if err != nil {
		t.Fatalf("resolveSealfileOutputDir: %v", err)
	}
	if got != filepath.Join(dir, "out") {
		t.Fatalf("flag 输出目录错误: got %q", got)
	}

	// 配置回落 → Abs 一致。
	var f2 sealfileFlags
	cfgSvc := &sealfileCfgStub{cfg: sealfileCfg{OutDir: filepath.Join(dir, "cfg-out")}}
	cmd2 := newSealfileTestCmd(&f2, nil)
	got2, err2 := resolveSealfileOutputDir(cmd2, &f2, cfgSvc)
	if err2 != nil {
		t.Fatalf("resolveSealfileOutputDir 配置: %v", err2)
	}
	if got2 != filepath.Join(dir, "cfg-out") {
		t.Fatalf("配置输出目录错误: got %q", got2)
	}
}
