// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_inspect_test.go 锁「真实存储审查」HTTP 入口（设计
// docs/designs/2026-11-04-secretdata-storage-inspection.md §5.2）。
//
// 覆盖：正常正向/反向、非支持卷 501（不得返回空结果）、越界 403、缺失 404、
// 参数缺失 400。使用真实 secretdata 卷（本地 inner FS）而非假能力，避免「自证」。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/secretdata"
)

// newInspectTestHandler 构造带一个 secretdata 卷（vault）与一个本地卷（plain）的 Handlers。
func newInspectTestHandler(t *testing.T) (*Handlers, syncpkg.FS) {
	t.Helper()
	root := t.TempDir()
	rt, err := storage.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	// 先只登记本地卷；secretdata 卷经 AddExternalVolume 注入（NewSet 会为声明卷
	// 预建外部后端占位，重复注入会报「卷名必须唯一」）。
	vols := []volume.Volume{{Name: "plain", Type: volume.TypeLocal, RootDir: root}}
	vs := registry.NewSet(vols, map[string]*storage.Root{"plain": rt}, nil, nil, "plain")
	t.Cleanup(func() { _ = vs.Close() })

	inner := syncpkg.NewLocalFS(t.TempDir(), nil)
	be, err := secretdata.NewBackend(t.Context(), volume.Volume{Name: "vault", Type: "secretdata"}, inner, secretdata.Options{
		Secret:  []byte("inspect-test-key-000"),
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := vs.AddExternalVolume(volume.Volume{Name: "vault", Type: "secretdata"}, be); err != nil {
		t.Fatal(err)
	}
	// 非审查卷：普通外部 FS（不实现 StorageInspector）⇒ 必须 501（不得空结果）。
	plainExt := syncpkg.NewLocalFS(t.TempDir(), nil)
	if err := vs.AddExternalVolume(volume.Volume{Name: "flat", Type: "flat"}, inspectFakeBackend{fs: plainExt}); err != nil {
		t.Fatal(err)
	}
	h := &Handlers{volSet: vs, logger: testLogger()}
	return h, be.FS()
}

// inspectFakeBackend 是最小 ExternalBackend（测试用：非审查卷）。
type inspectFakeBackend struct{ fs syncpkg.FS }

func (f inspectFakeBackend) FS() syncpkg.FS { return f.fs }
func (f inspectFakeBackend) Close() error   { return nil }

func newInspectMux(h *Handlers) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/volumes/{name}/trace", h.volumeTraceHandler)
	mux.HandleFunc("GET /api/volumes/{name}/explain", h.volumeExplainHandler)
	return mux
}

func doInspectRequest(t *testing.T, mux *http.ServeMux, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestVolumeInspect_TraceAndExplain(t *testing.T) {
	t.Parallel()
	h, fsys := newInspectTestHandler(t)
	mux := newInspectMux(h)

	// 写入必须走**卷键空间**（`<owner>/user/<rel>`）——与 handler 的 traceKeyFor 同源。
	v, ok := h.volSet.ByName("vault")
	if !ok {
		t.Fatal("vault 不存在")
	}
	loc, lerr := v.ResolveUserLocation("anonymous", "docs/a.txt")
	if lerr != nil {
		t.Fatal(lerr)
	}
	content := []byte(strings.Repeat("inspect-data-", 40))
	if err := fsys.WriteFile(t.Context(), syncpkg.LogicalKey{}, loc.FSPath(), strings.NewReader(string(content)), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// 正向。
	rec := doInspectRequest(t, mux, "/api/volumes/vault/trace?path="+url.QueryEscape("docs/a.txt"))
	if rec.Code != http.StatusOK {
		t.Fatalf("trace status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var trResp struct {
		Trace *volume.PathTrace `json:"trace"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &trResp); err != nil || trResp.Trace == nil {
		t.Fatalf("trace 响应解析失败: %v (%s)", err, rec.Body.String())
	}
	if trResp.Trace.Kind != "file" || trResp.Trace.Size != int64(len(content)) || len(trResp.Trace.Chunks) == 0 {
		t.Fatalf("trace 内容异常: %+v", trResp.Trace)
	}

	// 反向（容器）。
	container := trResp.Trace.Meta.RealPath
	if i := strings.LastIndex(container, "/"); i >= 0 {
		container = container[:i]
	}
	rec = doInspectRequest(t, mux, "/api/volumes/vault/explain?real="+url.QueryEscape(container)+"&disk=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("explain status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var exResp struct {
		Explain *volume.RealExplanation `json:"explain"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &exResp); err != nil || exResp.Explain == nil {
		t.Fatalf("explain 响应解析失败: %v (%s)", err, rec.Body.String())
	}
	if exResp.Explain.Kind != "container" || exResp.Explain.Container == nil ||
		!strings.HasSuffix(exResp.Explain.Container.LogicalDir, "user/docs") {
		t.Fatalf("explain 内容异常: %+v", exResp.Explain)
	}
	if len(exResp.Explain.Entries) == 0 {
		t.Fatal("容器解释应列出逐条用途")
	}
}

func TestVolumeInspect_Errors(t *testing.T) {
	t.Parallel()
	h, _ := newInspectTestHandler(t)
	mux := newInspectMux(h)

	cases := []struct {
		name   string
		target string
		want   int
	}{
		{"卷不支持审查", "/api/volumes/flat/trace?path=a.txt", http.StatusNotImplemented},
		{"卷不存在", "/api/volumes/nope/trace?path=a.txt", http.StatusNotFound},
		{"越界", "/api/volumes/vault/explain?real=" + url.QueryEscape("../other"), http.StatusForbidden},
		{"绝对路径", "/api/volumes/vault/explain?real=" + url.QueryEscape("/etc/passwd"), http.StatusForbidden},
		{"逻辑路径不存在", "/api/volumes/vault/trace?path=nope/x", http.StatusNotFound},
		{"real 缺失", "/api/volumes/vault/explain", http.StatusBadRequest},
	}
	for _, c := range cases {
		if rec := doInspectRequest(t, mux, c.target); rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d (body=%s)", c.name, rec.Code, c.want, rec.Body.String())
		}
	}
}
