// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// s3_trusted_meta_test.go 补写路径 meta 联动测试盲区（第 4 轮对抗评审 F2）：
// 此前 `s3WriteMetaAfter` / `writeCopyMeta` / `moveMetaAfterVolumeMove` 等在 *_test.go 中
// **零引用**，联动被删/写坏时读侧静默退化为直算/直通而测试仍全绿。本用例经真实 S3 网关
// PUT，断言磁盘上落有**可解析且哈希自洽**的 sidecar（真副作用断言，非替身）。

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
