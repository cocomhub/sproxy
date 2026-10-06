// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// audit_webui_e2e_test.go — 审计 + 加密卷 WebUI 全链路集成验证（task-13）。
//
// 全链路（建卷→切加密卷看明文→播放 Range→转存审计）的**浏览器/API 面**子集：
//
//   - 建卷：POST /api/volumes/user 创建 secretdata 封装卷（type=secretdata，e2e 专用后端，
//     target 指向本地 main 卷；这正是 UI「卷管理」面板 vol-manage 提交的同一路径——
//     面板表单由 /api/backends schema 驱动，字段就是 target 底层卷）；
//   - 切加密卷看明文：文件页「过滤卷」下拉（#volume-filter）选中该加密卷 →
//     GET /api/files?volume=secretdata 返回**明文文件名**（volume_category=wrapper），
//     DOM 文件行渲染明文视频名；
//   - 播放：点「▶ 播放」→ <video src="/download?filename=..."> 弹窗（加密卷视频行入口），
//     rawHTTP Range 请求 /download?filename=...&volume=secretdata → 206 + 解密明文段一致；
//   - 转存审计行：CLI 面（test/e2e_cli_audit_test.go）覆盖 cloud-download --transfer-volume
//     → 任务详情 audit 数组 download/transfer/encrypt——本文件服务端装配前提（in-process
//     RegisterRoutes 不跑 cmd/sproxy 的 setupSecretBackends 补装）下由真实二进制面保证，
//     浏览器面不重复。
//
// 服务端装配（secretdata 卷由 cmd/sproxy setupSecretBackends 补装，pkg/server 不负责）：
// 本文件注册 e2e 专用 secretdata 后端类型，其工厂用测试密钥直接构造 secretdata.NewBackend
// （绕过 secret_url → secrets 卷依赖，与 user_volumes_e2e_test.go 的 fake backend 同法——
// in-process 装配职责在测试侧补齐）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	"github.com/cocomhub/sproxy/pkg/server"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/secretdata"
	"github.com/mxschmitt/playwright-go"
)

// auditVaultType 是 e2e 建卷使用的加密封装卷类型名。用真实类型名 "secretdata"——
// 前端卷徽标分类按字面类型判 wrapper（volumeCategoryFor 硬编码 secretdata/secrets/egress，
// 见 pkg/files/read_ops.go），测试须用真名才能验证「卷徽标分类（wrapper 🔒）」链路。
// 工厂构造 secretdata.NewBackend（测试密钥 + 测试卷根，规避 secrets 卷依赖）；web/e2e
// 不经 cmd/sproxy，注册 "secretdata" 不与生产装配冲突。
//
// **测试隔离**：注册会出现在 /api/backends（user_volumes 用例断言下拉恰为 [baidupcs]），
// 故本文件**不调用 t.Parallel()**（Go 串行测试在并行测试恢复前跑完），并在服务器清理后
// 经 UnregisterBackendForTest 移除——并行 user_volumes 用例看不到本注册。
const auditVaultType = "secretdata"

// registerAuditVaultBackend 注册 secretdata 后端 + 创建表单 schema。
// 工厂闭包捕获 root/secret（调用方在服务器装配前传入）。不设 Once：测试结束清理
// （UnregisterBackendForTest）移除类型后，同一进程 -count=N 再次调用需能重注册。
func registerAuditVaultBackend(typ, root string, secret []byte) {
	registry.RegisterBackendSchema(typ, []registry.FieldSchema{{
		Key: "target", Type: "volume-select", Required: true, AllowWrapper: true,
	}})
	registry.RegisterBackend(typ, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
		return secretdata.NewBackend(context.Background(), v, syncpkg.NewLocalFS(root, nil), secretdata.Options{
			Secret:  secret,
			Block:   shardseal.BlockPolicy{Mode: "random", Min: 4096, Max: 16384},
			TempDir: filepath.Join(root, "tmp"),
		})
	})
}

