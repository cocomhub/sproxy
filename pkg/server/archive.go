// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cocomhub/sproxy/pkg/compressx"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/server/archive"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// ArchiveRequest 是 POST /api/archive 的请求体。
type ArchiveRequest struct {
	Files []string `json:"files"`
	// Encrypt 为 true 时归档输出 AES-256-GCM 流式加密（.tar.gz.aes，
	// roadmap P2 加密归档插件化）；密钥来自 archiveKey（装配层注入）。
	Encrypt bool `json:"encrypt"`
	// Cipher 是加密算法选型（roadmap P2 残余：双层加密/选型）：空 =
	// aes-256-gcm（默认）；经 RegisterCipher 注册的算法名（如 7z-mhe）。
	// 与 Encrypt 同用；Encrypt=false 时忽略。
	Cipher string `json:"cipher,omitempty"`
	// Compression 是归档压缩算法选型（roadmap 11.10-⑨）：空 = gzip
	// （默认，零回归）；gzip/zstd/brotli 经 compressx 解析，非法值 400
	// （不静默回退）。响应 Content-Encoding 标注实际算法（可观测）。
	Compression string `json:"compression,omitempty"`
}

// archiveKey 返回归档加密密钥（archive.key_file 配置：base64 32B 或 raw 32B）。
// 未配置 / 读取失败 → nil（加密归档请求将报错 fail-closed）。
func (h *Handlers) archiveKey() []byte {
	cfg := h.cfgPtr.Load()
	if cfg == nil || cfg.Archive.KeyFile == "" {
		return nil
	}
	b, err := os.ReadFile(cfg.Archive.KeyFile)
	if err != nil {
		return nil
	}
	key := bytes.TrimSpace(b)
	if dec, derr := base64.StdEncoding.DecodeString(string(key)); derr == nil && len(dec) == 32 {
		return dec
	}
	if len(key) == 32 {
		return key
	}
	return nil
}

// archiveHandler 处理 POST /api/archive。
// 接收 JSON {"files": ["file1.txt", "dir/file2.txt"]}，
// 返回 application/tar+gzip 流式归档文件。
// 使用 io.Pipe 实现流式打包，不占用额外磁盘空间。
func (h *Handlers) archiveHandler(w http.ResponseWriter, r *http.Request) {
	req, ok := archiveDecodeBody(w, r)
	if !ok {
		return
	}

	logger := h.logger.With("archive", "create")

	// 压缩算法选型（roadmap 11.10-⑨）：空 = gzip（默认零回归）；非法算法
	// 在响应头前拦截（400，不静默回退——显式语义）。
	compAlgo, ok := archiveParseCompression(req.Compression, w)
	if !ok {
		return
	}

	validated, ok := validateArchiveFiles(req.Files, w)
	if !ok {
		return
	}

	w.Header().Set(headerContentType, compAlgo.ContentType())

	// 根据请求文件列表推导归档名：单文件保留原文件名，多文件用公共前缀目录名
	archiveSetContentDisposition(w, validated, req, compAlgo)

	// 双层加密选型 fail-closed：未注册算法在响应头前拦截（pipe 内报错时
	// HTTP 已 200 但 body 中断——请求前校验保证 4xx）。
	if archiveCipherInvalid(req, w) {
		return
	}
	h.RecordAudit(r.Context(), AuditEvent{
		Action: "archive", ObjectType: "file", Object: strings.Join(req.Files, ","), Result: AuditResultSuccess,
		Detail: "归档流式打包已开始（tar+gzip）",
	})
	w.WriteHeader(http.StatusOK)

	// 流式打包：io.Pipe 中 tar + gzip
	pr, pw := io.Pipe()
	defer pr.Close()
	var closeOnce sync.Once
	owner := normalizeOwner(ownerFromRequest(r))
	go func() {
		var pipeErr error
		defer closeOnce.Do(func() {
			if pipeErr != nil {
				pw.CloseWithError(pipeErr)
			} else {
				pw.Close()
			}
		})
		pipeErr = h.archiveStream(pw, r, req, owner, validated, compAlgo, logger)
	}()

	_, copyErr := io.Copy(w, pr)
	if copyErr != nil {
		logger.Warn("archive response copy interrupted", "error", copyErr)
	}
}

