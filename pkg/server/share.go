// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	maxShareTTL      = 30 * 24 * time.Hour // 最长 30 天
	maxShareEntries  = 10000               // 最多 10000 条分享链接
	maxShareBodySize = 4096                // 请求体最大 4KB
)

// ShareLink 表示一个文件分享链接。
type ShareLink struct {
	Token        string    `json:"token"`
	Filename     string    `json:"filename"`
	AbsPath      string    `json:"-"` // 创建时解析的绝对路径
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	MaxDownloads int       `json:"max_downloads"` // 0 = 不限
	Downloads    int       `json:"downloads"`
	OneTime      bool      `json:"one_time"`
}

// ShareStore 管理内存中的分享链接。
type ShareStore struct {
	mu    sync.RWMutex
	links map[string]*ShareLink
}

// NewShareStore 创建 ShareStore 实例。
func NewShareStore() *ShareStore {
	return &ShareStore{links: make(map[string]*ShareLink)}
}

// Create 生成新的分享链接并存储。
func (s *ShareStore) Create(filename, absPath string, ttl time.Duration, maxDownloads int, oneTime bool) (*ShareLink, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("生成 token 失败: %w", err)
	}
	token := hex.EncodeToString(b)
	link := &ShareLink{
		Token:        token,
		Filename:     filename,
		AbsPath:      absPath,
		CreatedAt:    time.Now(),
		ExpiresAt:    time.Now().Add(ttl),
		MaxDownloads: maxDownloads,
		OneTime:      oneTime,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.links) >= maxShareEntries {
		// 达到上限时删除最旧的 10% 条目
		evictCount := maxShareEntries / 10
		evicted := 0
		for k, v := range s.links {
			if evicted >= evictCount {
				break
			}
			if time.Now().After(v.ExpiresAt) {
				delete(s.links, k)
				evicted++
			}
		}
		if evicted == 0 {
			return nil, fmt.Errorf("分享链接已满，请稍后重试")
		}
	}
	s.links[token] = link
	return link, nil
}

// Consume 原子性地检查并消费一个分享链接。
// 返回链接信息供后续使用，如果链接无效则返回 nil。
func (s *ShareStore) Consume(token string) *ShareLink {
	s.mu.Lock()
	defer s.mu.Unlock()

	link := s.links[token]
	if link == nil {
		return nil
	}

	if time.Now().After(link.ExpiresAt) {
		delete(s.links, token)
		return nil
	}

	if link.MaxDownloads > 0 && link.Downloads >= link.MaxDownloads {
		delete(s.links, token)
		return nil
	}

	link.Downloads++
	if link.OneTime {
		delete(s.links, token)
	}

	return link
}

// List 返回所有分享链接的副本。
func (s *ShareStore) List() []*ShareLink {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*ShareLink, 0, len(s.links))
	for _, link := range s.links {
		cp := *link
		result = append(result, &cp)
	}
	return result
}

// Revoke 删除指定 token 的分享链接。链接不存在时返回 error。
func (s *ShareStore) Revoke(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.links[token]; !ok {
		return fmt.Errorf("分享链接不存在: %s", token)
	}
	delete(s.links, token)
	return nil
}

// createShareHandler 处理 POST /api/share。
// 请求体 JSON: {"filename":"…","ttl":"24h","max_downloads":0,"one_time":false}
func (h *Handlers) createShareHandler(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxShareBodySize)

	var req struct {
		Filename     string `json:"filename"`
		TTL          string `json:"ttl"`
		MaxDownloads int    `json:"max_downloads"`
		OneTime      bool   `json:"one_time"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体解析失败"}, http.StatusBadRequest)
		return
	}
	if req.Filename == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "filename 不能为空"}, http.StatusBadRequest)
		return
	}
	remotePath, err := ValidateFilePath(req.Filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}

	fullPath := h.safePath(remotePath)
	if fullPath == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	if _, err = os.Stat(fullPath); os.IsNotExist(err) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		return
	}

	// 解析并限制 TTL
	ttl := 24 * time.Hour
	if req.TTL != "" {
		if d, ttlErr := time.ParseDuration(req.TTL); ttlErr == nil && d > 0 {
			ttl = min(d, maxShareTTL)
		}
	}

	link, err := h.shareStore.Create(req.Filename, fullPath, ttl, req.MaxDownloads, req.OneTime)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "创建分享链接失败"}, http.StatusInternalServerError)
		return
	}

	sendJSONResponse(w, map[string]any{
		"token":         link.Token,
		"filename":      link.Filename,
		"expires_at":    link.ExpiresAt.Format(time.RFC3339),
		"max_downloads": link.MaxDownloads,
		"one_time":      link.OneTime,
	}, http.StatusOK)
}

// accessShareHandler 处理 GET /s/{token}，直接流式传输文件内容。
func (h *Handlers) accessShareHandler(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}

	// 原子消费：检查有效性 + 递增计数 + 一次性删除
	link := h.shareStore.Consume(token)
	if link == nil {
		http.Error(w, "分享链接不存在或已失效", http.StatusNotFound)
		return
	}

	// 直接流式传输文件，不暴露文件路径
	f, err := os.Open(link.AbsPath)
	if err != nil {
		http.Error(w, "文件读取失败", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		http.Error(w, "文件状态读取失败", http.StatusInternalServerError)
		return
	}

	w.Header().Set(headerContentType, contentTypeOctetStream)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filepath.Base(link.Filename)))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", stat.Size()))
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// listSharesHandler 处理 GET /api/shares，返回所有分享链接。
func (h *Handlers) listSharesHandler(w http.ResponseWriter, r *http.Request) {
	links := h.shareStore.List()

	type shareItem struct {
		Token        string `json:"token"`
		Filename     string `json:"filename"`
		CreatedAt    string `json:"created_at"`
		ExpiresAt    string `json:"expires_at"`
		MaxDownloads int    `json:"max_downloads"`
		Downloads    int    `json:"downloads"`
		OneTime      bool   `json:"one_time"`
		Expired      bool   `json:"expired"`
	}

	now := time.Now()
	items := make([]shareItem, 0, len(links))
	for _, l := range links {
		expired := now.After(l.ExpiresAt) || (l.MaxDownloads > 0 && l.Downloads >= l.MaxDownloads)
		items = append(items, shareItem{
			Token:        l.Token,
			Filename:     l.Filename,
			CreatedAt:    l.CreatedAt.Format(time.RFC3339),
			ExpiresAt:    l.ExpiresAt.Format(time.RFC3339),
			MaxDownloads: l.MaxDownloads,
			Downloads:    l.Downloads,
			OneTime:      l.OneTime,
			Expired:      expired,
		})
	}

	sendJSONResponse(w, map[string]any{"shares": items}, http.StatusOK)
}

// revokeShareHandler 处理 DELETE /api/shares/{token}，撤销指定分享链接。
func (h *Handlers) revokeShareHandler(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "token 不能为空"}, http.StatusBadRequest)
		return
	}

	if err := h.shareStore.Revoke(token); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: err.Error()}, http.StatusNotFound)
		return
	}

	sendJSONResponse(w, UploadResponse{Success: true, Message: "分享链接已撤销"}, http.StatusOK)
}
