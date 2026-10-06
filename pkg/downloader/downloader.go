// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package downloader 提供云端下载插件框架（**顶层包**）。
// 各下载器实现（HTTP、FTP 等）通过 Registry 注册，按 source URL 匹配调度。
//
// 归属：它是 P6①「可复用的扩展工具集合」——只依赖 pkg/plugin（插槽注册表）与标准库，
// **不依赖任何领域包**，故为 G0 基础包。2026-09 之前它住在 pkg/server/downloader
// （装配层的子包）；S4-A 把它提升为顶层，使云下载域（pkg/cloud）与其余消费者都能直接引用。
package downloader

import (
	"context"
	"io"
	"time"

	"github.com/cocomhub/sproxy/pkg/files/meta"
)

// ProgressFunc 是下载进度回调函数。
// downloaded 是已下载字节数，total 是总大小（-1 表示未知）。
type ProgressFunc func(downloaded, total int64)

// IntegrityMode 描述下载结果的完整性可信度（checksum 三态）。
// 值含义见 README/设计规格：越往高越接近「权威」①态。
type IntegrityMode int

const (
	// ModeUnknown 是零值：未声明完整性（未实现校验或下载器未标识）。
	ModeUnknown IntegrityMode = iota
	// ModeLocalOnly 仅本地 checksum+size（② 态，可能含外部校验）：
	// 校验值源自下载器自身写盘结果或本地重算，无服务端带外对账。
	ModeLocalOnly
	// ModeSelfVerified 下载器自算 checksum（② 态）：
	// 下载器下网后自行计算并比对（如 HTTP 对 Content-Length/自算哈希）。
	ModeSelfVerified
	// ModeAuthority 权威匹配：本地 == 服务端带外值（① 态）。
	ModeAuthority
)

// Result 是下载完成的结果。
type Result struct {
	Size          int64         // 实际下载大小
	Checksum      string        // SHA-256 十六进制
	ModTime       time.Time     // 原始文件修改时间（从 HTTP Last-Modified 提取）
	ETag          string        // 服务器 ETag（用于 If-Range 续传一致性校验）
	Integrity     IntegrityMode // 结果的完整性可信度（②/① 态）
	AuthorityHash string        // 服务端带外权威 hash（如 pikpak GCID；可空）
	// Meta 是下载流顺便计算的 FileMeta（可信卷；全量路径计算，续传路径为 nil 由
	// 调用方经 meta.FromFile 补算）。整文件/分块 sha256+md5，零额外 I/O 遍数。
	Meta *meta.FileMeta `json:"-"` // 不序列化（运行时伴随产物）
}

// IntegrityProvider 供调度侧/校验管道查询下载器声明的完整性归属。
// 下载器实现后，消费者可用类型断言判断其 checksum 的三态归属（ModeLocalOnly / ModeSelfVerified）。
type IntegrityProvider interface {
	// IntegrityMode 返回该下载器产出的 Result.Integrity 默认声明。
	IntegrityMode() IntegrityMode
}

// QuotaSink 是可选写盘记账 sink：io.Writer + 本次下载（写盘会话）结束时回调。
// 由调用方（如 cloud download 配额）注入；nil factory 时下载器直写底层文件。
// Finish(success, oldSize)：success=true 表示本次写盘成功落定（释放未用 reserve +
// 覆盖写释放 oldSize）；false 表示放弃（保留已 commit 供续传 / 回拨由实现决定）。
type QuotaSink interface {
	io.Writer
	Finish(success bool, oldSize int64)
}

// SinkFactory 由调用方注入，把下载器写盘目标包装为带记账的 sink。
// contentLength 是本次写盘会话预计写入的字节数（已知时）或 <=0（未知）；
// resume 为 true 时表示在既有 .partial 上追加（增量写入），false 表示新建/截断重建。
// nil 返回表示无需包装（直写）。创建失败返回错误（本次下载中止，不可重试）。
type SinkFactory func(w io.Writer, contentLength int64, resume bool) (QuotaSink, error)

// Downloader 是云端下载器接口。
// 各协议实现通过 Registry 注册，按 source URL 匹配调度。
type Downloader interface {
	// Download 从 source 下载到 destPath。
	// ctx 取消时尽早退出，保留已下载的部分。
	// onProgress 可为 nil（不关心进度）。
	Download(ctx context.Context, source string, destPath string, onProgress ProgressFunc) (*Result, error)

	// Supports 判断是否支持该 source（如根据 URL scheme 判断）。
	Supports(source string) bool

	// Name 返回下载器名称（如 "http"、"ftp"）。
	Name() string
}

// WriterDownloader 是支持写盘记账 sink 注入的下载器（HTTPDownloader 实现）。
// DownloadWithWriter 与 Download 等价，但允许调用方把写盘字节经 QuotaSink 记账
// （外部下载配额边写边记）。调用方通过类型断言判断下载器是否支持。
type WriterDownloader interface {
	Downloader
	DownloadWithWriter(ctx context.Context, source string, destPath string, onProgress ProgressFunc, sinkFactory SinkFactory) (*Result, error)
}
