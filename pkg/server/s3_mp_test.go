// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// s3_mp_test.go 验证 S3 分块上传（roadmap P2 S3 服务端扩展）：
//  1. POST ?uploads → UploadId；PUT ?partNumber&uploadId → ETag；POST ?uploadId → 完整文件。
//  2. DELETE ?uploadId（abort）→ 清 parts（会话残留清理）。

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// TestS3Server_MultipartUpload init → part → complete 全流程。
func TestS3Server_MultipartUpload(t *testing.T) {
	t.Parallel()
	url, _, _ := newTestServerCreds(t, nil)
	cl := &http.Client{Transport: netutil.IsolatedTransport()}
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	// 1. Initiate（POST ?uploads）。
	auth := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodPost, "/s3/big.bin?uploads", host, nil, now)
	req, _ := http.NewRequest(http.MethodPost, url+"/s3/big.bin?uploads", nil)
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", fmt.Sprintf("%x", sha256sum(nil)))
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("init = %d body=%s", resp.StatusCode, body[:min(len(body), 200)])
	}
	var initRes struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		UploadID string   `xml:"UploadId"`
	}
	if xerr := xml.Unmarshal(body, &initRes); xerr != nil || initRes.UploadID == "" {
		t.Fatalf("init 解析失败: %v body=%s", xerr, body)
	}
	uploadID := initRes.UploadID

	// 2. UploadPart（PUT ?partNumber=1&uploadId=<id>）。
	part1 := []byte("hello part1 ")
	authP := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodPut, "/s3/big.bin?partNumber=1&uploadId="+uploadID, host, part1, now)
	reqP, _ := http.NewRequest(http.MethodPut, url+"/s3/big.bin?partNumber=1&uploadId="+uploadID, bytes.NewReader(part1))
	reqP.Header.Set("Authorization", authP)
	reqP.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	reqP.Header.Set("x-amz-content-sha256", fmt.Sprintf("%x", sha256sum(part1)))
	respP, err := cl.Do(reqP)
	if err != nil {
		t.Fatalf("part1: %v", err)
	}
	io.Copy(io.Discard, respP.Body)
	respP.Body.Close()
	if respP.StatusCode != 200 {
		t.Fatalf("part1 = %d", respP.StatusCode)
	}
	etag1 := respP.Header.Get("ETag")
	if etag1 == "" {
		t.Fatal("part1 应返回 ETag")
	}

	part2 := []byte("world part2")
	authP2 := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodPut, "/s3/big.bin?partNumber=2&uploadId="+uploadID, host, part2, now)
	reqP2, _ := http.NewRequest(http.MethodPut, url+"/s3/big.bin?partNumber=2&uploadId="+uploadID, bytes.NewReader(part2))
	reqP2.Header.Set("Authorization", authP2)
	reqP2.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	reqP2.Header.Set("x-amz-content-sha256", fmt.Sprintf("%x", sha256sum(part2)))
	respP2, err := cl.Do(reqP2)
	if err != nil {
		t.Fatalf("part2: %v", err)
	}
	io.Copy(io.Discard, respP2.Body)
	respP2.Body.Close()
	if respP2.StatusCode != 200 {
		t.Fatalf("part2 = %d", respP2.StatusCode)
	}

	// 3. Complete（POST ?uploadId body）。
	completeBody := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part><Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, etag1, respP2.Header.Get("ETag"))
	authC := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodPost, "/s3/big.bin?uploadId="+uploadID, host, []byte(completeBody), now)
	reqC, _ := http.NewRequest(http.MethodPost, url+"/s3/big.bin?uploadId="+uploadID, strings.NewReader(completeBody))
	reqC.Header.Set("Authorization", authC)
	reqC.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	reqC.Header.Set("x-amz-content-sha256", fmt.Sprintf("%x", sha256sum([]byte(completeBody))))
	respC, err := cl.Do(reqC)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	io.Copy(io.Discard, respC.Body)
	respC.Body.Close()
	if respC.StatusCode != 200 {
		t.Fatalf("complete = %d", respC.StatusCode)
	}

	// 4. GET 验证完整文件。
	authG := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodGet, "/s3/big.bin", host, nil, now)
	reqG, _ := http.NewRequest(http.MethodGet, url+"/s3/big.bin", nil)
	reqG.Header.Set("Authorization", authG)
	reqG.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	reqG.Header.Set("x-amz-content-sha256", fmt.Sprintf("%x", sha256sum(nil)))
	respG, err := cl.Do(reqG)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, _ := io.ReadAll(respG.Body)
	respG.Body.Close()
	if respG.StatusCode != 200 || string(got) != "hello part1 world part2" {
		t.Fatalf("GET = %d body=%q", respG.StatusCode, got)
	}
}

// TestS3Server_AbortMultipart DELETE ?uploadId → 会话清理。
func TestS3Server_AbortMultipart(t *testing.T) {
	t.Parallel()
	url, _, _ := newTestServerCreds(t, nil)
	cl := &http.Client{Transport: netutil.IsolatedTransport()}
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	// Init。
	auth := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodPost, "/s3/abort.bin?uploads", host, nil, now)
	req, _ := http.NewRequest(http.MethodPost, url+"/s3/abort.bin?uploads", nil)
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", fmt.Sprintf("%x", sha256sum(nil)))
	resp, _ := cl.Do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var initRes struct {
		UploadID string `xml:"UploadId"`
	}
	_ = xml.Unmarshal(body, &initRes)

	// Upload part。
	part := []byte("partial")
	authP := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodPut, "/s3/abort.bin?partNumber=1&uploadId="+initRes.UploadID, host, part, now)
	reqP, _ := http.NewRequest(http.MethodPut, url+"/s3/abort.bin?partNumber=1&uploadId="+initRes.UploadID, bytes.NewReader(part))
	reqP.Header.Set("Authorization", authP)
	reqP.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	reqP.Header.Set("x-amz-content-sha256", fmt.Sprintf("%x", sha256sum(part)))
	respP, _ := cl.Do(reqP)
	io.Copy(io.Discard, respP.Body)
	respP.Body.Close()
	if respP.StatusCode != 200 {
		t.Fatalf("part = %d", respP.StatusCode)
	}

	// Abort（DELETE ?uploadId）。
	authA := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodDelete, "/s3/abort.bin?uploadId="+initRes.UploadID, host, nil, now)
	reqA, _ := http.NewRequest(http.MethodDelete, url+"/s3/abort.bin?uploadId="+initRes.UploadID, nil)
	reqA.Header.Set("Authorization", authA)
	reqA.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	reqA.Header.Set("x-amz-content-sha256", fmt.Sprintf("%x", sha256sum(nil)))
	respA, _ := cl.Do(reqA)
	io.Copy(io.Discard, respA.Body)
	respA.Body.Close()
	if respA.StatusCode != 204 && respA.StatusCode != 200 {
		t.Fatalf("abort = %d", respA.StatusCode)
	}

	// 目标文件不应存在（未 complete）。
	authG := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodGet, "/s3/abort.bin", host, nil, now)
	reqG, _ := http.NewRequest(http.MethodGet, url+"/s3/abort.bin", nil)
	reqG.Header.Set("Authorization", authG)
	reqG.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	reqG.Header.Set("x-amz-content-sha256", fmt.Sprintf("%x", sha256sum(nil)))
	respG, _ := cl.Do(reqG)
	io.Copy(io.Discard, respG.Body)
	respG.Body.Close()
	if respG.StatusCode != http.StatusNotFound {
		t.Fatalf("abort 后 GET 应 404, got %d", respG.StatusCode)
	}
}
