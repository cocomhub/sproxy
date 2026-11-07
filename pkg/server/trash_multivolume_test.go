// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// trash_multivolume_test.go 锁定回收站的**多卷语义**（用户裁定 2026-10-19 ③：各卷独立管理 +
// 汇总展示）。背景：软删落**文件所在卷**的 trash，而此前列表/恢复/清空只认 `tenantFor(owner)`
// 的**默认卷根** ⇒ 其它卷（user 卷等）的条目**不可见、不可恢复**（记档缺陷）。
//
// 覆盖（每条都可区分修复前后）：
//  ① 汇总列表（缺省）列出**两卷**条目且各带 `volume` 字段（修复前只 1 条 ✗）；
//  ② `?volume=` 只列该卷；
//  ③ restore 缺省**跨卷定位**（非默认卷的条目也能恢复）；
//  ④ empty `?volume=` 只清该卷（其它卷条目仍在）；
//  ⑤ empty 缺省只清默认卷（**既有语义零回归**：破坏性默认不静默扩大）；
//  ⑥ empty `?all=1` 清全部卷。

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// uploadFileSignedTo 带卷上传（`?volume=<name>`；volumeName 为空 ⇒ 默认卷）。
func uploadFileSignedTo(t *testing.T, baseURL, filename string, body []byte, volumeName string) int {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, werr := part.Write(body); werr != nil {
		t.Fatalf("write body: %v", werr)
	}
	if cerr := mw.Close(); cerr != nil {
		t.Fatalf("close multipart: %v", cerr)
	}
	target := baseURL + "/upload"
	if volumeName != "" {
		target += "?volume=" + volumeName
	}
	req, err := http.NewRequest(http.MethodPost, target, &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(headerContentType, mw.FormDataContentType())
	req.Header.Set("X-File-Checksum", sha256hex(body))
	// 上传端点按**请求体**验签（与既有 uploadFileSigned 同口径）。
	signBodyRequest(req, testAccessKey, testAccessSecret, buf.Bytes())
	resp, derr := testHTTPClient(t).Do(req)
	if derr != nil {
		t.Fatalf("upload: %v", derr)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// softDeleteSigned 带卷软删（`?filename=&volume=&soft=true` + 校验和头）。
func softDeleteSigned(t *testing.T, cl *http.Client, baseURL, filename, volumeName, checksum string) int {
	t.Helper()
	target := baseURL + "/delete?filename=" + filename + "&soft=true"
	if volumeName != "" {
		target += "&volume=" + volumeName
	}
	req, _ := http.NewRequest(http.MethodPost, target, nil)
	req.Header.Set("X-File-Checksum", checksum)
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// trashListedEntry 是回收站列表条目（含多卷视图的 volume 字段）。
type trashListedEntry struct {
	TrashRel string `json:"trash_rel"`
	Name     string `json:"name"`
	Volume   string `json:"volume"`
}

// listTrashSigned 列出回收站（volumeName 为空 ⇒ **汇总**全部本地卷）。
func listTrashSigned(t *testing.T, cl *http.Client, baseURL, volumeName string) (int, []trashListedEntry) {
	t.Helper()
	target := baseURL + "/api/trash"
	if volumeName != "" {
		target += "?volume=" + volumeName
	}
	req, _ := http.NewRequest(http.MethodGet, target, nil)
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("list trash: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body struct {
		Entries []trashListedEntry `json:"entries"`
	}
	if uerr := json.Unmarshal(raw, &body); uerr != nil {
		t.Fatalf("unmarshal trash list: %v (%s)", uerr, raw)
	}
	return resp.StatusCode, body.Entries
}

// restoreTrashSigned 恢复回收站条目（volumeName 为空 ⇒ 跨卷定位）。
func restoreTrashSigned(t *testing.T, cl *http.Client, baseURL, trashRel, volumeName string) int {
	t.Helper()
	target := baseURL + "/api/trash/restore?file=" + trashRel
	if volumeName != "" {
		target += "&volume=" + volumeName
	}
	req, _ := http.NewRequest(http.MethodPost, target, nil)
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// emptyTrashSigned 清空回收站（volumeName 非空 ⇒ 该卷；all ⇒ 全部卷；皆无 ⇒ 默认卷）。
func emptyTrashSigned(t *testing.T, cl *http.Client, baseURL, volumeName string, all bool) int {
	t.Helper()
	target := baseURL + "/api/trash/empty"
	switch {
	case volumeName != "":
		target += "?volume=" + volumeName
	case all:
		target += "?all=1"
	}
	req, _ := http.NewRequest(http.MethodPost, target, nil)
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("empty trash: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// downloadSigned 下载（带卷）并返回状态与内容（用于验证恢复后的文件真的可读）。
func downloadSigned(t *testing.T, cl *http.Client, baseURL, filename, volumeName string) (int, []byte) {
	t.Helper()
	target := baseURL + "/download?filename=" + filename
	if volumeName != "" {
		target += "&volume=" + volumeName
	}
	req, _ := http.NewRequest(http.MethodGet, target, nil)
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// TestTrash_MultiVolume_SummaryAndPerVolume 见文件头覆盖清单。
func TestTrash_MultiVolume_SummaryAndPerVolume(t *testing.T) {
	t.Parallel()
	dir1, dir2 := t.TempDir(), t.TempDir()
	url, _, _ := newTestServerCreds(t, func(c *Config) {
		c.StorageRoot = dir1
		c.Volumes = []VolumeConfig{
			{Name: "default", Root: dir1},
			{Name: "v2", Root: dir2},
		}
	})
	cl := &http.Client{Transport: netutil.IsolatedTransport()}
	b1 := []byte("trash default volume content")
	b2 := []byte("trash v2 volume content")

	// 落两卷：a.txt → 默认卷；b.txt → v2。
	if st := uploadFileSignedTo(t, url, "a.txt", b1, ""); st != 200 {
		t.Fatalf("upload default = %d", st)
	}
	if st := uploadFileSignedTo(t, url, "b.txt", b2, "v2"); st != 200 {
		t.Fatalf("upload v2 = %d", st)
	}
	if st := softDeleteSigned(t, cl, url, "a.txt", "", sha256hex(b1)); st != 200 {
		t.Fatalf("soft delete default = %d", st)
	}
	if st := softDeleteSigned(t, cl, url, "b.txt", "v2", sha256hex(b2)); st != 200 {
		t.Fatalf("soft delete v2 = %d", st)
	}

	// ① 汇总（缺省）：两卷各 1 条，volume 字段正确（修复前只看默认卷 ⇒ 仅 1 条）。
	st, all := listTrashSigned(t, cl, url, "")
	if st != 200 {
		t.Fatalf("list summary = %d", st)
	}
	if len(all) != 2 {
		t.Fatalf("汇总应 2 条（默认卷 + v2），实测 %d: %+v", len(all), all)
	}
	byVol := map[string]string{}
	for _, e := range all {
		byVol[e.Volume] = e.Name
	}
	if byVol["default"] != "user/a.txt" || byVol["v2"] != "user/b.txt" {
		t.Fatalf("汇总条目卷/名不符: %+v", byVol)
	}

	// ② ?volume=v2：只该卷。
	st2, onlyV2 := listTrashSigned(t, cl, url, "v2")
	if st2 != 200 || len(onlyV2) != 1 || onlyV2[0].Volume != "v2" || onlyV2[0].Name != "user/b.txt" {
		t.Fatalf("?volume=v2 应只 1 条且卷=v2: st=%d %+v", st2, onlyV2)
	}

	// ③ 跨卷恢复（缺省不带 volume）：v2 的条目也能恢复（修复前 404 ✗）。
	if st3 := restoreTrashSigned(t, cl, url, onlyV2[0].TrashRel, ""); st3 != 200 {
		t.Fatalf("跨卷恢复应成功，实测 %d", st3)
	}
	if _, after := listTrashSigned(t, cl, url, "v2"); len(after) != 0 {
		t.Fatalf("恢复后 v2 回收站应空: %+v", after)
	}
	// 强断言：恢复后的文件**内容可取回**（仅「回收站空了」不足以证明恢复正确——meta sidecar
	// 未随迁时读路径会失败）。
	if st, got := downloadSigned(t, cl, url, "b.txt", "v2"); st != 200 || !bytes.Equal(got, b2) {
		t.Fatalf("恢复后应能下载且内容一致: st=%d len=%d want=%d", st, len(got), len(b2))
	}

	// ③-b 探针：恢复后的文件必须**可再次软删**（恢复的完整性：读路径 ✓ 且删除路径 ✓；
	// 首轮实测此处曾 404 ⇒ 若红说明恢复产物在删除面不可用，属真实缺陷而非用例问题）。
	if st := softDeleteSigned(t, cl, url, "b.txt", "v2", sha256hex(b2)); st != 200 {
		t.Fatalf("恢复后的文件应可再次软删（恢复完整性），实测 %d", st)
	}
	if _, v2b := listTrashSigned(t, cl, url, "v2"); len(v2b) != 1 {
		t.Fatalf("再次软删后 v2 应有 1 条: %+v", v2b)
	}
	if st := emptyTrashSigned(t, cl, url, "v2", false); st != 200 {
		t.Fatalf("清 v2 供后续步骤: %d", st)
	}

	// ④ empty?volume=v2：只清该卷（默认卷条目仍在）。用**新上传**补一条 v2 软删。
	b3 := []byte("trash v2 second content")
	if st := uploadFileSignedTo(t, url, "c.txt", b3, "v2"); st != 200 {
		t.Fatalf("upload c to v2 = %d", st)
	}
	if st := softDeleteSigned(t, cl, url, "c.txt", "v2", sha256hex(b3)); st != 200 {
		t.Fatalf("soft delete c(v2) = %d", st)
	}
	if st4 := emptyTrashSigned(t, cl, url, "v2", false); st4 != 200 {
		t.Fatalf("empty v2 = %d", st4)
	}
	if _, v2 := listTrashSigned(t, cl, url, "v2"); len(v2) != 0 {
		t.Fatalf("empty?volume=v2 后 v2 应空: %+v", v2)
	}
	if _, def := listTrashSigned(t, cl, url, "default"); len(def) != 1 {
		t.Fatalf("empty?volume=v2 不得影响默认卷: %+v", def)
	}

	// ⑤ empty（缺省）：只清**默认卷**（既有语义零回归）。
	b4 := []byte("trash v2 third content")
	if st := uploadFileSignedTo(t, url, "d.txt", b4, "v2"); st != 200 {
		t.Fatalf("upload d to v2 = %d", st)
	}
	if st := softDeleteSigned(t, cl, url, "d.txt", "v2", sha256hex(b4)); st != 200 {
		t.Fatalf("soft delete d(v2) = %d", st)
	}
	if st5 := emptyTrashSigned(t, cl, url, "", false); st5 != 200 {
		t.Fatalf("empty default = %d", st5)
	}
	if _, def := listTrashSigned(t, cl, url, "default"); len(def) != 0 {
		t.Fatalf("empty（缺省）应清空默认卷: %+v", def)
	}
	if _, v2 := listTrashSigned(t, cl, url, "v2"); len(v2) != 1 {
		t.Fatalf("empty（缺省）**不得**清其它卷（零回归）: %+v", v2)
	}

	// ⑥ empty?all=1：清全部卷。
	if st6 := emptyTrashSigned(t, cl, url, "", true); st6 != 200 {
		t.Fatalf("empty all = %d", st6)
	}
	if _, allAfter := listTrashSigned(t, cl, url, ""); len(allAfter) != 0 {
		t.Fatalf("empty?all=1 后汇总应空: %+v", allAfter)
	}
}