// archiveDecodeBody 解析归档请求体：JSON 解码（1MB 上限）+ drainAndVerifyBody EOF
// 校验 + files 非空校验。失败已回包，返回 false，调用方应 return。
func archiveDecodeBody(w http.ResponseWriter, r *http.Request) (ArchiveRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB
	var req ArchiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无法解析请求体"}, http.StatusBadRequest)
		return req, false
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return req, false
	}
	if len(req.Files) == 0 {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "files 不能为空"}, http.StatusBadRequest)
		return req, false
	}
	return req, true
}

// archiveParseCompression 解析压缩算法选型（roadmap 11.10-⑨）：空 = gzip 默认零回归；
// 非法算法在响应头前拦截（400，不静默回退——显式语义）。返回 (算法, true)；false
// 表示已回包、调用方应 return。
func archiveParseCompression(name string, w http.ResponseWriter) (compressx.Algorithm, bool) {
	if name == "" {
		return compressx.Gzip, true
	}
	compAlgo, perr := compressx.Parse(name)
	if perr != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: perr.Error()}, http.StatusBadRequest)
		return compressx.Gzip, false
	}
	return compAlgo, true
}

// archiveSetContentDisposition 根据请求文件列表推导归档名并设置 Content-Disposition
// 头：单文件保留原文件名（加密时追加 .aes 后缀），多文件用公共前缀目录名。
func archiveSetContentDisposition(w http.ResponseWriter, validated []string, req ArchiveRequest, compAlgo compressx.Algorithm) {
	if len(validated) == 1 {
		baseName := filepath.Base(validated[0])
		if baseName == "" || baseName == "." {
			baseName = "file"
		}
		suffix := ".tar." + compAlgo.Ext()
		if req.Encrypt {
			suffix += ".aes"
		}
		w.Header().Set(headerContentDisposition, formatContentDisposition(baseName+suffix))
	} else {
		name := commonArchiveName(validated)
		w.Header().Set(headerContentDisposition, formatContentDisposition(name+".tar."+compAlgo.Ext()))
	}
}

// archiveCipherInvalid 校验双层加密选型 fail-closed：未注册算法在响应头前拦截（pipe
// 内报错时 HTTP 已 200 但 body 中断——请求前校验保证 4xx）。返回 true 表示已回包、
// 调用方应 return。
func archiveCipherInvalid(req ArchiveRequest, w http.ResponseWriter) bool {
	if !req.Encrypt {
		return false
	}
	cipherName := req.Cipher
	if cipherName == "" {
		cipherName = "aes-256-gcm"
	}
	if _, ok := files.LookupCipher(cipherName); !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: fmt.Sprintf("未知加密算法 %q", cipherName)}, http.StatusBadRequest)
		return true
	}
	return false
}

// archiveCipherWriter 按请求创建加密 writer（未启用加密返回 (nil, nil)）；密钥缺失
// 或算法未注册返回 error（fail-closed，pipe 内报错时 HTTP 已 200 但 body 中断）。
func (h *Handlers) archiveCipherWriter(req ArchiveRequest, pw *io.PipeWriter) (io.WriteCloser, error) {
	if !req.Encrypt {
		return nil, nil
	}
	key := h.archiveKey()
	if len(key) != 32 {
		return nil, fmt.Errorf("archive: 加密密钥不可用（需配置 archive.key_file）")
	}
	cipherName := req.Cipher
	if cipherName == "" {
		cipherName = "aes-256-gcm"
	}
	cw, cwerr := files.NewCipherWriter(cipherName, key, pw)
	if cwerr != nil {
		return nil, cwerr
	}
	return cw, nil
}

// archiveStream 执行归档打包 goroutine 主体：把 validated 文件逐条写入 tar.gz
// （可选加密），返回聚合的 pipeErr 供 closeOnce 统一关管（任一失败已打 Error 日志）。
func (h *Handlers) archiveStream(pw *io.PipeWriter, r *http.Request, req ArchiveRequest, owner string, validated []string, compAlgo compressx.Algorithm, logger *slog.Logger) error {
	var pipeErr error
	var dest io.Writer = pw
	cw, cerr := h.archiveCipherWriter(req, pw)
	if cerr != nil {
		return cerr
	}
	if cw != nil {
		dest = cw
	}
	gw, gwerr := compressx.NewWriter(compAlgo, dest, 0)
	if gwerr != nil {
		return gwerr
	}
	tw := tar.NewWriter(gw)

	fileErr, aborted := h.archiveWriteFiles(tw, r, owner, validated, logger)
	if aborted != nil {
		return aborted
	}
	pipeErr = fileErr

	// 按序关闭
	if err := tw.Close(); err != nil {
		logger.Error("tar writer 关闭失败", "error", err)
		pipeErr = err
	}
	if err := gw.Close(); err != nil {
		logger.Error("压缩 writer 关闭失败", "error", err)
		pipeErr = err
	}
	if cw != nil {
		if cerr := cw.Close(); cerr != nil {
			logger.Error("cipher writer 关闭失败", "error", cerr)
			pipeErr = cerr
		}
	}
	return pipeErr
}

