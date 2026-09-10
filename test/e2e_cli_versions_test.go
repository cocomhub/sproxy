// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// e2e_cli_versions_test.go 覆盖 sclient `meta version` 命令族（真二进制 + 子进程）：
// list / restore / delete 的接口 + 磁盘版本文件效应，以及 versioning 缺省关闭时的
// 501 语义（CLI 非零退出）。
package sproxy_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
)

// versioningConfig 打开文件版本管理并把上限设为 5（供 startCLIEnv 的 extraConfig）。
const versioningConfig = "versioning:\n  enabled: true\n  max_versions: 5\n"

// versionListResp 是 `meta version list --json` / GET /api/versions 的响应容器。
// VersionID/Size 必须用 int64（服务端版本 ID 为毫秒时间戳×1000+随机后缀，float64 会丢精度）。
type versionListResp struct {
	Filename string               `json:"filename"`
	Versions []client.VersionInfo `json:"versions"`
}

// findVersion 在 versions 中按 id 查找，返回 (版本, 是否存在)。
func findVersion(versions []client.VersionInfo, id int64) (client.VersionInfo, bool) {
	for _, v := range versions {
		if v.VersionID == id {
			return v, true
		}
	}
	return client.VersionInfo{}, false
}

// readLocalFile 读取本地文件内容（失败即 Fatalf）。
func readLocalFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return data
}

// onlyFileNamed 断言 root 下 basename == name 的普通文件恰有一个，返回其路径。
func onlyFileNamed(t *testing.T, root, name string) string {
	t.Helper()
	found := findFilesNamed(t, root, name)
	if len(found) != 1 {
		t.Fatalf("应恰有 1 个 %s, got %d: %v", name, len(found), found)
	}
	return found[0]
}

// versionOp 执行一次版本 restore/delete 操作（op ∈ {"restore","delete"}）。
//
// 无条件走 CLI 契约路径（真 sclient 子进程）：
// `sclient meta version <op> <filename> <version_id>`；并断言 CLI 自身的文本输出，
// 以此**证明该操作确实由 sclient 二进制执行**（HTTP 直调不会产生这些 stdout）。
//
// versionID 由服务端 newVersionID() 生成（毫秒时间戳 ×1000 + 随机后缀 + 进程内单调递增），
// 恒为正；客户端 RestoreVersion/DeleteVersion 的 `versionID <= 0` 守卫不再被触发。
// 此处**不做任何按符号分派/HTTP 兜底**——一旦 ID 再次变负，CLI 会立刻失败并使本用例变红，
// 从而把该回归挡在门禁内，而不是被测试的兜底逻辑掩盖。
func versionOp(t *testing.T, env *cliEnv, op, filename string, versionID int64) {
	t.Helper()
	idStr := strconv.FormatInt(versionID, 10)
	var wantMsg string
	switch op {
	case "restore":
		wantMsg = "已恢复文件 '" + filename + "' 到版本 " + idStr
	case "delete":
		wantMsg = "已删除文件 '" + filename + "' 的版本 " + idStr
	default:
		t.Fatalf("未知版本操作 %q", op)
	}
	out := env.sclient(t, env.TmpDir, "meta", "version", op, filename, idStr)
	if !strings.Contains(out, wantMsg) {
		t.Fatalf("%s 应由真实 CLI 子进程执行，stdout 应含 %q, got:\n%s", op, wantMsg, out)
	}
}

