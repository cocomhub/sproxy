// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// blockaccess.go 实现 remoteFS 的 BlockAccessor（roadmap 4.3 P2 跨 FS 块级增量）：
//   - OpenReaderAt：读面链路 GET /download/chunk?offset=&size=（零新端点，复用既有块读）。
//   - OpenWriterAt：写面链路 3 端点会话（open→write→close，独立授权面与整文件写同源）。
//
// engine.syncFileBlock 对远端目标自动生效（BlockAccessor 断言）。

// chunkReaderAt 是经 /remote/download + Range 的 io.ReaderAt：每次 ReadAt 发一次
// Range 读请求（read 面白名单端点，零新增）。
type chunkReaderAt struct {
	c        *Client
	ref      Ref
	fileSize int64
}

func (r *chunkReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("remote: 负偏移 %d", off)
	}
	if off >= r.fileSize {
		return 0, io.EOF
	}
	length := int64(len(p))
	if off+length > r.fileSize {
		length = r.fileSize - off
	}
	end := off + length - 1
	rc, err := r.c.Open(context.Background(), r.ref, WithRange(off, end))
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	n, err := io.ReadFull(rc, p[:length])
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return n, err
	}
	return n, nil
}

// blockWriterAt 是写面块写会话（open→write→close）：持有会话 id，WriteAt 逐块偏移写。
type blockWriterAt struct {
	c       *Client
	ref     Ref
	blockID string
	closed  bool
}

func (w *blockWriterAt) WriteAt(p []byte, off int64) (int, error) {
	if w.closed {
		return 0, fmt.Errorf("remote: 块写会话已关闭")
	}
	resp, err := w.c.writeDo(context.Background(), w.ref, "/remote/block/write",
		map[string]string{"id": w.blockID, "offset": strconv.FormatInt(off, 10)},
		strings.NewReader(string(p)), nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return 0, fmt.Errorf("remote: 块写 %s@%d 失败 %d: %s", w.ref.Path, off, resp.StatusCode, string(b))
	}
	return len(p), nil
}

// Close 完成块写会话（设 mtime + 释放锁）。
func (w *blockWriterAt) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	resp, err := w.c.writeDo(context.Background(), w.ref, "/remote/block/close",
		map[string]string{"id": w.blockID}, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("remote: 块写完成 %s 失败 %d", w.ref.Path, resp.StatusCode)
	}
	return nil
}

// OpenReaderAt 打开远端路径的随机读（块级读旧文件/源文件）。
func (f *remoteFS) OpenReaderAt(ctx context.Context, path string) (io.ReaderAt, io.Closer, error) {
	p, err := normalizeRelPath(path)
	if err != nil {
		return nil, nil, err
	}
	ref := Ref{Node: f.ref.Node, Volume: f.ref.Volume, Path: p}
	st, err := f.c.Stat(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	if st == nil {
		return nil, nil, fmt.Errorf("remote: 块读 %q 不存在", path)
	}
	return &chunkReaderAt{c: f.c, ref: ref, fileSize: st.Size}, nil, nil
}

// OpenWriterAt 打开远端路径的随机写（块级差异写；open→write→close 会话）。
func (f *remoteFS) OpenWriterAt(ctx context.Context, path string, size, mtime int64) (io.WriterAt, io.Closer, error) {
	p, err := normalizeRelPath(path)
	if err != nil {
		return nil, nil, err
	}
	ref := Ref{Node: f.ref.Node, Volume: f.ref.Volume, Path: p}
	if err := f.c.requireWrite(); err != nil {
		return nil, nil, err
	}
	// open 会话：预分配 size + mtime。
	resp, werr := f.c.writeDo(ctx, ref, "/remote/block/open",
		map[string]string{"size": strconv.FormatInt(size, 10), "mtime": strconv.FormatInt(mtime, 10)}, nil, nil)
	if werr != nil {
		return nil, nil, werr
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("remote: 块写会话打开 %s 失败 %d", path, resp.StatusCode)
	}
	var out struct {
		BlockID string `json:"block_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, nil, err
	}
	if out.BlockID == "" {
		return nil, nil, fmt.Errorf("remote: 块写会话空 id")
	}
	return &blockWriterAt{c: f.c, ref: ref, blockID: out.BlockID}, &blockWriterAt{c: f.c, ref: ref, blockID: out.BlockID}, nil
}
