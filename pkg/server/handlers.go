// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/web"
)

type Handlers struct {
	cfgPtr        *atomic.Pointer[Config]
	version       string
	buildAt       string
	checksumStore *ChecksumStore
	uploadStore   *UploadStore
	logger        *slog.Logger
}

func RegisterRoutes(ctx context.Context, mux *http.ServeMux, cfgPtr *atomic.Pointer[Config], version, buildAt string, tunnelKey []byte, logger *slog.Logger) {
	cfg := cfgPtr.Load()
	log := defaultLogger(logger)

	// 初始化 ChecksumStore
	cs := NewChecksumStore(cfg.UploadsDir, log.With("component", "checksum_store"))

	h := &Handlers{
		cfgPtr:        cfgPtr,
		version:       version,
		buildAt:       buildAt,
		checksumStore: cs,
		uploadStore:   NewUploadStore(cfg.UploadsDir, log.With("component", "upload_store")),
		logger:        log,
	}

	// 本地路由子 mux（无 authMiddleware，隧道密钥已提供认证）
	localMux := http.NewServeMux()
	localMux.HandleFunc("POST /upload", h.upload)
	localMux.HandleFunc("GET /download", h.download)
	localMux.HandleFunc("POST /delete", h.delete)
	localMux.HandleFunc("GET /api/files", h.listFiles)

	// 分块上传/下载路由（本地）
	localMux.HandleFunc("POST /upload/init", h.uploadInit)
	localMux.HandleFunc("POST /upload/chunk", h.uploadChunk)
	localMux.HandleFunc("GET /upload/status", h.uploadStatus)
	localMux.HandleFunc("POST /upload/complete", h.uploadComplete)
	localMux.HandleFunc("GET /download/chunk", h.downloadChunk)

	// 速率限制中间件（仅应用于 tunnel handler）
	var tunnelHandler http.Handler = tunnel.NewLocalHandler(tunnelKey, requestLogMiddleware(log.With("component", "request"), localMux), log.With("component", "tunnel"))
	if cfg.RateLimit.Enabled {
		rl := NewRateLimiter(cfg.RateLimit.Requests, cfg.RateLimit.Window, log.With("component", "rate_limiter"))
		tunnelHandler = rl.Middleware(tunnelHandler)
	}

	mux.HandleFunc("POST /upload", h.authMiddleware(h.upload))
	mux.HandleFunc("GET /download", h.authMiddleware(h.download))
	mux.HandleFunc("POST /delete", h.authMiddleware(h.delete))
	mux.HandleFunc("GET /api/files", h.authMiddleware(h.listFiles))
	mux.HandleFunc("POST /upload/init", h.authMiddleware(h.uploadInit))
	mux.HandleFunc("POST /upload/chunk", h.authMiddleware(h.uploadChunk))
	mux.HandleFunc("GET /upload/status", h.authMiddleware(h.uploadStatus))
	mux.HandleFunc("POST /upload/complete", h.authMiddleware(h.uploadComplete))
	mux.HandleFunc("GET /download/chunk", h.authMiddleware(h.downloadChunk))
	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("GET /version", h.versionHandler)
	mux.Handle("POST /tunnel", tunnelHandler)

	// Web UI
	subFS, err := fs.Sub(web.StaticFS, "static")
	if err != nil {
		h.logger.Error("web static fs sub error", "error", err)
	} else {
		mux.Handle("GET /ui/", http.StripPrefix("/ui/", http.FileServer(http.FS(subFS))))
	}

	// GET / -> /ui/ 重定向
	mux.HandleFunc("GET /", h.webRedirect)
}

// requestLogMiddleware 记录 HTTP 请求的基本信息：方法、路径、远程地址、耗时。
func requestLogMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("请求", "method", r.Method, "path", r.URL.Path,
			"remote", r.RemoteAddr, "user_agent", r.UserAgent(), "duration", time.Since(start))
	})
}