// TestE2E_CLI_VersionsLifecycle 覆盖版本管理完整生命周期：
// 覆盖上传保存旧版本 → list（CLI + API 交叉核对 + 磁盘版本文件内容）→
// restore（当前文件回到旧内容、restore 前自动备份）→ delete（版本文件消失、当前文件不动）。
func TestE2E_CLI_VersionsLifecycle(t *testing.T) {
	env := startCLIEnv(t, versioningConfig)

	v1 := []byte("version one content")
	v2 := []byte("version two content!!") // 与 v1 等长不同内容
	checksumV1 := sha256hex(v1)
	checksumV2 := sha256hex(v2)

	localPath := filepath.Join(env.TmpDir, "vfile.txt")

	// 1) 首传 V1（无旧文件 → 不产生版本）
	if err := os.WriteFile(localPath, v1, 0644); err != nil {
		t.Fatalf("写 V1 失败: %v", err)
	}
	env.sclient(t, env.TmpDir, "upload", "vfile.txt")

	// 2) 覆盖上传 V2 → 保存 V1 为历史版本
	if err := os.WriteFile(localPath, v2, 0644); err != nil {
		t.Fatalf("写 V2 失败: %v", err)
	}
	env.sclient(t, env.TmpDir, "upload", "vfile.txt")

	// 3) meta version list --json：恰 1 个版本（V1）
	var vl versionListResp
	env.sclientJSON(t, env.TmpDir, &vl, "meta", "version", "list", "vfile.txt")
	if len(vl.Versions) != 1 {
		t.Fatalf("覆盖上传后应恰有 1 个历史版本, got %d: %+v", len(vl.Versions), vl.Versions)
	}
	ver := vl.Versions[0]
	if ver.Size != int64(len(v1)) {
		t.Fatalf("版本 size 应为 %d, got %d", len(v1), ver.Size)
	}
	if ver.Checksum != checksumV1 {
		t.Fatalf("版本 checksum 应为 V1: got %s, want %s", ver.Checksum, checksumV1)
	}
	vid := ver.VersionID
	// 版本 ID 恒为正（服务端 newVersionID()：毫秒时间戳×1000+随机后缀，进程内单调递增）。
	// 显式断言该不变量：ID 若再次变负，此处立即以明确信息变红，而非让后续 CLI
	// 调用以 "version_id must be positive" 的间接形式失败。
	if vid <= 0 {
		t.Fatalf("版本 ID 应为正数（服务端 newVersionID 不变量）, got %d", vid)
	}

	// 4) 接口交叉核对：GET /api/versions 含同一 version_id
	var apiVersions versionListResp
	getJSON(t, env.BaseURL+"/api/versions?filename=vfile.txt", &apiVersions)
	if _, ok := findVersion(apiVersions.Versions, vid); !ok {
		t.Fatalf("GET /api/versions 应含 version_id %d, got %+v", vid, apiVersions.Versions)
	}
	if len(apiVersions.Versions) != 1 {
		t.Fatalf("GET /api/versions 应恰 1 个版本, got %d", len(apiVersions.Versions))
	}

	// 5) 磁盘：版本文件按 version_id 命名，落在 version 桶，内容 == V1；当前文件 == V2
	verName := strconv.FormatInt(vid, 10)
	verFiles := findFilesNamed(t, env.StorageRoot, verName)
	if len(verFiles) != 1 {
		t.Fatalf("应恰有 1 个版本文件 %s, got %d: %v", verName, len(verFiles), verFiles)
	}
	if !strings.Contains(filepath.ToSlash(verFiles[0]), "/version/") {
		t.Fatalf("版本文件应落在 version 桶, got %s", verFiles[0])
	}
	if got := readLocalFile(t, verFiles[0]); string(got) != string(v1) {
		t.Fatalf("版本文件内容应为 V1: got %q, want %q", got, v1)
	}
	cur := findFilesNamed(t, env.StorageRoot, "vfile.txt")
	if len(cur) != 1 {
		t.Fatalf("应恰有 1 个当前 vfile.txt, got %d: %v", len(cur), cur)
	}
	if got := readLocalFile(t, cur[0]); string(got) != string(v2) {
		t.Fatalf("当前文件内容应为 V2: got %q", got)
	}

	// 6) restore → 当前文件回到 V1；stat checksum 一致；restore 前备份使版本数 +1
	versionOp(t, env, "restore", "vfile.txt", vid)

	if got := readLocalFile(t, onlyFileNamed(t, env.StorageRoot, "vfile.txt")); string(got) != string(v1) {
		t.Fatalf("restore 后当前文件内容应为 V1: got %q", got)
	}
	status, headers := statFile(t, env.BaseURL, "vfile.txt")
	if status != http.StatusOK {
		t.Fatalf("restore 后 stat 应 200, got %d", status)
	}
	if got := headers.Get("X-File-Checksum"); got != checksumV1 {
		t.Fatalf("restore 后 checksum 应为 V1: got %s, want %s", got, checksumV1)
	}
	var afterRestore versionListResp
	env.sclientJSON(t, env.TmpDir, &afterRestore, "meta", "version", "list", "vfile.txt")
	if len(afterRestore.Versions) != 2 {
		t.Fatalf("restore 前应自动备份 V2 → 版本数应为 2, got %d: %+v",
			len(afterRestore.Versions), afterRestore.Versions)
	}
	// 备份出的新版本内容应为 restore 前的当前内容 V2
	backupFound := false
	for _, v := range afterRestore.Versions {
		if v.VersionID != vid {
			backupFound = true
			if v.Checksum != checksumV2 {
				t.Fatalf("restore 备份版本 checksum 应为 V2: got %s, want %s", v.Checksum, checksumV2)
			}
		}
	}
	if !backupFound {
		t.Fatalf("restore 后应存在一个非原 id 的备份版本, got %+v", afterRestore.Versions)
	}

	// 7) delete 原版本 → 列表回 1、API 不含该 id、磁盘版本文件消失、当前文件仍为 V1
	versionOp(t, env, "delete", "vfile.txt", vid)

	var afterDelete versionListResp
	env.sclientJSON(t, env.TmpDir, &afterDelete, "meta", "version", "list", "vfile.txt")
	if len(afterDelete.Versions) != 1 {
		t.Fatalf("delete 后应剩 1 个版本, got %d: %+v", len(afterDelete.Versions), afterDelete.Versions)
	}
	if _, ok := findVersion(afterDelete.Versions, vid); ok {
		t.Fatalf("delete 后不应再含 version_id %d", vid)
	}
	var apiAfterDelete versionListResp
	getJSON(t, env.BaseURL+"/api/versions?filename=vfile.txt", &apiAfterDelete)
	if _, ok := findVersion(apiAfterDelete.Versions, vid); ok {
		t.Fatalf("delete 后 GET /api/versions 不应再含 version_id %d", vid)
	}
	if got := findFilesNamed(t, env.StorageRoot, verName); len(got) != 0 {
		t.Fatalf("delete 后磁盘不应再有版本文件 %s, got %v", verName, got)
	}
	if got := readLocalFile(t, onlyFileNamed(t, env.StorageRoot, "vfile.txt")); string(got) != string(v1) {
		t.Fatalf("delete 版本不应影响当前文件（应仍为 V1）: got %q", got)
	}

	// 8) 负例：删除非法版本 "0" → CLI 非零退出，且当前文件与版本列表不变。
	//    命中 client 侧 versionID<=0 守卫（该守卫按裁定保留，不放行非正 ID）。
	currentBefore := readLocalFile(t, onlyFileNamed(t, env.StorageRoot, "vfile.txt"))
	stdout, stderr, err := env.sclientRun(t, env.TmpDir, "meta", "version", "delete", "vfile.txt", "0")
	if err == nil {
		t.Fatalf("删除不存在的版本 0 应非零退出\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if got := readLocalFile(t, onlyFileNamed(t, env.StorageRoot, "vfile.txt")); string(got) != string(currentBefore) {
		t.Fatalf("失败的版本删除不应改动当前文件")
	}
	var afterNeg versionListResp
	env.sclientJSON(t, env.TmpDir, &afterNeg, "meta", "version", "list", "vfile.txt")
	if len(afterNeg.Versions) != 1 {
		t.Fatalf("失败的版本删除不应改变版本数（应仍为 1）, got %d", len(afterNeg.Versions))
	}
}

// TestE2E_CLI_VersionsDisabled 锁定 versioning 缺省关闭语义：
// 服务端 501 → CLI 非零退出（meta version list 不可用）。
func TestE2E_CLI_VersionsDisabled(t *testing.T) {
	env := startCLIEnv(t, "") // 未开 versioning

	if err := os.WriteFile(filepath.Join(env.TmpDir, "nov.txt"), []byte("no versioning"), 0644); err != nil {
		t.Fatalf("写本地文件失败: %v", err)
	}
	env.sclient(t, env.TmpDir, "upload", "nov.txt")

	stdout, stderr, err := env.sclientRun(t, env.TmpDir, "meta", "version", "list", "nov.txt")
	if err == nil {
		t.Fatalf("versioning 关闭时 meta version list 应非零退出\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	// 无副作用：文件仍留在磁盘上
	if got := findFilesNamed(t, env.StorageRoot, "nov.txt"); len(got) != 1 {
		t.Fatalf("应仍有 1 个 nov.txt, got %v", got)
	}
}
