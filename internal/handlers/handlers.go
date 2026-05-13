// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
)

type UploadResponse struct {
	Success bool     `json:"success"`
	Message string   `json:"message"`
	Files   []string `json:"files,omitempty"`
}

func sendJSONResponse(w http.ResponseWriter, response any, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(response)
}

type Handlers struct {
	cfg        *Config
	client     *http.Client
	uploadsDir string
	version    string
	buildAt    string

	sendSize     *int64
	curBandwidth *int64
}

func RegisterRoutes(mux *http.ServeMux, cfg *Config, client *http.Client, uploadsDir, version, buildAt string, tunnelKey []byte) {
	var sendSize int64
	var curBandwidth int64

	h := &Handlers{
		cfg:          cfg,
		client:       client,
		uploadsDir:   uploadsDir,
		version:      version,
		buildAt:      buildAt,
		sendSize:     &sendSize,
		curBandwidth: &curBandwidth,
	}

	go func() {
		var lastSendSize int64 = 0
		for {
			time.Sleep(time.Second)
			newSendSize := atomic.LoadInt64(h.sendSize)
			bandwidth := (newSendSize - lastSendSize) / 1024
			atomic.StoreInt64(h.curBandwidth, bandwidth)
			if bandwidth > 0 {
				slog.Info("bandwidth", "kbps", bandwidth)
			}
			lastSendSize = newSendSize
		}
	}()

	mux.HandleFunc("/upload", h.upload)
	mux.HandleFunc("/download", h.download)
	mux.HandleFunc("/delete", h.delete)
	mux.HandleFunc("/bandwidth", h.getCurBandwidth)
	mux.HandleFunc("/healthz", h.healthz)
	mux.HandleFunc("/version", h.versionHandler)
	mux.HandleFunc("/{host}/{filepath...}", h.transfer)
	mux.Handle("POST /tunnel", tunnel.NewHandler(tunnelKey))
}

func (h *Handlers) transfer(w http.ResponseWriter, r *http.Request) {
	reqID := r.Header.Get("X-Request-ID")
	if reqID == "" {
		reqID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	host := r.PathValue("host")
	filepathVal := r.PathValue("filepath")
	logger := slog.With("req_id", reqID)

	if !h.isHostAllowed(host) {
		logger.Warn("host not allowed", "target", host, "method", r.Method, "status", http.StatusForbidden)
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}

	parsedURL, err := url.Parse(fmt.Sprintf("https://%s/%s?%s", host, filepathVal, r.URL.Query().Encode()))
	if err != nil {
		http.Error(w, "parse url error: "+err.Error(), http.StatusBadRequest)
		return
	}
	logger.Info("transfer request", "method", r.Method, "target", parsedURL.String())

	req, err := http.NewRequestWithContext(r.Context(), r.Method, parsedURL.String(), r.Body)
	if err != nil {
		http.Error(w, "create request error: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.Header = r.Header.Clone()
	stripHopByHopHeaders(req.Header)
	req.Header.Set("Host", host)
	req.URL = parsedURL
	req.Host = host

	client := h.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		logger.Error("do request error", "target", parsedURL.String(), "method", r.Method, "status", http.StatusBadGateway, "error", err.Error())
		http.Error(w, "do request error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	_, err = io.Copy(bandwidthRecorder{h: h, w: w}, resp.Body)
	if err != nil {
		logger.Error("transfer copy response body error", "error", err.Error())
		return
	}
	logger.Info("transfer done", "method", r.Method, "target", parsedURL.String(), "status", resp.StatusCode)
}

type bandwidthRecorder struct {
	h *Handlers
	w io.Writer
}

func (r bandwidthRecorder) Write(data []byte) (int, error) {
	atomic.AddInt64(r.h.sendSize, int64(len(data)))
	return r.w.Write(data)
}

func (h *Handlers) getCurBandwidth(w http.ResponseWriter, r *http.Request) {
	bandwidth := atomic.LoadInt64(h.curBandwidth)
	_, _ = w.Write(fmt.Appendf(nil, "%d", bandwidth))
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
	if r.Method != "POST" {
		http.Error(w, "只支持POST方法", http.StatusMethodNotAllowed)
		return
	}
	reqID := r.Header.Get("X-Request-ID")
	if reqID == "" {
		reqID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	logger := slog.With("req_id", reqID)

	_ = r.ParseMultipartForm(100 << 20)

	file, handler, err := r.FormFile("file")
	if err != nil {
		logger.Error("读取文件失败", "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "读取文件失败"}, http.StatusBadRequest)
		return
	}
	defer file.Close()

	uploadDir := h.uploadsDir
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		logger.Error("创建目录失败", "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "创建目录失败"}, http.StatusInternalServerError)
		return
	}

	fileMD5 := r.Header.Get("X-File-MD5")
	if fileMD5 == "" {
		logger.Warn("X-File-MD5 头不能为空")
		sendJSONResponse(w, UploadResponse{Success: false, Message: "Header 异常"}, http.StatusBadRequest)
		return
	}

	filePath := filepath.Join(uploadDir, handler.Filename)
	if stat, err := os.Stat(filePath); err == nil {
		uploadFileMD5, err := FileMD5(filePath)
		if err != nil {
			logger.Error("计算文件MD5失败", "error", err.Error())
			sendJSONResponse(w, UploadResponse{Success: false, Message: "计算文件MD5失败"}, http.StatusInternalServerError)
			return
		}
		if uploadFileMD5 != fileMD5 {
			logger.Warn("文件已存在，但MD5校验失败", "server_md5", uploadFileMD5, "client_md5", fileMD5, "filename", handler.Filename)
			sendJSONResponse(w, UploadResponse{Success: false, Message: "文件已存在，但MD5校验失败"}, http.StatusConflict)
			return
		}
		sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("文件已上传成功, size: %d", stat.Size())}, http.StatusOK)
		return
	}

	tempFile, err := os.Create(filePath + ".tmp")
	if err != nil {
		logger.Error("创建文件失败", "error", err.Error(), "filename", handler.Filename)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "创建文件失败"}, http.StatusInternalServerError)
		return
	}
	defer tempFile.Close()

	hash := md5.New()
	multiWriter := io.MultiWriter(tempFile, hash)

	if _, err := io.Copy(multiWriter, file); err != nil {
		logger.Error("保存文件失败", "error", err.Error(), "filename", handler.Filename)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "保存文件失败"}, http.StatusInternalServerError)
		return
	}
	_ = tempFile.Close()

	serverMD5 := hex.EncodeToString(hash.Sum(nil))
	if serverMD5 != fileMD5 {
		_ = os.Remove(filePath + ".tmp")
		logger.Warn("文件MD5校验失败", "server_md5", serverMD5, "client_md5", fileMD5, "filename", handler.Filename)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件MD5校验失败"}, http.StatusBadRequest)
		return
	}

	if err := os.Rename(filePath+".tmp", filePath); err != nil {
		logger.Error("重命名文件失败", "error", err.Error(), "filename", handler.Filename)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "重命名文件失败"}, http.StatusInternalServerError)
		return
	}

	sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("文件上传成功, size: %d", handler.Size)}, http.StatusOK)
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
	filePath := filepath.Join(h.uploadsDir, filename)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件不存在"}, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, filePath)
}

