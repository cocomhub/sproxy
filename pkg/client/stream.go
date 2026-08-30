// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// OpenDownload 流式下载远端文件，返回响应 body（io.ReadCloser）。
//
// 与 Download 不同，OpenDownload 不写本地文件、不做 checksum 校验，直接把
// resp.Body 交给调用方消费；由调用方负责 Close。供 pkg/sync.HTTPTransport
// 的 FS.OpenRead 使用（pkg/sync 负责 mtime/checksum 的差异判定，不需要此处复算）。
// 走既有 doRequest 的签名/隧道/直连逻辑。
func (c *FileClient) OpenDownload(ctx context.Context, filename string) (io.ReadCloser, error) {
	if containsPathTraversal(filename) {
		return nil, fmt.Errorf("filename 不能包含路径穿越符 '..'")
	}
	urlPath := "/download?" + url.Values{"filename": {filename}}.Encode()
	resp, err := c.doRequest(ctx, "GET", urlPath, nil, nil)
	if err != nil {
		return nil, fmt.Errorf(errFmtRequestFailed, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("下载失败 (状态码: %d): %s", resp.StatusCode, string(body))
	}
	return resp.Body, nil
}
