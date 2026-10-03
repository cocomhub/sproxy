// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// e2e_cli_secretdata_test.go 覆盖 secret 加密卷的端到端用例（真二进制 + 子进程）：
//
//   - config 声明 `type: secrets` + `type: secretdata` 卷可装配（Imp-2 修复守护：此前
//     assembleVolumes 对 secretdata 恒「卷集未就绪」失败，config.example vault 示例不可用）——
//     服务器能正常启动 + GET /api/volumes 可见两卷即装配证明；
//   - 跨包互操作：独立挂载方用同一密钥重开同一 vault 根 → 加密/解密 roundtrip + Delete
//     即时物理删（磁盘无 meta/分块残留）+ 底层匿名性（容器目录 5-30 随机、无 data/meta 结构词）；
//   - 多 target 副本复制：写后主/副本双 root 都含同一容器目录（副本复制运行，非仅记账）。
//
// 说明（可达性边界）：当前 server 写面（pkg/files）只走 storage.Root（本地卷）；外部卷
// （secretdata）写/读/删不暴露于普通 HTTP 文件面（无对应端点），故加密卷内容的端到端
// 验证经「服务器装配 + 独立挂载同一根」完成，而不是 CLI upload/download——CLI 真流程对
// 加密卷的不可达属架构现状（报告已记录，见 task-12-report.md）。
package sproxy_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/secretdata"
)

// secretKeyBytes 是 e2e 测试密钥（32B hex，服务器与独立挂载方共用同一字节）。
const secretKeyBytes = "0123456789abcdef0123456789abcdef"

// newSecretReader 用指定密钥挂载 root 上的 secretdata 卷视图（与服务器装配同路径：
// syncpkg.NewLocalFS(root) → secretdata.NewFS；小块策略加速测试）。
func newSecretReader(t *testing.T, root string, secret []byte) *secretdata.SecretdataFS {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	fs, err := secretdata.NewFS(syncpkg.NewLocalFS(root, nil), secretdata.Options{
		Secret:  secret,
		Block:   shardseal.BlockPolicy{Mode: "random", Min: 4096, Max: 16384},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("secretdata.NewFS(%s): %v", root, err)
	}
	return fs
}

// seedSecretKey 在 secrets 卷的本地视图落盘密钥文件（<keysRoot>/secrets/datakey）——
// 服务器 setupSecretBackends 启动期即解析 secret_url 读该文件，须先于服务器启动存在。
func seedSecretKey(t *testing.T, keysRoot string) []byte {
	t.Helper()
	secret := []byte(secretKeyBytes)
	secretDir := filepath.Join(keysRoot, "secrets")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatalf("mkdir secrets 卷: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secretDir, "datakey"), secret, 0o600); err != nil {
		t.Fatalf("写密钥文件: %v", err)
	}
	return secret
}

// listContainerDirs 列出 root 下的容器目录名（secretdata 卷随机容器；排序）。
func listContainerDirs(t *testing.T, root string) []string {
	t.Helper()
	inner := syncpkg.NewLocalFS(root, nil)
	entries, err := inner.ListDir(context.Background(), "")
	if err != nil {
		t.Fatalf("ListDir(%s): %v", root, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir {
			out = append(out, e.Name)
		}
	}
	sort.Strings(out)
	return out
}

// walkSecretRoot 递归遍历 root 下全部普通文件（basename 交给 match）。
func walkSecretRoot(t *testing.T, root string, match func(name string) bool) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			if os.IsNotExist(werr) {
				return nil
			}
			return werr
		}
		if !d.IsDir() && match(d.Name()) {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir %s: %v", root, err)
	}
	sort.Strings(found)
	return found
}

