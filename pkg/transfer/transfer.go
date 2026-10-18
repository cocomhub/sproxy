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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
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
	// Destination 是目标卷 FS（**直接传装饰链**——不套包装器：包装器会吞掉
	// WriteIfAbsent 等可选能力（接口嵌入不提升动态方法），且触发 R23 包装器门禁）。
	Destination syncpkg.FS
	// Scheme / Volume 是目标卷协议与卷名（URL 生成 `<scheme>://<volume>/<rel>`；
	// 由调用方从卷元信息提供，避免为拿这两个值去包一层 FS）。
	Scheme string
	Volume string
	// Rel 是目标相对路径（含文件名；空 = 自动派生由调用方填）。
	Rel string
	// Owner 是请求者（ACL/配额判定）。
	Owner string
	// Overwrite 是否允许覆盖已存在目标（false = WriteIfAbsent 拒绝）。
	Overwrite bool
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
	// 写目标：优先 WriteIfAbsent（防覆盖），降级 WriteFile（见 writeUnique）。
	if err := writeUnique(ctx, req.Destination, rel, e.Reader, e.Size, e.ModTime, req.Overwrite); err != nil {
		return "", err
	}
	return targetURL(req.Scheme, req.Volume, rel), nil
}

// writeUnique 以「目标唯一」语义写目标 rel：
//   - Overwrite=false：优先 WriteIfAbsent（原子、无 TOCTOU、并发安全防覆盖）；装饰器链
//     （trusted.Guard/Wrap、capacity、staging）**无条件实现**该可选能力接口，但底层可能
//     未实现（返回 syncpkg.ErrUnsupported）——此时**回落** Stat 存在性检查 + WriteFile。
//     目标 FS **完全无**该能力时也走同一 Stat 检查（断言失败不等于「可覆盖」）。
//   - 目标已存在 → ErrExists（fail-closed，拒绝静默覆盖）；存在性检查出错（非不存在）
//     也 fail-closed（不得把未确认状态当不存在继续写）。
//   - Overwrite=true：直接 WriteFile（允许覆盖）。
func writeUnique(ctx context.Context, dest syncpkg.FS, rel string, r io.Reader, size, mtime int64, overwrite bool) error {
	if !overwrite {
		if pia, ok := dest.(syncpkg.WriteIfAbsent); ok {
			written, err := pia.WriteIfAbsent(ctx, rel, r, size, mtime)
			switch {
			case err == nil && written:
				return nil
			case err == nil:
				return ErrExists
			case !errors.Is(err, syncpkg.ErrUnsupported):
				// **写失败必须清理残骸**（独立审核 P2，2026-10-18）：非原子后端（s3 单 PUT 原子，
				// 但 webdav/ftp/baidupcs 等非原子）写中途失败会在目标路径留下**半截对象** ⇒ 若不清
				// 理，后续每次重试都会因「目标已存在」被判 ErrExists ⇒ **永久阻断**（用户只能人工
				// 删对象，resume 也走同一路径），且半截对象成为孤儿（其字节已由 settleTransferCharge
				// 计入 owner 配额）。与 executePacked 的「pack 失败即删」同纪律。
				// 安全性：本分支只在 Overwrite=false 且**存在性检查已通过**（目标原本不存在）后进入
				// ⇒ 该 rel 上的对象只能是本次写的残骸 ⇒ 删除正确。
				_ = dest.Delete(ctx, rel)
				return err
			}
			// ErrUnsupported = 能力探针（未消费 reader）→ 降级存在性检查。
		}
		// 无原子能力 / 底层未实现 → 存在性检查（**不论能力分支是否命中**都执行）。
		e, serr := dest.Stat(ctx, rel)
		if serr != nil && !errors.Is(serr, fs.ErrNotExist) {
			return fmt.Errorf("transfer: 目标 %q 存在性检查失败: %w", rel, serr)
		}
		if e != nil {
			return ErrExists
		}
	}
	if werr := dest.WriteFile(ctx, syncpkg.LogicalKey{}, rel, r, size, mtime); werr != nil {
		if !overwrite {
			// 同上：Overwrite=false 时该 rel 上的对象只能是本次残骸 ⇒ 清理，保证重试可自愈。
			_ = dest.Delete(ctx, rel)
		}
		return werr
	}
	return nil
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
	// V1：覆盖保护与 individual 一致——WriteIfAbsent 优先（拒绝静默覆盖），
	// 否则降级 WriteFile。
	if err := packedWrite(ctx, req, rel, pr); err != nil {
		// join pack goroutine（防调用方 close 源 Reader 与 goroutine 仍在 io.Copy 读并发）：
		// 让管道写端报错退出后等待 goroutine 收尾再返回。
		_ = pr.CloseWithError(err)
		<-packErrCh
		return "", err
	}
	// 等 pack goroutine 收尾（WriteIfAbsent/WriteFile 已消费 pr 至 EOF）。
	// 收尾失败（如 tar 声明长度与内容不符）时目标已收下字节但内容损坏 ⇒ 删除，不得留
	// 损坏产物（与 bucket 归档路径「失败清理」同纪律）。
	if err := <-packErrCh; err != nil {
		_ = req.Destination.Delete(ctx, rel)
		return "", err
	}
	// V5b：写后校验目标存在且非空（防 0 字节写入失败/卷吞写）。
	if st, serr := req.Destination.Stat(ctx, rel); serr != nil {
		return "", fmt.Errorf("transfer: 写后校验失败: %w", serr)
	} else if st == nil || st.Size <= 0 {
		return "", fmt.Errorf("transfer: 目标 %q 为空（打包转存未落盘）", rel)
	}
	return targetURL(req.Scheme, req.Volume, rel), nil
}

// packedWrite 打包流的覆盖保护写：复用 writeUnique（WriteIfAbsent 优先，拒绝覆盖；
// 装饰器 ErrUnsupported 回落 WriteFile）。
func packedWrite(ctx context.Context, req TransferRequest, rel string, pr io.Reader) error {
	return writeUnique(ctx, req.Destination, rel, pr, 0, 0, req.Overwrite)
}

// targetURL 生成目标 URL（<scheme>://<volume>/<rel>；路径段 percent-encode，
// 与 cloud 转存 transferURL 同规约——`#`/`%`/空格等文件名才能 ResolveURL 往返）。
func targetURL(scheme, volume, rel string) string {
	return scheme + "://" + volume + "/" + escapeRel(rel)
}

// escapeRel 逐段 percent-encode 卷内相对路径（保留 `/` 分隔）。
func escapeRel(rel string) string {
	segs := strings.Split(strings.TrimPrefix(rel, "/"), "/")
	esc := make([]string, 0, len(segs))
	for _, s := range segs {
		esc = append(esc, url.PathEscape(s))
	}
	return strings.Join(esc, "/")
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
