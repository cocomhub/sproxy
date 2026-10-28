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
// 算法标识字段（ChecksumAlgorithm/AuthorityHashAlgorithm）由**下载方**填写——转存层
// 只透传，不推断（用户裁定 2026-10-28：不能无脑写 GCID；各下载器声明自己用什么算法）。
type Result struct {
	Size                   int64         // 实际下载大小
	Checksum               string        // 校验和值（本地落盘；算法见 ChecksumAlgorithm）
	ChecksumAlgorithm      string        // Checksum 的算法标识（如 "sha256"；空 = 未知/旧实现）
	ModTime                time.Time     // **源文件**修改时间（HTTP Last-Modified / 提供方带外时间）
	ETag                   string        // 服务器 ETag（用于 If-Range 续传一致性校验）
	Integrity              IntegrityMode // 结果的完整性可信度（②/① 态）
	AuthorityHash          string        // 服务端带外权威 hash（如 PikPak GCID；算法见 AuthorityHashAlgorithm）
	AuthorityHashAlgorithm string        // AuthorityHash 的算法标识（如 "pikpak-gcid";空 = 未知）

	// 源文件时间（提供方带外；零值 = 未知——消费方不得用「下载完成时刻」顶替，
	// 用户裁定 2026-10-28：mtime/ctime 必须来自源，避免伪造时间语义）。
	SourceCreatedTime time.Time // 源文件**创建**时间（PikPak created_time 等）

	// 下载审计统计（用户裁定 2026-10-28：转存产物 meta 记录耗时/起止/分源字节/带宽）。
	// 由下载器在完成时填充；0/零值 = 该下载器未统计（消费方不得编造）。
	StartedAt      time.Time // 下载开始时刻（本次会话，含续传会话起点）
	FinishedAt     time.Time // 下载结束时刻（成功落定）
	AnonymousBytes int64     // 匿名（免账号）通道下载字节（分享直链区）
	AccountBytes   int64     // 账号通道下载字节（转存/网盘区 + 分享区降级）
	BytesPerSecond int64     // 平均带宽（(AnonymousBytes+AccountBytes)/耗时秒）
}

// 校验和/权威 hash 的**算法标识常量**（下载方在 Result 里声明，转存层只透传不推断——
// 用户裁定 2026-10-28：不能无脑写 GCID，各下载器说自己用什么算法）。
const (
	// IntegrityChecksumSHA256 表示 Checksum 是本地落盘文件 SHA-256 十六进制（小写）。
	IntegrityChecksumSHA256 = "sha256"
	// IntegrityAlgPikpakGCID 表示 AuthorityHash 是 PikPak 官方 GCID
	// （= sha1(concat(sha1(每个分块)))；见 pkg/integrity/gcid.go）。
	IntegrityAlgPikpakGCID = "pikpak-gcid"
)

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
