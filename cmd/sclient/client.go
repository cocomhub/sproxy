// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/internal/handlers"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

type serverResponse struct {
	Success  bool     `json:"success"`
	Message  string   `json:"message"`
	FileMD5  string   `json:"file_md5"`
	MD5Match *bool    `json:"md5_match"`
	Files    []string `json:"files,omitempty"`
}

func CalculateMD5(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type ProgressReader struct {
	reader    io.Reader
	total     int64
	read      int64
	label     string
	lastPrint time.Time
}

func NewProgressReader(reader io.Reader, total int64, label string) *ProgressReader {
	return &ProgressReader{
		reader:    reader,
		total:     total,
		label:     label,
		lastPrint: time.Now(),
	}
}

func (pr *ProgressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	pr.read += int64(n)

	if time.Since(pr.lastPrint) >= time.Second || err != nil {
		pr.lastPrint = time.Now()
		if pr.total > 0 {
			percent := float64(pr.read) * 100.0 / float64(pr.total)
			fmt.Printf("\r%s: %.1f%% (%d/%d)", pr.label, percent, pr.read, pr.total)
		} else {
			fmt.Printf("\r%s: %d bytes", pr.label, pr.read)
		}
	}

	return n, err
}

func UploadFile(serverURL, filePath string, checkMD5 bool, verbose bool, timeout int) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("打开文件失败: %w", err)
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("获取文件信息失败: %w", err)
	}
	fileSize := fileInfo.Size()

	var fileMD5 string
	if checkMD5 {
		h := md5.New()
		if _, err := io.Copy(h, file); err != nil {
			return fmt.Errorf("计算MD5失败: %w", err)
		}
		fileMD5 = hex.EncodeToString(h.Sum(nil))
		fmt.Printf("文件 MD5: %s\n", fileMD5)
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("重置文件指针失败: %w", err)
		}
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		defer pw.Close()
		defer mw.Close()
		part, err := mw.CreateFormFile("file", filepath.Base(filePath))
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		progressReader := NewProgressReader(file, fileSize, "上传")
		if _, err := io.Copy(part, progressReader); err != nil {
			pw.CloseWithError(err)
			return
		}
	}()

	if verbose {
		fmt.Fprintf(os.Stderr, "POST %s\n", serverURL)
	}

	req, err := http.NewRequest("POST", serverURL, pr)
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if checkMD5 {
		req.Header.Set("X-File-MD5", fileMD5)
	}

	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	fmt.Println()

	var result serverResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}

	fmt.Printf("成功: %v, 消息: %s\n", result.Success, result.Message)
	if result.FileMD5 != "" {
		fmt.Printf("文件MD5: %s\n", result.FileMD5)
	}
	if result.MD5Match != nil {
		fmt.Printf("MD5匹配: %v\n", *result.MD5Match)
	}

	if !result.Success {
		return fmt.Errorf("上传失败: %s", result.Message)
	}

	return nil
}

func DownloadFile(serverURL, filename, outputPath string, checkMD5 bool, verbose bool, timeout int) error {
	if outputPath == "" {
		outputPath = filename
	}

	params := url.Values{}
	params.Set("filename", filename)
	fullURL := serverURL + "?" + params.Encode()

	if verbose {
		fmt.Fprintf(os.Stderr, "GET %s\n", fullURL)
	}

	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}
	resp, err := client.Get(fullURL)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("下载失败 (状态码: %d): %s", resp.StatusCode, string(body))
	}

	out, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("创建文件失败: %w", err)
	}
	defer out.Close()

	progressReader := NewProgressReader(resp.Body, resp.ContentLength, "下载")
	if _, err := io.Copy(out, progressReader); err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}
	fmt.Println()

	if checkMD5 {
		serverMD5 := resp.Header.Get("X-File-MD5")
		if serverMD5 != "" {
			localMD5, err := CalculateMD5(outputPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "计算本地MD5失败: %v\n", err)
			} else {
				fmt.Printf("服务器 MD5: %s\n", serverMD5)
				fmt.Printf("本地 MD5: %s\n", localMD5)
				if serverMD5 == localMD5 {
					fmt.Println("MD5 校验通过")
				} else {
					fmt.Fprintln(os.Stderr, "MD5 校验失败")
				}
			}
		}
	}

	fmt.Printf("文件已下载到: %s\n", outputPath)
	return nil
}

