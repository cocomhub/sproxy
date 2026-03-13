// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	Version = "dev"
	BuildAt = "unknown"
)

var (
	// 记录已发送的字节数
	sendSize int64 = 0
	// 当前带宽
	curBandwidth int64 = 0
)

var (
	cfgPath = flag.String("config", "config.yaml", "配置文件路径")
	showVer = flag.Bool("version", false, "打印版本与构建信息后退出")
	// 命令行标志定义
	uploadsDir = flag.String("uploads-dir", "./uploads", "uploads file dir")
)

func main() {
	flag.Parse()

	if *showVer {
		fmt.Printf("Version: %s\n", Version)
		fmt.Printf("BuildAt: %s\n", BuildAt)
		os.Exit(0)
	}

	slog.Info("config", "path", *cfgPath)

	go func() {
		var lastSendSize int64 = 0
		for {
			time.Sleep(time.Second)
			newSendSize := atomic.LoadInt64(&sendSize)
			bandwidth := (newSendSize - lastSendSize) / 1024
			atomic.StoreInt64(&curBandwidth, bandwidth)
			if bandwidth > 0 {
				log.Printf("bandwidth: %d KB/s", bandwidth)
			}
			lastSendSize = newSendSize
		}
	}()

	http.HandleFunc("/upload", uploadHandler)
	http.HandleFunc("/download", downloadHandler)
	http.HandleFunc("/delete", deleteHandler)
	http.HandleFunc("/{host}/{filepath...}", transfer)
	http.HandleFunc("/bandwidth", getCurBandwidth)

	log.Printf("downserver start at: http://localhost:18080")
	log.Printf("uploads dir: %s", *uploadsDir)

	s := &http.Server{Addr: ":18080"}

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)

	go func() {
		<-signalChan

		if err := s.Shutdown(context.Background()); err != nil {
			log.Fatalf("shutdown error: %s", err.Error())
		}
	}()

	if err := s.ListenAndServe(); err != nil {
		if err == http.ErrServerClosed {
			log.Printf("listen and serve error: %s", err.Error())
		} else {
			log.Fatalf("listen and serve error: %s", err.Error())
		}
	}

	log.Printf("downserver exit")
}

// 辅助函数：发送 JSON 响应
func sendJSONResponse(w http.ResponseWriter, response any, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(response)
}

type UploadResponse struct {
	Success bool     `json:"success"`
	Message string   `json:"message"`
	Files   []string `json:"files,omitempty"`
}

