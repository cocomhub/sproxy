// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
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
	if got := vcExtraBool(volume.Volume{Extra: map[string]any{"erasure": true}}, "erasure"); !got {
		t.Error("vcExtraBool(true) 应为 true")
	}
	if got := vcExtraBool(volume.Volume{Extra: map[string]any{"erasure": false}}, "erasure"); got {
		t.Error("vcExtraBool(false) 应为 false")
	}
	if got := vcExtraBool(volume.Volume{Extra: map[string]any{}}, "erasure"); got {
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

// TestEnsureDefaultSecretsVolume 启动默认建本地卷作默认 secrets 卷（§9.1）。
func TestEnsureDefaultSecretsVolume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	set := newTestSet(t)
	root := t.TempDir()
	mgr, err := ensureDefaultSecretsVolume(ctx, set, root, nil)
	if err != nil {
		t.Fatalf("ensureDefaultSecretsVolume: %v", err)
	}
	if mgr == nil {
		t.Fatal("返回 nil Manager")
	}
	if set.External("default-secrets") == nil {
		t.Fatal("默认 secrets 卷未挂到 Set.External")
	}
	// 通过 Manager 创建并读回一个 secret，验证链路可用。
	if _, cerr := mgr.Create(ctx, "testkey"); cerr != nil {
		t.Fatalf("Create: %v", cerr)
	}
	if got, rerr := mgr.Read(ctx, "testkey"); rerr != nil || len(got) == 0 {
		t.Errorf("Read: got=%d len err=%v", len(got), rerr)
	}
	// 默认 secrets 卷应恰落 <root>/secrets（设计 §6.1/§9），不得双重 append 成
	// <root>/secrets/secrets。
	if _, serr := os.Stat(filepath.Join(root, "secrets", "testkey")); serr != nil {
		t.Errorf("默认 secrets 卷未落 <root>/secrets/testkey: %v", serr)
	}
	if _, serr := os.Stat(filepath.Join(root, "secrets", "secrets", "testkey")); serr == nil {
		t.Error("默认 secrets 卷错误落到 <root>/secrets/secrets（双重 append）")
	}
	// 幂等：重复调用不报错、返回同一 Manager（读取同一密钥）。
	mgr2, err := ensureDefaultSecretsVolume(ctx, set, root, nil)
	if err != nil {
		t.Fatalf("二次装配: %v", err)
	}
	if got, err := mgr2.Read(ctx, "testkey"); err != nil || len(got) == 0 {
		t.Errorf("二次装配读回: err=%v", err)
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
	v := volume.Volume{Name: "sv", Type: typ, RootDir: root, Extra: map[string]any{"target": "local", "root": root}}
	be, err := registry.NewBackend(context.Background(), v)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	if be == nil || be.FS() == nil {
		t.Fatal("backend 或 FS 为 nil")
	}
	// 用 FS 真跑一次：写 + 读。
	if werr := be.FS().WriteFile(context.Background(), "k", strings.NewReader("val"), 3, 0); werr != nil {
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
	if _, err := ensureDefaultSecretsVolume(ctx, set, localRoot, nil); err != nil {
		t.Fatalf("ensureDefaultSecretsVolume: %v", err)
	}
	// 生产装配 setupSecretBackends 会调用 registerSecretsBackend()（声明协议 "secrets"，
	// 供 secret_url 的 ResolveURL 寻址）。此处显式调用使本测试**自包含**——不依赖
	// 其它测试触发 registerSecretsOnce 的套件顺序（隔离运行/-shuffle 也能通过）。
	registerSecretsBackend()
	// 给 secretdata 卷造密钥：在默认 secrets 卷建一个。
	mgr := secrets.ManagerOfExternal(set.External("default-secrets"))
	if mgr == nil {
		t.Fatal("默认 secrets 卷 ManagerOfExternal 反取失败")
	}
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
		"secret_url": "secrets://default/datakey",
	}}
	be, err := registry.NewBackend(ctx, v)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	// 直接 WriteFile/OpenRead 透明加解密。
	if werr := be.FS().WriteFile(ctx, "a.mp4", strings.NewReader("hello secret"), 12, 0); werr != nil {
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
// registerSecretVolumeBackends() + setupSecretBackends() 在**与 sync 解耦**下打通：
//   - registerSecretVolumeBackends 早于卷装配即注册 secrets/secretdata 生产类型
//     （config 声明 type:secretdata/secrets 卷时 assembleVolumes 不会「未注册后端」panic）；
//   - setupSecretBackends 把装配卷集 Store 进 secretDataSet → 生产 "secretdata" 类型经
//     secret_url（secrets://default/datakey）解析密钥 → NewBackend → 透明加解密可用。
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
	// 第二步：卷装配（模拟 RegisterRoutes 完成后）——默认卷 + 默认 secrets 卷 + Store 集成卷。
	set := newTestSet(t)
	localRoot := t.TempDir()
	if err := setupSecretBackends(ctx, set, localRoot, nil); err != nil {
		t.Fatalf("setupSecretBackends: %v", err)
	}
	if set.External("default-secrets") == nil {
		t.Fatal("默认 secrets 卷未挂到 Set.External")
	}
	// 造密钥 + 经生产 "secretdata" 类型 NewBackend（走 secretDataSet 懒解析）。
	mgr := secrets.ManagerOfExternal(set.External("default-secrets"))
	if mgr == nil {
		t.Fatal("默认 secrets 卷 ManagerOfExternal 反取失败")
	}
	if _, err := mgr.Create(ctx, "datakey"); err != nil {
		t.Fatalf("Create key: %v", err)
	}
	dataRoot := t.TempDir()
	v := volume.Volume{Name: "sd", Type: "secretdata", RootDir: dataRoot, Extra: map[string]any{
		"target":     "local",
		"root":       dataRoot,
		"secret_url": "secrets://default/datakey",
	}}
	be, err := registry.NewBackend(ctx, v)
	if err != nil {
		t.Fatalf("NewBackend(生产 secretata): %v", err)
	}
	if werr := be.FS().WriteFile(ctx, "a.mp4", strings.NewReader("hello secret"), 12, 0); werr != nil {
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

// TestSetupSecretBackends_Secretdata_MultiTarget（任务 9d 修复轮 Imp-1）：extra.targets
// 多 local root → 生产多 target 装配生效——装配层解析副本 local root 构造副本底层 FS →
// 写后主/副本两 root 都有同一容器（副本复制运行，非仅记账）。跨外部卷接线留后续片。
func TestSetupSecretBackends_Secretdata_MultiTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	set := newTestSet(t)
	if _, err := ensureDefaultSecretsVolume(ctx, set, t.TempDir(), nil); err != nil {
		t.Fatalf("ensureDefaultSecretsVolume: %v", err)
	}
	registerSecretsBackend()
	mgr := secrets.ManagerOfExternal(set.External("default-secrets"))
	if mgr == nil {
		t.Fatal("默认 secrets 卷 ManagerOfExternal 反取失败")
	}
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
		"secret_url": "secrets://default/datakey",
		"targets":    []string{replicaRoot},
	}}
	be, err := registry.NewBackend(ctx, v)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	if werr := be.FS().WriteFile(ctx, "a.mp4", strings.NewReader("hello secret"), 12, 0); werr != nil {
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
		"secret_url": "secrets://default/datakey",
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
// type:secretdata 卷（如 config.example vault）在 RegisterRoutes 返回后由 setupSecretBackends
// **补装**——assembleVolumes 对 deferred 类型跳过 backend 构造但登记卷元数据（含 Extra/
// Capacity），set 就绪后本函数遍历 set.All 找到未装配的 secretdata 卷 → registry.NewBackend
// （此时 secretDataSet 已 Store、secrets 卷可 ResolveURL）→ AttachExternal 挂回 Set。断言：
//   - config 声明 secretdata 卷补装后可寻址（Set.External 非 nil）且透明加解密可用；
//   - secrets 卷（config 声明 + 默认）优先装配，secretdata 的 secret_url 可解析。
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
			"secret_url": "secrets://default/datakey",
		}}
	set := registry.NewSet(
		[]volume.Volume{{Name: "default", Type: volume.TypeLocal, RootDir: root}, vault},
		map[string]*storage.Root{"default": rt},
		map[string]registry.ExternalBackend{},
		map[string]*quota.Pool{"default": quota.NewPool(0), "vault": quota.NewPool(100)},
		"default",
	)
	t.Cleanup(func() { _ = set.Close() })

	// 先建默认 secrets 卷 + 造密钥（setupSecretBackends 补装 secretdata 卷时需解析
	// secrets://default/datakey——密钥须先于补装存在）。
	if _, err := ensureDefaultSecretsVolume(ctx, set, localRoot, nil); err != nil {
		t.Fatalf("ensureDefaultSecretsVolume: %v", err)
	}
	mgr := secrets.ManagerOfExternal(set.External("default-secrets"))
	if mgr == nil {
		t.Fatal("默认 secrets 卷 ManagerOfExternal 反取失败")
	}
	if _, err := mgr.Create(ctx, "datakey"); err != nil {
		t.Fatalf("Create key: %v", err)
	}

	// setupSecretBackends：Store set + 确保默认 secrets 卷（幂等）+ 补装 config 声明
	// secretdata 卷（此时密钥已存在、secrets 卷可 ResolveURL）。
	if err := setupSecretBackends(ctx, set, localRoot, nil); err != nil {
		t.Fatalf("setupSecretBackends: %v", err)
	}
	// config 声明 secretdata 卷补装后可寻址（Imp-2 核心：不再「卷集未就绪」恒失败）。
	be := set.External("vault")
	if be == nil {
		t.Fatal("config 声明 secretdata 卷补装后应可寻址（Set.External 非 nil）")
	}
	if be.FS() == nil {
		t.Fatal("secretdata backend FS 为 nil")
	}
	if werr := be.FS().WriteFile(ctx, "a.mp4", strings.NewReader("hello secret"), 12, 0); werr != nil {
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

// TestSetupSecretBackends_ConfigSecretdata_BackfillFail_BootFail（修复轮 I1 守护）：config
// 声明 type:secretdata 卷补装失败 → `setupSecretBackends` 返回错误（调用方 boot fail，
// fail-closed）——不得再 WARN 静默降级。注入失败：secret_url 指向不存在的密钥
// （secrets://default/nokey）→ 补装 `registry.NewBackend` 解析失败 → 返回错误并指明卷名。
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
			"secret_url": "secrets://default/nokey", // 密钥不存在 → 补装解析失败
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
	err := setupSecretBackends(ctx, set, localRoot, nil)
	if err == nil {
		t.Fatal("config 声明 secretdata 卷补装失败应返回错误（boot fail-closed），却成功")
	}
	if !strings.Contains(err.Error(), "vault") {
		t.Errorf("错误应指明失败卷名 vault，got %v", err)
	}
}

