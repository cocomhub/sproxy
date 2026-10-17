// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/capacity"
	"github.com/cocomhub/sproxy/pkg/volume/enclosure"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/secretdata"
	"github.com/cocomhub/sproxy/pkg/volume/secrets"
)

// TestVcExtraByteSize 验证 vcExtraByteSize 解析 extra.meta_pad_bytes 的多种形态：
// 字符串 Go 单位（"8MiB"）、解码器产出 float64（JSON 数字）、Go 内联 map 产出 int64/int，
// 缺省/非数值/非正数返回 0。
func TestVcExtraByteSize(t *testing.T) {
	t.Parallel()
	const key = "meta_pad_bytes"
	cases := []struct {
		name  string
		extra map[string]any
		want  int64
	}{
		{name: "缺省", extra: map[string]any{}, want: 0},
		{name: "字符串-Go单位", extra: map[string]any{key: "8MiB"}, want: 8 << 20},
		{name: "float64-JSON数字", extra: map[string]any{key: float64(1 << 20)}, want: 1 << 20},
		{name: "float64-小数截断", extra: map[string]any{key: float64(1024.9)}, want: 1024},
		{name: "int64", extra: map[string]any{key: int64(4096)}, want: 4096},
		{name: "int", extra: map[string]any{key: 8192}, want: 8192},
		{name: "非法字符串", extra: map[string]any{key: "1XB"}, want: 0},
		{name: "零值", extra: map[string]any{key: int64(0)}, want: 0},
		{name: "负数忽略", extra: map[string]any{key: int64(-4096)}, want: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := vcExtraByteSize(volume.Volume{Extra: c.extra}, key); int64(got) != c.want {
				t.Errorf("vcExtraByteSize(%q)=%d, want %d", key, int64(got), c.want)
			}
		})
	}
}

// TestVcExtraBoolAndStrings 验证任务 9d 新增的 extra 解析辅助：vcExtraBool（erasure）与
// vcExtraStrings（targets 副本卷名列表，兼容 []string 与 JSON 解码的 []any）。
func TestVcExtraBoolAndStrings(t *testing.T) {
	t.Parallel()
	// vcExtraBool：true/false/缺省。
	if !vcExtraBool(volume.Volume{Extra: map[string]any{"erasure": true}}, "erasure") {
		t.Error("vcExtraBool(true) 应为 true")
	}
	if vcExtraBool(volume.Volume{Extra: map[string]any{"erasure": false}}, "erasure") {
		t.Error("vcExtraBool(false) 应为 false")
	}
	if vcExtraBool(volume.Volume{Extra: map[string]any{}}, "erasure") {
		t.Error("vcExtraBool(缺省) 应为 false")
	}
	// vcExtraStrings：[]string 直通、[]any 兼容、空项过滤、缺省 nil。
	if got := vcExtraStrings(volume.Volume{Extra: map[string]any{"targets": []string{"a", "b"}}}, "targets"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("vcExtraStrings([]string)=%v", got)
	}
	if got := vcExtraStrings(volume.Volume{Extra: map[string]any{"targets": []any{"replica1", float64(2)}}}, "targets"); len(got) != 1 || got[0] != "replica1" {
		t.Errorf("vcExtraStrings([]any)=%v（应过滤非字符串）", got)
	}
	if got := vcExtraStrings(volume.Volume{Extra: map[string]any{"targets": []string{"", "x"}}}, "targets"); len(got) != 1 || got[0] != "x" {
		t.Errorf("vcExtraStrings 空串应过滤：%v", got)
	}
	if got := vcExtraStrings(volume.Volume{Extra: map[string]any{}}, "targets"); got != nil {
		t.Errorf("vcExtraStrings(缺省) 应为 nil，got %v", got)
	}
}

// TestSecretdataOptionsFromVolume_AssemblyKeys（Imp-3 生产可达守护 + M-1 回归）：装配解析
// 把 gc_interval/preserve_mtime 接进 Options（后台**可选** GC 生产可达——远程卷/多进程场景
// 孤儿兜底）；**dedup 不接线（方案 A 降级：Dedup 为预留能力，装配层忽略 extra.dedup 键，
// 生产 Options.Dedup 恒 false）**。block_policy 的 min/max 统一走 vcPositiveInt，**YAML
// 整数（int）不再被 float64 分支静默忽略**（M-1）。
func TestSecretdataOptionsFromVolume_AssemblyKeys(t *testing.T) {
	t.Parallel()
	v := volume.Volume{Extra: map[string]any{
		// gc_interval 字符串 Go duration / int 纳秒双形态；preserve_mtime bool。
		"gc_interval":    "30s",
		"preserve_mtime": true,
		// dedup 键虽传入但装配层**忽略**（Dedup 降级预留、不接线）。
		"dedup": true,
		// M-1：block_policy 的 min/max 用 **YAML 整数（int）**——原 vcExtraBlockPolicy
		// 只读 float64，YAML 下 `min: 4096` 被静默忽略回退默认 1MiB-200MiB。
		"block_policy": map[string]any{"mode": "random", "min": 4096, "max": 16384},
		// ByteSize 统一（2026-10-03）：字节配置支持 "8MiB" 人类可读字符串与纯数字。
		"meta_pad_bytes": "8MiB",
		"max_file_bytes": float64(4096),
	}}
	opts := secretdataOptionsFromVolume(v)
	if opts.GCInterval != 30*time.Second {
		t.Errorf("gc_interval 解析失败: %v（want 30s）", opts.GCInterval)
	}
	if opts.Dedup {
		t.Error("dedup 键应被装配层忽略（Dedup 降级预留、不接线，生产不可达）")
	}
	if !opts.PreserveMTime {
		t.Error("preserve_mtime=true 未接入 Options")
	}
	if opts.Block.Min != 4096 || opts.Block.Max != 16384 {
		t.Errorf("block_policy YAML 整数被静默忽略: min=%d max=%d（want 4096/16384）", opts.Block.Min, opts.Block.Max)
	}
	// ByteSize 字符串与数字形态（sizex.ByteSize 统一：人类可读 "8MiB" + 纯数字）。
	if got := int64(opts.MetaPadBytes); got != 8<<20 {
		t.Errorf("meta_pad_bytes(8MiB 字符串) = %d，want %d", got, 8<<20)
	}
	if got := int64(opts.MaxFileBytes); got != 4096 {
		t.Errorf("max_file_bytes(数字) = %d，want 4096", got)
	}
	// 缺省：GCInterval 0（后台 GC 关闭，默认禁用）、dedup/preserve_mtime false、字节 0。
	def := secretdataOptionsFromVolume(volume.Volume{Extra: map[string]any{}})
	if def.GCInterval != 0 || def.Dedup || def.PreserveMTime {
		t.Errorf("缺省应全关，got GCInterval=%v dedup=%v preserve_mtime=%v", def.GCInterval, def.Dedup, def.PreserveMTime)
	}
	if def.MetaPadBytes != 0 || def.MaxFileBytes != 0 {
		t.Errorf("缺省字节配置应为 0（传默认），got meta_pad=%d max_file=%d", def.MetaPadBytes, def.MaxFileBytes)
	}
}

