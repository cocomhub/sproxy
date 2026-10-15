// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/quota"
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
// 迁移后是 **enclosure 声明**（`source: default:videos` + `wrapper: <处理层>`），装配由
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
		{Name: "vault", Type: enclosure.TypeEnclosure, Capacity: 100, Extra: map[string]any{
			"source": "default:videos", "wrapper": reattachWrapType,
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
	if werr := be.FS().WriteFile(ctx, "alice/user/keep.bin", strings.NewReader(body), int64(len(body)), 0); werr != nil {
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
		{Name: "vault", Type: enclosure.TypeEnclosure, Extra: map[string]any{
			"source":     "main:alice/user/vault",
			"wrapper":    "secretdata",
			"secret_url": "secrets://default/datakey",
		}},
	}
	set := registry.NewSet(vols, map[string]*storage.Root{"main": rt}, map[string]registry.ExternalBackend{},
		map[string]*quota.Pool{"main": quota.NewPool(0), "vault": quota.NewPool(0)}, "main")
	t.Cleanup(func() { _ = set.Close() })

	// 1) 声明解析：wrapper 必须已被生产注册（此前恒 ErrUnknownWrapper）。
	if _, perr := enclosure.Parse(vols); perr != nil {
		t.Fatalf("生产装配下封装声明应可解析（wrapper: secretdata 须已注册）: %v", perr)
	}

	// 默认 secrets 卷 + 密钥（secretdata 的 secret_url 解析目标）。
	localRoot := t.TempDir()
	if err := setupSecretBackends(ctx, set, localRoot, nil); err != nil {
		t.Fatalf("setupSecretBackends: %v", err)
	}
	t.Cleanup(func() { secretDataSet.Store(nil) })
	mgr := secrets.ManagerOfExternal(set.External("default-secrets"))
	if mgr == nil {
		t.Fatal("默认 secrets 卷反取失败")
	}
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
	if werr := be.FS().WriteFile(ctx, key, strings.NewReader(body), int64(len(body)), 0); werr != nil {
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

// TestDefaultSecretsVolume_EncryptedRoot_StoresCiphertext 默认 secrets 卷落在**默认卷根**下的
// `secrets/`：若该卷启用了 at-rest 加密（`extra.encrypt`），密钥文件必须同样**加密落盘**
// ——此前装配用裸 `NewLocalFS(<storage_root>/secrets)`（纯 os I/O）⇒ 卷内容全加密而**密钥明文**
// 落盘，at-rest 加密对该配置形同虚设（门禁 R26 登记项里记的「待裁定」项）。
//
// 判据：磁盘字节不得等于明文 hex 密钥，且经 Manager 读回应与创建值一致。
//
// 变异：装配回退裸 LocalFS → 本用例红。
func TestDefaultSecretsVolume_EncryptedRoot_StoresCiphertext(t *testing.T) {
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

	mgr, err := ensureDefaultSecretsVolume(ctx, set, root, nil)
	if err != nil {
		t.Fatalf("ensureDefaultSecretsVolume: %v", err)
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
		t.Fatalf("加密默认卷上的 secret 不得明文落盘（got %q）", raw)
	}
	got, err := mgr.Read(ctx, "datakey")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(key) {
		t.Fatalf("读回密钥 = %q，期望 %q", got, key)
	}
}

// TestSecretsDirFSFor_SubdirLayoutAndRootSemantics `type: secrets`/默认 secrets 卷的**目录布局**
// 必须是 `<root>/secrets/<名>`（两个入口同布局），且落在受管卷根内的那部分必须**经 Root**
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
	fsys, _ := secretsDirFSFor(set, dir, nil)
	ctx := context.Background()
	if werr := fsys.WriteFile(ctx, "datakey", strings.NewReader("v"), 1, 0); werr != nil {
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
	if werr := fsys.WriteFile(ctx, "k2", strings.NewReader("v"), 1, 0); !errors.Is(werr, storage.ErrSealed) {
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
	if werr := fsys.WriteFile(context.Background(), "datakey", strings.NewReader("v"), 1, 0); werr != nil {
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
		{Name: "keys", Type: enclosure.TypeEnclosure, Extra: map[string]any{
			"source": "main:alice/user/vault", "wrapper": "secrets",
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
	if werr := be.FS().WriteFile(ctx, key, strings.NewReader(body), int64(len(body)), 0); werr != nil {
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
