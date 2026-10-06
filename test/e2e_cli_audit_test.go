// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// e2e_cli_audit_test.go 覆盖「加密卷全链路 + 云任务审计行」的端到端用例（真二进制 + 子进程）。
//
// 链路（task-13 端到端集成验证：建卷→切加密卷看明文→播放→转存→审计）：
//   - 建卷：config 装配 secretdata 封装卷（vault，secret_url → secrets 卷密钥文件）——
//     这是 secretdata 卷当前唯一的生产装配方式（sclient 无建卷子命令、运行期 API 亦不支持
//     secretdata 类型，见 e2e_cli_secretdata_test.go「可达性边界」说明），配置装配成功即建卷证明；
//   - 切加密卷看明文：GET /api/files?volume=vault → 外部卷 ListDir 透传并**解密出明文文件名**
//     （条目带 volume=vault + volume_category=wrapper 标注加密封装卷）——加密卷明文目录视图；
//   - 播放：GET /download?filename=...&volume=vault + Range → 206 + 解密明文段 == 原文件段
//     （browser <video> 原生 Range 播放的服务端语义，rawHTTP Range 请求等价断言）；
//   - 转存审计：cloud-download submit --transfer-volume vault → GET /api/cloud/tasks/{id}
//     → 任务 audit 数组含 download + transfer + encrypt 行（阶段耗时/字节/带宽审计行，随任务
//     终态持久化）。
//
// 断言铁律：每条正例落到真实副作用（API 响应内容 / 206 响应体 / 任务 audit 数组 / 磁盘加密容器）。
package sproxy_test

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// e2eAuditRow 是云任务 audit 行的精简解析形状（仅断言所需字段）。
type e2eAuditRow struct {
	Type  string `json:"type"`
	Step  string `json:"step"`
	Bytes int64  `json:"bytes"`
	DurMS int64  `json:"dur_ms"`
	Err   string `json:"err,omitempty"`
}

// auditStepTypeSet 返回 audit 行的 Step/Type 取值集合（row.Type 与 row.Step 同值，取并集）。
func auditStepTypeSet(rows []e2eAuditRow) map[string]bool {
	set := make(map[string]bool, len(rows)*2)
	for _, r := range rows {
		set[r.Type] = true
		set[r.Step] = true
	}
	return set
}

