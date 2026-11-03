// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volumes_api_branches_test.go 覆盖 /api/volumes 系列端点的**参数校验与未装配回落**分支
// （主流程已在 volumes_api_test.go / volumes_rebalance_test.go 钉住）：
//
//   - 缺参数 / 非法 max_bytes ⇒ 400（fail-closed，不落到「默认值」静默继续）；
//   - 卷功能未装配 ⇒ 400（明确报错而不是 panic / 误当成功）；
//   - 外部卷参与的 move 在文件服务未装配 ⇒ 400（不得静默 no-op）；
//   - refresh 在「无状态缓存 / 外部卷后端不支持索引 / 索引重建失败」的明确状态码；
//   - 跨卷复制的错误分支（源缺失 / 目标目录不可建 / 目标被目录占用）。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// decodeRebalanceResp 解析 rebalance 响应（断言用）。
func decodeRebalanceResp(t *testing.T, body []byte) rebalanceVolumeResponse {
	t.Helper()
	var out rebalanceVolumeResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解析 rebalance 响应: %v (body=%s)", err, body)
	}
	return out
}

// TestVolumesAPI_MoveRejectsMissingParams 验证 move 的三参数缺一即 400（不得落到全路径默认值）。
func TestVolumesAPI_MoveRejectsMissingParams(t *testing.T) {
	t.Parallel()
	baseURL, _, _ := twoVolumeServer(t)
	cases := []struct {
		name         string
		from, to, fn string
	}{
		{"缺filename", "main", "disk2", ""},
		{"缺from", "", "disk2", "a.txt"},
		{"缺to", "main", "", "a.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, body, err := moveVolumeCore(baseURL, tc.from, tc.to, tc.fn)
			if err != nil {
				t.Fatalf("move 请求失败: %v", err)
			}
			if status != http.StatusBadRequest {
				t.Fatalf("status=%d want 400, body=%s", status, body)
			}
			if !strings.Contains(string(body), "均不能为空") {
				t.Fatalf("响应应说明缺参数, body=%s", body)
			}
		})
	}
}

// TestVolumesAPI_MoveUnassembledVolSet 验证卷功能未装配时 move 明确 400（不 panic、不 no-op）。
func TestVolumesAPI_MoveUnassembledVolSet(t *testing.T) {
	t.Parallel()
	h := &Handlers{logger: testLogger()}
	req := httptest.NewRequest(http.MethodPost, "/api/volumes/move?from_volume=main&to_volume=disk2&filename=a.txt", nil)
	rec := httptest.NewRecorder()
	h.moveVolumeHandler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "卷功能未装配") {
		t.Fatalf("响应应说明卷功能未装配, body=%s", rec.Body.String())
	}
}

// TestMoveFileBetweenVolumes_SameVolumeCoreNoop 验证核心层的同卷幂等（handler 层已提前
// 短路，此处钉住核心自身的防御语义：源已在目标卷 ⇒ 200 且不动文件）。
func TestMoveFileBetweenVolumes_SameVolumeCoreNoop(t *testing.T) {
	t.Parallel()
	baseURL, h, dirs := twoVolumeServer(t)
	rebalanceUploadFiles(t, baseURL, dirs, h, map[string]string{"a.txt": "payload"})
	if _, err := os.Stat(filepath.Join(dirs[0], "alice", "user", "a.txt")); err != nil {
		t.Fatalf("上传后源文件应存在: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/volumes/move?from_volume=main&to_volume=main&filename=a.txt", nil)
	req = req.WithContext(withActor(req.Context(), "alice"))
	status, resp := h.moveFileBetweenVolumes(req, "alice", "a.txt", "main", "main")
	if status != http.StatusOK || !resp.Success {
		t.Fatalf("同卷核心应幂等成功, got (%d,%+v)", status, resp)
	}
	if !strings.Contains(resp.Message, "无需移动") {
		t.Fatalf("响应应说明无需移动, got %q", resp.Message)
	}
}

// TestMoveFileBetweenVolumes_InvalidRel 验证核心层对「无法映射到 owner user 桶」的路径
// 如实 400（不猜测路径）。
func TestMoveFileBetweenVolumes_InvalidRel(t *testing.T) {
	t.Parallel()
	_, h, _ := twoVolumeServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/volumes/move", nil)
	req = req.WithContext(withActor(req.Context(), "alice"))
	if status, resp := h.moveFileBetweenVolumes(req, "alice", "/absolute.txt", "main", "disk2"); status != http.StatusBadRequest || resp.Success {
		t.Fatalf("无法映射到 owner user 桶的路径应 400, got (%d,%+v)", status, resp)
	}
}

// TestVolumesAPI_MoveExternalSourceMissing 验证外部卷作源时的 move 走域侧组合路径，
// 源不存在如实 404（不误报 500，也不静默成功）。
func TestVolumesAPI_MoveExternalSourceMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = dir
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: dir}}
	h := buildVolSetHandlers(t, cfg)
	be := &opsBackend{fs: newOpsFS()}
	if err := h.volSet.AddExternalVolume(volume.Volume{
		Name: "vault", Type: "secretdata",
		ACL: volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{"alice": {}}},
	}, be); err != nil {
		t.Fatalf("AddExternalVolume: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/volumes/move?from_volume=vault&to_volume=main&filename=a.txt", nil)
	req = req.WithContext(withActor(req.Context(), "alice"))
	rec := httptest.NewRecorder()
	h.moveVolumeHandler(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "源文件不存在") {
		t.Fatalf("响应应说明源文件不存在, body=%s", rec.Body.String())
	}
}

