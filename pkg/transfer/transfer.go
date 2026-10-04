// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package transfer 提供统一「传输/转存」管道抽象（用户裁定 2026-10-04：
// 归档 = 转存 + 可选预处理）。
//
// 单一 code path：读 source → [可选预处理] → 写 destination（目标卷）。
//   - 无预处理（None） = 纯转存（individual，单文件原样）
//   - 有预处理（Pack/PackEncrypt） = 旧概念归档升级（多文件打包后转存）
//   - 行为正交：打包与否不影响落盘；落盘到哪不影响打包
//
// 目标卷经 sync.FS 抽象（secretdata 加密卷 / 普通卷纯上传，透明），
// 原子写（WriteIfAbsent）防静默覆盖；ACL/配额/失败队列由调用方装配。
package transfer

import (
	"context"
	"io"
	"strings"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// SourceEntry 是传输源的一个文件条目（元信息；本层不真实读取加密数据，
// 只汇总——图片分块等封装由具体 Preprocessor 实现处理）。
type SourceEntry struct {
	// Rel 是源相对路径（正斜杠，卷视图内）。
	Rel string
	// Size 是文件大小（字节；未知 = 0）。
	Size int64
	// ModTime 是文件修改时间（Unix 秒；未知 = 0）。
	ModTime int64
	// Reader 是文件内容读取器（nil = 由调用方按 Rel 另取）。
	Reader io.Reader
}

// Preprocessor 是可选预处理层：把源产物流封装为单一聚合流。
// 实现可插拔（注册表见 registry.go）；nil 预处理 = 纯转存（individual）。
type Preprocessor interface {
	// Name 返回预处理类型名（"" = 无 = individual）。
	Name() string
	// Pack 把 entries 封装为单一聚合流写到 w（tar.gz / tar.aes / 分块封装）。
	// 只汇总元信息与流组织，不真实读写加密数据（分块封装时由具体实现决定）。
	Pack(ctx context.Context, entries []SourceEntry, w io.Writer) error
}

// TransferRequest 是一次传输请求（统一转存/归档的输入）。
type TransferRequest struct {
	// Entries 是源文件条目（≥1）。
	Entries []SourceEntry
	// Preprocess 是预处理（nil = individual 纯转存）。
	Preprocess Preprocessor
	// Destination 是目标卷（经 VolumeFS 解析）。
	Destination VolumeFS
	// Rel 是目标相对路径（含文件名；空 = 自动派生由调用方填）。
	Rel string
	// Owner 是请求者（ACL/配额判定）。
	Owner string
	// Overwrite 是否允许覆盖已存在目标（false = WriteIfAbsent 拒绝）。
	Overwrite bool
}

// VolumeFS 是目标卷写入视图（sync.FS + 能力接口查询）。
type VolumeFS interface {
	syncpkg.FS
	// Scheme 返回目标卷协议（URL 生成：<scheme>://<volume>/<rel>）。
	Scheme() string
	// Volume 返回目标卷名（URL authority）。
	Volume() string
}

// Execute 执行一次传输：写 source 流（经预处理）到目标卷 rel。
// 返回目标 URL（<scheme>://<volume>/<rel>）供 ResolveURL 取用。
func Execute(ctx context.Context, req TransferRequest) (string, error) {
	if len(req.Entries) == 0 {
		return "", ErrEmptySource
	}
	if req.Destination == nil {
		return "", ErrNoDestination
	}
	rel := req.Rel
	if rel == "" {
		rel = AutoRel(req.Entries[0].Rel)
	}
	// 预处理流组织：None = 单文件直写；Pack = 聚合流后直写。
	if req.Preprocess != nil {
		return executePacked(ctx, req, rel)
	}
	return executeIndividual(ctx, req, rel)
}

// executeIndividual 纯转存：单条目直写目标卷（复用 WriteIfAbsent 原子写）。
func executeIndividual(ctx context.Context, req TransferRequest, rel string) (string, error) {
	if len(req.Entries) != 1 {
		return "", ErrIndividualMulti
	}
	e := req.Entries[0]
	// 写目标：优先 WriteIfAbsent（防覆盖），降级 WriteFile。
	if !req.Overwrite {
		if pia, ok := req.Destination.(syncpkg.WriteIfAbsent); ok {
			written, err := pia.WriteIfAbsent(ctx, rel, e.Reader, e.Size, e.ModTime)
			if err != nil {
				return "", err
			}
			if !written {
				return "", ErrExists
			}
			return targetURL(req.Destination, rel), nil
		}
	}
	if err := req.Destination.WriteFile(ctx, rel, e.Reader, e.Size, e.ModTime); err != nil {
		return "", err
	}
	return targetURL(req.Destination, rel), nil
}

// executePacked 预处理后转存：聚合流写目标卷。
func executePacked(ctx context.Context, req TransferRequest, rel string) (string, error) {
	// 打包输出到 io.Pipe（流式）；goroutine 打包 → 主线程读 pr 写卷。
	pr, pw := io.Pipe()
	defer pr.Close() // 无论成败关闭读端（goroutine 侧 pw.CloseWithError 释放）
	packErrCh := make(chan error, 1)
	go func() {
		err := req.Preprocess.Pack(ctx, req.Entries, pw)
		pw.CloseWithError(err)
		packErrCh <- err
	}()
	if err := req.Destination.WriteFile(ctx, rel, pr, 0, 0); err != nil {
		return "", err
	}
	if err := <-packErrCh; err != nil {
		return "", err
	}
	return targetURL(req.Destination, rel), nil
}

// targetURL 生成目标 URL（<scheme>://<volume>/<rel>）。
func targetURL(d VolumeFS, rel string) string {
	return d.Scheme() + "://" + d.Volume() + "/" + strings.TrimPrefix(rel, "/")
}

// AutoRel 自动派生目标相对路径（用源 basename；目录前缀由调用方 policy 决定）。
func AutoRel(srcRel string) string {
	if srcRel == "" {
		return ""
	}
	if i := strings.LastIndex(srcRel, "/"); i >= 0 {
		return srcRel[i+1:]
	}
	return srcRel
}
