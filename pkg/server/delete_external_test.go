// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// delete_external_test.go 验证**外部卷对象的用户可见删除入口**（评审 F1，P1 功能死路）。
//
// 背景：外部卷没有 *storage.Root，`locateForRead`/`volumeFileExists` 恒未命中 ⇒ 其对象此前
// 没有任何用户可见的删除/定位入口；卷写满（used==capacity）后该卷**一切写入恒 507**（含覆盖
// 写缩小），只能靠同步删除传播或运维手工删计数文件+重启。本文件的用例钉住新增的硬删入口：
// 经装配层同一可信链（`Guard(Wrap(CapacityFS(raw)))`）删主文件 + 联动删 sidecar + 释放卷级计数。

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume/capacity"
)

// delAwareExtFS 让 extFS.Delete 真正从内存表移除（extFS 的 Delete 是 no-op——它只服务读路径
// 测试；删除语义必须可观测，否则「文件应已删除」断言会假绿/假红）。
type delAwareExtFS struct{ *extFS }

func (f *delAwareExtFS) Delete(_ context.Context, p string) error {
	delete(f.files, p)
	return nil
}

// externalDeleteEnv 装配「本地默认卷 main + 外部卷 baidu（FS 经 CapacityFS 记账）」的 Handlers。
func externalDeleteEnv(t *testing.T, objs map[string]string) (*Handlers, *extFS, *capacity.LedgerCounter) {
	t.Helper()
	base := &extFS{files: objs}
	fs := &delAwareExtFS{extFS: base}
	pool := quota.NewPool(1000)
	led, lerr := capacity.OpenLedger(t.TempDir(), "test")
	if lerr != nil {
		t.Fatal(lerr)
	}
	cnt := capacity.NewLedgerCounter(led, pool.MaxBytes())
	wrapped := capacity.Wrap(fs, cnt)
	h := newExternalEnvWithFS(t, "baidu", wrapped)
	return h, base, cnt
}

