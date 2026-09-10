// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// e2e_cli_volumes_test.go 覆盖多卷配置下的 sclient 卷能力（真二进制 + 子进程）：
// volumes / upload --volume / list --volume / mv --to-volume（跨卷迁移）。
//
// 卷配置（extraConfig）：
//   - main（volumes[0]，root 留空）→ 归一为 --storage-root 目录，仍是默认卷，
//     从而凭据 store 种子路径 <uploadsDir>/anonymous/meta/credentials.json 不变；
//   - disk2（root 指向独立临时目录）。
//
// placement: prefer-default 使新文件落默认卷，确保 --volume 是唯一的落卷决定因素。
package sproxy_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
)

// volListResp 是 `volumes --json` / GET /api/volumes 的响应容器。
type volListResp struct {
	Volumes []client.VolumeInfo `json:"volumes"`
}

// findVolume 在 vols 中按名查找。
func findVolume(vols []client.VolumeInfo, name string) (client.VolumeInfo, bool) {
	for _, v := range vols {
		if v.Name == name {
			return v, true
		}
	}
	return client.VolumeInfo{}, false
}

// cliFileList 运行 `list --json`（可带 --volume 等前置 flag）并返回文件列表。
//
// 空列表在 JSON 模式下输出为空（cmd/sclient/list.go 走 fm.Println，
// JSONFormatter 对该调用 no-op）→ 空输出即零文件，不得当作解析失败。
func cliFileList(t *testing.T, env *cliEnv, prefix ...string) []client.FileInfo {
	t.Helper()
	args := append(append([]string{"--json"}, prefix...), "list")
	out := env.sclient(t, env.TmpDir, args...)
	if strings.TrimSpace(out) == "" {
		return nil
	}
	var resp struct {
		Files []client.FileInfo `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("list --json 解析失败: %v\nstdout:\n%s", err, out)
	}
	return resp.Files
}

// hasFileNamed 报告 files 中是否存在指定文件名。
func hasFileNamed(files []client.FileInfo, name string) bool {
	for _, f := range files {
		if f.Name == name {
			return true
		}
	}
	return false
}

// TestE2E_CLI_Volumes 覆盖多卷 CLI 全链路：
// volumes 列表（CLI + API 交叉核对）→ 显式落 disk2 卷 → 分卷 list 隔离 →
// 跨卷 mv 到 main 卷（磁盘 + API checksum 双证）→ 未知卷负例。
func TestE2E_CLI_Volumes(t *testing.T) {
	// disk2 的卷根目录：位于独立临时目录，与默认卷根（uploadsDir）物理隔离。
	disk2Root := filepath.Join(t.TempDir(), "disk2")
	extraConfig := fmt.Sprintf(
		"placement: prefer-default\nvolumes:\n  - name: main\n  - name: disk2\n    root: %q\n",
		filepath.ToSlash(disk2Root))
	env := startCLIEnv(t, extraConfig)

	// 1) volumes --json：main 与 disk2 均可见、allowed、缺省 ACL = deny（默认开放）
	var vols volListResp
	env.sclientJSON(t, env.TmpDir, &vols, "volumes")
	for _, name := range []string{"main", "disk2"} {
		v, ok := findVolume(vols.Volumes, name)
		if !ok {
			t.Fatalf("volumes 应含 %q, got %+v", name, vols.Volumes)
		}
		if !v.Allowed {
			t.Fatalf("卷 %q 应 allowed, got %+v", name, v)
		}
		if v.Mode != "deny" {
			t.Fatalf("卷 %q 缺省 ACL mode 应为 deny（默认开放）, got %q", name, v.Mode)
		}
	}
	// 接口交叉核对：GET /api/volumes 卷名集合一致
	var apiVols volListResp
	getJSON(t, env.BaseURL+"/api/volumes", &apiVols)
	if len(apiVols.Volumes) != len(vols.Volumes) {
		t.Fatalf("GET /api/volumes 卷数应与 CLI 一致: %d vs %d", len(apiVols.Volumes), len(vols.Volumes))
	}
	for _, name := range []string{"main", "disk2"} {
		if _, ok := findVolume(apiVols.Volumes, name); !ok {
			t.Fatalf("GET /api/volumes 应含 %q, got %+v", name, apiVols.Volumes)
		}
	}

	// 2) 上传显式落到 disk2 卷
	content := []byte("volume cli content")
	checksum := sha256hex(content)
	if err := os.WriteFile(filepath.Join(env.TmpDir, "volfile.txt"), content, 0644); err != nil {
		t.Fatalf("写本地文件失败: %v", err)
	}
	env.sclient(t, env.TmpDir, "--volume", "disk2", "upload", "volfile.txt")

	// 磁盘副作用：落在 disk2 卷根而非默认卷根
	onDisk2 := findFilesNamed(t, disk2Root, "volfile.txt")
	if len(onDisk2) != 1 {
		t.Fatalf("--volume disk2 上传后 disk2 卷根应恰有 1 个 volfile.txt, got %d: %v", len(onDisk2), onDisk2)
	}
	if got, err := os.ReadFile(onDisk2[0]); err != nil {
		t.Fatalf("读取 disk2 落盘文件失败: %v", err)
	} else if string(got) != string(content) {
		t.Fatalf("disk2 落盘内容不一致: got %q, want %q", got, content)
	}
	if got := findFilesNamed(t, env.StorageRoot, "volfile.txt"); len(got) != 0 {
		t.Fatalf("默认卷根不应有 volfile.txt, got %v", got)
	}

	// 3) 分卷 list：disk2 可见且带卷名，main 不可见
	disk2Files := cliFileList(t, env, "--volume", "disk2")
	if !hasFileNamed(disk2Files, "volfile.txt") {
		t.Fatalf("disk2 卷列表应含 volfile.txt, got %+v", disk2Files)
	}
	for _, f := range disk2Files {
		if f.Name == "volfile.txt" && f.Volume != "disk2" {
			t.Fatalf("volfile.txt 的卷名应为 disk2, got %q", f.Volume)
		}
	}
	if mainFiles := cliFileList(t, env, "--volume", "main"); hasFileNamed(mainFiles, "volfile.txt") {
		t.Fatalf("main 卷列表不应含 volfile.txt, got %+v", mainFiles)
	}

	// 4) 跨卷迁移：disk2 → main（同相对路径）
	env.sclient(t, env.TmpDir, "--volume", "disk2", "mv", "volfile.txt", "volfile.txt", "--to-volume", "main")

	if got := findFilesNamed(t, disk2Root, "volfile.txt"); len(got) != 0 {
		t.Fatalf("跨卷迁移后 disk2 卷根不应再有 volfile.txt, got %v", got)
	}
	moved := findFilesNamed(t, env.StorageRoot, "volfile.txt")
	if len(moved) != 1 {
		t.Fatalf("跨卷迁移后默认卷根应恰有 1 个 volfile.txt, got %d: %v", len(moved), moved)
	}
	if got, err := os.ReadFile(moved[0]); err != nil {
		t.Fatalf("读取迁移后文件失败: %v", err)
	} else if sha256hex(got) != checksum {
		t.Fatalf("迁移后文件 checksum 应为 E: got %s, want %s", sha256hex(got), checksum)
	}
	// 接口：auto 定位到迁移后的文件且 checksum 一致
	status, headers := statFile(t, env.BaseURL, "volfile.txt")
	if status != http.StatusOK {
		t.Fatalf("迁移后 stat 应 200, got %d", status)
	}
	if got := headers.Get("X-File-Checksum"); got != checksum {
		t.Fatalf("迁移后 stat checksum 不一致: got %s, want %s", got, checksum)
	}

	// 5) 负例：未知卷 → 非零退出（只读操作，无副作用）
	stdout, stderr, err := env.sclientRun(t, env.TmpDir, "--volume", "nosuchvol", "list")
	if err == nil {
		t.Fatalf("未知卷应非零退出\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}

	// 6) 负例：向未知卷上传 → 非零退出，且**两个卷根均无该文件**（无副作用断言）。
	//    比只断错误码更强：失败的上传若在服务端产生任何落盘/卷状态变化都会被检出。
	if werr := os.WriteFile(filepath.Join(env.TmpDir, "nosuchvol.txt"), []byte("should not land"), 0644); werr != nil {
		t.Fatalf("写本地文件失败: %v", werr)
	}
	stdout, stderr, err = env.sclientRun(t, env.TmpDir, "--volume", "nosuchvol", "upload", "nosuchvol.txt")
	if err == nil {
		t.Fatalf("向未知卷上传应非零退出\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if got := findFilesPrefixed(t, env.StorageRoot, "nosuchvol.txt"); len(got) != 0 {
		t.Fatalf("失败的上传不应在默认卷落盘, got %v", got)
	}
	if got := findFilesPrefixed(t, disk2Root, "nosuchvol.txt"); len(got) != 0 {
		t.Fatalf("失败的上传不应在 disk2 卷落盘, got %v", got)
	}
	var volsAfter volListResp
	getJSON(t, env.BaseURL+"/api/volumes", &volsAfter)
	if len(volsAfter.Volumes) != len(vols.Volumes) {
		t.Fatalf("失败的上传不应改变卷集合: %d -> %d", len(vols.Volumes), len(volsAfter.Volumes))
	}
}