func (h *Handlers) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := h.cfgPtr.Load()
		if cfg != nil && cfg.AuthToken != "" {
			if r.Header.Get("Authorization") != "Bearer "+cfg.AuthToken {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

func (h *Handlers) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func (h *Handlers) versionHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(fmt.Appendf(nil, "Version: %s\nBuildAt: %s\n", h.version, h.buildAt))
}

func (h *Handlers) upload(w http.ResponseWriter, r *http.Request) {
	reqID := r.Header.Get("X-Request-ID")
	if reqID == "" {
		reqID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	logger := h.logger.With("req_id", reqID)

	cfg := h.cfgPtr.Load()
	// 限制请求体大小，防止 OOM；超过 MaxBytesReader 会向客户端返回 413
	if cfg.MaxUploadBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxUploadBytes)
	}
	// 仅在内存中缓冲 10 MiB，超出部分由 stdlib 落临时文件
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		logger.Warn("解析 multipart 失败", "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体过大或解析失败: " + err.Error()}, http.StatusRequestEntityTooLarge)
		return
	}

	file, handler, err := r.FormFile("file")
	if err != nil {
		logger.Error("读取文件失败", "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "读取文件失败"}, http.StatusBadRequest)
		return
	}
	defer file.Close()

	// 路径穿越校验
	if filepath.Base(handler.Filename) != handler.Filename {
		logger.Warn("无效的文件名", "filename", handler.Filename)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件名"}, http.StatusBadRequest)
		return
	}

	uploadDir := cfg.UploadsDir
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		logger.Error("创建目录失败", "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "创建目录失败"}, http.StatusInternalServerError)
		return
	}

	expectedChecksum := r.Header.Get("X-File-Checksum")
	if expectedChecksum == "" {
		logger.Warn("缺少 X-File-Checksum 请求头")
		sendJSONResponse(w, UploadResponse{Success: false, Message: "缺少 X-File-Checksum 请求头"}, http.StatusBadRequest)
		return
	}

	filePath := filepath.Join(uploadDir, handler.Filename)
	if stat, err := os.Stat(filePath); err == nil {
		if !verifyFileWithChecksum(filePath, expectedChecksum) {
			logger.Warn("文件已存在，但校验失败", "filename", handler.Filename)
			sendJSONResponse(w, UploadResponse{Success: false, Message: "文件已存在，但校验失败"}, http.StatusConflict)
			return
		}
		sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("文件已上传成功, size: %d", stat.Size()), Checksum: expectedChecksum}, http.StatusOK)
		return
	}

	tempFile, err := os.Create(filePath + ".tmp")
	if err != nil {
		logger.Error("创建文件失败", "error", err.Error(), "filename", handler.Filename)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "创建文件失败"}, http.StatusInternalServerError)
		return
	}
	defer tempFile.Close()
	// 任何错误路径都保证 .tmp 不残留；rename 成功后 .tmp 已不在，os.Remove 会无声失败
	defer os.Remove(filePath + ".tmp")

	// 边写边算 SHA-256，复用同一份字节流
	sha256Hash := sha256.New()
	multiWriter := io.MultiWriter(tempFile, sha256Hash)

	if _, err := io.Copy(multiWriter, file); err != nil {
		logger.Error("保存文件失败", "error", err.Error(), "filename", handler.Filename)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "保存文件失败"}, http.StatusInternalServerError)
		return
	}
	if err := tempFile.Close(); err != nil {
		logger.Error("关闭临时文件失败", "error", err.Error(), "filename", handler.Filename)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "保存文件失败"}, http.StatusInternalServerError)
		return
	}

	serverChecksum := hex.EncodeToString(sha256Hash.Sum(nil))
	if serverChecksum != expectedChecksum {
		logger.Warn("文件 SHA-256 校验失败", "server", serverChecksum, "client", expectedChecksum, "filename", handler.Filename)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件 SHA-256 校验失败"}, http.StatusBadRequest)
		return
	}

	if err := os.Rename(filePath+".tmp", filePath); err != nil {
		logger.Error("重命名文件失败", "error", err.Error(), "filename", handler.Filename)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "重命名文件失败"}, http.StatusInternalServerError)
		return
	}

	w.Header().Set("X-File-Checksum", serverChecksum)
	h.checksumStore.Set(handler.Filename, serverChecksum)

	// 处理文件修改时间
	if mtimeStr := r.Header.Get("X-File-MTime"); mtimeStr != "" {
		var mtimeInt int64
		if _, err := fmt.Sscanf(mtimeStr, "%d", &mtimeInt); err == nil && mtimeInt > 0 {
			modTime := time.Unix(0, mtimeInt)
			if err := os.Chtimes(filePath, modTime, modTime); err != nil {
				logger.Warn("设置文件时间戳失败", "filename", handler.Filename, "error", err)
			}
		}
	}

	sendJSONResponse(w, UploadResponse{
		Success:  true,
		Message:  fmt.Sprintf("文件上传成功, size: %d", handler.Size),
		Checksum: serverChecksum,
	}, http.StatusOK)
}