// TestRegisterSecretsBackendWithFS 注册 secrets backend 类型后 registry 可分派构造（含底层 FS 注入）。
func TestRegisterSecretsBackendWithFS(t *testing.T) {
	t.Parallel()
	typ := "secrets-test"
	root := t.TempDir()
	if err := os.MkdirAll(root+"/secrets", 0o700); err != nil {
		t.Fatal(err)
	}
	registerSecretsBackendWithFS(typ, func(ctx context.Context, v volume.Volume) (syncpkg.FS, error) {
		return secretsLocalFS(root), nil
	})
	t.Cleanup(func() { registry.UnregisterBackendForTest(typ) })
	v := volume.Volume{Name: "sv", Type: typ, RootDir: root, Extra: map[string]any{"target": "local", "root": root}}
	be, err := registry.NewBackend(context.Background(), v)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	if be == nil || be.FS() == nil {
		t.Fatal("backend 或 FS 为 nil")
	}
	// 用 FS 真跑一次：写 + 读。
	if werr := be.FS().WriteFile(context.Background(), syncpkg.LogicalKey{}, "k", strings.NewReader("val"), 3, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	rc, err := be.FS().OpenRead(context.Background(), "k")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	buf := make([]byte, 3)
	_, _ = rc.Read(buf)
	rc.Close()
	if string(buf) != "val" {
		t.Errorf("内容=%q", buf)
	}
}

// TestSetupSecretBackends_Secretdata 装配 secrets + secretdata，验证：
//   - 默认 secrets 卷已建；
//   - secretdata backend 经 secret_url 读到密钥、底层 target=local 落本地；
//   - 通过 secretdata FS 写读 files 可透明加解密。
func TestSetupSecretBackends_Secretdata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	set := newTestSet(t)
	localRoot := t.TempDir()
	// 生产装配（storeSecretDataSet + attachSecretdataVolumes）会先注册 secrets backend（声明协议 "secrets"，
	// 供 secret_url 的 ResolveURL 寻址）。此处显式调用使本测试**自包含**——不依赖
	// 其它测试触发 registerSecretsOnce 的套件顺序（隔离运行/-shuffle 也能通过）。
	registerSecretsBackend()
	// 密钥来源：**显式声明的 secrets 卷**（默认 secrets 卷是 per-owner 的、非已装配卷，
	// 装配期无 owner 上下文不可寻址——见 defaultSecretdataSecret）。
	mgr := assembleTestSecretsVolume(t, ctx, set, "keys", localRoot)
	if _, err := mgr.Create(ctx, "datakey"); err != nil {
		t.Fatalf("Create key: %v", err)
	}
	// 手动注册 secretdata 后端（独立类型名，闭包捕获本测试的 set——不依赖全局
	// setupSecretdataOnce 的共享 set，避免跨测试残留）。
	typ := "secretdata-test"
	registerSecretdataBackendWithFS(typ, func(ctx context.Context, v volume.Volume) ([]byte, error) {
		return defaultSecretdataSecret(ctx, v, set)
	})
	t.Cleanup(func() { registry.UnregisterBackendForTest(typ) })
	dataRoot := t.TempDir()
	v := volume.Volume{Name: "sd", Type: typ, RootDir: dataRoot, Extra: map[string]any{
		"target":     "local",
		"root":       dataRoot,
		"secret_url": "secrets://keys/datakey",
	}}
	be, err := registry.NewBackend(ctx, v)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	// 直接 WriteFile/OpenRead 透明加解密。
	if werr := be.FS().WriteFile(ctx, syncpkg.LogicalKey{}, "a.mp4", strings.NewReader("hello secret"), 12, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	rc, err := be.FS().OpenRead(ctx, "a.mp4")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	buf, _ := io.ReadAll(rc)
	rc.Close()
	if string(buf) != "hello secret" {
		t.Errorf("还原=%q", buf)
	}
}

// TestRegisterSecretVolumeBackends_Unconditional（Imp-1 装配门控回归）：生产装配路径
// registerSecretVolumeBackends() + storeSecretDataSet() 在**与 sync 解耦**下打通：
//   - registerSecretVolumeBackends 早于卷装配即注册 secrets/secretdata 生产类型
//     （config 声明 type:secretdata/secrets 卷时 assembleVolumes 不会「未注册后端」panic）；
//   - storeSecretDataSet 把装配卷集 Store 进 secretDataSet → 生产 "secretdata" 类型经
//     secret_url（secrets://keys/datakey）解析密钥 → NewBackend → 透明加解密可用。
//
// 本测试操作全局单例（secretDataSet 持有者 + 生产类型注册 Once），故不并行（t.Parallel）
// 以免与其它读该持有者的用例互踩；对外只依赖本测试自己造的 test set（newTestSet）。
func TestRegisterSecretVolumeBackends_Unconditional(t *testing.T) {
	// sproxy:serial: 生产注册路径 CAS 全局单例（secretDataSet + 生产类型 Once），不并发。
	ctx := context.Background()
	// 第一步：生产类型注册（早于卷装配，幂等 Once——可安全重复调用）。
	registerSecretVolumeBackends()
	if !hasBackendType("secretdata") {
		t.Fatal("生产 secretata 类型未注册")
	}
	if !hasBackendType("secrets") {
		t.Fatal("生产 secrets 类型未注册")
	}
	// 第二步：卷装配（模拟 RegisterRoutes 完成后）——Store 已装配卷集供 secretdata 工厂懒解析。
	set := newTestSet(t)
	localRoot := t.TempDir()
	// 本用例卷集无 config 声明卷，只需 Store（补装阶段无事可做）。
	if err := storeSecretDataSet(set); err != nil {
		t.Fatalf("storeSecretDataSet: %v", err)
	}
	// 密钥来源 = 显式声明的 secrets 卷（默认 secrets 卷 per-owner，非已装配卷）。
	mgr := assembleTestSecretsVolume(t, ctx, set, "keys", localRoot)
	if _, err := mgr.Create(ctx, "datakey"); err != nil {
		t.Fatalf("Create key: %v", err)
	}
	dataRoot := t.TempDir()
	v := volume.Volume{Name: "sd", Type: "secretdata", RootDir: dataRoot, Extra: map[string]any{
		"target":     "local",
		"root":       dataRoot,
		"secret_url": "secrets://keys/datakey",
	}}
	be, err := registry.NewBackend(ctx, v)
	if err != nil {
		t.Fatalf("NewBackend(生产 secretata): %v", err)
	}
	if werr := be.FS().WriteFile(ctx, syncpkg.LogicalKey{}, "a.mp4", strings.NewReader("hello secret"), 12, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	rc, err := be.FS().OpenRead(ctx, "a.mp4")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	buf, _ := io.ReadAll(rc)
	rc.Close()
	if string(buf) != "hello secret" {
		t.Errorf("还原=%q", buf)
	}
	// config 声明 type: secrets 卷（assembleVolumes 对 cfg.Volumes 中 type 卷调用
	// registry.NewBackend）——生产 secrets 类型已注册、可寻址构造（底层本地 secrets/ 视图）。
	sv := volume.Volume{Name: "cfg-secrets", Type: "secrets", RootDir: localRoot,
		Extra: map[string]any{"target": "local", "root": localRoot}}
	sbe, err := registry.NewBackend(ctx, sv)
	if err != nil {
		t.Fatalf("NewBackend(config 声明 secrets 卷): %v", err)
	}
	if sbe == nil || sbe.FS() == nil {
		t.Fatal("secrets backend 或 FS 为 nil（config 声明 type:secrets 卷不可寻址）")
	}
	t.Cleanup(func() {
		// 清除全局持有，避免影响其它用例。
		secretDataSet.Store(nil)
	})
}

// TestRegisterSecretSchemas_SecretURLField（D1，2026-10-06）：secretdata 建卷 schema 补
// secret_url（text 必填）——UI 表单渲染该字段后建卷可提交到后端（后端 defaultSecretdataSecret
// 缺 secret_url fail-closed）；secrets 类型仍仅 target。钉住静态 schema 内容：
//   - secretdata.fields = [target(volume-select,required), secret_url(text,required)]；
//   - secrets.fields = [target(volume-select,required)]。
func TestRegisterSecretSchemas_SecretURLField(t *testing.T) {
	// sproxy:serial: 生产注册路径全局单例（生产类型 Once 已消费，读取共享静态表）。
	registerSecretVolumeBackends()

	sd := registry.BackendSchema("secretdata")
	if len(sd) != 2 {
		t.Fatalf("secretdata schema 应含 2 字段（target + secret_url），got %+v", sd)
	}
	if sd[0].Key != "target" || sd[0].Type != "volume-select" || !sd[0].Required || !sd[0].AllowWrapper {
		t.Fatalf("secretdata schema[0] 应为 target(volume-select,required,allow_wrapper)，got %+v", sd[0])
	}
	if sd[1].Key != "secret_url" || sd[1].Type != "text" || !sd[1].Required {
		t.Fatalf("secretdata schema[1] 应为 secret_url(text,required)，got %+v", sd[1])
	}
	sec := registry.BackendSchema("secrets")
	if len(sec) != 1 || sec[0].Key != "target" || sec[0].Type != "volume-select" || !sec[0].Required {
		t.Fatalf("secrets schema 应仅含 target(volume-select,required)，got %+v", sec)
	}
}

// TestSetupSecretBackends_Secretdata_MultiTarget（任务 9d 修复轮 Imp-1）：extra.targets
// 多 local root → 生产多 target 装配生效——装配层解析副本 local root 构造副本底层 FS →
// 写后主/副本两 root 都有同一容器（副本复制运行，非仅记账）。跨外部卷接线留后续片。
func TestSetupSecretBackends_Secretdata_MultiTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	set := newTestSet(t)
	registerSecretsBackend()
	mgr := assembleTestSecretsVolume(t, ctx, set, "keys", t.TempDir())
	if _, err := mgr.Create(ctx, "datakey"); err != nil {
		t.Fatalf("Create key: %v", err)
	}
	typ := "secretdata-multitarget"
	registerSecretdataBackendWithFS(typ, func(ctx context.Context, v volume.Volume) ([]byte, error) {
		return defaultSecretdataSecret(ctx, v, set)
	})
	t.Cleanup(func() { registry.UnregisterBackendForTest(typ) })
	primaryRoot := t.TempDir()
	replicaRoot := t.TempDir()
	v := volume.Volume{Name: "sd", Type: typ, RootDir: primaryRoot, Extra: map[string]any{
		"target":     "local",
		"root":       primaryRoot,
		"secret_url": "secrets://keys/datakey",
		"targets":    []string{replicaRoot},
	}}
	be, err := registry.NewBackend(ctx, v)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	if werr := be.FS().WriteFile(ctx, syncpkg.LogicalKey{}, "a.mp4", strings.NewReader("hello secret"), 12, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	rc, err := be.FS().OpenRead(ctx, "a.mp4")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	buf, _ := io.ReadAll(rc)
	rc.Close()
	if string(buf) != "hello secret" {
		t.Errorf("还原=%q", buf)
	}
	// 副本复制生效：主/副本两 root 都应含同一容器目录（非仅记账）。
	primaryDirs := rootContainerDirs(t, primaryRoot)
	replicaDirs := rootContainerDirs(t, replicaRoot)
	if len(primaryDirs) != 1 {
		t.Fatalf("主 target 应含 1 个容器目录，got %+v", primaryDirs)
	}
	if len(replicaDirs) != 1 {
		t.Fatalf("副本 target 应含 1 个容器目录（副本复制未生效），got %+v", replicaDirs)
	}
	if primaryDirs[0] != replicaDirs[0] {
		t.Errorf("主/副本容器目录名不一致：%q vs %q", primaryDirs[0], replicaDirs[0])
	}
}

// hasBackendType 检查注册表是否含指定后端类型（测试辅助）。
func hasBackendType(typ string) bool {
	return slices.Contains(registry.BackendTypes(), typ)
}

// rootContainerDirs 列出本地 root 下的容器目录名（复制验证用）。
func rootContainerDirs(t *testing.T, root string) []string {
	t.Helper()
	fs := syncpkg.NewLocalFS(root, nil)
	entries, err := fs.ListDir(context.Background(), "")
	if err != nil {
		t.Fatalf("ListDir(%s): %v", root, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir {
			out = append(out, e.Name)
		}
	}
	return out
}

// TestSecretdataBackend_PreAssembly_NoSet（Imp-1 边界）：config 声明 type:secretdata 卷
// 的构造发生在 assembleVolumes（RegisterRoutes 内），此时卷集尚未 Store 进 secretDataSet。
// 后端类型已注册（registerSecretVolumeBackends 早于卷装配）→ NewBackend 应返回**明确错误**
// （卷集未就绪），而非「未注册后端」panic 或 nil 解引用。
func TestSecretdataBackend_PreAssembly_NoSet(t *testing.T) {
	// sproxy:serial: 依赖全局注册表/secretDataSet 单例（与 TestRegisterSecretVolumeBackends_Unconditional 同族）。
	registerSecretVolumeBackends()
	t.Cleanup(func() { secretDataSet.Store(nil) })
	ctx := context.Background()
	v := volume.Volume{Name: "sd", Type: "secretdata", RootDir: "/tmp", Extra: map[string]any{
		"target":     "local",
		"root":       "/tmp",
		"secret_url": "secrets://keys/datakey",
	}}
	be, err := registry.NewBackend(ctx, v)
	if err == nil {
		_ = be.Close()
		t.Fatal("卷集未就绪时构造 secretdata 后端应返回错误（fail-closed）")
	}
	if !strings.Contains(err.Error(), "卷集未就绪") {
		t.Errorf("错误应明确指示卷集未就绪，got %v", err)
	}
}

// TestSetupSecretBackends_ConfigSecretdata_Assembled（Imp-2 时序修复守护）：config 声明
// type:secretdata 卷（如 config.example vault）在 RegisterRoutes 返回后由 attachSecretdataVolumes
// **补装**——assembleVolumes 对 deferred 类型跳过 backend 构造但登记卷元数据（含 Extra/
// Capacity），set 就绪后本函数遍历 set.All 找到未装配的 secretdata 卷 → registry.NewBackend
// （此时 secretDataSet 已 Store、secrets 卷可 ResolveURL）→ AttachExternal 挂回 Set。断言：
//   - config 声明 secretdata 卷补装后可寻址（Set.External 非 nil）且透明加解密可用；
//   - secretdata 的 `secret_url` 指向**显式声明的 secrets 卷**时可解析（默认 secrets 卷自
//     2026-10-15 起是 per-owner 的，不在已装配卷集内）。
func TestSetupSecretBackends_ConfigSecretdata_Assembled(t *testing.T) {
	// sproxy:serial: 依赖生产注册路径的全局单例（secretDataSet + 生产类型 Once）。
	ctx := context.Background()
	registerSecretVolumeBackends()
	t.Cleanup(func() { secretDataSet.Store(nil) })

	localRoot := t.TempDir()
	dataRoot := t.TempDir()
	// 模拟 assembleVolumes 已登记 config 声明卷（deferred 类型跳过 backend 构造，卷元数据
	// + 容量池已在 Set；External 留空待补装）：default 本地卷 + vault secretdata 卷描述。
	root := filepath.Join(t.TempDir(), "default")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rt, rerr := storage.OpenRoot(root)
	if rerr != nil {
		t.Fatalf("OpenRoot: %v", rerr)
	}
	vault := volume.Volume{Name: "vault", Type: "secretdata", RootDir: dataRoot,
		Capacity: 100,
		Extra: map[string]any{
			"target":     "local",
			"root":       dataRoot,
			"secret_url": "secrets://keys/datakey",
		}}
	set := registry.NewSet(
		[]volume.Volume{{Name: "default", Type: volume.TypeLocal, RootDir: root}, vault},
		map[string]*storage.Root{"default": rt},
		map[string]registry.ExternalBackend{},
		map[string]*quota.Pool{"default": quota.NewPool(0), "vault": quota.NewPool(100)},
		"default",
	)
	t.Cleanup(func() { _ = set.Close() })

	// 先建**显式声明的 secrets 卷** + 造密钥（补装 secretdata 卷时需解析
	// secrets://keys/datakey——密钥须先于补装存在；默认 secrets 卷是 per-owner 的，装配期无
	// owner 上下文不可寻址）。
	mgr := assembleTestSecretsVolume(t, ctx, set, "keys", localRoot)
	if _, err := mgr.Create(ctx, "datakey"); err != nil {
		t.Fatalf("Create key: %v", err)
	}

	// 生产装配序前两步：Store set + 先行补装（此处 config 声明 secretdata 卷的 secret_url 指向
	// **本地** secrets 卷 keys ⇒ 属「不依赖封装声明卷」一轮；此时密钥已存在、可 ResolveURL）。
	if err := storeSecretDataSet(set); err != nil {
		t.Fatalf("storeSecretDataSet: %v", err)
	}
	if err := attachSecretdataVolumes(ctx, set, localRoot, nil, secretdataRefsIndependent); err != nil {
		t.Fatalf("attachSecretdataVolumes: %v", err)
	}
	// config 声明 secretdata 卷补装后可寻址（Imp-2 核心：不再「卷集未就绪」恒失败）。
	be := set.External("vault")
	if be == nil {
		t.Fatal("config 声明 secretdata 卷补装后应可寻址（Set.External 非 nil）")
	}
	if be.FS() == nil {
		t.Fatal("secretdata backend FS 为 nil")
	}

	// **嵌套封装卷（方案甲）**：`type: secretdata` + 用户面 `extra.target: "<卷>/<子目录>"` 的
	// config 声明走**生产完整链路**——RegisterRoutes（assembleVolumes 内翻译出声明词汇 + 注入
	// 封印 + 按**声明判据**跳过按类型分派）→ 补装两轮（必须**跳过**它：按类型分派会
	// 给 target=main/videos 构造「需由装配层解析注入」的错误后端 ⇒ boot fail）→
	// enclosure.AttachAll（真实 secretdata wrapper 经 main/videos 区域视图装配）。断言见子用例注释。
	t.Run("nested_target_keeps_wrapper_type", func(t *testing.T) {
		registerEnclosureWrappers() // 生产封装处理层（secretdata/secrets wrapper）
		mainDir := t.TempDir()
		cfg := server.Default()
		cfg.Addr = "127.0.0.1:0"
		cfg.StorageRoot = mainDir
		cfg.Volumes = []server.VolumeConfig{
			{Name: "main", Root: mainDir, VolCapacity: 1 << 30},
			{Name: "vault", Type: "secretdata", Extra: map[string]any{
				"target":     "main/videos",
				"secret_url": "secrets://keys/datakey",
			}},
		}
		if verr := cfg.Validate(); verr != nil {
			t.Fatalf("cfg.Validate: %v", verr)
		}
		var nestedCfgPtr atomic.Pointer[server.Config] // 独立于外层用例的 cfgPtr（避免遮蔽）
		nestedCfgPtr.Store(cfg)
		h := server.RegisterRoutes(ctx, server.RegisterRoutesOpts{
			Mux: http.NewServeMux(), CfgPtr: &nestedCfgPtr, Logger: discardLoggerMain(),
			Version: "test", BuildAt: "test",
		})
		t.Cleanup(func() { _ = h.Close() })
		vols := h.Volumes()

		// ① 卷描述 Type 保持 wrapper 类型（enclosure 只是内部装配机制）⇒ 协议/分类/回显全按
		// wrapper 语义（此前 type=enclosure 会让 SchemeOf 无协议、CategoryOf≠wrapper）。
		v, ok := vols.ByName("vault")
		if !ok || v.Type != "secretdata" {
			t.Fatalf("嵌套封装卷的 Type 应保持 secretdata: ok=%v type=%q", ok, v.Type)
		}
		if got := registry.SchemeOf(v.Type); got != "secretdata" {
			t.Fatalf("SchemeOf(%q)=%q, want secretdata（无协议则 cloud 转存/归档创建即拒）", v.Type, got)
		}
		if got := registry.CategoryOf(v.Type); got != "wrapper" {
			t.Fatalf("CategoryOf(%q)=%q, want wrapper（建卷表单/防环校验据它判封装语义）", v.Type, got)
		}

		// ② 生产 boot 顺序：Store 卷集 + 先行补装（本卷集里声明的 vault 是**嵌套**形态 ⇒
		// 两轮补装都必须跳过它）→ 造密钥 → enclosure.AttachAll（嵌套封装卷由此装配）。
		if serr := storeSecretDataSet(vols); serr != nil {
			t.Fatalf("storeSecretDataSet: %v", serr)
		}
		if serr := attachSecretdataVolumes(ctx, vols, cfg.StorageRoot, discardLoggerMain(), secretdataRefsIndependent); serr != nil {
			t.Fatalf("attachSecretdataVolumes（先行）: %v", serr)
		}
		if vols.External("vault") != nil {
			t.Fatal("嵌套封装卷不得由补装两轮按类型分派装配（须交 enclosure.AttachAll）")
		}
		// 密钥来源：显式声明的 secrets 卷（默认 secrets 卷自 2026-10-15 起是 per-owner 的，
		// 装配期无 owner 上下文 ⇒ 不可作服务器级 secret_url 目标）。
		mgr := assembleTestSecretsVolume(t, ctx, vols, "keys", cfg.StorageRoot)
		if _, kerr := mgr.Create(ctx, "datakey"); kerr != nil {
			t.Fatalf("Create key: %v", kerr)
		}
		if aerr := enclosure.AttachAll(ctx, vols, discardLoggerMain(), enclosure.AttachOptions{StorageRoot: cfg.StorageRoot}); aerr != nil {
			t.Fatalf("AttachAll: %v", aerr)
		}

		// ③ 写保护：源区间封印生效（占用区间只读）。
		rt := vols.Root("main")
		if rt == nil {
			t.Fatal("main 本地根未装配")
		}
		if rule := rt.SealAt("videos/x.bin"); rule == nil || rule.Wrapper != "vault" {
			t.Fatalf("源卷 main 的 videos 区间应被 vault 封印, rules=%+v", rt.Seals())
		}

		// ④ 数据面：真实 secretdata 封装后端（区域视图上的分块加密）——读回一致 + 落盘为密文。
		nbe := vols.External("vault")
		if nbe == nil || nbe.FS() == nil {
			t.Fatal("嵌套封装卷后端未挂回卷集（enclosure 装配失败）")
		}
		const body = "nested enclosure payload"
		const key = "alice/user/n.bin"
		if werr := nbe.FS().WriteFile(ctx, syncpkg.LogicalKey{}, key, strings.NewReader(body), int64(len(body)), 0); werr != nil {
			t.Fatalf("封装卷写入: %v", werr)
		}
		rc2, oerr := nbe.FS().OpenRead(ctx, key)
		if oerr != nil {
			t.Fatalf("封装卷读回: %v", oerr)
		}
		got, rerr := io.ReadAll(rc2)
		_ = rc2.Close()
		if rerr != nil || string(got) != body {
			t.Fatalf("读回=%q err=%v, want %q", got, rerr, body)
		}
		if n := dirBytes(t, filepath.Join(mainDir, "videos")); n == 0 {
			t.Fatal("物理落点应为 main/videos 区域（前缀映射失效？）")
		}
		if plaintextOnDisk(t, filepath.Join(mainDir, "videos"), body) {
			t.Fatal("封装卷写入应以密文落盘（分块加密未生效）")
		}
	})

	// **多层嵌套链 + 显式自定义 secrets 卷**（用户裁定 2026-10-15 第 2/3 点）：
	// per-owner 自动桶是「零配置默认」，但**不排除**显式声明的自定义 secrets 卷——底层可为任意
	// 卷链。本子用例把链条拉到 4 层并全程经生产装配路径：
	//
	//	keys（type: secrets，本地）            ← 手工放置的密钥文件（文档工作流）
	//	vault（secretdata over main）           ← 用 keys/datakey 作加密密钥
	//	deepkeys（secrets over vault/keys-area）← 密钥库自身落在**加密区**内（secrets over secretdata）
	//	deep（secretdata over main，运行期建）  ← secret_url 引用 deepkeys/master（跨加密层的密钥解析）
	//
	// 断言：每层装配成功 + 数据面读回一致 + 各层物理落点为密文；变异（enclosure 组合能力退化 /
	// secret_url 不支持嵌套 secrets 卷）→ 本子用例红。
	t.Run("multi_layer_nesting_chain", func(t *testing.T) {
		registerEnclosureWrappers() // 生产封装处理层（secretdata/secrets wrapper）
		base := t.TempDir()
		mainDir := filepath.Join(base, "main")
		keysDir := filepath.Join(base, "keys")
		if err := os.MkdirAll(mainDir, 0o755); err != nil {
			t.Fatalf("mkdir main: %v", err)
		}
		// 底层密钥**手工放置**（docs/secret-volume.md §2 的部署工作流）：<keys>/secrets/datakey。
		if err := os.MkdirAll(filepath.Join(keysDir, "secrets"), 0o700); err != nil {
			t.Fatalf("mkdir keys/secrets: %v", err)
		}
		const chainKey = "6f1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8"
		if werr := os.WriteFile(filepath.Join(keysDir, "secrets", "datakey"), []byte(chainKey), 0o600); werr != nil {
			t.Fatalf("write datakey: %v", werr)
		}

		cfg := server.Default()
		cfg.Addr = "127.0.0.1:0"
		cfg.StorageRoot = mainDir
		cfg.Volumes = []server.VolumeConfig{
			{Name: "main", Root: mainDir, VolCapacity: 1 << 30},
			// 显式自定义 secrets 卷（本地密钥目录；`extra.root` 是**父目录**，内部补 /secrets）。
			{Name: "keys", Type: "secrets", Extra: map[string]any{"target": "local", "root": keysDir}},
			// 第 1 层：加密数据区（密钥取自 keys）。
			{Name: "vault", Type: "secretdata", Extra: map[string]any{
				"target": "main/vault-area", "secret_url": "secrets://keys/datakey",
			}},
			// 第 2 层：secrets over secretdata（密钥库落在**加密区**内 = 多层嵌套链）。
			{Name: "deepkeys", Type: "secrets", Extra: map[string]any{"target": "vault/keys-area"}},
		}
		if verr := cfg.Validate(); verr != nil {
			t.Fatalf("cfg.Validate: %v", verr)
		}
		var chainCfgPtr atomic.Pointer[server.Config]
		chainCfgPtr.Store(cfg)
		h := server.RegisterRoutes(ctx, server.RegisterRoutesOpts{
			Mux: http.NewServeMux(), CfgPtr: &chainCfgPtr, Logger: discardLoggerMain(),
			Version: "test", BuildAt: "test",
		})
		t.Cleanup(func() { _ = h.Close() })
		vols := h.Volumes()

		// 生产 boot 顺序（四步）：Store → 先行补装 → AttachAll（多层链由此装配）→ 后置补装。
		if serr := storeSecretDataSet(vols); serr != nil {
			t.Fatalf("storeSecretDataSet: %v", serr)
		}
		if serr := attachSecretdataVolumes(ctx, vols, cfg.StorageRoot, discardLoggerMain(), secretdataRefsIndependent); serr != nil {
			t.Fatalf("attachSecretdataVolumes（先行）: %v", serr)
		}
		if aerr := enclosure.AttachAll(ctx, vols, discardLoggerMain(), enclosure.AttachOptions{StorageRoot: cfg.StorageRoot}); aerr != nil {
			t.Fatalf("AttachAll（多层链装配）: %v", aerr)
		}
		if serr := attachSecretdataVolumes(ctx, vols, cfg.StorageRoot, discardLoggerMain(), secretdataRefsEnclosure); serr != nil {
			t.Fatalf("attachSecretdataVolumes（后置）: %v", serr)
		}

		// ① 第 0/1 层：显式 secrets 卷 + 加密数据区。
		for _, name := range []string{"keys", "vault"} {
			if layerBE := vols.External(name); layerBE == nil || layerBE.FS() == nil {
				t.Fatalf("卷 %q 未装配", name)
			}
		}
		// ② 第 2 层：secrets over secretdata（wrapper 组合能力）。
		deepBE := vols.External("deepkeys")
		if deepBE == nil || deepBE.FS() == nil {
			t.Fatal("secrets over secretdata（多层嵌套链）未装配")
		}
		deepMgr := secrets.ManagerOfExternal(deepBE)
		if deepMgr == nil {
			t.Fatal("嵌套 Secrets 卷 Manager 反取失败")
		}
		key, kerr := deepMgr.Create(ctx, "master")
		if kerr != nil {
			t.Fatalf("在嵌套（经加密区）secrets 卷里创建密钥失败: %v", kerr)
		}
		// 物理落点：密钥经 secretdata 透明加解密 ⇒ 明文不得出现在磁盘任何文件中。
		if plaintextOnDisk(t, filepath.Join(mainDir, "vault-area"), string(key)) {
			t.Fatal("嵌套 secrets 卷的密钥应以密文落盘（secrets over secretdata 未生效）")
		}

		// ③ 第 3 层（运行期建卷）：secretdata 的 secret_url 引用**嵌套 secrets 卷**里的密钥
		// （`secrets://deepkeys/master`）——解析要穿过「读取该卷 → 其底层经 vault 解密」整条链。
		deepVol := volume.Volume{Name: "deep", Type: "secretdata", Extra: map[string]any{
			"target": "main/deep-area", "secret_url": "secrets://deepkeys/master",
		}}
		tv, nested := server.TranslateNestedVolume(deepVol)
		if !nested {
			t.Fatal("嵌套 target 语法应被翻译为封装声明")
		}
		if aerr := server.AttachEnclosureVolume(ctx, vols, discardLoggerMain(), cfg.StorageRoot, tv); aerr != nil {
			t.Fatalf("引用嵌套 secrets 卷的 secretdata 装配失败（多层链断链）: %v", aerr)
		}
		dbe := vols.External("deep")
		if dbe == nil || dbe.FS() == nil {
			t.Fatal("第 3 层（引用嵌套 secrets 卷的 secretdata）未装配")
		}
		const body = "multi layer chain payload"
		const key2 = "alice/user/chain.bin"
		if werr := dbe.FS().WriteFile(ctx, syncpkg.LogicalKey{}, key2, strings.NewReader(body), int64(len(body)), 0); werr != nil {
			t.Fatalf("第 3 层写入: %v", werr)
		}
		rc3, oerr := dbe.FS().OpenRead(ctx, key2)
		if oerr != nil {
			t.Fatalf("第 3 层读回: %v", oerr)
		}
		got3, rerr3 := io.ReadAll(rc3)
		_ = rc3.Close()
		if rerr3 != nil || string(got3) != body {
			t.Fatalf("第 3 层读回=%q err=%v，want %q", got3, rerr3, body)
		}
		if plaintextOnDisk(t, filepath.Join(mainDir, "deep-area"), body) {
			t.Fatal("第 3 层写入应以密文落盘")
		}
	})

	// **外部底座多层链**（docs/secret-volume.md 批次表 ⑦ 举例「secrets over secretdata over
	// baidupcs」）：把多层嵌套链的底座从本地卷换成**外部卷**（用测试注册的外部后端替身替代需要
	// 真实账号/网络的 baidupcs/s3——外部源的键空间/封印/特权视图接线口径一致，差别只在后端实现），
	// 钉住「外部源封印 + 加密 + secrets」三能力组合：
	//   - ① 外部源封印在**装配处**注入（套在该后端 `FS()` 上）⇒ 直连外部后端写封装区恒 `ErrSealed`
	//     （未封印区域照常可写 = 零回归）；
	//   - ② 封装卷经 `UnsealedSource` 特权视图仍可读写自己的区间（封印不挡自己）；
	//   - ③ secretdata 的密文真落在**外部后端的键空间**内；
	//   - ④ 最上层 secrets 的密钥经加密层以密文落盘。
	//
	//	main（本地默认卷）
	//	keys（type: secrets，本地）              ← 手工放置的密钥文件（文档部署工作流）
	//	ext（外部卷，测试替身）                   ← 链条底座（模拟 baidupcs/s3 等外部后端）
	//	vault（secretdata over ext/vault-area）   ← 加密层落在**外部后端**上
	//	deepkeys（secrets over vault/keys-area）  ← 密钥库自身落在加密区（= 批次表 ⑦ 的形态）
	//
	// 变异：装配处不包装外部后端（`injectSeals` 略过 `acc.external`）→ ① 红；`SourceFS` 对外部源
	// 改回裸 `be.FS()`（不取 `UnsealedSource`）→ 补装期前置校验/写入被自己的封印拒 → 红。
	t.Run("multi_layer_chain_over_external_source", func(t *testing.T) {
		registerEnclosureWrappers() // 生产封装处理层（secretdata/secrets wrapper）
		// 外部底座后端：测试注册的唯一类型名 + t.Cleanup 反注册（不改全局 seam ⇒ 并行安全模式）。
		const extType = "testext-chain-base"
		extDir := t.TempDir()
		registry.RegisterBackend(extType, func(_ context.Context, _ volume.Volume) (registry.ExternalBackend, error) {
			return chainExtBackend{fs: syncpkg.NewLocalFS(extDir, nil)}, nil
		})
		t.Cleanup(func() { registry.UnregisterBackendForTest(extType) })

		base := t.TempDir()
		mainDir := filepath.Join(base, "main")
		keysDir := filepath.Join(base, "keys")
		if err := os.MkdirAll(mainDir, 0o755); err != nil {
			t.Fatalf("mkdir main: %v", err)
		}
		// 底层密钥**手工放置**（与上一子用例同工作流）：<keys>/secrets/datakey。
		if err := os.MkdirAll(filepath.Join(keysDir, "secrets"), 0o700); err != nil {
			t.Fatalf("mkdir keys/secrets: %v", err)
		}
		const chainKey = "7f1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8"
		if werr := os.WriteFile(filepath.Join(keysDir, "secrets", "datakey"), []byte(chainKey), 0o600); werr != nil {
			t.Fatalf("write datakey: %v", werr)
		}

		cfg := server.Default()
		cfg.Addr = "127.0.0.1:0"
		cfg.StorageRoot = mainDir
		cfg.Volumes = []server.VolumeConfig{
			{Name: "main", Root: mainDir, VolCapacity: 1 << 30},
			{Name: "keys", Type: "secrets", Extra: map[string]any{"target": "local", "root": keysDir}},
			// 外部底座：非本地类型 ⇒ assembleVolumes 走 registry.NewBackend（外部卷路径）。
			{Name: "ext", Type: extType, VolCapacity: 1 << 30},
			{Name: "vault", Type: "secretdata", Extra: map[string]any{
				"target": "ext/vault-area", "secret_url": "secrets://keys/datakey",
			}},
			{Name: "deepkeys", Type: "secrets", Extra: map[string]any{"target": "vault/keys-area"}},
		}
		if verr := cfg.Validate(); verr != nil {
			t.Fatalf("cfg.Validate: %v", verr)
		}
		var extCfgPtr atomic.Pointer[server.Config]
		extCfgPtr.Store(cfg)
		h := server.RegisterRoutes(ctx, server.RegisterRoutesOpts{
			Mux: http.NewServeMux(), CfgPtr: &extCfgPtr, Logger: discardLoggerMain(),
			Version: "test", BuildAt: "test",
		})
		t.Cleanup(func() { _ = h.Close() })
		vols := h.Volumes()

		// 生产 boot 顺序（四步）：Store → 先行补装 → AttachAll（外部底座链由此装配）→ 后置补装。
		if serr := storeSecretDataSet(vols); serr != nil {
			t.Fatalf("storeSecretDataSet: %v", serr)
		}
		if serr := attachSecretdataVolumes(ctx, vols, cfg.StorageRoot, discardLoggerMain(), secretdataRefsIndependent); serr != nil {
			t.Fatalf("attachSecretdataVolumes（先行）: %v", serr)
		}
		if aerr := enclosure.AttachAll(ctx, vols, discardLoggerMain(), enclosure.AttachOptions{StorageRoot: cfg.StorageRoot}); aerr != nil {
			t.Fatalf("AttachAll（外部底座链装配）: %v", aerr)
		}

		// ① 各层装配成功（含外部底座 + 两层封装）。
		for _, name := range []string{"keys", "ext", "vault", "deepkeys"} {
			if layerBE := vols.External(name); layerBE == nil || layerBE.FS() == nil {
				t.Fatalf("卷 %q 未装配", name)
			}
		}
		// ② 外部源封印：直连外部后端写封装区（含封装区内的子路径）→ 恒拒且可归因 ErrSealed。
		extView := vols.External("ext").FS()
		for _, key := range []string{"vault-area/x.bin", "vault-area/alice/user/x.bin"} {
			if werr := extView.WriteFile(ctx, syncpkg.LogicalKey{}, key, strings.NewReader("p"), 1, 0); !errors.Is(werr, storage.ErrSealed) {
				t.Fatalf("外部源封印路径 %q 直连写入必须被拒（ErrSealed）, got %v", key, werr)
			}
		}
		// 外部卷未封印区域照常可写（零回归：封印不误伤底座其余键空间）。
		if werr := extView.WriteFile(ctx, syncpkg.LogicalKey{}, "plain/x.bin", strings.NewReader("ok"), 2, 0); werr != nil {
			t.Fatalf("外部卷未封印路径写应放行, got %v", werr)
		}
		// ③ vault（secretdata over 外部源）：经特权视图读写往返，密文落外部后端。
		const body = "external base chain payload"
		const dataKey = "alice/user/ext.bin"
		vbe := vols.External("vault")
		if werr := vbe.FS().WriteFile(ctx, syncpkg.LogicalKey{}, dataKey, strings.NewReader(body), int64(len(body)), 0); werr != nil {
			t.Fatalf("vault（secretdata over 外部源）写入: %v", werr)
		}
		rc, oerr := vbe.FS().OpenRead(ctx, dataKey)
		if oerr != nil {
			t.Fatalf("vault 读回: %v", oerr)
		}
		got, rerr := io.ReadAll(rc)
		_ = rc.Close()
		if rerr != nil || string(got) != body {
			t.Fatalf("vault 读回=%q err=%v，want %q", got, rerr, body)
		}
		if plaintextOnDisk(t, extDir, body) {
			t.Fatal("vault 写入应以密文落到外部后端（分块加密未生效）")
		}
		// 正锚点（防假绿）：密文确实落在**外部后端**的物理目录里——否则上面「无明文」的断言可能空跑。
		countExtFiles := func() int {
			n := 0
			_ = filepath.WalkDir(extDir, func(_ string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					n++
				}
				return nil
			})
			return n
		}
		afterVault := countExtFiles()
		if afterVault == 0 {
			t.Fatal("vault 写入未落到外部后端物理目录（外部底座链未真正经外部源）")
		}
		// ④ 最上层 secrets（secrets over secretdata over 外部源）：密钥创建成功且密文落盘。
		deepMgr := secrets.ManagerOfExternal(vols.External("deepkeys"))
		if deepMgr == nil {
			t.Fatal("外部底座链的最上层 Secrets 卷 Manager 反取失败")
		}
		key, kerr := deepMgr.Create(ctx, "master")
		if kerr != nil {
			t.Fatalf("在外部底座链的最上层 secrets 卷里创建密钥失败: %v", kerr)
		}
		if plaintextOnDisk(t, extDir, string(key)) {
			t.Fatal("外部底座链上的密钥应以密文落盘（secrets over secretdata 未生效）")
		}
		if countExtFiles() <= afterVault {
			t.Fatal("最上层 secrets 的密钥未落到外部后端物理目录（多层链未真正落在外部底座上）")
		}
	})

	// **config 声明顺序限制解除**（2026-10-15）：把「引用嵌套 secrets 卷」的场景从运行期建卷
	// （上一子用例的第 3 层）提前到 **config 声明**——config 里同时声明「嵌套 secrets 卷」与
	// 「`secret_url` 指向它的 secretdata 卷」。这要求装配序为
	// 「Store 卷集 → 先行补装（不依赖封装卷的 secretdata）→ AttachAll（嵌套 secrets 卷由此装配）
	//  → 后置补装（引用封装卷的 secretdata）」；旧序（补装早于 AttachAll）下密钥解析必然
	// 「源卷后端未装配」而 boot fail。
	//
	//	main（本地）
	//	nested-keys（secrets over main/keyarea）  ← 键空间根 = 区域视图，密钥手工放置
	//	vault（secretdata，本地 target，secret_url=secrets://nested-keys/datakey）← config 声明
	//
	// 断言：boot 成功 + vault 已装配 + 透明加解密可用 + 密文落盘。
	t.Run("config_secretdata_refs_nested_secrets", func(t *testing.T) {
		registerEnclosureWrappers() // 生产封装处理层（secretdata/secrets wrapper）
		base := t.TempDir()
		mainDir := filepath.Join(base, "main")
		if err := os.MkdirAll(mainDir, 0o755); err != nil {
			t.Fatalf("mkdir main: %v", err)
		}
		// 嵌套 secrets 卷的区域视图**即其键空间根**（不再拼接 /secrets 段）⇒ 密钥手工放
		// `<main>/keyarea/datakey`（区域为「纯文件」形态 ⇒ 通过封装卷的防误删前置校验）。
		keyArea := filepath.Join(mainDir, "keyarea")
		if err := os.MkdirAll(keyArea, 0o700); err != nil {
			t.Fatalf("mkdir keyarea: %v", err)
		}
		const nestedKey = "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809"
		if werr := os.WriteFile(filepath.Join(keyArea, "datakey"), []byte(nestedKey), 0o600); werr != nil {
			t.Fatalf("write nested datakey: %v", werr)
		}

		dataRoot := t.TempDir()
		cfg := server.Default()
		cfg.Addr = "127.0.0.1:0"
		cfg.StorageRoot = mainDir
		cfg.Volumes = []server.VolumeConfig{
			{Name: "main", Root: mainDir, VolCapacity: 1 << 30},
			{Name: "nested-keys", Type: "secrets", Extra: map[string]any{"target": "main/keyarea"}},
			{Name: "vault", Type: "secretdata", Extra: map[string]any{
				"target": "local", "root": dataRoot, "secret_url": "secrets://nested-keys/datakey",
			}},
		}
		if verr := cfg.Validate(); verr != nil {
			t.Fatalf("cfg.Validate: %v", verr)
		}
		var subCfgPtr atomic.Pointer[server.Config]
		subCfgPtr.Store(cfg)
		h := server.RegisterRoutes(ctx, server.RegisterRoutesOpts{
			Mux: http.NewServeMux(), CfgPtr: &subCfgPtr, Logger: discardLoggerMain(),
			Version: "test", BuildAt: "test",
		})
		t.Cleanup(func() { _ = h.Close() })
		vols := h.Volumes()

		// 生产 boot 顺序（四步）：Store 卷集 → 先行补装 → AttachAll → 后置补装（全 fail-closed）。
		// 本子用例钉住「旧序（补装早于 AttachAll）必然失败 → 新序成功」的装配序修复。
		if serr := storeSecretDataSet(vols); serr != nil {
			t.Fatalf("storeSecretDataSet: %v", serr)
		}
		if serr := attachSecretdataVolumes(ctx, vols, cfg.StorageRoot, discardLoggerMain(), secretdataRefsIndependent); serr != nil {
			t.Fatalf("先行补装: %v", serr)
		}
		if vols.External("vault") != nil {
			t.Fatal("引用嵌套 secrets 卷的 secretdata 卷不得在 AttachAll 之前补装（此时密钥不可解析）")
		}
		if aerr := enclosure.AttachAll(ctx, vols, discardLoggerMain(), enclosure.AttachOptions{StorageRoot: cfg.StorageRoot}); aerr != nil {
			t.Fatalf("AttachAll（嵌套 secrets 卷装配）: %v", aerr)
		}
		if serr := attachSecretdataVolumes(ctx, vols, cfg.StorageRoot, discardLoggerMain(), secretdataRefsEnclosure); serr != nil {
			t.Fatalf("后置补装（secret_url 指向嵌套 secrets 卷）: %v", serr)
		}

		// ① 嵌套 secrets 卷 + 配置声明的 secretdata 卷都已装配。
		for _, name := range []string{"nested-keys", "vault"} {
			if layerBE := vols.External(name); layerBE == nil || layerBE.FS() == nil {
				t.Fatalf("卷 %q 未装配", name)
			}
		}
		// ② 数据面：密钥取自嵌套 secrets 卷 ⇒ vault 真能透明加解密（写读往返 + 密文落盘）。
		const body = "config-nested-secret payload"
		const key = "alice/user/n.bin"
		vbe := vols.External("vault")
		if werr := vbe.FS().WriteFile(ctx, syncpkg.LogicalKey{}, key, strings.NewReader(body), int64(len(body)), 0); werr != nil {
			t.Fatalf("vault 写入: %v", werr)
		}
		rc, oerr := vbe.FS().OpenRead(ctx, key)
		if oerr != nil {
			t.Fatalf("vault 读回: %v", oerr)
		}
		got, rerr := io.ReadAll(rc)
		_ = rc.Close()
		if rerr != nil || string(got) != body {
			t.Fatalf("读回=%q err=%v，want %q", got, rerr, body)
		}
		if plaintextOnDisk(t, dataRoot, body) {
			t.Fatal("vault 写入应以密文落盘（分块加密未生效）")
		}
	})
	if werr := be.FS().WriteFile(ctx, syncpkg.LogicalKey{}, "a.mp4", strings.NewReader("hello secret"), 12, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	rc, err := be.FS().OpenRead(ctx, "a.mp4")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	buf, _ := io.ReadAll(rc)
	rc.Close()
	if string(buf) != "hello secret" {
		t.Errorf("还原=%q", buf)
	}
}

// TestSecretdataVolume_PerOwnerDefaultSecrets per-owner 默认 secrets 桶的 `secret_url` 寻址
// （用户裁定 2026-10-15 第 3 点）：authority = `default-secrets`（或 `default`/空）→ **请求 owner
// 的**默认桶（`<默认卷根>/<owner>/meta/secrets/`——owner 租户根的 meta 桶内，与 `/api/secrets`
// 同一份密钥）；解析需要 ctx 中的 owner（带身份的建卷请求注入，见 pkg/server createUserVolumeHandler
// 的 `secrets.WithOwner`）。
// 无 owner（装配期服务级声明）→ fail-closed 并提示替代写法（显式命名 secrets 卷）。
//
// 变异：把 per-owner 分支改回「一律拒绝」→ ① 红；去掉 owner 归属直接读共享目录 → ③ 红
// （bob 会读到 alice 的密钥）。
func TestSecretdataVolume_PerOwnerDefaultSecrets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	set := newTestSet(t)
	typ := "secretdata-perowner-test"
	registerSecretdataBackendWithFS(typ, func(ctx context.Context, v volume.Volume) ([]byte, error) {
		return defaultSecretdataSecret(ctx, v, set)
	})
	t.Cleanup(func() { registry.UnregisterBackendForTest(typ) })

	// alice 的默认桶里**手工放置**密钥（与 HTTP 面 /api/secrets 写入同一路径
	// `<默认卷根>/<owner>/meta/secrets/<名>`——meta 桶内；跨模块等价性由 pkg/server 的 handler
	// 用例 + e2e 磁盘副作用断言覆盖）。
	const aliceKey = "7c9f1a3b5d7e90123456789abcdef0123456789abcdef0123456789abcdef012"
	aliceBucket := filepath.Join(set.DefaultRoot().AbsPath(), "alice", "meta", "secrets")
	if err := os.MkdirAll(aliceBucket, 0o700); err != nil {
		t.Fatalf("mkdir alice bucket: %v", err)
	}
	if werr := os.WriteFile(filepath.Join(aliceBucket, "datakey"), []byte(aliceKey), 0o600); werr != nil {
		t.Fatalf("write alice datakey: %v", werr)
	}

	dataRoot := t.TempDir()
	sd := volume.Volume{Name: "sd", Type: typ, RootDir: dataRoot, Extra: map[string]any{
		"target":     "local",
		"root":       dataRoot,
		"secret_url": "secrets://default-secrets/datakey",
	}}

	// ① 带 owner 上下文 → 解析到该 owner 的桶，透明加解密可用。
	be, berr := registry.NewBackend(secrets.WithOwner(ctx, "alice"), sd)
	if berr != nil {
		t.Fatalf("带 owner 上下文构造应成功（per-owner 默认桶）: %v", berr)
	}
	t.Cleanup(func() { _ = be.Close() })
	const body = "per-owner secret payload"
	if werr := be.FS().WriteFile(ctx, syncpkg.LogicalKey{}, "a.bin", strings.NewReader(body), int64(len(body)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	rc, oerr := be.FS().OpenRead(ctx, "a.bin")
	if oerr != nil {
		t.Fatalf("OpenRead: %v", oerr)
	}
	got, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil || string(got) != body {
		t.Fatalf("读回=%q err=%v，want %q", got, rerr, body)
	}
	// 磁盘落点：密钥在 alice 的默认桶（不是共享目录、也不是别人桶）。
	if _, serr := os.Stat(filepath.Join(set.DefaultRoot().AbsPath(), "alice", "meta", "secrets", "datakey")); serr != nil {
		t.Errorf("密钥应落 <默认卷根>/alice/meta/secrets/datakey: %v", serr)
	}

	// ② 无 owner（装配期/服务级声明）→ fail-closed + 替代写法提示。
	be2, err2 := registry.NewBackend(ctx, sd)
	if err2 == nil {
		_ = be2.Close()
		t.Fatal("装配期（无 owner 上下文）应拒绝解析 per-owner 默认桶")
	}
	if !strings.Contains(err2.Error(), "无 owner") || !strings.Contains(err2.Error(), "显式命名") {
		t.Errorf("错误应说明无 owner 上下文并给出替代写法, got %v", err2)
	}

	// ③ 跨 owner：bob 的桶里没有该密钥 → 构造失败（不串号、不回落 alice 或共享目录）。
	if be3, err3 := registry.NewBackend(secrets.WithOwner(ctx, "bob"), sd); err3 == nil {
		_ = be3.Close()
		t.Fatal("bob 默认桶无该密钥时应失败（不得读到 alice 的密钥）")
	}
}

// TestSetupSecretBackends_ConfigSecretdata_BackfillFail_BootFail（修复轮 I1 守护）：config
// 声明 type:secretdata 卷补装失败 → `attachSecretdataVolumes` 返回错误（调用方 boot fail，
// fail-closed）——不得再 WARN 静默降级。注入失败：secret_url 指向不存在的密钥
// （secrets://keys/nokey）→ 补装 `registry.NewBackend` 解析失败 → 返回错误并指明卷名。
func TestSetupSecretBackends_ConfigSecretdata_BackfillFail_BootFail(t *testing.T) {
	// sproxy:serial: 依赖生产注册路径全局单例（secretDataSet + 生产类型 Once）。
	ctx := context.Background()
	registerSecretVolumeBackends()
	t.Cleanup(func() { secretDataSet.Store(nil) })

	localRoot := t.TempDir()
	dataRoot := t.TempDir()
	root := filepath.Join(t.TempDir(), "default")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rt, rerr := storage.OpenRoot(root)
	if rerr != nil {
		t.Fatalf("OpenRoot: %v", rerr)
	}
	vault := volume.Volume{Name: "vault", Type: "secretdata", RootDir: dataRoot,
		Extra: map[string]any{
			"target":     "local",
			"root":       dataRoot,
			"secret_url": "secrets://keys/nokey", // 密钥不存在 → 补装解析失败
		}}
	set := registry.NewSet(
		[]volume.Volume{{Name: "default", Type: volume.TypeLocal, RootDir: root}, vault},
		map[string]*storage.Root{"default": rt},
		map[string]registry.ExternalBackend{},
		map[string]*quota.Pool{"default": quota.NewPool(0), "vault": quota.NewPool(100)},
		"default",
	)
	t.Cleanup(func() { _ = set.Close() })

	// 默认 secrets 卷会正常装配（localRoot 可写），但 vault 的 secret_url 解析失败 → 补装
	// 返回错误（boot fail-closed，不得静默消失）。
	if serr := storeSecretDataSet(set); serr != nil {
		t.Fatalf("storeSecretDataSet: %v", serr)
	}
	err := attachSecretdataVolumes(ctx, set, localRoot, nil, secretdataRefsIndependent)
	if err == nil {
		t.Fatal("config 声明 secretdata 卷补装失败应返回错误（boot fail-closed），却成功")
	}
	if !strings.Contains(err.Error(), "vault") {
		t.Errorf("错误应指明失败卷名 vault，got %v", err)
	}
}

// reattachWrapType 是「重启重装配」用例专用的封装处理层名（避免与生产注册表冲突）。
const reattachWrapType = "reattach-secretdata-test"

// reattachWrapper 用**真实 secretdata**（固定密钥）做处理层：足以验证「二次装配复用既有字节
// 且能解密读回」，同时不触碰生产全局注册路径。
type reattachWrapper struct{}

func (reattachWrapper) Name() string { return reattachWrapType }

func (reattachWrapper) Build(ctx context.Context, v volume.Volume, src syncpkg.FS, _ map[string]any) (registry.ExternalBackend, error) {
	tmp, terr := os.MkdirTemp("", "reattach-tmp")
	if terr != nil {
		return nil, terr
	}
	return secretdata.NewBackend(ctx, v, src, secretdata.Options{
		Secret: []byte("reattach-fixed-key-material-01"), MetaPadBytes: 4096, TempDir: tmp,
	})
}

var reattachWrapperOnce sync.Once

func registerReattachWrapper() {
	reattachWrapperOnce.Do(func() { enclosure.RegisterWrapper(reattachWrapper{}) })
}

// TestEnclosureVolume_RestartReattach（阶段 2 迁移：重启收养语义的底座等价物）：嵌套封装卷在
// 迁移后是**封装声明**（`source: default:videos` + `wrapper: <处理层>`，卷 Type 保持 wrapper
// 类型），装配由
// `applyEnclosureSeals`（封印）+ `AttachAll`（推迟装配）完成。
//
// 重启时底层子目录（default/videos）**已经存在且含上次写入的字节**——过渡层为此需要
// `adoptNestedDir` 标记做「自建目录收养」（否则「需新空子目录」boot-fail）；enclosure 底座
// **没有「子目录须不存在」前置条件**（区域本就是既有数据的落点，同一区间可被声明唯一占有），
// 故二次装配天然成功且复用既有字节——本用例钉住该等价能力（去掉它 = 重启 boot-fail 回归）。
func TestEnclosureVolume_RestartReattach(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	registerReattachWrapper()

	root := filepath.Join(t.TempDir(), "default")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir default: %v", err)
	}
	rt, rerr := storage.OpenRoot(root)
	if rerr != nil {
		t.Fatalf("OpenRoot: %v", rerr)
	}
	vols := []volume.Volume{
		{Name: "default", Type: volume.TypeLocal, RootDir: root},
		{Name: "vault", Type: reattachWrapType, Capacity: 100, Extra: map[string]any{
			"source": "default:videos",
		}},
	}
	newSet := func() *registry.Set {
		return registry.NewSet(
			append([]volume.Volume(nil), vols...),
			map[string]*storage.Root{"default": rt},
			map[string]registry.ExternalBackend{},
			map[string]*quota.Pool{"default": quota.NewPool(0), "vault": quota.NewPool(100)},
			"default",
		)
	}
	// 装配步骤与生产 boot 顺序一致：源卷封印（assembleVolumes 内做）→ 封装后端补装。
	attach := func(set *registry.Set) {
		t.Helper()
		if serr := rt.SetSeals([]storage.SealRule{{Path: "videos", Wrapper: "vault", Mode: storage.SealHidden}}); serr != nil {
			t.Fatalf("SetSeals: %v", serr)
		}
		if aerr := enclosure.AttachAll(ctx, set, nil, enclosure.AttachOptions{StorageRoot: t.TempDir()}); aerr != nil {
			t.Fatalf("AttachAll: %v", aerr)
		}
	}

	set := newSet()
	t.Cleanup(func() { _ = set.Close() })
	attach(set)
	be := set.External("vault")
	if be == nil || be.FS() == nil {
		t.Fatal("首次装配后 vault backend 应可寻址")
	}
	const body = "persisted across restart"
	if werr := be.FS().WriteFile(ctx, syncpkg.LogicalKey{}, "alice/user/keep.bin", strings.NewReader(body), int64(len(body)), 0); werr != nil {
		t.Fatalf("写入封装卷: %v", werr)
	}
	if _, serr := os.Stat(filepath.Join(root, "videos")); serr != nil {
		t.Fatalf("装配后应有 default/videos 区域: %v", serr)
	}
	// 物理落点在区域之内（前缀映射生效，未外溢到源卷其它位置）：区域内有真实字节，
	// 且源卷根除 LAYOUT_VERSION 外无新增顶层条目。
	if n := dirBytes(t, filepath.Join(root, "videos")); n == 0 {
		t.Fatal("封装字节应落地 default/videos 之内（区域内应有落盘字节）")
	}
	ents, rerr2 := os.ReadDir(root)
	if rerr2 != nil {
		t.Fatalf("ReadDir: %v", rerr2)
	}
	for _, e := range ents {
		if e.Name() != "videos" && e.Name() != "LAYOUT_VERSION" {
			t.Fatalf("封装卷不应在源卷根新增其它条目: %s", e.Name())
		}
	}

	// 「重启」：同一声明、子目录已存在且含数据 → 二次装配必须成功（无收养门禁）且旧数据可读回。
	set2 := newSet()
	t.Cleanup(func() { _ = set2.Close() })
	attach(set2)
	be2 := set2.External("vault")
	if be2 == nil || be2.FS() == nil {
		t.Fatal("重启后 vault backend 应可寻址且 FS 可用（enclosure 无「子目录须不存在」门禁）")
	}
	rc, oerr := be2.FS().OpenRead(ctx, "alice/user/keep.bin")
	if oerr != nil {
		t.Fatalf("重启后应能读回既有数据: %v", oerr)
	}
	defer func() { _ = rc.Close() }()
	got, rerr2 := io.ReadAll(rc)
	if rerr2 != nil {
		t.Fatalf("ReadAll: %v", rerr2)
	}
	if string(got) != body {
		t.Fatalf("重启后数据应为 %q, got %q", body, got)
	}
}

// dirBytes 返回目录树下全部文件的字节合计（离路径/不可读目录跳过）。
func dirBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // 遍历容错：仅统计已可读文件字节
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// TestEnclosureWrapper_Secretdata_ProductionAssembly（S4/S5 收尾）：
// `type: enclosure` + `wrapper: secretdata` 是**文档与 config.example 承诺的用法**，但此前
// 生产装配层**没有任何 wrapper 注册**（`RegisterWrapper` 只在测试里被调用）⇒
// `enclosure.Parse` 恒 `ErrUnknownWrapper`、`AttachAll` 失败 = **封装卷在生产不可用（boot
// fail）**，且设计 §5 的防护薄层（禁直链/身份）从未接线。
//
// 本用例走生产路径，一次钉住四件事：
//  1. 「生产已注册可用的 wrapper」（删掉注册 → 本用例红）；
//  2. 声明可解析 + 后端可补装（boot 不再 fail）；
//  3. 写经封装卷 → 源目录落**密文**（明文字节不出现在源目录任何文件里）+ 读回明文；
//  4. S5 真值：链上物理字节报告者可用（`secretdata` 在 wrapper 位置 = 最外层实现者），
//     且真值 > 逻辑大小（meta pad/分块头膨胀）——封装卷配额不再被逻辑大小放大绕过。
//
// sproxy:serial: 生产注册路径（全局注册表 Once + secretDataSet 单例 + 共享卷集），不并发。
func TestEnclosureWrapper_Secretdata_ProductionAssembly(t *testing.T) {
	ctx := context.Background()
	// 生产注册顺序：secret 后端类型（含 secretdata）+ 本 PR 新增的封装 wrapper 注册。
	registerSecretVolumeBackends()
	registerEnclosureWrappers()

	// 装配卷集：源卷 main（本地）+ 封装卷 vault（main 的 alice/user/vault 子树）。
	root := t.TempDir()
	rt, err := storage.OpenRoot(root)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	vols := []volume.Volume{
		{Name: "main", Type: volume.TypeLocal, RootDir: root},
		{Name: "vault", Type: volume.TypeSecretdata, Extra: map[string]any{
			"source":     "main:alice/user/vault",
			"secret_url": "secrets://keys/datakey",
		}},
	}
	set := registry.NewSet(vols, map[string]*storage.Root{"main": rt}, map[string]registry.ExternalBackend{},
		map[string]*quota.Pool{"main": quota.NewPool(0), "vault": quota.NewPool(0)}, "main")
	t.Cleanup(func() { _ = set.Close() })

	// 1) 声明解析：wrapper 必须已被生产注册（此前恒 ErrUnknownWrapper）。
	if _, perr := enclosure.Parse(vols); perr != nil {
		t.Fatalf("生产装配下封装声明应可解析（wrapper: secretdata 须已注册）: %v", perr)
	}

	// 显式声明的 secrets 卷 + 密钥（secretdata 的 secret_url 解析目标；默认 secrets 卷自
	// 2026-10-15 起是 per-owner 的、不在已装配卷集内 ⇒ 装配期无 owner 上下文不可寻址）。
	localRoot := t.TempDir()
	// 生产装配序前两步（本卷集声明的 vault 是封装声明卷 ⇒ 交 AttachAll 装配）。
	if err := storeSecretDataSet(set); err != nil {
		t.Fatalf("storeSecretDataSet: %v", err)
	}
	if err := attachSecretdataVolumes(ctx, set, localRoot, nil, secretdataRefsIndependent); err != nil {
		t.Fatalf("attachSecretdataVolumes（先行）: %v", err)
	}
	t.Cleanup(func() { secretDataSet.Store(nil) })
	mgr := assembleTestSecretsVolume(t, ctx, set, "keys", localRoot)
	if _, cerr := mgr.Create(ctx, "datakey"); cerr != nil {
		t.Fatalf("Create key: %v", cerr)
	}

	// 2) 补装（生产 boot 路径；此前这里会因 wrapper 未注册而 boot fail）。
	if aerr := enclosure.AttachAll(ctx, set, nil); aerr != nil {
		t.Fatalf("封装卷补装（生产路径）: %v", aerr)
	}
	be := set.External("vault")
	if be == nil || be.FS() == nil {
		t.Fatal("封装卷后端未挂回卷集")
	}

	// 3) 写读往返：用户可见明文。
	const body = "hello enclosure volume"
	key := "alice/user/docs/x.bin" // 面级键 = <owner>/<bucket>/<rel>（FSPath 形态）
	if werr := be.FS().WriteFile(ctx, syncpkg.LogicalKey{}, key, strings.NewReader(body), int64(len(body)), 0); werr != nil {
		t.Fatalf("封装卷写入: %v", werr)
	}
	rc, oerr := be.FS().OpenRead(ctx, key)
	if oerr != nil {
		t.Fatalf("封装卷读取: %v", oerr)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != body {
		t.Fatalf("明文还原=%q，期望 %q", got, body)
	}
	// 源目录里**不得出现明文**（at-rest 加密真实生效；源目录唯一可见形态是密文分块）。
	if plain := findPlaintext(t, root, body); plain != "" {
		t.Fatalf("源目录出现明文（at-rest 加密未生效）: %s", plain)
	}
	// 且内容确实落在声明的子树下（前缀映射生效，未外溢）：子树内**有真实字节**，
	// 否则「源目录无明文」只是空目录的假绿。
	var bytes int64
	if werr := filepath.WalkDir(filepath.Join(root, "alice", "user", "vault"),
		func(_ string, e fs.DirEntry, werr error) error {
			if werr != nil || e.IsDir() {
				return nil //nolint:nilerr // 遍历容错：仅统计已可读文件字节
			}
			if info, ierr := e.Info(); ierr == nil {
				bytes += info.Size()
			}
			return nil
		}); werr != nil {
		t.Fatalf("遍历源子树: %v", werr)
	}
	if bytes == 0 {
		t.Fatal("源子树内应有落盘字节（封装卷内容确实写入源卷子树）")
	}

	// 4) S5 真值报告：链上最外层实现者 = wrapper 位置的 secretdata。
	rep, ok := capacity.TrueUsageOf(be.FS())
	if !ok {
		t.Fatal("封装卷链上应有物理字节报告者（secretdata wrapper）")
	}
	truth, tok := rep.TrueUsage()
	if !tok || truth <= int64(len(body)) {
		t.Fatalf("真值应 > 逻辑大小（分块头/meta pad 膨胀）: truth=%d ok=%v", truth, tok)
	}
	if sz, sok := rep.TrueSize(key); !sok || sz < int64(len(body)) {
		t.Fatalf("单文件真值应 >= 逻辑大小: %d ok=%v", sz, sok)
	}
}

// findPlaintext 在 root 下递归找含 needle 的文件（返回路径；无则空串）。
func findPlaintext(t *testing.T, root, needle string) string {
	t.Helper()
	var found string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" {
			return nil //nolint:nilerr // 遍历容错：目录读失败不影响「是否出现明文」判定
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		if bytes.Contains(b, []byte(needle)) {
			found = p
		}
		return nil
	})
	return found
}

// TestSecretsVolume_EncryptedRoot_StoresCiphertext `type: secrets`（target=local）卷落在**受管
// 卷根**下时：若该卷启用了 at-rest 加密（`extra.encrypt`），密钥文件必须同样**加密落盘**
// ——此前装配可能用裸 `NewLocalFS(<root>/secrets)`（纯 os I/O）⇒ 卷内容全加密而**密钥明文**
// 落盘，at-rest 加密对该配置形同虚设（门禁 R26 登记项里记的「待裁定」项）。
//
// 装配走生产两步（`secretsDirFSFor` 解析视图 + `secrets.NewBackend` 构造，不经全局注册表 ⇒ 可并发）。
//
// 判据：磁盘字节不得等于明文 hex 密钥；经 Manager 读回与创建值一致；本地 0600 加固保留
// （`ChmodSecret` 回调由 NewBackend 依视图能力自动接线——经 Root 的视图不是 `*LocalFS`，
// 仅靠旧的类型断言会**静默跳过**加固）。
//
// 变异：装配回退裸 LocalFS → 密文/权限断言红。
func TestSecretsVolume_EncryptedRoot_StoresCiphertext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	rt, err := storage.OpenRoot(root)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	if serr := rt.SetEncryption(bytes.Repeat([]byte{5}, 32)); serr != nil {
		t.Fatalf("SetEncryption: %v", serr)
	}
	set := registry.NewSet(
		[]volume.Volume{{Name: "default", Type: volume.TypeLocal, RootDir: root}},
		map[string]*storage.Root{"default": rt},
		map[string]registry.ExternalBackend{}, nil, "default")
	t.Cleanup(func() { _ = set.Close() })
	registerSecretsBackend()

	v := volume.Volume{Name: "cfg-secrets", Type: "secrets", RootDir: root,
		Extra: map[string]any{"target": "local", "root": root}}
	be, berr := secrets.NewBackend(ctx, v, secretsDirFSFor(set, root, nil))
	if berr != nil {
		t.Fatalf("secrets.NewBackend: %v", berr)
	}
	mgr := secrets.ManagerOfExternal(be)
	if mgr == nil {
		t.Fatal("ManagerOfExternal 反取失败")
	}
	key, err := mgr.Create(ctx, "datakey")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	secretAbs := filepath.Join(root, "secrets", "datakey")
	raw, rerr := os.ReadFile(secretAbs)
	if rerr != nil {
		t.Fatalf("读落盘 secret: %v", rerr)
	}
	if runtime.GOOS != "windows" {
		// 本地 0600 加固必须随「经 Root」的视图保留（回调 `secrets.WithLocalChmod` 承接——
		// 经 Root 的视图不是 *LocalFS，旧的类型断言会静默跳过）。
		if fi, serr := os.Stat(secretAbs); serr != nil {
			t.Fatalf("Stat: %v", serr)
		} else if fi.Mode().Perm() != 0o600 {
			t.Fatalf("secret 权限应为 0600（本地加固）, got %o", fi.Mode().Perm())
		}
	}
	if string(raw) == string(key) {
		t.Fatalf("加密卷上的 secret 不得明文落盘（got %q）", raw)
	}
	got, err := mgr.Read(ctx, "datakey")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(key) {
		t.Fatalf("读回密钥 = %q，期望 %q", got, key)
	}
}

// TestSecretsDirFSFor_SubdirLayoutAndRootSemantics `type: secrets` 卷的**目录布局**
// 必须是 `<root>/secrets/<名>`，且落在受管卷根内的那部分必须**经 Root**
// （封印/加解密/迁移随之生效）。此前的实现把**父目录**本身当键空间（漏拼 `/secrets`）⇒ 密钥
// 文件落错位置、`secret_url` 解析失败 → secretdata 卷装配 boot fail（e2e_cli_secretdata 实测）。
//
// 变异：`secretsFSFor` 收父目录（漏 `/secrets` 拼接）→ 落盘位置断言红；回落裸 LocalFS → 封印断言红。
func TestSecretsDirFSFor_SubdirLayoutAndRootSemantics(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rt, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	set := registry.NewSet(
		[]volume.Volume{{Name: "main", Type: volume.TypeLocal, RootDir: dir}},
		map[string]*storage.Root{"main": rt},
		map[string]registry.ExternalBackend{}, nil, "main")
	t.Cleanup(func() { _ = set.Close() })
	fsys := secretsDirFSFor(set, dir, nil)
	ctx := context.Background()
	if werr := fsys.WriteFile(ctx, syncpkg.LogicalKey{}, "datakey", strings.NewReader("v"), 1, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	if _, serr := os.Stat(filepath.Join(dir, "secrets", "datakey")); serr != nil {
		t.Fatalf("密钥应落 <root>/secrets/<名>（与默认 secrets 卷同布局）: %v", serr)
	}
	if _, serr := os.Stat(filepath.Join(dir, "datakey")); serr == nil {
		t.Fatal("密钥不得落在父目录本身（漏拼 /secrets 段）")
	}
	// Root 语义：封印必须生效（证明视图经 storage.Root，而非裸 LocalFS）。
	if serr := rt.SetSeals([]storage.SealRule{{Path: "secrets", Wrapper: "vault"}}); serr != nil {
		t.Fatalf("SetSeals: %v", serr)
	}
	if werr := fsys.WriteFile(ctx, syncpkg.LogicalKey{}, "k2", strings.NewReader("v"), 1, 0); !errors.Is(werr, storage.ErrSealed) {
		t.Fatalf("封印应生效（经 Root 的视图）, got %v", werr)
	}
}

// TestDefaultSecretsFS_LocalTargetSubdirLayout `type: secrets` 卷（target=local + root=父目录）
// 的 FS 视图（`defaultSecretsFS`）必须把**父目录 + /secrets** 作为键空间。
func TestDefaultSecretsFS_LocalTargetSubdirLayout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	fsys, err := defaultSecretsFS(context.Background(), volume.Volume{
		Name: "mykeys", Type: "secrets", Extra: map[string]any{"target": "local", "root": root},
	})
	if err != nil {
		t.Fatalf("defaultSecretsFS: %v", err)
	}
	if werr := fsys.WriteFile(context.Background(), syncpkg.LogicalKey{}, "datakey", strings.NewReader("v"), 1, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	if _, serr := os.Stat(filepath.Join(root, "secrets", "datakey")); serr != nil {
		t.Fatalf("密钥应落 <root>/secrets/<名>: %v", serr)
	}
}

// TestEnclosureWrapper_Secrets_ProductionAssembly：`type: enclosure` + `wrapper: secrets`
// 此前在生产装配下恒 `ErrUnknownWrapper`（只注册了 secretdata）⇒ secrets 卷无法作为封装卷
// 装配（阶段 2 迁移的 target→enclosure 翻译会产出该形态，故必须注册）。
// 变异：去掉 registerEnclosureWrappers 里的 secretsWrapper 注册 → 本用例红。
//
// 可并发：只用封装处理层注册（Once + RWMutex 读安全）与**本用例自建**的卷集，不触碰
// secretDataSet 等跨用例全局状态。
func TestEnclosureWrapper_Secrets_ProductionAssembly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	registerEnclosureWrappers()

	if w, ok := enclosure.LookupWrapper("secrets"); !ok || w.Name() != "secrets" {
		t.Fatalf("生产装配必须注册 secrets wrapper（否则 secrets 封装卷 boot fail）: ok=%v", ok)
	}

	root := t.TempDir()
	rt, err := storage.OpenRoot(root)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	vols := []volume.Volume{
		{Name: "main", Type: volume.TypeLocal, RootDir: root},
		{Name: "keys", Type: "secrets", Extra: map[string]any{
			"source": "main:alice/user/vault",
		}},
	}
	set := registry.NewSet(vols, map[string]*storage.Root{"main": rt}, map[string]registry.ExternalBackend{},
		map[string]*quota.Pool{"main": quota.NewPool(0), "keys": quota.NewPool(0)}, "main")
	t.Cleanup(func() { _ = set.Close() })

	if _, perr := enclosure.Parse(vols); perr != nil {
		t.Fatalf("secrets 封装声明应可解析（wrapper 须已注册）: %v", perr)
	}
	if aerr := enclosure.AttachAll(ctx, set, nil); aerr != nil {
		t.Fatalf("secrets 封装卷补装（生产路径）: %v", aerr)
	}
	be := set.External("keys")
	if be == nil || be.FS() == nil {
		t.Fatal("secrets 封装卷后端未挂回卷集")
	}
	// secrets 后端把源视图当文件树用：写入即可读回（内容明文——秘密文件本身靠封印保护）。
	const body = "s3cr3t-key-material"
	key := "alice/user/k1"
	if werr := be.FS().WriteFile(ctx, syncpkg.LogicalKey{}, key, strings.NewReader(body), int64(len(body)), 0); werr != nil {
		t.Fatalf("secrets 封装卷写入: %v", werr)
	}
	rc, oerr := be.FS().OpenRead(ctx, key)
	if oerr != nil {
		t.Fatalf("secrets 封装卷读回: %v", oerr)
	}
	defer func() { _ = rc.Close() }()
	got, rerr := io.ReadAll(rc)
	if rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	if string(got) != body {
		t.Fatalf("读回内容不符: %q", got)
	}
	// 物理落点：确实写在 source 声明的子树之内（前缀映射生效、未外溢）。
	found := findPlaintext(t, root, body)
	if found == "" {
		t.Fatal("secrets 封装卷内容应落盘（源目录找不到写入的字节）")
	}
	if !strings.Contains(filepath.ToSlash(found), "/alice/user/vault/") {
		t.Fatalf("封装内容应落在声明的 source 子树内, got %s", found)
	}
}

// plaintextOnDisk 报告 dir 下任一文件内容包含明文子串（封装卷落盘不变量：区域内的字节恒为
// 密文容器/meta，不含明文负载）。
// chainExtBackend 是「外部底座多层链」用例的 fake 外部后端（只提供 FS/Close，镜像
// pkg/server 的 fakeExtBackend）：物理落点是一个本地临时目录，便于断言「密文落盘」与
// 「封印直连写被拒」。它替代 baidupcs/s3 这类需要真实账号/网络的外部后端——装配侧对二者的
// 接线口径相同（registry.NewBackend 构造 → 封印套在 be.FS() → 封装卷取 UnsealedSource 特权视图）。
type chainExtBackend struct{ fs syncpkg.FS }

func (b chainExtBackend) FS() syncpkg.FS { return b.fs }
func (b chainExtBackend) Close() error   { return nil }

func plaintextOnDisk(t *testing.T, dir, plaintext string) bool {
	t.Helper()
	found := false
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // 遍历错误/目录跳过（命中明文才是断言目标）
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if strings.Contains(string(data), plaintext) {
			found = true
		}
		return nil
	})
	return found
}

