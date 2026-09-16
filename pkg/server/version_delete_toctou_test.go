// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// version_delete_toctou_test.go 是版本删除「定位-删除」TOCTOU 窗口的钉住测试
// （2026-09-17 安全加固，计划 2026-09-17-security-toctou.md 任务 3）。
//
// 现状结论（先读代码再断言）：deleteVersionHandler 的定位（FindVersionFile → Stat）
// 与删除（root.Remove(verRel)）都基于同一个由规范化 version_id 派生的路径段
// version/<remotePath>/<id>——不存在「按用户输入拼 key、再用另一形态定位」的分叉，
// 定位与删除天然同路径。窗口内的并发替换只会替换同一路径下的内容，删除的正是被定位
// 的对象（不会误删别的版本目录项）。因此窗口已闭合，本文件只补钉住测试（无生产
// 改动），且测试带真实盘上替换注入：把被定位的 v1 路径内容替换为别的版本的内容，
// 断言删除仍作用于该路径、且同目录的 v2 版本不受影响。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestDeleteVersion_TOCTOU_ReplaceBeforeDelete 验证：定位 v1 后、删除前把 v1 所在
// 路径的内容替换为另一版本的内容时，删除的仍是被定位路径的对象（v1 消失），
// 同目录的 v2 版本不受影响——不存在「定位路径与删除路径分叉」。
func TestDeleteVersion_TOCTOU_ReplaceBeforeDelete(t *testing.T) {
	t.Parallel()
	url, cfgPtr, _ := newTestServer(t, func(cfg *Config) {
		cfg.Versioning.Enabled = true
		cfg.Versioning.MaxVersions = 10
	})

	body1 := []byte("version-one-content")
	body2 := []byte("version-two-content")
	uploadFile(t, url, "toctou.txt", body1, map[string]string{
		"X-File-Checksum": sha256hex(body1),
	})
	uploadFile(t, url, "toctou.txt", body2, map[string]string{
		"X-File-Checksum": sha256hex(body2),
	})

	listResp, err := http.Get(url + "/api/versions?filename=toctou.txt")
	if err != nil {
		t.Fatal(err)
	}
	var listResult struct {
		Versions []VersionInfo `json:"versions"`
	}
	if decErr := json.NewDecoder(listResp.Body).Decode(&listResult); decErr != nil {
		listResp.Body.Close()
		t.Fatal(decErr)
	}
	listResp.Body.Close()
	if len(listResult.Versions) == 0 {
		t.Fatal("应有至少一个历史版本")
	}
	v1 := listResult.Versions[0]

	// 服务端 StorageRoot 从 cfgPtr 可读：<root>/<tenant>/version/toctou.txt/<id>。
	storageRoot := cfgPtr.Load().StorageRoot
	v1Abs := filepath.Join(storageRoot, "anonymous", "version", "toctou.txt", fmt.Sprintf("%d", v1.VersionID))
	if _, statErr := os.Stat(v1Abs); statErr != nil {
		t.Fatalf("定位 v1 磁盘路径失败（%s）: %v", v1Abs, statErr)
	}
	// 替换 v1 路径内容为另一段（模拟并发写者改写该版本路径——校验/定位对象与删除对象
	// 同一路径，删除的正是它）。
	if writeErr := os.WriteFile(v1Abs, []byte("tampered-by-concurrent-writer"), 0o644); writeErr != nil {
		t.Fatalf("替换版本文件失败: %v", writeErr)
	}

	delURL := fmt.Sprintf("%s/api/versions?filename=toctou.txt&version_id=%d", url, v1.VersionID)
	req, err := http.NewRequest("DELETE", delURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("删除版本应 200, got %d", resp.StatusCode)
	}
	if _, statErr2 := os.Stat(v1Abs); !os.IsNotExist(statErr2) {
		t.Fatalf("v1 路径应已删除, stat err=%v", statErr2)
	}

	// 同目录 v2 版本不受影响（列表仍在）。
	listResp2, err := http.Get(url + "/api/versions?filename=toctou.txt")
	if err != nil {
		t.Fatal(err)
	}
	var listResult2 struct {
		Versions []VersionInfo `json:"versions"`
	}
	if decErr := json.NewDecoder(listResp2.Body).Decode(&listResult2); decErr != nil {
		listResp2.Body.Close()
		t.Fatal(decErr)
	}
	listResp2.Body.Close()
	for _, v := range listResult2.Versions {
		if v.VersionID == v1.VersionID {
			t.Fatalf("v1 应已删除, 仍出现在列表: %+v", v)
		}
	}
}