// TestSetupSecretBackends_DefaultSecretsFail_NotFatal（修复轮 I1 守护）：**默认 secrets 卷**
// 装配失败仅降级（WARN，不阻断 boot）——卷集已含同名 "default-secrets" 卷元数据（无
// external）→ ensureDefaultSecretsVolume 的 AddExternalVolume 因重名失败 → setupSecretBackends
// 记 WARN 继续，返回 nil（与 config 声明卷补装失败的 boot fail 区分）。
func TestSetupSecretBackends_DefaultSecretsFail_NotFatal(t *testing.T) {
	// sproxy:serial: 依赖生产注册路径全局单例（secretDataSet + 生产类型 Once）。
	ctx := context.Background()
	registerSecretVolumeBackends()
	t.Cleanup(func() { secretDataSet.Store(nil) })

	localRoot := t.TempDir()
	root := filepath.Join(t.TempDir(), "default")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rt, rerr := storage.OpenRoot(root)
	if rerr != nil {
		t.Fatalf("OpenRoot: %v", rerr)
	}
	// 卷集 volumes 已含 "default-secrets" 卷元数据（无 external）→ 默认 secrets 卷无法
	// AddExternalVolume（重名）→ ensureDefaultSecretsVolume 失败（默认 secrets 降级）。
	set := registry.NewSet(
		[]volume.Volume{
			{Name: "default", Type: volume.TypeLocal, RootDir: root},
			{Name: "default-secrets", Type: "secrets", RootDir: localRoot,
				Extra: map[string]any{"target": "local", "root": localRoot}},
		},
		map[string]*storage.Root{"default": rt},
		map[string]registry.ExternalBackend{},
		map[string]*quota.Pool{"default": quota.NewPool(0)},
		"default",
	)
	t.Cleanup(func() { _ = set.Close() })

	// 默认 secrets 卷装配失败 → 仅降级，不返回错误（不阻断 boot）；无 config 声明 secretdata
	// 卷，补装循环为空。
	if err := setupSecretBackends(ctx, set, localRoot, nil); err != nil {
		t.Fatalf("默认 secrets 卷失败应仅降级（WARN，不阻断 boot），got %v", err)
	}
}