// sidecarFor 造一份描述 given 内容的合法 sidecar 并放到外部卷的 meta 键空间。
func sidecarFor(t *testing.T, fs *extFS, ownerKey string, body []byte) *meta.FileMeta {
	t.Helper()
	calc, err := meta.NewCalculator(int64(len(body)), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if _, werr := calc.ReadFrom(strings.NewReader(string(body))); werr != nil {
		t.Fatal(werr)
	}
	fm := calc.Finish()
	raw, merr := meta.Marshal(fm)
	if merr != nil {
		t.Fatal(merr)
	}
	fs.files[meta.MetaPath(ownerKey)] = string(raw)
	return fm
}

// TestDeleteFile_ExternalVolume_HardDeleteReleasesCapacity 外部卷对象可经 /delete 硬删：
// 主文件消失 + 卷级计数释放（打破「写满后永久 507」死路）。
func TestDeleteFile_ExternalVolume_HardDeleteReleasesCapacity(t *testing.T) {
	t.Parallel()
	body := []byte("external-delete-body")
	ownerKey := "alice/user/docs/f.bin"
	h, fs, cnt := externalDeleteEnv(t, map[string]string{ownerKey: string(body)})
	if err := cnt.TryAdd(int64(len(body))); err != nil { // 预置占用（模拟已写入的字节）
		t.Fatal(err)
	}
	if got := cnt.Used(); got != int64(len(body)) {
		t.Fatalf("预置占用 = %d, want %d", got, len(body))
	}

	if _, err := h.fileService().DeleteFile(context.Background(), files.DeleteFileInput{
		Owner: "alice", RemotePath: "docs/f.bin", ExplicitVol: "baidu",
		ExpectedChecksum: sha256hex(body),
	}); err != nil {
		t.Fatalf("外部卷硬删应成功: %v", err)
	}
	if _, still := fs.files[ownerKey]; still {
		t.Fatal("主文件应已删除")
	}
	if got := cnt.Used(); got != 0 {
		t.Fatalf("卷级计数应释放为 0, got %d", got)
	}
}

// TestDeleteFile_ExternalVolume_ChecksumMismatchRejected 提供 checksum 时与 sidecar **记录**
// 的整文件 SHA-256 交叉比对（不重算——远端整文件回读不可接受）；不一致 → 409 且对象保留。
func TestDeleteFile_ExternalVolume_ChecksumMismatchRejected(t *testing.T) {
	t.Parallel()
	body := []byte("external-delete-checksum")
	ownerKey := "alice/user/docs/f.bin"
	h, fs, _ := externalDeleteEnv(t, map[string]string{ownerKey: string(body)})
	sidecarFor(t, fs, ownerKey, body) // sidecar 记录 body 的哈希

	wrong := sha256hex([]byte("something-else"))
	_, err := h.fileService().DeleteFile(context.Background(), files.DeleteFileInput{
		Owner: "alice", RemotePath: "docs/f.bin", ExplicitVol: "baidu",
		ExpectedChecksum: wrong,
	})
	he, ok := err.(*files.HTTPError)
	if !ok || he.Status != http.StatusConflict {
		t.Fatalf("checksum 不一致应 409, got %v", err)
	}
	if _, still := fs.files[ownerKey]; !still {
		t.Fatal("校验失败时不得删除对象")
	}

	// 正确 checksum → 删除成功（对象 + sidecar 均清）。
	if _, derr := h.fileService().DeleteFile(context.Background(), files.DeleteFileInput{
		Owner: "alice", RemotePath: "docs/f.bin", ExplicitVol: "baidu",
		ExpectedChecksum: sha256hex(body),
	}); derr != nil {
		t.Fatalf("正确 checksum 应删除成功: %v", derr)
	}
	if _, still := fs.files[ownerKey]; still {
		t.Fatal("对象应已删除")
	}
	if _, metaStill := fs.files[meta.MetaPath(ownerKey)]; metaStill {
		t.Fatal("sidecar 应联动删除")
	}
}

// TestDeleteFile_ExternalVolume_MissingAndIdempotent 显式外部卷上对象不存在 → 404；
// AllowMissing（批量族）→ 幂等成功。
func TestDeleteFile_ExternalVolume_MissingAndIdempotent(t *testing.T) {
	t.Parallel()
	h, _, _ := externalDeleteEnv(t, map[string]string{})
	_, err := h.fileService().DeleteFile(context.Background(), files.DeleteFileInput{
		Owner: "alice", RemotePath: "docs/nope.bin", ExplicitVol: "baidu",
		ExpectedChecksum: sha256hex([]byte("x")),
	})
	if he, ok := err.(*files.HTTPError); !ok || he.Status != http.StatusNotFound {
		t.Fatalf("不存在应 404, got %v", err)
	}
	res, ierr := h.fileService().DeleteFile(context.Background(), files.DeleteFileInput{
		Owner: "alice", RemotePath: "docs/nope.bin", ExplicitVol: "baidu",
		ExpectedChecksum: sha256hex([]byte("x")), AllowMissing: true,
	})
	if ierr != nil || !res.Idempotent {
		t.Fatalf("AllowMissing 应幂等成功, got res=%+v err=%v", res, ierr)
	}
}

// TestDelete_HTTPFace_ExternalVolume 经真实 HTTP 面（POST /delete?volume=…）删除外部卷对象。
func TestDelete_HTTPFace_ExternalVolume(t *testing.T) {
	t.Parallel()
	body := []byte("external-delete-http")
	ownerKey := "alice/user/docs/f.bin"
	h, fs, _ := externalDeleteEnv(t, map[string]string{ownerKey: string(body)})

	mux := http.NewServeMux()
	mux.HandleFunc("POST /delete", h.delete)
	req := httptest.NewRequest(http.MethodPost,
		"/delete?filename=docs/f.bin&volume=baidu", nil)
	req = req.WithContext(withActor(req.Context(), "alice"))
	req.Header.Set("X-File-Checksum", sha256hex(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /delete = %d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Success bool `json:"success"`
	}
	if uerr := json.Unmarshal(rec.Body.Bytes(), &out); uerr != nil || !out.Success {
		t.Fatalf("响应应 success, body=%s err=%v", rec.Body.String(), uerr)
	}
	if _, still := fs.files[ownerKey]; still {
		t.Fatal("主文件应已删除")
	}
}

// TestStat_ExternalVolume_Works 外部卷对象的 /stat 经读路径（resolveDownloadPath 的外部卷
// 分支）已可用——本用例钉住它，避免 F1 修复时误改读路径。
func TestStat_ExternalVolume_Works(t *testing.T) {
	t.Parallel()
	body := []byte("external-stat-body")
	ownerKey := "alice/user/f.bin"
	h, _, _ := externalDeleteEnv(t, map[string]string{ownerKey: string(body)})

	mux := http.NewServeMux()
	mux.HandleFunc("HEAD /api/files/stat", h.fileService().Stat)
	req := httptest.NewRequest(http.MethodHead, "/api/files/stat?filename=f.bin&volume=baidu", nil)
	req = req.WithContext(withActor(req.Context(), "alice"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("外部卷 stat = %d", rec.Code)
	}
	if got := rec.Header().Get("X-File-Size"); got != "18" {
		t.Fatalf("X-File-Size = %q, want 18", got)
	}
}

var _ syncpkg.FS = (*capacity.CapacityFS)(nil)

// TestS3Delete_ExistingObject_NoChecksumRequired 回归（#765 归口引入）：S3 的 DeleteObject
// **无 checksum 语义**（客户端不发 X-File-Checksum），而 `DeleteFile` 的 checksum 门禁会先回
// 400 —— 既有对象因此删不掉。DELETE 归口统一写内核时必须显式 `SkipChecksumVerify`（安全由
// 同 rel 文件锁 + 本地卷 quarantine 承担）。
func TestS3Delete_ExistingObject_NoChecksumRequired(t *testing.T) {
	t.Parallel()
	url, _ := newS3QuotaServer(t, func(c *Config) { c.Volumes[0].VolCapacity = 1 << 20 })
	cl := s3TestClient()
	host := strings.TrimPrefix(url, "http://")
	now := time.Now()
	body := []byte("s3-delete-body")

	put, _ := http.NewRequest(http.MethodPut, url+"/s3/key.txt", bytes.NewReader(body))
	put.Header.Set("Authorization", sigV4SignHost(testAccessKey, testAccessSecret, http.MethodPut, "/s3/key.txt", host, body, now))
	put.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	put.Header.Set("x-amz-content-sha256", hex.EncodeToString(sha256sum(body)))
	presp, perr := cl.Do(put)
	if perr != nil {
		t.Fatalf("PUT: %v", perr)
	}
	pbody, _ := io.ReadAll(presp.Body)
	presp.Body.Close()
	if presp.StatusCode != 200 && presp.StatusCode != 201 {
		t.Fatalf("PUT = %d body=%q", presp.StatusCode, pbody)
	}

	del, _ := http.NewRequest(http.MethodDelete, url+"/s3/key.txt", nil)
	del.Header.Set("Authorization", sigV4SignHost(testAccessKey, testAccessSecret, http.MethodDelete, "/s3/key.txt", host, nil, now))
	del.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	del.Header.Set("x-amz-content-sha256", hex.EncodeToString(sha256sum(nil)))
	dresp, derr := cl.Do(del)
	if derr != nil {
		t.Fatalf("DELETE: %v", derr)
	}
	dbody, _ := io.ReadAll(dresp.Body)
	dresp.Body.Close()
	if dresp.StatusCode != 204 && dresp.StatusCode != 200 {
		t.Fatalf("S3 DELETE 既有对象应成功（无 checksum 语义）, got %d body=%q", dresp.StatusCode, dbody)
	}
}
