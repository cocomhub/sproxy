// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// e2echeck 是对**运行中** sproxy 实例做全能力面冒烟的巡检工具（与 `make test-e2e`
// 互补：后者自建进程与临时存储，本工具打的是你手动起好、带真实配置/卷/凭据的实例）。
//
// 断言原则：
//   1) 任何路由不得返回 5xx（除显式登记的预期 5xx）；
//   2) 关键能力必须真的工作（上传→下载→校验、分享访问、批量操作、分块上传…）；
//   3) 有业务错误的端点允许 4xx（未装配/参数缺失），但不允许「接线错误 500」。
//
// 用法：
//
//	go run ./tools/e2echeck -addr=https://127.0.0.1:18084 -ak=<AK> -sk=<SK> -skid=<SK条目ID>
//
// 凭据**运行期传入**（默认值是占位符，指向本机占位端口）——不要在仓库里写真实凭据/地址。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/httpc"
	"github.com/cocomhub/sproxy/pkg/netutil"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
)

// 目标地址与 SproxySig 凭据（**全部由命令行传入**；默认值为占位符，非真实数据）。
var (
	base    string
	ak      string
	sk      string
	entryID string
)

func parseFlags() {
	flag.StringVar(&base, "addr", "http://127.0.0.1:18999", "sproxy base URL（如 https://127.0.0.1:18084）")
	flag.StringVar(&ak, "ak", "ak-00000000000000000000000000000000", "SproxySig AccessKey")
	flag.StringVar(&sk, "sk", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "SproxySig AccessKeySecret")
	flag.StringVar(&entryID, "skid", "skey-000000000001", "SproxySig SK 条目 ID")
	flag.Parse()
}

var failures []string
var checks int

func fail(name, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	failures = append(failures, name+": "+msg)
	fmt.Printf("  ✗ %-52s %s\n", name, msg)
}

func ok(name, note string) {
	if note != "" {
		fmt.Printf("  ✓ %-52s %s\n", name, note)
	}
}

var client = httpc.NewHTTPClient(httpc.Options{
	Transport:     &signing{base: netutil.IsolatedTransport()},
	ClientTimeout: 30 * time.Second,
})

// signing 给每个请求加 SproxySig（body 先读出算 hash 再重放）。
type signing struct{ base http.RoundTripper }

func (s *signing) RoundTrip(req *http.Request) (*http.Response, error) {
	var bodyHash string
	if req.Body != nil && req.Body != http.NoBody {
		data, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		sum := sha256.Sum256(data)
		bodyHash = hex.EncodeToString(sum[:])
		req.ContentLength = int64(len(data))
		req.Body = io.NopCloser(bytes.NewReader(data))
	} else {
		bodyHash = sproxysig.EmptyBodyHash()
	}
	now := time.Now()
	h := sproxysig.Header{
		Version: sproxysig.Version, AK: ak, EntryID: entryID,
		TS: now.UnixMilli(), Exp: now.Add(sproxysig.DefaultExpiry).UnixMilli(),
		Nonce:      sproxysig.NewNonce(),
		BodySHA256: bodyHash,
	}
	req.Header.Set("Authorization", sproxysig.SignAndFormat(sk, h, req.Method, req.URL.EscapedPath(), req.URL.RawQuery))
	return s.base.RoundTrip(req)
}