func (h *Handlers) delete(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "只支持POST方法"}, http.StatusMethodNotAllowed)
		return
	}
	reqID := r.Header.Get("X-Request-ID")
	if reqID == "" {
		reqID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	logger := slog.With("req_id", reqID)

	filename := r.URL.Query().Get("filename")
	if filename == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件名不能为空"}, http.StatusBadRequest)
		return
	}
	if filepath.Base(filename) != filename {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件名"}, http.StatusBadRequest)
		return
	}
	filePath := filepath.Join(h.uploadsDir, filename)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件不存在"}, http.StatusNotFound)
		return
	}
	fileMD5 := r.Header.Get("X-File-MD5")
	if fileMD5 == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "Header 异常"}, http.StatusBadRequest)
		logger.Warn("X-File-MD5 is empty", "filename", filename)
		return
	}
	uploadFileMD5, err := FileMD5(filePath)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "计算文件MD5失败: " + err.Error()}, http.StatusInternalServerError)
		return
	}
	if uploadFileMD5 != fileMD5 {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件MD5校验失败"}, http.StatusBadRequest)
		logger.Warn("文件MD5校验失败", "server_md5", uploadFileMD5, "client_md5", fileMD5, "filename", filename)
		return
	}
	if err := os.Remove(filePath); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "删除文件失败: " + err.Error()}, http.StatusInternalServerError)
		return
	}
	sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("文件删除成功: %s", filename)}, http.StatusOK)
}

func FileMD5(filename string) (string, error) {
	f, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return MD5(f)
}

func MD5(src io.Reader) (string, error) {
	dst := md5.New()
	if _, err := io.Copy(dst, src); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", dst.Sum(nil)), nil
}

func (h *Handlers) isHostAllowed(host string) bool {
	if h.cfg == nil || len(h.cfg.AllowedHosts) == 0 {
		return true
	}
	reqHost := host
	onlyHost := host
	if _h, _, err := net.SplitHostPort(host); err == nil {
		onlyHost = _h
	}
	for _, a := range h.cfg.AllowedHosts {
		if strings.Contains(a, ":") {
			if a == reqHost {
				return true
			}
		} else {
			if a == onlyHost {
				return true
			}
		}
	}
	return false
}

func stripHopByHopHeaders(h http.Header) {
	if conn := h.Get("Connection"); conn != "" {
		for f := range strings.SplitSeq(conn, ",") {
			k := strings.TrimSpace(f)
			if k != "" {
				h.Del(k)
			}
		}
	}
	toDelete := []string{
		"Connection",
		"Proxy-Connection",
		"Keep-Alive",
		"TE",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	}
	for _, k := range toDelete {
		h.Del(k)
	}
}
