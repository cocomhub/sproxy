// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// ShareLink 表示一个文件分享链接。
type ShareLink struct {
	Token        string    `json:"token"`
	Filename     string    `json:"filename"`
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
func (s *ShareStore) Create(filename string, ttl time.Duration, maxDownloads int, oneTime bool) (*ShareLink, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("生成 token 失败: %w", err)
	}
	token := hex.EncodeToString(b)
	link := &ShareLink{
		Token:        token,
		Filename:     filename,
		ExpiresAt:    time.Now().Add(ttl),
		MaxDownloads: maxDownloads,
		OneTime:      oneTime,
	}
	s.mu.Lock()
	s.links[token] = link
	s.mu.Unlock()
	return link, nil
}

// Get 返回指定 token 的分享链接（不检查有效性）。
func (s *ShareStore) Get(token string) *ShareLink {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.links[token]
}

// Delete 删除指定 token 的分享链接。
func (s *ShareStore) Delete(token string) {
	s.mu.Lock()
	delete(s.links, token)
	s.mu.Unlock()
}

// createShareHandler 处理 POST /api/share。
// 请求体 JSON: {"filename":"…","ttl":"24h","max_downloads":0,"one_time":false}
func (h *Handlers) createShareHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Filename     string `json:"filename"`
		TTL          string `json:"ttl"`
		MaxDownloads int    `json:"max_downloads"`
		OneTime      bool   `json:"one_time"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体解析失败: " + err.Error()}, http.StatusBadRequest)
		return
	}
	if req.Filename == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "filename 不能为空"}, http.StatusBadRequest)
		return
	}
	if _, err := ValidateFilePath(req.Filename); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件名: " + err.Error()}, http.StatusBadRequest)
		return
	}

	ttl := 24 * time.Hour
	if req.TTL != "" {
		if d, err := time.ParseDuration(req.TTL); err == nil && d > 0 {
			ttl = d
		}
	}

	link, err := h.shareStore.Create(req.Filename, ttl, req.MaxDownloads, req.OneTime)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "创建分享链接失败: " + err.Error()}, http.StatusInternalServerError)
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

// accessShareHandler 处理 GET /s/{token}，重定向到文件下载。
func (h *Handlers) accessShareHandler(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}

	link := h.shareStore.Get(token)
	if link == nil {
		http.Error(w, "分享链接不存在或已失效", http.StatusNotFound)
		return
	}

	if time.Now().After(link.ExpiresAt) {
		h.shareStore.Delete(token)
		http.Error(w, "分享链接已过期", http.StatusGone)
		return
	}

	if link.MaxDownloads > 0 && link.Downloads >= link.MaxDownloads {
		h.shareStore.Delete(token)
		http.Error(w, "分享链接已达下载次数上限", http.StatusGone)
		return
	}

	// 递增下载次数（需要写锁）
	h.shareStore.mu.Lock()
	link.Downloads++
	if link.OneTime {
		delete(h.shareStore.links, token)
	}
	h.shareStore.mu.Unlock()

	http.Redirect(w, r, "/download?filename="+url.QueryEscape(link.Filename), http.StatusFound)
}
