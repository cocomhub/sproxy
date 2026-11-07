// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// s3_trusted_meta_test.go 补写路径 meta 联动测试盲区（第 4 轮对抗评审 F2）：
// 此前 `s3WriteMetaAfter` / `writeCopyMeta` / `moveMetaAfterVolumeMove` 等在 *_test.go 中
// **零引用**，联动被删/写坏时读侧静默退化为不校验/直通而测试仍全绿。本用例经真实 S3 网关
// PUT，断言磁盘上落有**可解析且哈希自洽**的 sidecar（真副作用断言，非替身）。

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/netutil"
)

// TestS3Put_CreatesTrustedSidecar S3 单对象 PUT 后 meta 桶存在可解析、哈希自洽的 sidecar。
func TestS3Put_CreatesTrustedSidecar(t *testing.T) {
	t.Parallel()
	url, cfgPtr, _ := newTestServerCreds(t, nil)
	cl := &http.Client{Transport: netutil.IsolatedTransport()}
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	body := []byte("trusted-sidecar-content-0123456789")
	key := "dir/x.bin"
	auth := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodPut, "/s3/"+key, host, body, now)
	req, _ := http.NewRequest(http.MethodPut, url+"/s3/"+key, bytes.NewReader(body))
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", fmt.Sprintf("%x", sha256sum(body)))
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("put = %d", resp.StatusCode)
	}

	storageRoot := cfgPtr.Load().StorageRoot
	var found string
	_ = filepath.WalkDir(storageRoot, func(p string, d os.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(filepath.ToSlash(p), "/meta/"+key+".meta") {
			found = p
		}
		return nil
	})
	if found == "" {
		t.Fatalf("S3 PUT 后应在 meta 桶生成 sidecar（storageRoot=%s, key=%s）", storageRoot, key)
	}
	raw, rerr := os.ReadFile(found)
	if rerr != nil {
		t.Fatal(rerr)
	}
	fm, uerr := meta.Unmarshal(raw)
	if uerr != nil {
		t.Fatalf("sidecar 反序列化: %v", uerr)
	}
	if err := meta.Validate(fm); err != nil {
		t.Fatalf("sidecar Validate: %v", err)
	}
	want := fmt.Sprintf("%x", sha256sum(body))
	if fm.TotalSHA256 != want {
		t.Fatalf("sidecar TotalSHA256=%s want %s", fm.TotalSHA256, want)
	}
}

// TestS3Multipart_SweepStaleOrphans 回归（M9a，第 7 轮 MAJOR）：客户端崩溃/断连未发 abort 的
// 分块会话（parts + meta）必须被清扫（否则永久残留占 chunk 桶与配额）。懒触发（init 顺带），
// TTL 24h。变异：去掉 sweepStaleMultipart 调用/实现 → 过期会话不被删 → 红。
func TestS3Multipart_SweepStaleOrphans(t *testing.T) {
	t.Parallel()
	h := &Handlers{logger: testLogger()}
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })

	writePart := func(uploadID string, partNum int) {
		rel := "chunk/" + multipartPartPrefix + uploadID + partSuffix + strconv.Itoa(partNum)
		if werr := rootWriteFile(root, rel, []byte("part-body")); werr != nil {
			t.Fatalf("write part: %v", werr)
		}
	}
	writeMeta := func(uploadID string) {
		if werr := rootWriteFile(root, "chunk/"+multipartPartPrefix+uploadID+".meta", []byte("k")); werr != nil {
			t.Fatalf("write meta: %v", werr)
		}
	}
	age := func(uploadID string, d time.Duration) {
		for _, rel := range []string{
			"chunk/" + multipartPartPrefix + uploadID + ".meta",
			"chunk/" + multipartPartPrefix + uploadID + partSuffix + "1",
		} {
			full := filepath.Join(dir, filepath.FromSlash(rel))
			old := time.Now().Add(-d)
			if cerr := os.Chtimes(full, old, old); cerr != nil {
				t.Fatalf("Chtimes(%s): %v", rel, cerr)
			}
		}
	}

	// 过期会话（25h）+ 活跃会话（1h）。
	writeMeta("stale-session")
	writePart("stale-session", 1)
	age("stale-session", 25*time.Hour)
	writeMeta("live-session")
	writePart("live-session", 1)
	age("live-session", time.Hour)

	if n := h.s3Service().SweepStaleMultipart(root); n != 1 {
		t.Fatalf("应清扫 1 个过期会话，实际 %d", n)
	}
	for _, rel := range []string{
		"chunk/" + multipartPartPrefix + "stale-session.meta",
		"chunk/" + multipartPartPrefix + "stale-session" + partSuffix + "1",
	} {
		if _, serr := root.Stat(rel); serr == nil {
			t.Fatalf("过期会话残留: %s", rel)
		}
	}
	for _, rel := range []string{
		"chunk/" + multipartPartPrefix + "live-session.meta",
		"chunk/" + multipartPartPrefix + "live-session" + partSuffix + "1",
	} {
		if _, serr := root.Stat(rel); serr != nil {
			t.Fatalf("活跃会话不得被误清: %s (%v)", rel, serr)
		}
	}
}