// archiveWriteFiles 把 validated 文件逐条写入 tar writer：addFileToTar 失败记录聚合
// 错误并继续打包其余文件；客户端断开时返回 (已聚合错误, ctx.Err()) 供调用方早退。
func (h *Handlers) archiveWriteFiles(tw *tar.Writer, r *http.Request, owner string, validated []string, logger *slog.Logger) (error, error) {
	var fileErr error
	for _, relPath := range validated {
		// 检查客户端是否断开连接，避免 goroutine 泄漏
		select {
		case <-r.Context().Done():
			return fileErr, r.Context().Err()
		default:
		}

		// 跨卷定位（T6b）：归档源文件按 owner 卷视图定位（addFileToTar 经 os.Root 相对
		// 打开——中间目录符号链接不逃逸，TOCTOU 交叉校验保留）。非默认卷文件可打包；
		// 默认卷被 ACL 排除时默认卷遗留不可见 → 跳过（不读取无权卷内容，fail-closed）。
		root, userRel, externalOnly := h.archiveFileRootFor(owner, relPath)
		if root == nil {
			// **不得静默缺项**（缺陷修复，2026-10-21）：此前只记 Error 后 continue ⇒ 归档
			// 「成功」返回却少了这些文件（用户实测「归档缺文件」且不知原因）。现在按失败聚合
			// 上抛；外部卷（加密卷/网盘卷/S3）文件单独给出明确原因。
			if externalOnly {
				logger.Error("归档失败：文件位于外部卷（服务端归档暂不支持）", "path", relPath)
				fileErr = errors.Join(fileErr, fmt.Errorf(
					"文件 %q 位于外部卷（加密卷/网盘卷/S3），服务端归档暂不支持；请直接下载该文件", relPath))
				continue
			}
			logger.Error("归档添加文件失败：文件不在可归档视图", "path", relPath)
			fileErr = errors.Join(fileErr, fmt.Errorf("文件 %q 不在可归档视图（无权访问或已被删除）", relPath))
			continue
		}
		if err := archive.AddFileToTar(tw, root, userRel, relPath, logger); err != nil {
			logger.Error("归档添加文件失败", "path", relPath, "error", err)
			fileErr = err
		}
	}
	return fileErr, nil
}

// commonArchiveName 从文件路径列表中推导公共归档名。
func commonArchiveName(paths []string) string {
	if len(paths) == 0 {
		return "archive"
	}
	if len(paths) == 1 {
		base := filepath.Base(paths[0])
		if base == "" || base == "." {
			return "archive"
		}
		return strings.TrimSuffix(base, filepath.Ext(base))
	}
	// 尝试取公共前缀目录
	dir := filepath.Dir(paths[0])
	for _, p := range paths[1:] {
		for !strings.HasPrefix(p, dir+"/") {
			parent := filepath.Dir(dir)
			if parent == "." || parent == "/" || parent == dir {
				return "archive"
			}
			dir = parent
		}
	}
	if dir == "." || dir == "/" {
		return "archive"
	}
	return filepath.Base(dir)
}

// validateArchiveFiles 验证归档请求中的文件路径，返回有效路径列表。
// 如果校验失败，已发送错误响应。
func validateArchiveFiles(files []string, w http.ResponseWriter) ([]string, bool) {
	validated := make([]string, 0, len(files))
	for _, f := range files {
		relPath, err := pathguard.ValidateFilePath(f)
		if err != nil {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件路径: " + f}, http.StatusBadRequest)
			return nil, false
		}
		// 读取侧守卫（审查 #4 收敛）：归档源不得引用服务端内部目录（.__ 前缀为服务端
		// 保留；只可经 kind 白名单由服务端按 owner 拼接）。UserRel 虽会拒绝 .__ 段，
		// 但归档源在流式输出后才解析，需在响应开始前拦截并给明确 400。
		if pathguard.HasServiceInternalPrefix(relPath) {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "不能访问服务端内部目录（.__ 前缀为服务端保留）: " + f}, http.StatusBadRequest)
			return nil, false
		}
		validated = append(validated, relPath)
	}
	return validated, true
}