// TestE2E_CLI_Audit_SecretVolumeChain 全链路：装配加密卷 → 上传视频 → ListDir 明文视图 →
// Range 206 播放 → 云下载转存加密 → 任务 audit（download/transfer/encrypt）。
func TestE2E_CLI_Audit_SecretVolumeChain(t *testing.T) {
	t.Parallel()
	env, vaultRoot := newSecretdataPlaybackEnv(t)

	// ---- 1) 上传「视频」到加密卷（vault 为 secretdata wrapper 卷，写路径自动加密）----
	content := make([]byte, 96*1024) // 96 KiB，跨多块，贴近真实视频分段
	for i := range content {
		content[i] = byte(i % 251)
	}
	videoFile := filepath.Join(env.TmpDir, "movie.mp4")
	if werr := os.WriteFile(videoFile, content, 0o600); werr != nil {
		t.Fatalf("写测试视频文件: %v", werr)
	}
	env.sclient(t, env.TmpDir, "--volume", "vault", "upload", "movie.mp4")

	// 磁盘副作用：vault 卷底层是加密容器（无明文逻辑名/无明文内容）——「加密落盘」证据。
	assertSecretLayoutAnonymity(t, vaultRoot, "user", "movie.mp4", "user/movie.mp4")

	// ---- 2) ListDir 明文视图：GET /api/files?volume=vault 返回**明文文件名** ----
	var list struct {
		Files []struct {
			Name           string `json:"name"`
			VolumeCategory string `json:"volume_category"`
			Size           int64  `json:"size"`
		} `json:"files"`
	}
	getJSON(t, env.BaseURL+"/api/files?volume=vault", &list)
	found := false
	for i := range list.Files {
		f := list.Files[i]
		if f.Name != "movie.mp4" {
			continue
		}
		found = true
		// 外部卷列表条目不逐条标注 volume（?volume= 已限定目标卷，见 listExternalVolume
		// 注释——URL 限定即卷名，省字段）——明文文件名 + wrapper 分类即加密卷明文视图证据。
		if f.VolumeCategory != "wrapper" {
			t.Errorf("movie.mp4 的 volume_category = %q, want wrapper（加密封装卷标注）", f.VolumeCategory)
		}
		if f.Size != int64(len(content)) {
			t.Errorf("movie.mp4 size = %d, want %d", f.Size, len(content))
		}
	}
	if !found {
		t.Fatalf("GET /api/files?volume=vault 应含明文 movie.mp4（加密卷 ListDir 明文视图）; got files=%+v", list.Files)
	}

	// ---- 3) 播放：Range 随机访问（browser <video> 等价语义）→ 206 + 解密明文段 ----
	dlURL := env.BaseURL + "/download?filename=movie.mp4&volume=vault"
	status, hdr, body := rangeRequest(t, dlURL, "bytes=4096-8191")
	if status != http.StatusPartialContent {
		t.Fatalf("加密卷 Range 应 206, got %d: %s", status, body)
	}
	if cr := hdr.Get("Content-Range"); cr != "bytes 4096-8191/98304" {
		t.Fatalf("Content-Range=%q want bytes 4096-8191/98304", cr)
	}
	if !bytes.Equal(body, content[4096:8192]) {
		t.Fatalf("解密 Range 段 != 原文件段（len=%d）", len(body))
	}

	// ---- 4) 云下载 + 转存到该加密卷 → 任务审计行（download/transfer/encrypt）----
	payload := make([]byte, 160*1024) // 160 KiB，跨多块
	for i := range payload {
		payload[i] = byte((i * 7) % 251)
	}
	src := newPlaybackSrcServer(t, payload)

	out := env.sclient(t, env.TmpDir, "cloud-download", "submit",
		"--transfer-volume", "vault", src.URL+"/video.bin")
	if !strings.Contains(out, "video.bin") {
		t.Fatalf("submit 输出应含 video.bin, got:\n%s", out)
	}
	tid := onlyCloudTaskID(t, env)
	env.sclient(t, env.TmpDir, "cloud-download", "wait", tid, "--timeout", "2m")

	// 转存产物已加密落盘（vault 卷底层无明文 video.bin）。
	assertSecretLayoutAnonymity(t, vaultRoot, "user", "video.bin", "user/video.bin")

	// 任务详情 audit 数组：GET /api/cloud/tasks/{id} 响应含 download/transfer/encrypt 行。
	var detail struct {
		ID     string        `json:"id"`
		Status string        `json:"status"`
		Audit  []e2eAuditRow `json:"audit"`
	}
	getJSON(t, env.BaseURL+"/api/cloud/tasks/"+tid, &detail)
	if detail.ID != tid {
		t.Fatalf("任务详情 ID = %q, want %q", detail.ID, tid)
	}
	if detail.Status != "completed" {
		t.Fatalf("wait 后任务状态应为 completed, got %q", detail.Status)
	}
	if len(detail.Audit) == 0 {
		t.Fatalf("任务 %s 详情应含 audit 行（download/transfer/encrypt）, got 空", tid)
	}
	set := auditStepTypeSet(detail.Audit)
	for _, step := range []string{"download", "transfer", "encrypt"} {
		if !set[step] {
			t.Errorf("任务审计缺 %q 行; audit=%+v", step, detail.Audit)
		}
	}
	// download 行字节数 == 实际下载量（阶段字节/带宽审计行真实证据）。
	var dlBytes int64
	for _, r := range detail.Audit {
		if r.Type == "download" || r.Step == "download" {
			dlBytes = r.Bytes
		}
	}
	if dlBytes != int64(len(payload)) {
		t.Errorf("download 审计行 bytes = %d, want %d（源内容实际字节）", dlBytes, len(payload))
	}
}
