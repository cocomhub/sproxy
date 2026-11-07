// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package webdav 提供 WebDAV 网关：把 `pkg/sync.FS`（本地或 `remote://` 远端）桥接为
// WebDAV 协议（RFC 4918），使任意工具（curl / rsync / 文件管理器 / 编辑器）能直接
// 读写 sproxy 卷内的文件，无需了解 mesh / remote 内部寻址。
//
// 用法：
//
//	fs := remoteClient.FS(ref)          // sync.FS（本地可用 pkg/sync.NewLocalFS）
//	h := webdav.NewHandler(fs)          // http.Handler
//	http.ListenAndServe("127.0.0.1:8080", h)
//
// 实现说明：`golang.org/x/net/webdav.Handler` 需要 `webdav.FileSystem` 接口
// （Mkdir/OpenFile/RemoveAll/Rename/Stat），本包用 `webdavFS` 把 `sync.FS` 的 7 方法
// 适配过去。读路径（GET/PROPFIND）要求 File 支持 Seek（http.ServeContent 用
// Seek(0, end) 取大小、Seek(0, start) 定位），故 readFile 全量读入内存 bytes.Reader；
// 写路径（PUT/COPY/MKCOL 后 Copy）先缓冲到内存，Stat() 触发 flush 落盘（webdav
// handler 在 io.Copy 后先 Stat 再 Close，故 Stat 必须此时已落盘并返回有效元数据）。
package webdav

// 本包只保留 **remote:// 适配**（`remote.go`）：把 `sync.FS → WebDAV` 的 RFC 4918 桥接
// 实现放在 `webdavcore`（`pkg/server` 不能 import 本包——本包依赖 `pkg/remote`）。
//
// 历史：本文件曾与 `webdavcore/local.go` **整文件重复**（21 个函数逐字相同：webdavFS 的
// 7 个接口方法、webdavFileInfo、readFile/writeFile、NewHandler…），两处各自演进的风险是
// 真实的（一处修了路径穿越、另一处没修）。现在实现唯一在 webdavcore，本包只做转发。

import (
	"net/http"

	"github.com/cocomhub/sproxy/pkg/gateway/webdav/webdavcore"
	"github.com/cocomhub/sproxy/pkg/sync"
)

// NewHandler 返回桥接 fs 的 WebDAV handler（委托 `webdavcore.NewHandler`）。
//
// 保留该导出名是为了不破坏既有调用方（`remote.go` 与测试）；新代码可直接用
// `webdavcore.NewHandler`。fs 为 nil 时返回 nil（调用方负责非空校验）。
func NewHandler(fs sync.FS) http.Handler {
	return webdavcore.NewHandler(fs)
}
