// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// wrapper_enclosure_fixture_test.go 是**阶段 2 迁移后的封装卷测试夹具**（共享）：
//
//   - 通用文件写/读 mux 原语（upload/delete/rename/mkdir/rmdir/download/list 族）——
//     原先与过渡层的 links 测试同文件，随过渡层删除后抽取至此；
//   - 封装处理层（`enclosure.Wrapper`）替身注册：让 `type: <wrapper>` + `extra.target:
//     "<卷>/<子目录>"` 的用户面声明经**装配层翻译**落到 enclosure 底座（封印章流程）;
//   - `nestedRootBackend`：可作用户卷底层的本地根 backend（FS = LocalFS(root)）。

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/enclosure"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// ---- 通用文件 mux 原语 ----

// actorFileWriteMux 构造把固定 actor 注入请求 ctx 后转发文件写/读 handler 的 mux。
// 模拟 authMiddleware 验签后 withActor 的行为（复用 upload/delete/rename/list/download 族）。
func actorFileWriteMux(h *Handlers, actor string) *http.ServeMux {
	wrap := func(hf http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(withActor(r.Context(), actor))
			hf(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", wrap(h.upload))
	mux.HandleFunc("POST /delete", wrap(h.delete))
	mux.HandleFunc("POST /rename", wrap(h.rename))
	mux.HandleFunc("POST /mkdir", wrap(h.mkdir))
	mux.HandleFunc("POST /rmdir", wrap(h.rmdir))
	mux.HandleFunc("GET /download", wrap(h.download))
	mux.HandleFunc("GET /api/files", wrap(h.listFiles))
	mux.HandleFunc("POST /upload/init", wrap(h.uploadInit))
	mux.HandleFunc("POST /upload/chunk", wrap(h.uploadChunk))
	mux.HandleFunc("POST /upload/complete", wrap(h.uploadComplete))
	return mux
}

// headerFilePath 是上传子目录路径头（Go ≥1.26 multipart 文件名会 filepath.Base 掉目录，
// 子目录必须走 X-File-Path——与 upload_owner_test 等既有测试一致）。
const headerFilePath = "X-File-Path"

// writeMuxUpload 走真实 upload handler 上传 filename 到显式卷 vol（带 X-File-Checksum），
// 返回状态码与响应体。filename 含子目录时经 X-File-Path 头传递（保留完整路径）。
func writeMuxUpload(t *testing.T, mux *http.ServeMux, filename, vol string, body []byte) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if vol != "" {
		if err := mw.WriteField("volume", vol); err != nil {
			t.Fatalf("write volume field: %v", err)
		}
	}
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err = part.Write(body); err != nil {
		t.Fatalf("write part: %v", err)
	}
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/upload", &buf)
	req.Header.Set(headerContentType, mw.FormDataContentType())
	req.Header.Set(headerFileChecksum, sha256hex(body))
	req.Header.Set(headerFilePath, filename)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// writeMuxDelete 走真实 delete handler 删除 filename（显式卷 vol + X-File-Checksum）。
func writeMuxDelete(t *testing.T, mux *http.ServeMux, filename, vol, checksum string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/delete?filename="+filename+"&volume="+vol, nil)
	req.Header.Set(headerFileChecksum, checksum)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// writeMuxDownload 走真实 download handler 读取 filename（显式卷 vol），返回状态码与内容。
func writeMuxDownload(t *testing.T, mux *http.ServeMux, filename, vol string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/download?filename="+filename+"&volume="+vol, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Code, rr.Body.Bytes()
}

// writeMuxList 走真实 list handler 列 subdir（显式卷 vol），返回状态码。
func writeMuxList(t *testing.T, mux *http.ServeMux, subdir, vol string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/files?subdir="+subdir+"&volume="+vol, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// assertBodyHas 断言响应体含指定子串。
func assertBodyHas(t *testing.T, body string, sub string) {
	t.Helper()
	if !strings.Contains(body, sub) {
		t.Fatalf("响应体缺少 %q: %s", sub, body)
	}
}

// ---- 封装处理层替身（enclosure.Wrapper） ----

// passthruBackend 是「原样透传源视图」的封装后端：把 region 视图直接当卷 FS 用
// （等效于「只做区域映射、不改字节」的处理层）。
type passthruBackend struct{ fs syncpkg.FS }

func (b *passthruBackend) FS() syncpkg.FS { return b.fs }
func (b *passthruBackend) Close() error   { return nil }

// passthruWrapper 是 passthruBackend 的封装处理层实现（Name = 注册名）。
type passthruWrapper struct{ name string }

func (w passthruWrapper) Name() string { return w.name }

func (w passthruWrapper) Build(_ context.Context, _ volume.Volume, src syncpkg.FS, _ map[string]any) (registry.ExternalBackend, error) {
	return &passthruBackend{fs: src}, nil
}

var fixtureWrapperOnce sync.Map // name → *sync.Once

// registerFixtureWrapper 注册测试用封装处理层（同名幂等；`RegisterWrapper` 重复注册会 panic）。
// 无需任何「类型推迟标记」：装配层按**声明**判据（enclosure.IsDeclared）跳过 registry.NewBackend
// ——显式 `type: enclosure` 与嵌套 wrapper 类型同样无需标记为 deferred。
func registerFixtureWrapper(name string) {
	onceAny, _ := fixtureWrapperOnce.LoadOrStore(name, &sync.Once{})
	onceAny.(*sync.Once).Do(func() { enclosure.RegisterWrapper(passthruWrapper{name: name}) })
}

// wrapperFSType 是「真实 FS 封装后端」类型名：按用户面语法 `target=<卷>/<子目录>` 以已创建卷
// 的子目录作封装根（经装配层翻译出封装声明词汇、走 enclosure 装配；处理层只看得到区域相对视图）。
const wrapperFSType = "wrap-fs-test"

// registerWrapperFSBackend 注册 wrapperFSType 的封装处理层（幂等）。
func registerWrapperFSBackend() { registerFixtureWrapper(wrapperFSType) }

// ---- 嵌套底层用户卷 backend（FS = LocalFS(root)） ----

// nestedRootType 是可作嵌套底层的用户卷 backend（FS 为 LocalFS，供 FSFor/Stat 解析底层
// 子目录存在性）。factory 从 extra.root 建本地根。
const nestedRootType = "nested-volroot"

// nestedRootBackend 以 LocalFS 为 FS 的用户卷 backend。
type nestedRootBackend struct {
	fs syncpkg.FS
}

func (b *nestedRootBackend) FS() syncpkg.FS  { return b.fs }
func (b *nestedRootBackend) Close() error    { return nil }
func (b *nestedRootBackend) Usage() int64    { return 0 }
func (b *nestedRootBackend) Capacity() int64 { return 0 }

var registerNestedRootOnce sync.Once

func registerNestedRootBackend() {
	registerNestedRootOnce.Do(func() {
		registry.RegisterBackend(nestedRootType, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
			root, _ := v.Extra["root"].(string)
			if root == "" {
				return nil, errNestedRootRequiresRoot
			}
			return &nestedRootBackend{fs: syncpkg.NewLocalFS(root, nil)}, nil
		})
		// 同名的封装处理层：使 `type: nestedRootType` + `extra.target: "<卷>/<子>"` 的声明
		// 被判为封装声明（nestedRootType 已被注册为 wrapper ⇒ 嵌套翻译 + IsDeclared 判定通过）。
		registerFixtureWrapper(nestedRootType)
	})
}

// errNestedRootRequiresRoot 是 nestedRootType 缺 extra.root 的构造错误（哨兵便于断言）。
var errNestedRootRequiresRoot = errString("nested-root: extra.root 必填")

// errString 是测试用常量错误类型（fmt.Errorf 常量文案即可，无需 errors.New 引入）。
type errString string

func (e errString) Error() string { return string(e) }

// ---- 用户卷 API 测试夹具 ----

// newNestedWrapperAPIHandlers 装配封装卷测试 Handlers（main 本地卷 + 用户卷 store +
// 可作嵌套底层的用户卷类型 + 封装处理层）。
func newNestedWrapperAPIHandlers(t *testing.T) (*Handlers, *UserVolumeStore) {
	t.Helper()
	registerUserVolWrapBackend()
	registerNestedRootBackend()
	registerWrapperFSBackend()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	store := NewUserVolumeStore(cfg.StorageRoot)
	h.SetUserVolumeStore(store)
	return h, store
}

// postUserVol 发送创建用户卷请求。
func postUserVol(t *testing.T, mux *http.ServeMux, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/volumes/user", bytes.NewReader(data))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}
