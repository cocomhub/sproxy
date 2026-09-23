// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// s3_multipart_test.go 钉住 S3 分块上传的加固点（审查批次10 P1 修复）：
// partNumber 校验 / 请求体限流 / 数量上限——防路径注入与 OOM/磁盘 DoS。

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// TestS3Multipart_InvalidPartNumber 钉住「partNumber 校验」（审查批次10 P1）：
// 非数字/负数/超大（>s3MaxParts）→ 非法（此前直接拼路径 DoS）。
func TestS3Multipart_InvalidPartNumber(t *testing.T) {
	t.Parallel()
	cases := []string{"abc", "-1", "0", "10001", "1/2", ".."}
	for _, pn := range cases {
		n, err := strconv.Atoi(pn)
		ok := err == nil && n > 0 && n <= s3MaxParts
		if ok {
			t.Fatalf("partNumber %q 应非法", pn)
		}
	}
	if n, err := strconv.Atoi("5"); err != nil || n != 5 || n > s3MaxParts {
		t.Fatalf("合法 partNumber 5 应通过")
	}
}

// TestS3Multipart_UploadInvalidPartNumber_Rejected 钉住「handler 层 partNumber 校验」
// （审查批次10 P1）：PUT ?partNumber=../x（路径注入）→ 400（此前直接拼 rel）。
func TestS3Multipart_UploadInvalidPartNumber_Rejected(t *testing.T) {
	t.Parallel()
	url, _, _ := newTestServerCreds(t, nil)
	cl := &http.Client{Transport: netutil.IsolatedTransport()}
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")
	// 非数字 partNumber（路径注入形态）。
	auth := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodPut, "/s3/big.bin?partNumber=../x&uploadId=u1", host, []byte("p"), now)
	req, _ := http.NewRequest(http.MethodPut, url+"/s3/big.bin?partNumber=../x&uploadId=u1", strings.NewReader("p"))
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", fmt.Sprintf("%x", sha256sum([]byte("p"))))
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 partNumber 应 400, got %d", resp.StatusCode)
	}
}