// auditVaultE2EServer 启动带用户卷装配的 e2e 服务（main 本地卷 + SetUserVolumeStore +
// 已注册 secretdata 后端）。返回 baseURL 与清理函数。
func auditVaultE2EServer(t *testing.T, vaultRoot string) (string, func()) {
	t.Helper()
	base := t.TempDir()
	mainRoot := filepath.Join(base, "main")
	if err := os.MkdirAll(mainRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := []byte("0123456789abcdef0123456789abcdef")
	registerAuditVaultBackend(auditVaultType, vaultRoot, secret)
	// 测试结束移除测试注册的后端 + schema：防止同一进程后续（并行）user_volumes 用例
	// 断言「下拉恰为 [baidupcs]」时被本类型污染。注册表仅影响后续建卷，已构造的后端
	// 对象仍被本服务器 volSet 持有，不受影响。
	t.Cleanup(func() { registry.UnregisterBackendForTest(auditVaultType) })

	baseURL, h, cfg, cleanup := testServerCfgWithHandlers(t, func(c *server.Config) {
		c.Volumes = []server.VolumeConfig{{Name: "main", Root: mainRoot}}
	})
	t.Cleanup(cleanup)
	h.SetUserVolumeStore(server.NewUserVolumeStore(cfg.StorageRoot))
	return baseURL, func() {}
}

// createUserVolume POST /api/volumes/user 建卷（UI 卷管理面板同一路径），返回状态/响应体。
func createUserVolume(t *testing.T, baseURL, name, typ, extraJSON string) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q,"type":%q,"extra":%s}`, name, typ, extraJSON)
	req, rerr := http.NewRequest(http.MethodPost, baseURL+"/api/volumes/user", strings.NewReader(body))
	if rerr != nil {
		t.Fatalf("构造建卷请求: %v", rerr)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, derr := http.DefaultClient.Do(req)
	if derr != nil {
		t.Fatalf("POST /api/volumes/user: %v", derr)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

// volumesFromAPI GET /api/volumes 并返回卷名集合。
func volumesFromAPI(t *testing.T, baseURL string) []string {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/volumes")
	if err != nil {
		t.Fatalf("GET /api/volumes: %v", err)
	}
	defer resp.Body.Close()
	var payload struct {
		Volumes []struct {
			Name string `json:"name"`
		} `json:"volumes"`
	}
	if jerr := json.NewDecoder(resp.Body).Decode(&payload); jerr != nil {
		t.Fatalf("解析 /api/volumes: %v", jerr)
	}
	out := make([]string, 0, len(payload.Volumes))
	for _, v := range payload.Volumes {
		out = append(out, v.Name)
	}
	return out
}

// rangeGET 对 url 发带 Range 头的 GET（播放器语义），返回 status/headers/body。
func rangeGET(t *testing.T, url, rangeHdr string) (int, http.Header, []byte) {
	t.Helper()
	req, rerr := http.NewRequest(http.MethodGet, url, nil)
	if rerr != nil {
		t.Fatalf("构造 Range GET %s: %v", url, rerr)
	}
	if rangeHdr != "" {
		req.Header.Set("Range", rangeHdr)
	}
	resp, derr := http.DefaultClient.Do(req)
	if derr != nil {
		t.Fatalf("Range GET %s: %v", url, derr)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, cerr := buf.ReadFrom(resp.Body); cerr != nil {
		t.Fatalf("读 Range GET %s 响应体: %v", url, cerr)
	}
	return resp.StatusCode, resp.Header, buf.Bytes()
}

// TestAuditWebUI_CreateVault_SwitchList_PlayRange 全链路：
// 建卷（POST /api/volumes/user）→ 文件页切到加密卷 → 明文列表（API 响应 + DOM 行）→
// ▶ 播放弹窗 <video src> 接线 → rawHTTP Range 206 + 解密明文段一致。
func TestAuditWebUI_CreateVault_SwitchList_PlayRange(t *testing.T) {
	vaultRoot := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(vaultRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	baseURL, _ := auditVaultE2EServer(t, vaultRoot)

	// ---- 1) 建卷：真实 POST /api/volumes/user（UI 卷管理面板同一路径）----
	status, body := createUserVolume(t, baseURL, auditVaultType, auditVaultType, `{"target":"main"}`)
	if status != http.StatusOK {
		t.Fatalf("POST /api/volumes/user status=%d body=%s（建卷失败）", status, body)
	}
	var vols []string
	testutil.WaitFor(t, 15*time.Second, func() bool {
		vols = volumesFromAPI(t, baseURL)
		return containsString(vols, auditVaultType)
	}, func() string { return "建卷后 /api/volumes 应含 " + auditVaultType })

	// ---- 2) 上传「视频」到该加密卷（真实上传写路径 → secretdata 自动加密）----
	content := make([]byte, 96*1024) // 96 KiB，跨多块
	for i := range content {
		content[i] = byte(i % 251)
	}
	if upStatus, upBody := seedUploadToVolume(t, baseURL, auditVaultType, "movie.mp4", content); upStatus != http.StatusOK {
		t.Fatalf("seed upload to %s status=%d body=%s", auditVaultType, upStatus, upBody)
	}

	// ---- 3) 文件页切到加密卷 → 明文列表（捕获 GET /api/files?volume= 响应 + DOM 行）----
	page, stop := pageFixture(t)
	defer stop()
	page.Goto(baseURL + "/ui/")

	if err := waitLoc(page, "#volume-filter option[value='"+auditVaultType+"']", playwright.WaitForSelectorStateAttached, 8000); err != nil {
		t.Fatalf("文件页「过滤卷」下拉未含 %s: %v", auditVaultType, err)
	}
	volResp, err := page.ExpectResponse("**/api/files?*", func() error {
		_, serr := page.Locator("#volume-filter").SelectOption(playwright.SelectOptionValues{Values: &[]string{auditVaultType}})
		return serr
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("选中 %s 未触发 GET /api/files?volume=: %v", auditVaultType, err)
	}
	if !strings.Contains(volResp.URL(), "volume="+auditVaultType) {
		t.Fatalf("过滤卷请求应透传 volume=%s, got URL %s", auditVaultType, volResp.URL())
	}
	var listPayload struct {
		Files []struct {
			Name           string `json:"name"`
			VolumeCategory string `json:"volume_category"`
			Size           int64  `json:"size"`
		} `json:"files"`
	}
	if jerr := volResp.JSON(&listPayload); jerr != nil {
		t.Fatalf("解析 /api/files?volume=%s 响应: %v", auditVaultType, jerr)
	}
	if !containsFile(listPayload.Files, "movie.mp4") {
		t.Fatalf("加密卷明文目录应含 movie.mp4（ListDir 明文视图）; got files=%+v", listPayload.Files)
	}
	for i := range listPayload.Files {
		f := listPayload.Files[i]
		if f.Name == "movie.mp4" {
			if f.VolumeCategory != "wrapper" {
				t.Errorf("movie.mp4 volume_category = %q, want wrapper", f.VolumeCategory)
			}
			if f.Size != int64(len(content)) {
				t.Errorf("movie.mp4 size = %d, want %d", f.Size, len(content))
			}
		}
	}

	// DOM 行：movie.mp4 明文文件名已在文件页渲染（切卷后明文目录视图接通）。
	// 注：加密卷 filtered 视图的条目 `volume` 字段为空（listExternalVolume 注释：
	// 卷名已由 ?volume= 限定、无需逐条标注），故前端的卷徽标以 `fi.volume` 为渲染前提
	// 时不显示——wrapper 徽标分类（🔒 卷名）由 app-render.test.js 单测锁定；本 e2e 的
	// 数据面（API volume_category=wrapper）已在上面断言。
	if werr := waitLoc(page, "#file-table tr", nil, 8000); werr != nil {
		t.Fatalf("file table 未渲染: %v", werr)
	}
	if lerr := waitLoc(page, "text=movie.mp4", nil, 8000); lerr != nil {
		t.Fatalf("加密卷明文视图应渲染 movie.mp4: %v", lerr)
	}

	// ---- 4) 播放：点「▶ 播放」→ <video> 弹窗以明文文件名打开（加密卷视频行入口）----
	if cerr := page.Locator("#file-table .file-video-play-btn").First().Click(); cerr != nil {
		t.Fatalf("点击 ▶ 播放: %v", cerr)
	}
	if verr := waitLoc(page, ".video-modal video", playwright.WaitForSelectorStateVisible, 8000); verr != nil {
		t.Fatalf("视频弹窗未显示: %v", verr)
	}
	src, serr := page.Locator(".video-modal video").GetAttribute("src")
	if serr != nil {
		t.Fatalf("读取 <video> src: %v", serr)
	}
	if !strings.Contains(src, "/download?filename=movie.mp4") {
		t.Fatalf("<video> src = %q, want 含明文文件名 /download?filename=movie.mp4（加密卷视频行入口）", src)
	}
	// 注：filtered 视图条目 volume 为空（listExternalVolume 注释），故 <video> 服务端按
	// auto 路由定位（main 无同名文件 → 唯一命中 vault）——确定性卷路径由下面显式
	// ?volume= 的 Range 206 断言覆盖。

	// ---- 5) rawHTTP Range 请求该 URL → 206 + 解密明文段一致（标准 Range 播放）----
	rangeURL := baseURL + "/download?filename=movie.mp4&volume=" + auditVaultType
	status, hdr, seg := rangeGET(t, rangeURL, "bytes=4096-8191")
	if status != http.StatusPartialContent {
		t.Fatalf("加密卷 Range 应 206, got %d: %s", status, seg)
	}
	if cr := hdr.Get("Content-Range"); cr != "bytes 4096-8191/98304" {
		t.Fatalf("Content-Range=%q want bytes 4096-8191/98304", cr)
	}
	if !bytes.Equal(seg, content[4096:8192]) {
		t.Fatalf("解密 Range 段 != 原文件段（len=%d）", len(seg))
	}
}

// containsString 判断切片是否含目标串。
func containsString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// containsFile 判断明文列表是否含目标文件名。
func containsFile(files []struct {
	Name           string `json:"name"`
	VolumeCategory string `json:"volume_category"`
	Size           int64  `json:"size"`
}, name string) bool {
	for _, f := range files {
		if f.Name == name {
			return true
		}
	}
	return false
}