// TestSecretdataVolume_QuotaAtFSLayer 钉住 **secretdata 卷配额接线**（用户要求「各种卷
// 操作后配额正确」）：config 声明 secretdata 卷（target local ⇒ WithLedgerDirIfLocal 本地
// 账本）上传记账、超限拒绝、删除释放——配额在 FS 层真实生效且与池同源。
func TestSecretdataVolume_QuotaAtFSLayer(t *testing.T) {
	t.Parallel()
	registerSecretVolumeBackends()
	t.Cleanup(func() { secretDataSet.Store(nil) })

	localRoot := t.TempDir()
	dataRoot := t.TempDir()
	root := filepath.Join(t.TempDir(), "default")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rt, rerr := storage.OpenRoot(root)
	if rerr != nil {
		t.Fatalf("OpenRoot: %v", rerr)
	}
	vault := volume.Volume{Name: "vault", Type: "secretdata", RootDir: dataRoot, Capacity: 100,
		Extra: map[string]any{"target": "local", "root": dataRoot, "secret_url": "secrets://keys/datakey"}}
	set := registry.NewSet(
		[]volume.Volume{{Name: "default", Type: volume.TypeLocal, RootDir: root}, vault},
		map[string]*storage.Root{"default": rt},
		map[string]registry.ExternalBackend{},
		map[string]*quota.Pool{"default": quota.NewPool(0), "vault": quota.NewPool(100)},
		"default",
	)
	t.Cleanup(func() { _ = set.Close() })

	ctx := context.Background()
	mgr := assembleTestSecretsVolume(t, ctx, set, "keys", localRoot)
	if _, err := mgr.Create(ctx, "datakey"); err != nil {
		t.Fatalf("Create key: %v", err)
	}
	if err := storeSecretDataSet(set); err != nil {
		t.Fatalf("storeSecretDataSet: %v", err)
	}
	if err := attachSecretdataVolumes(ctx, set, localRoot, nil, secretdataRefsIndependent); err != nil {
		t.Fatalf("attachSecretdataVolumes: %v", err)
	}
	be := set.External("vault")
	if be == nil {
		t.Fatal("vault 未装配")
	}
	fs := be.FS()

	// 1) 首次写 60 记账成功（secretdata 加密卷 + 本地账本链路）。
	if err := fs.WriteFile(ctx, storage.LogicalKey{}, "a.bin", strings.NewReader(strings.Repeat("x", 60)), 60, 0); err != nil {
		t.Fatalf("写入 60 应成功: %v", err)
	}
	if got := set.Pool("vault").Usage(); got != 60 {
		t.Fatalf("写入后卷用量 %d, want 60", got)
	}
	// 2) 二次写 60 超限（120>100）必须被拒。
	if err := fs.WriteFile(ctx, storage.LogicalKey{}, "b.bin", strings.NewReader(strings.Repeat("y", 60)), 60, 0); err == nil {
		t.Fatal("第二次写 60 超限应被拒绝")
	}
	if got := set.Pool("vault").Usage(); got != 60 {
		t.Fatalf("超限拒绝后卷用量 %d, want 60（不得入账）", got)
	}
	// 3) Delete 释放 → 池归零。
	if err := fs.Delete(ctx, "a.bin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := set.Pool("vault").Usage(); got != 0 {
		t.Fatalf("删除后卷用量 %d, want 0", got)
	}
	// 4) 释放后可再写（配额回路打通）。
	if err := fs.WriteFile(ctx, storage.LogicalKey{}, "c.bin", strings.NewReader(strings.Repeat("z", 60)), 60, 0); err != nil {
		t.Fatalf("释放后写入 60 应成功: %v", err)
	}
	// 5) 账本落**节点本地**（target local ⇒ WithLedgerDirIfLocal 生效）——不污染加密卷
	//    物理布局（否则账本 blob 混入 secretdata 内容分块）。这条钉住接线：若本地账本
	//    未生效，目录不存在 ⇒ 本断言必红。
	if _, err := os.Stat(filepath.Join(localRoot, "meta", "quota", "vault")); err != nil {
		t.Fatalf("secretdata 卷账本应落节点本地目录（WithLedgerDirIfLocal 未生效）: %v", err)
	}
}
