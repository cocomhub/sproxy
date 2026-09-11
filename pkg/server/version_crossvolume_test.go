// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// version_crossvolume_test.go 覆盖 A2-D「跨卷版本可见性」：文件被跨卷 move 到别的卷后，
// 留在原卷的版本仍可 list / restore / delete（版本字节不迁移、配额仍计在原卷池 + owner 全局）。
//
// 修复前：resolveVersionTarget 只按「user 文件的 home 卷」返回租户（且仅当文件已删除时才跨卷扫
// 版本目录），move 后 user 文件在目标卷、版本在源卷 → /api/versions 返回空、restore/delete 404。

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// deleteVersionReq 发送 DELETE /api/versions?filename=&version_id=，返回状态与响应体。
func deleteVersionReq(t *testing.T, baseURL, filename string, versionID int64) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest("DELETE",
		fmt.Sprintf("%s/api/versions?filename=%s&version_id=%d", baseURL, filename, versionID), nil)
	if err != nil {
		t.Fatalf("new delete-version req: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete-version: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// TestVersionCrossVolume_MoveKeepsVersionsVisible 多卷：卷 A 建文件并产生版本 → 跨卷 move 到
// 卷 B → /api/versions 仍列出该版本（跨卷合并）→ restore 成功且内容正确、文件落 B（不产生跨卷
// 双份）→ delete 版本成功（作用于 A 上的版本文件）。
func TestVersionCrossVolume_MoveKeepsVersionsVisible(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 1 << 20},
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}
	baseURL, h, _ := uploadingLockServer(t, "alice", volumes, func(cfg *Config) {
		cfg.Versioning.Enabled = true
		cfg.Versioning.MaxVersions = 10
	})

	v1 := []byte("cross-volume version one")
	v2 := []byte("cross-volume version two (current)")

	// 1) 文件落 main（prefer-default），覆盖写产生版本 v1（留在 main）。
	if status, _, body := volumeUpload(t, baseURL, "cv.txt", v1, ""); status != http.StatusOK {
		t.Fatalf("首传应 200, got %d %s", status, body)
	}
	if status, _, body := volumeUpload(t, baseURL, "cv.txt", v2, ""); status != http.StatusOK {
		t.Fatalf("覆盖写应 200, got %d %s", status, body)
	}
	if !diskFileExists(t, dirs[0], "alice", "cv.txt") {
		t.Fatal("cv.txt 应在 main（prefer-default）")
	}
	verDirMain := filepath.Join(dirs[0], "alice", "version", "cv.txt")
	if ents, err := os.ReadDir(verDirMain); err != nil || len(ents) != 1 {
		t.Fatalf("main version 应有 1 个版本: err=%v entries=%d", err, len(ents))
	}

	// 2) 跨卷 move main → disk2（版本不迁移，仍留 main）。
	if code, _, err := moveVolumeCore(baseURL, "main", "disk2", "cv.txt"); err != nil || code != http.StatusOK {
		t.Fatalf("move 应 200, got code=%d err=%v", code, err)
	}
	if diskFileExists(t, dirs[0], "alice", "cv.txt") {
		t.Fatal("move 后 main 不应残留 cv.txt")
	}
	if !diskFileExists(t, dirs[1], "alice", "cv.txt") {
		t.Fatal("move 后 cv.txt 应在 disk2")
	}
	// 配额语义不变：版本字节仍计在源卷（main）池。
	if got := h.volSet.Pool("main").Usage(); got != int64(len(v1)) {
		t.Fatalf("move 后 main 池=%d want %d（版本字节仍在源卷池）", got, len(v1))
	}

	// 3) 核心断言：文件在 disk2，/api/versions 仍可列出 main 上的版本。
	listed := listVersionsJSON(t, baseURL, "cv.txt")
	if len(listed.Versions) != 1 {
		t.Fatalf("跨卷 move 后 listVersions 应仍见 1 个版本（留 main）, got %d（修复前=0）", len(listed.Versions))
	}
	if listed.Versions[0].Size != int64(len(v1)) {
		t.Fatalf("版本 size=%d want %d", listed.Versions[0].Size, len(v1))
	}
	verID := listed.Versions[0].VersionID

	// 4) restore：从 main 的版本恢复到文件当前所在卷 disk2，内容 = v1。
	restoreURL := fmt.Sprintf("%s/api/versions/restore?filename=cv.txt&version_id=%d", baseURL, verID)
	resp, err := http.Post(restoreURL, "application/json", nil)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("跨卷 restore 应 200, got %d %s", resp.StatusCode, rb)
	}
	got, rerr := os.ReadFile(filepath.Join(dirs[1], "alice", "user", "cv.txt"))
	if rerr != nil {
		t.Fatalf("读取恢复后文件: %v", rerr)
	}
	if !bytes.Equal(got, v1) {
		t.Fatalf("恢复内容 = %q, want %q（main 上的 v1）", got, v1)
	}
	// AD-4：恢复不得在源卷（main）留下第二份 user 文件。
	if diskFileExists(t, dirs[0], "alice", "cv.txt") {
		t.Fatal("跨卷 restore 不应把 user 文件写回 main（跨卷双份）")
	}

	// 5) delete 版本：作用于 main 上的版本文件（restore 前备份在 disk2 又多 1 个 → 合并 2 个）。
	listed2 := listVersionsJSON(t, baseURL, "cv.txt")
	if len(listed2.Versions) != 2 {
		t.Fatalf("restore 后应合并 2 个版本（main v1 + disk2 恢复前备份）, got %d", len(listed2.Versions))
	}
	status, body := deleteVersionReq(t, baseURL, "cv.txt", verID)
	if status != http.StatusOK {
		t.Fatalf("跨卷 delete 版本应 200, got %d %s", status, body)
	}
	if ents, err := os.ReadDir(verDirMain); err != nil || len(ents) != 0 {
		t.Fatalf("删除后 main version 应空: err=%v entries=%d", err, len(ents))
	}
	// 建议 5：释放指向正确卷池的直接断言——被删版本（v1=main）从 main 卷池下降
	// （releaseVersionUsage 作用于版本所在卷）；disk2 保留 disk2 上的恢复前备份字节（v2）
	// 与 disk2 user 文件（restore 后，v1 内容）。move 后 main 池曾 = len(v1)=24，删除后归零。
	if got := h.volSet.Pool("main").Usage(); got != 0 {
		t.Fatalf("删除 main 版本后主卷池应归零, got %d（releaseVersionUsage 未指向版本所在卷）", got)
	}
	// disk2 池 = user 文件（v1=24，restore 后）+ disk2 版本目录（恢复前备份 v2=34）。
	if got := h.volSet.Pool("disk2").Usage(); got != int64(len(v1)+len(v2)) {
		t.Fatalf("删除 main 版本后 disk2 卷池=%d want %d（user 文件 + 恢复前备份字节）", got, len(v1)+len(v2))
	}
	// 剩余 1 个版本（disk2 的恢复前备份）仍可见。
	listed3 := listVersionsJSON(t, baseURL, "cv.txt")
	if len(listed3.Versions) != 1 {
		t.Fatalf("删除 main 版本后应剩 disk2 的 1 个版本, got %d", len(listed3.Versions))
	}
}