func DeleteFile(serverURL, filename string, verbose bool, timeout int) error {
	params := url.Values{}
	params.Set("filename", filename)
	fullURL := serverURL + "?" + params.Encode()

	if verbose {
		fmt.Fprintf(os.Stderr, "POST %s\n", fullURL)
	}

	req, err := http.NewRequest("POST", fullURL, nil)
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}

	deleteFileMD5, err := handlers.FileMD5(filename)
	if err != nil {
		return fmt.Errorf("计算文件MD5失败: " + err.Error())
	}
	req.Header.Set("X-File-MD5", deleteFileMD5)

	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	var result serverResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}

	fmt.Printf("成功: %v, 消息: %s\n", result.Success, result.Message)

	if !result.Success {
		return fmt.Errorf("删除失败: %s", result.Message)
	}

	return nil
}

func ListFiles(fullURL string, timeout int) error {
	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}
	resp, err := client.Get(fullURL)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取响应失败: %w", err)
	}

	var result struct {
		Files []string `json:"files"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		fmt.Println(string(body))
		return nil
	}

	if len(result.Files) == 0 {
		fmt.Println("no files found")
		return nil
	}

	for _, f := range result.Files {
		fmt.Println(f)
	}

	return nil
}

func TunnelRequest(cfg *SclientConfig, method, targetURL string, headers map[string]string, body, outputFile string, verbose bool) error {
	c, err := tunnel.NewClient(cfg.TunnelKey, strings.TrimRight(cfg.ServerURL, "/")+cfg.TunnelEndpoint, time.Duration(cfg.Timeout)*time.Second)
	if err != nil {
		return fmt.Errorf("创建 tunnel 客户端失败: %w", err)
	}

	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}

	req, err := http.NewRequest(method, targetURL, bodyReader)
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	finalOutputFile := outputFile
	if finalOutputFile == "" {
		baseOutputFile := path.Base(req.URL.Path)
		if baseOutputFile == "." || baseOutputFile == "" || baseOutputFile == "/" {
			baseOutputFile = "index.html"
		}
		finalOutputFile = baseOutputFile
		no := 1
		for {
			if _, err := os.Stat(finalOutputFile); errors.Is(err, os.ErrNotExist) {
				break
			}
			finalOutputFile = fmt.Sprintf("%s.%d", baseOutputFile, no)
			no++
		}
	}

	f, err := os.Create(finalOutputFile)
	if err != nil {
		return fmt.Errorf("创建结果文件失败: %w", err)
	}
	defer f.Close()

	if verbose {
		tunnelURL := strings.TrimRight(cfg.ServerURL, "/") + cfg.TunnelEndpoint
		fmt.Fprintf(os.Stderr, "--%s-- #Tunnel %s\n", time.Now().Format("2006-01-02 15:04:05"), tunnelURL)
		fmt.Fprintf(os.Stderr, "[请求] %s %s\n", method, targetURL)
		for k := range req.Header {
			fmt.Fprintf(os.Stderr, "%s: %s\n", k, req.Header.Get(k))
		}
		fmt.Fprintln(os.Stderr)
	}

	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("tunnel 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if verbose {
		fmt.Fprintf(os.Stderr, "[响应状态] %s\n", resp.Status)
		for k := range resp.Header {
			fmt.Fprintf(os.Stderr, "%s: %s\n", k, resp.Header.Get(k))
		}
		fmt.Fprintln(os.Stderr)
	}

	contentLength := resp.ContentLength
	if contentLength > 0 {
		fmt.Fprintf(os.Stderr, "长度：%d (%s) [%s]\n", contentLength, formatByte(float64(contentLength)), resp.Header.Get("Content-Type"))
		fmt.Fprintf(os.Stderr, "正在保存至: ‘%s’\n\n", finalOutputFile)
	}

	barWidth := 50
	var totalRead, lastRead int64
	startAt := time.Now()
	lastPrintAt := time.Now()
	buf := make([]byte, 32*1024) // 32KB 缓冲区
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			written, writeErr := f.Write(buf[:n])
			if writeErr != nil {
				return fmt.Errorf("写入文件失败: %w", writeErr)
			}
			totalRead += int64(written)

			// 打印进度条 (仅在有总长度时打印)
			if contentLength > 0 && time.Since(lastPrintAt) > time.Second {
				// 1. 计算百分比
				percent := float64(totalRead) / float64(contentLength) * 100

				// 2. 计算speed
				speed := (float64(totalRead - lastRead)) / time.Since(lastPrintAt).Seconds()

				// 3. 计算eta
				eta := int64(float64(contentLength-totalRead) / speed)

				// 4. 绘制进度条的方块（比如总长50个字符）
				filled := int(percent / 100 * float64(barWidth))
				bar := strings.Repeat("=", max(filled-1, 0)) + ">" + strings.Repeat(" ", barWidth-filled)

				// 5. 格式化已下载和总大小（这里简单用字节表示，实际可以用 humanize 库美化）
				// 注意：最后加几个空格是为了擦除上一次打印可能残留的长字符
				fmt.Fprintf(os.Stderr, "\r%6.2f%% [%s] %s (%s/s) ETA: %s      ",
					percent, bar, formatByte(float64(totalRead)), formatByte(speed), formatETA(eta))

				lastRead = totalRead
				lastPrintAt = time.Now()
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("读取响应体失败: %w", err)
		}
	}

	if contentLength > 0 {
		endAt := time.Now()

		percent := float64(totalRead) / float64(contentLength) * 100
		speed := (float64(totalRead - lastRead)) / time.Since(lastPrintAt).Seconds()
		filled := int(percent / 100 * float64(barWidth))
		bar := strings.Repeat("=", filled) + strings.Repeat(" ", barWidth-filled)
		fmt.Fprintf(os.Stderr, "\r%6.2f%% [%s] %s (%s/s)   in %s    ",
			percent, bar, formatByte(float64(totalRead)), formatByte(speed), endAt.Sub(startAt))

		totalSpeed := float64(totalRead) / endAt.Sub(startAt).Seconds()
		fmt.Fprintf(os.Stderr, "\n\n%s (%s/s) - ‘%s’ saved [%d/%d]\n", endAt.Format("2006-01-02 15:04:05"),
			formatByte(totalSpeed), finalOutputFile, totalRead, contentLength)
	}
	modTimeStr := resp.Header.Get("Last-Modified")
	if modTimeStr != "" {
		modTime, err := time.Parse(time.RFC1123, modTimeStr)
		if err == nil {
			os.Chtimes(finalOutputFile, modTime, modTime)
		}
	}
	return nil
}

func formatByte(size float64) string {
	if size > 1024*1024 {
		return fmt.Sprintf("%.1f MB", size/1024/1024)
	} else if size > 1024 {
		return fmt.Sprintf("%.1f KB", size/1024)
	}
	return fmt.Sprintf("%.0f B", size)
}

func formatETA(seconds int64) string {
	if seconds <= 0 {
		return "--:--"
	}
	if seconds > 3600 {
		return fmt.Sprintf("%dh %dm", seconds/3600, (seconds%3600)/60)
	}
	if seconds > 60 {
		return fmt.Sprintf("%dm %ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%ds", seconds)
}