// transfer 处理文件传输请求
// 解析path中的远端文件路径，转发文件请求，并将获取的文件数据返回给客户端
// 例如：http://localhost:18080/baidu.com/file.txt 下载path中的文件并将文件返回
func transfer(w http.ResponseWriter, r *http.Request) {
	reqID := r.Header.Get("X-Request-ID")
	if reqID == "" {
		reqID = fmt.Sprintf("%d", time.Now().UnixNano())
	}

	host := r.PathValue("host")

	filepath := r.PathValue("filepath")

	parsedURL, err := url.Parse(fmt.Sprintf("https://%s/%s?%s", host, filepath, r.URL.Query().Encode()))
	if err != nil {
		http.Error(w, "parse url error: "+err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("transfer request[%s] to: %s", reqID, parsedURL.String())

	req, err := http.NewRequestWithContext(r.Context(), r.Method, parsedURL.String(), r.Body)
	if err != nil {
		http.Error(w, "create request error: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.Header = r.Header
	req.Header.Set("Host", host)
	req.URL = parsedURL
	req.Host = host

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "do request error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	// 记录每秒已发送的字节数
	written, err := io.Copy(BandwidthRecorder{w: w}, resp.Body)
	if err != nil {
		log.Printf("transfer request[%s] copy response body error: %s", reqID, err.Error())
		return
	}
	atomic.AddInt64(&sendSize, written)
}

type BandwidthRecorder struct {
	w io.Writer
}

func (r BandwidthRecorder) Write(data []byte) (int, error) {
	atomic.AddInt64(&sendSize, int64(len(data)))
	return r.w.Write(data)
}

// getCurBandwidth 获取当前带宽
func getCurBandwidth(w http.ResponseWriter, r *http.Request) {
	bandwidth := atomic.LoadInt64(&curBandwidth)
	w.Write(fmt.Appendf(nil, "%d", bandwidth))
}

// 上传文件处理器
func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "只支持POST方法", http.StatusMethodNotAllowed)
		return
	}

	// 限制上传文件大小（100MB）
	r.ParseMultipartForm(100 << 20)

	// 获取上传的文件
	file, handler, err := r.FormFile("file")
	if err != nil {
		log.Printf("读取文件失败: %s", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "读取文件失败"}, http.StatusBadRequest)
		return
	}
	defer file.Close()

	// 确保uploads目录存在
	uploadDir := *uploadsDir
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		log.Printf("创建目录失败: %s", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "创建目录失败"}, http.StatusInternalServerError)
		return
	}

	fileMD5 := r.Header.Get("X-File-MD5")
	if fileMD5 == "" {
		log.Printf("X-File-MD5 头不能为空")
		sendJSONResponse(w, UploadResponse{Success: false, Message: "Header 异常"}, http.StatusBadRequest)
		return
	}

	filePath := filepath.Join(uploadDir, handler.Filename)
	if stat, err := os.Stat(filePath); err == nil {
		uploadFileMD5, err := FileMD5(filePath)
		if err != nil {
			log.Printf("计算文件MD5失败: %s", err.Error())
			sendJSONResponse(w, UploadResponse{Success: false, Message: "计算文件MD5失败"}, http.StatusInternalServerError)
			return
		}
		if uploadFileMD5 != fileMD5 {
			log.Printf("文件已存在，但MD5校验失败: 服务器端MD5为%s，客户端MD5为%s", uploadFileMD5, fileMD5)
			sendJSONResponse(w, UploadResponse{Success: false, Message: "文件已存在，但MD5校验失败"}, http.StatusConflict)
			return
		}
		sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("文件已上传成功, size: %d", stat.Size())}, http.StatusOK)
		return
	}

	// 创建目标文件
	tempFile, err := os.Create(filePath + ".tmp")
	if err != nil {
		log.Printf("创建文件失败: %s", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "创建文件失败"}, http.StatusInternalServerError)
		return
	}
	defer tempFile.Close()

	// 同时计算 MD5 并写入文件
	hash := md5.New()
	multiWriter := io.MultiWriter(tempFile, hash)

	// 复制文件内容
	if _, err := io.Copy(multiWriter, file); err != nil {
		log.Printf("保存文件失败: %s", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "保存文件失败"}, http.StatusInternalServerError)
		return
	}
	tempFile.Close()

	// 计算服务器端的 MD5
	serverMD5 := hex.EncodeToString(hash.Sum(nil))
	if serverMD5 != fileMD5 {
		os.Remove(filePath + ".tmp")
		log.Printf("文件MD5校验失败: 服务器端MD5为%s，客户端MD5为%s", serverMD5, fileMD5)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件MD5校验失败"}, http.StatusBadRequest)
		return
	}

	if err := os.Rename(filePath+".tmp", filePath); err != nil {
		log.Printf("重命名文件失败: %s", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "重命名文件失败"}, http.StatusInternalServerError)
		return
	}

	sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("文件上传成功, size: %d", handler.Size)}, http.StatusOK)
}

// 下载文件处理器
func downloadHandler(w http.ResponseWriter, r *http.Request) {
	// 从查询参数获取文件名
	filename := r.URL.Query().Get("filename")
	if filename == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件名不能为空"}, http.StatusBadRequest)
		return
	}

	// 防止路径遍历攻击
	if filepath.Base(filename) != filename {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件名"}, http.StatusBadRequest)
		return
	}

	filePath := filepath.Join(*uploadsDir, filename)

	// 检查文件是否存在
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件不存在"}, http.StatusNotFound)
		return
	}

	// 设置响应头，触发下载
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=\"%s\"", filename))
	w.Header().Set("Content-Type", "application/octet-stream")

	// 发送文件
	http.ServeFile(w, r, filePath)
}

func deleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "只支持POST方法"}, http.StatusMethodNotAllowed)
		return
	}

	// 从查询参数获取文件名
	filename := r.URL.Query().Get("filename")
	if filename == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件名不能为空"}, http.StatusBadRequest)
		return
	}

	// 防止路径遍历攻击
	if filepath.Base(filename) != filename {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件名"}, http.StatusBadRequest)
		return
	}

	filePath := filepath.Join(*uploadsDir, filename)

	// 检查文件是否存在
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件不存在"}, http.StatusNotFound)
		return
	}

	fileMD5 := r.Header.Get("X-File-MD5")
	if fileMD5 == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "Header 异常"}, http.StatusBadRequest)
		log.Printf("X-File-MD5 is empty， filename: %s", filename)
		return
	}

	// 校验文件MD5
	uploadFileMD5, err := FileMD5(filePath)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "计算文件MD5失败: " + err.Error()}, http.StatusInternalServerError)
		return
	}

	if uploadFileMD5 != fileMD5 {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件MD5校验失败"}, http.StatusBadRequest)
		log.Printf("文件MD5校验失败: 服务器端MD5为%s，客户端MD5为%s", uploadFileMD5, fileMD5)
		return
	}

	// 删除文件
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
	_, err := io.Copy(dst, src)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", dst.Sum(nil)), nil
}