// assertSecretLayoutAnonymity 断言底层卷匿名性：根下容器目录名 5-30 随机、不含
// data/meta/secret 结构词；递归全文件不含明文逻辑文件名（磁盘上不出明文元数据）。
func assertSecretLayoutAnonymity(t *testing.T, root string, logicalNames ...string) {
	t.Helper()
	containers := listContainerDirs(t, root)
	if len(containers) == 0 {
		t.Fatal("写入后底层应含至少一个随机容器目录")
	}
	for _, c := range containers {
		if len(c) < 5 || len(c) > 30 {
			t.Errorf("容器目录名长度 %d 超出 5-30: %q", len(c), c)
		}
		if c == "data" || c == "meta" || c == "secret" || (len(c) >= 6 && c[:6] == "secret") {
			t.Errorf("容器目录名不应含 data/meta/secret 结构词: %q", c)
		}
	}
	// 全文件 basename 不得等于任何明文逻辑文件名（加密文件名三段 hex+随机，恒不泄逻辑名）。
	for _, f := range walkSecretRoot(t, root, func(string) bool { return true }) {
		base := filepath.Base(f)
		for _, ln := range logicalNames {
			if base == ln {
				t.Errorf("底层出现明文逻辑文件名 %q（路径 %s）", ln, f)
			}
		}
	}
}

// assertNoFileDataBlobs 断言底层无文件 meta（-/_）与分块（无标记）残留——即时物理删后
// 仅允许容器目录 meta（@，根容器恒保留）。
func assertNoFileDataBlobs(t *testing.T, root string) {
	t.Helper()
	for _, f := range walkSecretRoot(t, root, func(string) bool { return true }) {
		name := filepath.Base(f)
		if shardseal.IsDirMetaName(name) {
			continue // 容器目录 meta（@）允许（根容器恒保留）
		}
		t.Errorf("Delete 后底层不应残留文件 meta/分块 %q（路径 %s）", name, f)
	}
}

// TestE2E_CLI_Secretdata_VolumeAssembledAndInterop：config 声明 secrets+secretdata 卷
// 装配（Imp-2）→ 服务器启动成功；独立挂载方用同一密钥重开同一 vault 根 → 加密/解密
// roundtrip + Delete 即时物理删 + 底层匿名性。
func TestE2E_CLI_Secretdata_VolumeAssembledAndInterop(t *testing.T) {
	t.Parallel()
	keysRoot := filepath.Join(t.TempDir(), "keys")
	vaultRoot := filepath.Join(t.TempDir(), "vault")
	secret := seedSecretKey(t, keysRoot)

	extraConfig := fmt.Sprintf(`volumes:
  - name: main
  - name: mykeys
    type: secrets
    extra:
      target: local
      root: %q
  - name: vault
    type: secretdata
    extra:
      target: local
      root: %q
      secret_url: secrets://mykeys/datakey
`, keysRoot, vaultRoot)
	env := startCLIEnv(t, extraConfig)

	// 1) 装配成功（Imp-2 守护）：server 正常 boot（healthz OK 已由 startCLIEnv 保证——
	// setupSecretBackends 补装失败会导致启动失败）；GET /api/volumes 可见两卷。
	var vols volListResp
	getJSON(t, env.BaseURL+"/api/volumes", &vols)
	for _, name := range []string{"mykeys", "vault"} {
		if _, ok := findVolume(vols.Volumes, name); !ok {
			t.Fatalf("volumes 应含 %q（config 声明 secretdata/secrets 卷装配失败），got %+v",
				name, vols.Volumes)
		}
	}

	// 2) 跨包互操作：独立挂载方（同一密钥）重开同一 vault 根 → 加密写入 + 读回。
	fs := newSecretReader(t, vaultRoot, secret)
	ctx := context.Background()
	content := []byte("secret volume e2e payload: 加密卷目录隐私与即时物理删验证")
	if err := fs.WriteFile(ctx, "docs/top.txt", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("secretdata WriteFile: %v", err)
	}

	// 3) 底层匿名性（无 data/meta 结构词、容器 5-30 随机、无明文逻辑名）。
	assertSecretLayoutAnonymity(t, vaultRoot, "docs", "top.txt", "docs/top.txt")

	// 4) 读回内容一致（加密→解密 roundtrip）。
	rc, err := fs.OpenRead(ctx, "docs/top.txt")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	got, rerr := io.ReadAll(rc)
	rc.Close()
	if rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("解密内容不一致: len(got)=%d len(want)=%d", len(got), len(content))
	}

	// 5) Delete 即时物理删：meta + 分块从磁盘容器清空（无残留）。
	if err := fs.Delete(ctx, "docs/top.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	assertNoFileDataBlobs(t, vaultRoot)

	// 6) 重新挂载（重启模拟）→ 已删文件不可见（即时删不靠墓碑/GC 的确定性验证）。
	fs2 := newSecretReader(t, vaultRoot, secret)
	if e, _ := fs2.Stat(ctx, "docs/top.txt"); e != nil {
		t.Error("Delete 后重新挂载不应再可见该文件（即时物理删）")
	}
}