// archiveFileRootFor 返回归档源文件 relPath 在 owner 卷视图内的 root 与 user 桶 rel。
// 未命中（视图全无）或默认卷被 ACL 排除（遗留不可见）返回 (nil, "")——调用方跳过，
// 绝不读取无权卷内容（fail-closed）。volSet nil（旧装配）回落默认租户（唯一根）。
// 返回值第三项 external=true 表示「文件只存在于外部卷（加密/网盘/S3）」——服务端归档
// 基于 `os.Root`（符号链接安全 + TOCTOU 交叉校验），暂不支持外部卷；调用方必须**显式报错**
// 而不是静默缺项（缺陷修复，2026-10-21）。
func (h *Handlers) archiveFileRootFor(owner, relPath string) (*storage.Root, string, bool) {
	tnt := h.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		return nil, "", false
	}
	userRel, ok := tnt.UserRel(relPath)
	if !ok {
		return nil, "", false
	}
	if h.volSet != nil {
		if loc, found := h.locateOwnerFile(owner, userRel); found && loc != nil && loc.tenant != nil && loc.tenant.Root() != nil {
			return loc.tenant.Root(), userRel, false
		}
		// 本地视图未命中：若文件在**外部卷**上，明确告知不支持（而非让它走到本地根后
		// 报「stat 失败」这种误导性错误）。
		if src, found := h.resolveOwnerSource(context.Background(), owner, userRel, ""); found && src != nil && src.Local == nil {
			return nil, "", true
		}
		if !h.defaultVolumeAllows(owner) {
			return nil, "", false
		}
	}
	return tnt.Root(), userRel, false
}

// archiveDirHandler 处理 GET /api/archive-dir?dirname=xxx。
// 将指定目录及其内容打包下载。
func (h *Handlers) archiveDirHandler(w http.ResponseWriter, r *http.Request) {
	dirname := r.URL.Query().Get("dirname")
	if dirname == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "dirname 不能为空"}, http.StatusBadRequest)
		return
	}
	relPath, err := pathguard.ValidateFilePath(dirname)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的目录名"}, http.StatusBadRequest)
		return
	}

	// 源目录按请求者租户 user 桶解析（<root>/<tenant>/user/<path>）。
	srcs, ok := h.archiveDirResolveSources(w, r, relPath)
	if !ok {
		return
	}

	archiveName := filepath.Base(relPath) + tarGZExt
	w.Header().Set(headerContentType, "application/gzip")
	w.Header().Set(headerContentDisposition, formatContentDisposition(archiveName))
	w.WriteHeader(http.StatusOK)

	pr, pw := io.Pipe()
	defer pr.Close()
	var closeOnce sync.Once
	go func() {
		var pipeErr error
		defer closeOnce.Do(func() {
			if pipeErr != nil {
				pw.CloseWithError(pipeErr)
			} else {
				pw.Close()
			}
		})
		pipeErr = h.archiveDirPack(pw, r, srcs, relPath)
	}()
	_, copyErr := io.Copy(w, pr)
	if copyErr != nil {
		h.logger.Warn("archive dir response copy interrupted", "error", copyErr)
	}
}

// dirSrc 是目录归档的源租户（卷）与 user 桶相对路径对（多卷并存目录的逐卷打包源）。
type dirSrc struct {
	tnt     *storage.Tenant
	userRel string
}

// archiveDirResolveSources 解析请求目录的源租户集合：请求者租户 user 桶解析 + 单卷
// （唯一根 stat 校验）/多卷（逐卷收集存在目录的卷）分支；任一校验失败已回包返回 false。
func (h *Handlers) archiveDirResolveSources(w http.ResponseWriter, r *http.Request, relPath string) ([]dirSrc, bool) {
	owner := normalizeOwner(ownerFromRequest(r))
	tnt0 := h.tenantFor(owner)
	if tnt0 == nil || tnt0.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的目录路径"}, http.StatusBadRequest)
		return nil, false
	}
	userRel, ok := tnt0.UserRel(relPath)
	if !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的目录路径"}, http.StatusBadRequest)
		return nil, false
	}

	if h.volSet == nil {
		// 旧装配路径：唯一根 stat 校验目录存在 + 是目录（与单卷既有错误语义一致）。
		return archiveDirSingleVolumeSource(w, tnt0, userRel)
	}
	// 多卷（T6b）：目录可跨卷并存（换卷目录不一定每卷都有），收集 owner 视图内存在该目录的
	// 卷租户逐卷打包；默认卷被 ACL 排除时不参与（fail-closed，不打包无权卷遗留）。
	return h.archiveDirMultiVolumeSources(w, owner, userRel)
}

