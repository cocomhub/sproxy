// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	req.Header.Set("X-File-MD5", "")

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

func TunnelRequest(cfg *SclientConfig, method, targetURL string, headers map[string]string, body string, showHeaders, verbose bool) error {
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

	if verbose {
		tunnelURL := strings.TrimRight(cfg.ServerURL, "/") + cfg.TunnelEndpoint
		fmt.Fprintf(os.Stderr, "[Tunnel] POST %s\n", tunnelURL)
		fmt.Fprintf(os.Stderr, "[请求] %s %s\n", method, targetURL)
	}

	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("tunnel 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if verbose {
		fmt.Fprintf(os.Stderr, "[响应状态] %d\n", resp.StatusCode)
	}
	if showHeaders {
		for k, v := range resp.Header {
			for _, vv := range v {
				fmt.Printf("%s: %s\n", k, vv)
			}
		}
		fmt.Println()
	}

	respBodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取响应体失败: %w", err)
	}
	fmt.Print(string(respBodyBytes))
	return nil
}