// TestE2E_CLI_Secretdata_MultiTargetReplica：多 target 副本复制——config 声明
// extra.targets 副本 root；独立挂载方用 NewBackendMultiplicas（与服务器装配同路径）
// 写入后主/副本双 root 都含同一容器目录、内容可读回。
func TestE2E_CLI_Secretdata_MultiTargetReplica(t *testing.T) {
	t.Parallel()
	keysRoot := filepath.Join(t.TempDir(), "keys")
	primaryRoot := filepath.Join(t.TempDir(), "primary")
	replicaRoot := filepath.Join(t.TempDir(), "replica")
	secret := seedSecretKey(t, keysRoot)

	extraConfig := fmt.Sprintf(`volumes:
  - name: main
  - name: mykeys
    type: secrets
    extra:
      target: local
      root: %q
  - name: mt
    type: secretdata
    extra:
      target: local
      root: %q
      secret_url: secrets://mykeys/datakey
      targets:
        - %q
`, keysRoot, primaryRoot, replicaRoot)
	env := startCLIEnv(t, extraConfig)
	var vols volListResp
	getJSON(t, env.BaseURL+"/api/volumes", &vols)
	if _, ok := findVolume(vols.Volumes, "mt"); !ok {
		t.Fatalf("volumes 应含 mt（多 target secretdata 卷装配失败），got %+v", vols.Volumes)
	}

	// 跨包多 target 集成：与服务器装配同路径（NewBackendMultiplicas），写后双 root 一致。
	v := volume.Volume{Name: "mt", Type: "secretdata",
		Extra: map[string]any{"target": "local", "root": primaryRoot, "secret_url": "secrets://mykeys/datakey"}}
	be, err := secretdata.NewBackendMultiplicas(context.Background(), v,
		syncpkg.NewLocalFS(primaryRoot, nil),
		[]syncpkg.FS{syncpkg.NewLocalFS(replicaRoot, nil)},
		secretdata.Options{Secret: secret, Block: shardseal.BlockPolicy{Mode: "random", Min: 4096, Max: 16384}, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewBackendMultiplicas: %v", err)
	}
	fs := be.FS()
	content := []byte("multi-target replica content")
	if werr := fs.WriteFile(context.Background(), "rep.txt", bytes.NewReader(content), int64(len(content)), 0); werr != nil {
		t.Fatalf("多 target WriteFile: %v", werr)
	}
	// 读回内容一致。
	rc, err := fs.OpenRead(context.Background(), "rep.txt")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, content) {
		t.Fatal("多 target 读回内容不一致")
	}
	// 主/副本双 root 都含同一容器目录（副本复制运行，非仅记账）。
	primaryContainers := listContainerDirs(t, primaryRoot)
	replicaContainers := listContainerDirs(t, replicaRoot)
	if len(primaryContainers) != 1 {
		t.Fatalf("主 target 应含 1 个容器目录，got %+v", primaryContainers)
	}
	if len(replicaContainers) != 1 {
		t.Fatalf("副本 target 应含 1 个容器目录（副本复制未生效），got %+v", replicaContainers)
	}
	if primaryContainers[0] != replicaContainers[0] {
		t.Errorf("主/副本容器目录名不一致：%q vs %q", primaryContainers[0], replicaContainers[0])
	}
	// 主/副本双 root 的文件集合一致（容器自包含、字节复制）。
	primaryFiles := walkSecretRoot(t, primaryRoot, func(string) bool { return true })
	replicaFiles := walkSecretRoot(t, replicaRoot, func(string) bool { return true })
	if len(primaryFiles) != len(replicaFiles) {
		t.Errorf("主/副本文件集合不一致：%d vs %d", len(primaryFiles), len(replicaFiles))
	}
	for i := range primaryFiles {
		if filepath.Base(primaryFiles[i]) != filepath.Base(replicaFiles[i]) {
			t.Errorf("主/副本文件 basename 不一致：%q vs %q", primaryFiles[i], replicaFiles[i])
		}
	}
}