// archiveDirSingleVolumeSource 旧装配（无 volSet）唯一根：stat 校验目录存在且是目录。
// 校验失败已回包，返回 (srcs, true) 或 (nil, false)。
func archiveDirSingleVolumeSource(w http.ResponseWriter, tnt0 *storage.Tenant, userRel string) ([]dirSrc, bool) {
	info, statErr := tnt0.Root().Stat(userRel)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "目录不存在"}, http.StatusNotFound)
		} else {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "访问目录失败"}, http.StatusInternalServerError)
		}
		return nil, false
	}
	if !info.IsDir() {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "指定路径不是目录"}, http.StatusBadRequest)
		return nil, false
	}
	return []dirSrc{{tnt: tnt0, userRel: userRel}}, true
}

// archiveDirMultiVolumeSources 多卷（T6b + F6）：逐卷收集 owner 视图内存在该目录的卷
// 租户。F6（review 边界成文）：跨卷「同名文件/目录并存」（rel 在一卷是目录、另一卷是
// 文件）时，策略为**打包存在的目录、忽略同名文件条目**并返回 200——目录可跨卷并存、
// 不受 AD-4 文件唯一性约束，rel 在视图内存在目录即可打包。这与单卷「指定路径不是目录
// → 400」有语义差异（单卷 rel 唯一，不可能目录/文件同址；多卷仅异常共存时触发），属
// 刻意取舍，非漏洞。仅当视图内**完全没有该 rel 的目录**且存在同名文件时才回落 400
// （notDir 且无目录）。校验失败已回包返回 false。
func (h *Handlers) archiveDirMultiVolumeSources(w http.ResponseWriter, owner, userRel string) ([]dirSrc, bool) {
	var srcs []dirSrc
	notDir := false
	for _, v := range volume.AllowedVolumes(h.volSet.All(), owner) {
		src, ok := h.archiveDirVolumeSource(v, owner, userRel, &notDir)
		if ok {
			srcs = append(srcs, src)
		}
	}
	if notDir && len(srcs) == 0 {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "指定路径不是目录"}, http.StatusBadRequest)
		return nil, false
	}
	if len(srcs) == 0 {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "目录不存在"}, http.StatusNotFound)
		return nil, false
	}
	return srcs, true
}

// archiveDirVolumeSource 收集单卷的目录源：卷内存在目录返回 (source, true)；卷内无
// 该目录 / 出错返回 (false)；同名文件条目置 notDir 标记（供 F6 回落 400 判定）。
func (h *Handlers) archiveDirVolumeSource(v volume.Volume, owner, userRel string, notDir *bool) (dirSrc, bool) {
	exists, vErr := h.volumeFileExists(v.Name, owner, userRel)
	if vErr != nil || !exists {
		return dirSrc{}, false
	}
	tnt := h.volumeTenant(v.Name, owner)
	if tnt == nil || tnt.Root() == nil {
		return dirSrc{}, false
	}
	info, statErr := tnt.Root().Stat(userRel)
	if statErr != nil {
		return dirSrc{}, false
	}
	if !info.IsDir() {
		*notDir = true
		return dirSrc{}, false
	}
	return dirSrc{tnt: tnt, userRel: userRel}, true
}

// archiveDirPack 执行目录归档打包 goroutine 主体：把 srcs 各卷目录写入 tar.gz，返回
// 聚合的 pipeErr 供 closeOnce 统一关管。
func (h *Handlers) archiveDirPack(pw *io.PipeWriter, r *http.Request, srcs []dirSrc, relPath string) error {
	var pipeErr error
	gw := gzip.NewWriter(pw)
	tw := tar.NewWriter(gw)
	for _, src := range srcs {
		// 检查客户端是否断开连接
		select {
		case <-r.Context().Done():
			return r.Context().Err()
		default:
		}

		if err := archive.AddFileToTar(tw, src.tnt.Root(), src.userRel, filepath.ToSlash(relPath), h.logger); err != nil {
			pipeErr = err
		}
	}
	if err := tw.Close(); err != nil {
		pipeErr = err
	}
	if err := gw.Close(); err != nil {
		pipeErr = err
	}
	return pipeErr
}