// TestVolumesAPI_RebalanceRejects 验证 rebalance 的参数校验：缺卷名 / 非法 max_bytes → 400，
// 未装配卷功能 → 400。
func TestVolumesAPI_RebalanceRejects(t *testing.T) {
	t.Parallel()
	baseURL, _, _ := twoVolumeServer(t)

	if status, body := rebalanceVolume(t, baseURL, "", "disk2", ""); status != http.StatusBadRequest {
		t.Fatalf("缺 from_volume 应 400, got %d body=%s", status, body)
	}
	if status, body := rebalanceVolume(t, baseURL, "main", "", ""); status != http.StatusBadRequest {
		t.Fatalf("缺 to_volume 应 400, got %d body=%s", status, body)
	}
	for _, bad := range []string{"abc", "-1"} {
		status, body := rebalanceVolume(t, baseURL, "main", "disk2", bad)
		if status != http.StatusBadRequest {
			t.Fatalf("max_bytes=%q 应 400, got %d body=%s", bad, status, body)
		}
		if out := decodeRebalanceResp(t, body); out.Success {
			t.Fatalf("max_bytes=%q 应失败, got %+v", bad, out)
		}
	}

	h := &Handlers{logger: testLogger()}
	req := httptest.NewRequest(http.MethodPost, "/api/volumes/rebalance?from_volume=main&to_volume=disk2", nil)
	rec := httptest.NewRecorder()
	h.rebalanceVolumeHandler(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "卷功能未装配") {
		t.Fatalf("未装配卷功能应 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestRebalanceMigrateFiles_BudgetBranches 验证迁移预算的两条分支：预算放不下的大文件
// **跳过**（继续看更小的），以及成功迁满预算后 **停止**。
func TestRebalanceMigrateFiles_BudgetBranches(t *testing.T) {
	t.Parallel()
	baseURL, h, dirs := twoVolumeServer(t)
	rebalanceUploadFiles(t, baseURL, dirs, h, map[string]string{
		"big.txt":   strings.Repeat("B", 10),
		"small.txt": strings.Repeat("s", 4),
	})

	// 预算 7：放不下 big(10) → 跳过；放得下 small(4) → 迁 1 个、4 字节。
	status, body := rebalanceVolume(t, baseURL, "main", "disk2", "7")
	if status != http.StatusOK {
		t.Fatalf("status=%d want 200, body=%s", status, body)
	}
	out := decodeRebalanceResp(t, body)
	if out.Moved != 1 || out.BytesMoved != 4 {
		t.Fatalf("预算 7 应只迁 small(1 个/4 字节), got %+v", out)
	}

	// 预算 10：迁 big(10) 后 bytesMoved==maxBytes → 立即停止（不再迁 small）。
	status, body = rebalanceVolume(t, baseURL, "main", "disk2", "10")
	if status != http.StatusOK {
		t.Fatalf("status=%d want 200, body=%s", status, body)
	}
	out = decodeRebalanceResp(t, body)
	if out.Moved != 1 || out.BytesMoved != 10 {
		t.Fatalf("预算 10 应迁满即停（1 个/10 字节）, got %+v", out)
	}
	if out.Remaining != 0 {
		t.Fatalf("remaining 应为 0（big 迁走后 from 卷已空）, got %d", out.Remaining)
	}
	_ = h
}

// TestParseMaxBytes_Table 验证 max_bytes 解析：空=不限、纯数字、非法值报错。
func TestParseMaxBytes_Table(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"1024", 1024, false},
		{"12x", 12, false}, // Sscanf 前缀匹配（既有实现语义，锁定防漂移）
		{"abc", 0, true},
	}
	for _, tc := range cases {
		got, err := parseMaxBytes(tc.in)
		if tc.wantErr != (err != nil) {
			t.Fatalf("parseMaxBytes(%q) err=%v wantErr=%v", tc.in, err, tc.wantErr)
		}
		if !tc.wantErr && got != tc.want {
			t.Fatalf("parseMaxBytes(%q)=%d want %d", tc.in, got, tc.want)
		}
	}
}

// TestRebalanceMaxBytes_Table 验证查询参数形态：缺省/空 = 不限，负数与非法值 fail-closed。
func TestRebalanceMaxBytes_Table(t *testing.T) {
	t.Parallel()
	cases := []struct {
		query   string
		want    int64
		wantOK  bool
		comment string
	}{
		{"", 0, true, "缺省 = 不限"},
		{"?max_bytes=", 0, true, "空串 = 不限"},
		{"?max_bytes=8", 8, true, "合法值"},
		{"?max_bytes=-1", 0, false, "负数 fail-closed"},
		{"?max_bytes=abc", 0, false, "非法值 fail-closed"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/volumes/rebalance"+tc.query, nil)
		got, ok := rebalanceMaxBytes(req)
		if ok != tc.wantOK || got != tc.want {
			t.Fatalf("%s: got (%d,%v) want (%d,%v)", tc.comment, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestVolumesAPI_RefreshWithoutStateCache 验证无状态缓存时的明确状态码：
// 全部卷 refresh → 405；外部卷（后端无索引）refresh → 405（不静默 no-op）。
func TestVolumesAPI_RefreshWithoutStateCache(t *testing.T) {
	t.Parallel()
	baseURL, h, _ := newVolumesAPIServer(t, "alice", []VolumeConfig{{Name: "main", Root: t.TempDir()}}, nil)
	if h.stateCache != nil {
		t.Fatal("夹具前提：不应装配状态缓存")
	}
	if err := h.volSet.AddExternalVolume(volume.Volume{
		Name: "plain", Type: "ops-plain",
		ACL: volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{"alice": {}}},
	}, &opsBackend{fs: newOpsFS()}); err != nil {
		t.Fatalf("AddExternalVolume: %v", err)
	}

	if status, body := postRefresh(t, baseURL, ""); status != http.StatusMethodNotAllowed {
		t.Fatalf("全部卷 refresh 无缓存应 405, got %d body=%s", status, body)
	}
	// 外部卷后端无 IndexRefresher 且无状态缓存 ⇒ 405（不静默 no-op）。
	if status, body := postRefresh(t, baseURL, "plain"); status != http.StatusMethodNotAllowed {
		t.Fatalf("外部卷 refresh 无索引/无缓存应 405, got %d body=%s", status, body)
	}
}

// TestVolumesAPI_RefreshExternalIndexError 验证外部卷索引重建失败 → 500（不假成功）。
func TestVolumesAPI_RefreshExternalIndexError(t *testing.T) {
	t.Parallel()
	baseURL, h, _ := newVolumesAPIServer(t, "alice", []VolumeConfig{{Name: "main", Root: t.TempDir()}}, nil)
	if err := h.volSet.AddExternalVolume(volume.Volume{
		Name: "vault", Type: "secretdata",
		ACL: volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{"alice": {}}},
	}, &failingRefreshBackend{refreshableBackend: refreshableBackend{fs: newOpsFS()}}); err != nil {
		t.Fatalf("AddExternalVolume: %v", err)
	}
	status, body := postRefresh(t, baseURL, "vault")
	if status != http.StatusInternalServerError {
		t.Fatalf("索引重建失败应 500, got %d body=%s", status, body)
	}
	if !strings.Contains(string(body), "索引重建失败") {
		t.Fatalf("响应应说明索引重建失败, body=%s", body)
	}
}

// failingRefreshBackend 是「索引重建必失败」的外部卷后端（RefreshIndex 返回错误）。
type failingRefreshBackend struct{ refreshableBackend }

// openCoverageRoot 打开测试存储根（带 LAYOUT_VERSION 初始化）。
func openCoverageRoot(t *testing.T, dir string) *storage.Root {
	t.Helper()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

// writeCoverageFile 在 root 相对 rel 写入内容（父目录自动创建）。
func writeCoverageFile(t *testing.T, root *storage.Root, rel, content string) {
	t.Helper()
	if dir := filepath.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("建目录 %s: %v", dir, err)
		}
	}
	f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("写 %s: %v", rel, err)
	}
	if _, err = f.Write([]byte(content)); err != nil {
		t.Fatalf("写 %s: %v", rel, err)
	}
	if err = f.Close(); err != nil {
		t.Fatalf("关闭 %s: %v", rel, err)
	}
}

func (b *failingRefreshBackend) RefreshIndex(context.Context) error {
	return errors.New("index rebuild failed")
}

var _ registry.IndexRefresher = (*failingRefreshBackend)(nil)

// postRefresh 触发 POST /api/volumes/refresh（volume 为空 = 全部卷）。
func postRefresh(t *testing.T, baseURL, volName string) (int, []byte) {
	t.Helper()
	u := baseURL + "/api/volumes/refresh"
	if volName != "" {
		u += "?volume=" + url.QueryEscape(volName)
	}
	req, err := http.NewRequest(http.MethodPost, u, nil)
	if err != nil {
		t.Fatalf("构造 refresh 请求: %v", err)
	}
	hc := testHTTPClientAt()
	defer hc.CloseIdleConnections()
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("refresh 请求: %v", err)
	}
	defer resp.Body.Close()
	body := make([]byte, 0, 256)
	buf := make([]byte, 256)
	for {
		n, rerr := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if rerr != nil {
			break
		}
	}
	return resp.StatusCode, body
}

// TestCrossVolumeCopy_ErrorBranches 验证跨卷复制的错误分支：源缺失、目标父路径被文件占用、
// 目标被目录占用（rename 失败）——均必须报错，不静默成功。
func TestCrossVolumeCopy_ErrorBranches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	srcRoot := openCoverageRoot(t, srcDir)
	dstRoot := openCoverageRoot(t, dstDir)
	writeCoverageFile(t, srcRoot, "user/a.txt", "payload")

	if _, err := crossVolumeCopy(ctx, srcRoot, dstRoot, "user/missing.txt", "user/b.txt"); err == nil {
		t.Fatal("源缺失必须报错")
	}

	// 目标父路径被同名文件占用 ⇒ 建目录失败。
	writeCoverageFile(t, dstRoot, "user", "blocker")
	if _, err := crossVolumeCopy(ctx, srcRoot, dstRoot, "user/a.txt", "user/b.txt"); err == nil {
		t.Fatal("目标父路径被文件占用必须报错")
	}

	// 目标被目录占用 ⇒ 原子 rename 失败（不静默覆盖目录）。
	if err := dstRoot.Remove("user"); err != nil {
		t.Fatalf("移除占位文件: %v", err)
	}
	if err := dstRoot.MkdirAll("user/b.txt", 0o755); err != nil {
		t.Fatalf("占位目录: %v", err)
	}
	if _, err := crossVolumeCopy(ctx, srcRoot, dstRoot, "user/a.txt", "user/b.txt"); err == nil {
		t.Fatal("目标被目录占用必须报错")
	}
}

// TestVolumeDisplayCapacity_NilAndPool 验证展示容量取值：未装配卷集 → 卷声明容量；
// 有池 → 池上限（>0 时）。
func TestVolumeDisplayCapacity_NilAndPool(t *testing.T) {
	t.Parallel()
	v := volume.Volume{Name: "main", Capacity: 4096}
	if got := volumeDisplayCapacity(nil, v); got != 4096 {
		t.Fatalf("未装配卷集应回落声明容量, got %d", got)
	}
	dir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = dir
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: dir, VolCapacity: 4096}}
	h := buildVolSetHandlers(t, cfg)
	if got := volumeDisplayCapacity(h.volSet, v); got != 4096 {
		t.Fatalf("池上限应与卷声明容量一致, got %d", got)
	}
}

// TestVolumeAllowedFor_Unassembled 验证未装配卷集时 ACL 判定 fail-closed。
func TestVolumeAllowedFor_Unassembled(t *testing.T) {
	t.Parallel()
	if (&Handlers{logger: testLogger()}).volumeAllowedFor("alice", "main") {
		t.Fatal("未装配卷集必须 fail-closed（false）")
	}
}