func (h *Handlers) download(w http.ResponseWriter, r *http.Request) {
	filename := r.URL.Query().Get("filename")
	if filename == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件名不能为空"}, http.StatusBadRequest)
		return
	}
	if filepath.Base(filename) != filename {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件名"}, http.StatusBadRequest)
		return
	}
	cfg := h.cfgPtr.Load()
	filePath := filepath.Join(cfg.UploadsDir, filename)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件不存在"}, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
	w.Header().Set("Content-Type", "application/octet-stream")

	// 设置 SHA-256 checksum 响应头：优先从 store 读取，回退实时计算
	if cs, ok := h.checksumStore.Get(filename); ok {
		w.Header().Set("X-File-Checksum", cs)
	} else if cs, err := FileChecksum(filePath); err == nil {
		w.Header().Set("X-File-Checksum", cs)
	} else {
		h.logger.Warn("计算文件 checksum 失败", "error", err.Error(), "filename", filename)
	}

	// 返回文件修改时间
	if stat, err := os.Stat(filePath); err == nil {
		w.Header().Set("X-File-MTime", fmt.Sprintf("%d", stat.ModTime().UnixNano()))
	}

	http.ServeFile(w, r, filePath)
}

func (h *Handlers) delete(w http.ResponseWriter, r *http.Request) {
	reqID := r.Header.Get("X-Request-ID")
	if reqID == "" {
		reqID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	logger := h.logger.With("req_id", reqID)

	filename := r.URL.Query().Get("filename")
	if filename == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件名不能为空"}, http.StatusBadRequest)
		return
	}
	if filepath.Base(filename) != filename {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件名"}, http.StatusBadRequest)
		return
	}
	cfg := h.cfgPtr.Load()
	filePath := filepath.Join(cfg.UploadsDir, filename)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件不存在"}, http.StatusNotFound)
		return
	}

	expectedChecksum := r.Header.Get("X-File-Checksum")
	if expectedChecksum == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "缺少 X-File-Checksum 请求头"}, http.StatusBadRequest)
		logger.Warn("X-File-Checksum 为空", "filename", filename)
		return
	}

	if !verifyFileWithChecksum(filePath, expectedChecksum) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件校验失败"}, http.StatusBadRequest)
		logger.Warn("文件校验失败", "filename", filename)
		return
	}

	if err := os.Remove(filePath); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "删除文件失败: " + err.Error()}, http.StatusInternalServerError)
		return
	}
	h.checksumStore.Delete(filename)
	sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("文件删除成功: %s", filename)}, http.StatusOK)
}

type fileInfo struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Checksum string `json:"checksum"`
	ModTime  int64  `json:"mod_time"` // UnixNano
}

func (h *Handlers) listFiles(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfgPtr.Load()
	entries, err := os.ReadDir(cfg.UploadsDir)
	if os.IsNotExist(err) {
		sendJSONResponse(w, map[string]any{"files": []fileInfo{}}, http.StatusOK)
		return
	}
	if err != nil {
		h.logger.Error("读取上传目录失败", "error", err.Error())
		sendJSONResponse(w, map[string]any{"files": []fileInfo{}}, http.StatusInternalServerError)
		return
	}

	csMap := h.checksumStore.GetAll()
	files := make([]fileInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		fi := fileInfo{
			Name:    e.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime().UnixNano(),
		}
		if cs, ok := csMap[e.Name()]; ok {
			fi.Checksum = cs
		}
		files = append(files, fi)
	}
	sendJSONResponse(w, map[string]any{"files": files}, http.StatusOK)
}

func (h *Handlers) webRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
}

func verifyFileWithChecksum(filePath, expectedChecksum string) bool {
	f, err := os.Open(filePath)
	if err != nil {
		return false
	}
	defer f.Close()
	return verifyChecksum(expectedChecksum, f)
}