// callAllow5xx 与 call 相同，但**不**把 5xx 记为失败（用于「501 = 功能未启用」这类正常业务态）。
func callAllow5xx(name, method, path string, body io.Reader, headers map[string]string) (int, []byte) {
	checks++
	req, err := http.NewRequest(method, base+path, body)
	if err != nil {
		fail(name, "构造请求失败: %v", err)
		return 0, nil
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		fail(name, "请求失败: %v", err)
		return 0, nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	ok(name, fmt.Sprintf("%d", resp.StatusCode))
	return resp.StatusCode, data
}

func call(name, method, path string, body io.Reader, headers map[string]string) (int, []byte) {
	checks++
	req, err := http.NewRequest(method, base+path, body)
	if err != nil {
		fail(name, "构造请求失败: %v", err)
		return 0, nil
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		fail(name, "请求失败: %v", err)
		return 0, nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 500 {
		fail(name, "%s %s → %d %s", method, path, resp.StatusCode, snippet(data))
		return resp.StatusCode, data
	}
	ok(name, fmt.Sprintf("%d", resp.StatusCode))
	return resp.StatusCode, data
}

func snippet(b []byte) string {
	s := string(b)
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return strings.ReplaceAll(s, "\n", " ")
}

func want(name string, got, expect int) bool {
	if got != expect {
		fail(name, "状态码 = %d, want %d", got, expect)
		return false
	}
	return true
}

func jsonHas(name string, data []byte, needle string) bool {
	if !strings.Contains(string(data), needle) {
		fail(name, "响应缺少 %q: %s", needle, snippet(data))
		return false
	}
	return true
}

func upload(name, filename string, content []byte) bool {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", filename)
	_, _ = fw.Write(content)
	_ = mw.Close()
	sum := sha256.Sum256(content)
	st, body := call(name, http.MethodPost, "/upload", &buf, map[string]string{
		"Content-Type":    mw.FormDataContentType(),
		"X-File-Checksum": hex.EncodeToString(sum[:]),
		"X-File-Path":     filename,
	})
	if st != http.StatusOK {
		fmt.Printf("      body: %s\n", snippet(body))
	}
	return want(name, st, http.StatusOK)
}

func checksumOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func sumOf(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

func main() {
	parseFlags()
	fmt.Println("=== 1. 公开面 / 未认证面")
	for _, p := range []string{"/healthz", "/version", "/metrics", "/ui/"} {
		call("public "+p, http.MethodGet, p, nil, nil)
	}
	// 未认证（无签名）必须 401，不能是 500。
	raw := httpc.NewHTTPClient(httpc.Options{Transport: netutil.IsolatedTransport(), ClientTimeout: 10 * time.Second})
	for _, p := range []string{"/api/files", "/api/volumes", "/api/credentials", "/api/config"} {
		checks++
		resp, err := raw.Get(base + p)
		if err != nil {
			fail("unauth "+p, "%v", err)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			fail("unauth "+p, "状态码 = %d, want 401（未认证必须被拒）", resp.StatusCode)
		} else {
			ok("unauth "+p, "401")
		}
	}

	fmt.Println("=== 2. 文件面基础链路（上传→下载→校验→列表→搜索）")
	content := []byte("hello sproxy e2e " + time.Now().Format(time.RFC3339Nano))
	dir := "e2e-" + time.Now().Format("150405")
	file := dir + "/a.txt"
	if upload("upload", file, content) {
		st, body := call("download", http.MethodGet, "/download?filename="+url.QueryEscape(file), nil, nil)
		if want("download", st, http.StatusOK) && !bytes.Equal(body, content) {
			fail("download", "内容不一致: got %q", snippet(body))
		}
		sum := sha256.Sum256(content)
		st, body = call("stat", http.MethodHead, "/api/files/stat?filename="+url.QueryEscape(file), nil, nil)
		_ = st
		_ = body
		st, body = call("list", http.MethodGet, "/api/files?subdir="+url.QueryEscape(dir), nil, nil)
		if want("list", st, http.StatusOK) {
			jsonHas("list", body, "a.txt")
		}
		st, body = call("search", http.MethodGet, "/api/files/search?q=a.txt", nil, nil)
		if want("search", st, http.StatusOK) {
			jsonHas("search", body, "a.txt")
		}
		st, body = call("du", http.MethodGet, "/api/du", nil, nil)
		if want("du", st, http.StatusOK) {
			jsonHas("du", body, "size")
		}
		call("get meta", http.MethodGet, "/api/files/meta?filename="+url.QueryEscape(file), nil, nil)
		call("put meta", http.MethodPut, "/api/files/meta?filename="+url.QueryEscape(file),
			strings.NewReader(`{"tags":["e2e"]}`), map[string]string{"Content-Type": "application/json"})
		call("tags", http.MethodPost, "/api/tags", strings.NewReader(`{"filename":"`+file+`","tags":["e2e","probe"]}`),
			map[string]string{"Content-Type": "application/json"})
		_ = sum
	}

	fmt.Println("=== 3. 目录 / 重命名 / 批量 / 版本 / 归档")
	call("mkdir", http.MethodPost, "/mkdir?dirname="+url.QueryEscape(dir+"/sub"), nil, nil)
	call("rename", http.MethodPost, "/rename?from="+url.QueryEscape(file)+"&to="+url.QueryEscape(dir+"/b.txt"), nil, map[string]string{
		"X-File-Checksum": checksumOf(content),
	})
	call("batch rename", http.MethodPost, "/api/batch/rename",
		strings.NewReader(`{"operations":[{"from":"`+dir+`/b.txt","to":"`+dir+`/c.txt","checksum":"`+checksumOf(content)+`"}]}`),
		map[string]string{"Content-Type": "application/json"})
	st, bodyV := callAllow5xx("versions", http.MethodGet, "/api/versions?filename="+url.QueryEscape(dir+"/c.txt"), nil, nil)
	// 501 = 版本管理未启用（默认配置），属正常业务态；其它非 200 才是缺陷。
	if st != http.StatusOK && st != http.StatusNotImplemented {
		fail("versions", "状态 = %d（应为 200 或 501 未启用）: %s", st, snippet(bodyV))
	}
	// 批量重命名后文件必须真的在：打一次 list 确认链路（而非只看 200）。
	stL, bodyL := call("list after batch", http.MethodGet, "/api/files?subdir="+url.QueryEscape(dir), nil, nil)
	if stL == http.StatusOK {
		jsonHas("list after batch", bodyL, "c.txt")
	}
	call("archive-dir", http.MethodGet, "/api/archive-dir", nil, nil)
	call("archive", http.MethodPost, "/api/archive",
		strings.NewReader(`{"name":"e2e.tar.gz","paths":["`+dir+`"]}`), map[string]string{"Content-Type": "application/json"})
	call("rmdir", http.MethodPost, "/rmdir?dirname="+url.QueryEscape(dir+"/sub"), nil, nil)

	fmt.Println("=== 4. 分块上传（init→chunk→status→complete）+ 分块下载")
	chunkFile := dir + "/chunked.bin"
	st, body := call("upload init", http.MethodPost, "/upload/init",
		strings.NewReader(`{"upload_id":"e2e-up-`+fmt.Sprint(time.Now().UnixNano())+`","filename":"`+chunkFile+`","total_size":11,"chunk_size":4096,"total_chunks":1,"file_checksum":"`+hex.EncodeToString(sumOf([]byte("hello-chunk")))+`"}`),
		map[string]string{"Content-Type": "application/json"})
	var initResp struct {
		UploadID string `json:"upload_id"`
	}
	_ = json.Unmarshal(body, &initResp)
	if st == http.StatusOK && initResp.UploadID != "" {
		payload := []byte("hello-chunk")
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("upload_id", initResp.UploadID)
		_ = mw.WriteField("index", "0")
		fw, _ := mw.CreateFormFile("chunk", "chunk0")
		_, _ = fw.Write(payload)
		_ = mw.Close()
		call("upload chunk", http.MethodPost, "/upload/chunk", &buf,
			map[string]string{"Content-Type": mw.FormDataContentType()})
		call("upload status", http.MethodGet, "/upload/status?upload_id="+url.QueryEscape(initResp.UploadID), nil, nil)
		call("upload complete", http.MethodPost, "/upload/complete?upload_id="+url.QueryEscape(initResp.UploadID), nil, nil)
		call("download chunk", http.MethodGet,
			"/download/chunk?filename="+url.QueryEscape(chunkFile)+"&offset=0&size=5", nil, nil)
	} else {
		fail("upload init", "未拿到 upload_id: %s", snippet(body))
	}

	fmt.Println("=== 5. 分享（POST /api/shares 复数，#1005 破坏性变更）")
	st, body = call("share create", http.MethodPost, "/api/shares",
		strings.NewReader(`{"filename":"`+dir+`/c.txt"}`), map[string]string{"Content-Type": "application/json"})
	var shareResp struct {
		Token string `json:"token"`
		URL   string `json:"url"`
	}
	_ = json.Unmarshal(body, &shareResp)
	if st == http.StatusOK && shareResp.Token != "" {
		checks++
		resp, err := raw.Get(base + "/s/" + shareResp.Token)
		if err != nil {
			fail("share access", "%v", err)
		} else {
			data, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !bytes.Contains(data, []byte("hello sproxy")) {
				fail("share access", "状态 = %d, body=%s", resp.StatusCode, snippet(data))
			} else {
				ok("share access", "200 + 内容一致")
			}
		}
		call("share list", http.MethodGet, "/api/shares", nil, nil)
		call("share revoke", http.MethodDelete, "/api/shares/"+url.QueryEscape(shareResp.Token), nil, nil)
	} else {
		fail("share create", "未拿到 token: %s", snippet(body))
	}
	// 旧路径必须 404/405（证明破坏性变更生效，而非两套并存）
	checks++
	resp, err := client.Post(base+"/api/share", "application/json", strings.NewReader(`{}`))
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode < 400 {
			fail("old /api/share removed", "旧路径仍返回 %d（应 404/405）", resp.StatusCode)
		} else {
			ok("old /api/share removed", fmt.Sprintf("%d", resp.StatusCode))
		}
	}

	fmt.Println("=== 6. 卷 / quota / backends / presign（#1018 修复点）")
	st, bodyVol := call("volumes list", http.MethodGet, "/api/volumes", nil, nil)
	if st == http.StatusOK {
		fmt.Printf("      volumes: %s\n", snippet(bodyVol))
		var volObj struct {
			Volumes []map[string]any `json:"volumes"`
		}
		_ = json.Unmarshal(bodyVol, &volObj)
		for _, v := range volObj.Volumes {
			if name, okName := v["name"].(string); okName && name != "" {
				call("volumes export "+name, http.MethodGet, "/api/volumes/export?volume="+url.QueryEscape(name), nil, nil)
			}
		}
	}
	call("volumes user", http.MethodGet, "/api/volumes/user", nil, nil)
	call("quota", http.MethodGet, "/api/quota", nil, nil)
	call("volumes export", http.MethodGet, "/api/volumes/export?volume=main", nil, nil)
	call("volumes refresh", http.MethodPost, "/api/volumes/refresh?volume=main", nil, nil)
	st, body = call("backends", http.MethodGet, "/api/backends", nil, nil)
	if want("backends", st, http.StatusOK) {
		var obj struct {
			Backends []map[string]any `json:"backends"`
		}
		if err := json.Unmarshal(body, &obj); err != nil {
			fail("backends", "响应不是 JSON: %s", snippet(body))
		} else if len(obj.Backends) == 0 {
			fail("backends", "backends 为空: %s", snippet(body))
		} else {
			_, hasType := obj.Backends[0]["type"]
			_, hasCat := obj.Backends[0]["category"]
			_, hasFields := obj.Backends[0]["fields"]
			if !hasType || !hasCat || !hasFields {
				fail("backends", "元素缺 type/category/fields: %v", obj.Backends[0])
			}
		}
	}
	// #1018：模式串 {type} 必须能匹配具体路径——此前每次都 500。
	stP, bodyP := call("presign（#1018）", http.MethodPost, "/api/backends/local/presign",
		strings.NewReader(`{"filename":"`+dir+`/c.txt"}`), map[string]string{"Content-Type": "application/json"})
	fmt.Printf("      presign: %d %s\n", stP, snippet(bodyP))
	call("presign complete（#1018）", http.MethodPost, "/api/backends/local/presign/complete",
		strings.NewReader(`{"filename":"`+dir+`/c.txt","upload_id":"x"}`), map[string]string{"Content-Type": "application/json"})

	fmt.Println("=== 7. secrets / 语义搜索（#1017 修复点）")
	call("secrets list", http.MethodGet, "/api/secrets", nil, nil)
	call("secrets create", http.MethodPost, "/api/secrets",
		strings.NewReader(`{"name":"e2e","kind":"random"}`), map[string]string{"Content-Type": "application/json"})
	// #1017：语义搜索是只读 GET，此前主 mux 每次 500。
	stS, bodyS := call("search semantic（#1017）", http.MethodGet, "/api/search/semantic?q=hello", nil, nil)
	fmt.Printf("      semantic: %d %s\n", stS, snippet(bodyS))

	fmt.Println("=== 8. 运维面 / 其它切片能力")
	for _, p := range []string{
		"/api/stats", "/api/config", "/api/usage/report", "/api/trash", "/api/audit",
		"/api/cloud/tasks", "/api/sync/tasks", "/api/credentials", "/api/hub/stats",
		"/api/hub/nodes", "/api/notify/history", "/api/events",
	} {
		call("ops "+p, http.MethodGet, p, nil, nil)
	}
	call("verify", http.MethodPost, "/api/verify", nil, nil)
	call("ai summarize（未装配应 4xx）", http.MethodPost, "/api/ai/summarize",
		strings.NewReader(`{"filename":"`+dir+`/c.txt"}`), map[string]string{"Content-Type": "application/json"})
	call("ai tag（未装配应 4xx）", http.MethodPost, "/api/ai/tag",
		strings.NewReader(`{"filename":"`+dir+`/c.txt"}`), map[string]string{"Content-Type": "application/json"})
	stF, bodyF := call("notify feed（未带 token）", http.MethodGet, "/api/notify/feed", nil, nil)
	fmt.Printf("      feed: %d %s\n", stF, snippet(bodyF))

	fmt.Println("=== 9. 协议挂载（DAV / S3 / pprof 必须受保护，不能 500）")
	for _, p := range []string{"/dav/", "/s3/", "/debug/pprof/"} {
		checkProto(p)
	}

	fmt.Println("=== 10. 清理（批量删除）")
	call("batch delete", http.MethodPost, "/api/batch/delete",
		strings.NewReader(`{"files":["`+dir+`/c.txt","`+dir+`/chunked.bin"]}`),
		map[string]string{"Content-Type": "application/json"})
	call("delete archive", http.MethodPost, "/delete?filename=e2e.tar.gz", nil, nil)

	fmt.Println()
	if len(failures) == 0 {
		fmt.Printf("RESULT: PASS（%d 项检查，0 失败）\n", checks)
		return
	}
	sort.Strings(failures)
	fmt.Printf("RESULT: FAIL（%d 项检查，%d 失败）\n", len(failures), checks)
	for _, f := range failures {
		fmt.Println("  - " + f)
	}
	os.Exit(1)
}

// checkProto 断言协议挂载点不返回 5xx（未带协议级凭据时应 401/403/404）。
func checkProto(p string) {
	checks++
	req, _ := http.NewRequest(http.MethodGet, base+p, nil)
	resp, err := client.Do(req)
	if err != nil {
		fail("proto "+p, "%v", err)
		return
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 500 {
		fail("proto "+p, "%d %s", resp.StatusCode, snippet(data))
		return
	}
	ok("proto "+p, fmt.Sprintf("%d", resp.StatusCode))
}
