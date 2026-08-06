// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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
	mu       sync.RWMutex
	links    map[string]*ShareLink
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	logger   *slog.Logger
}

// NewShareStore 创建 ShareStore 实例。
func NewShareStore(logger *slog.Logger) *ShareStore {
	s := &ShareStore{
		links:  make(map[string]*ShareLink),
		stopCh: make(chan struct{}),
		logger: logger,
	}
	s.wg.Add(1)
	go s.cleanupLoop()
	return s
}

// cleanupLoop 定期清理过期的分享链接。
func (s *ShareStore) cleanupLoop() {
	defer s.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("share cleanupLoop panic", "panic", r)
		}
	}()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.cleanupExpired()
		case <-s.stopCh:
			s.cleanupExpired() // 退出前清理一次
			return
		}
	}
}

// cleanupExpired 清理所有已过期的分享链接。
func (s *ShareStore) cleanupExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.links {
		if now.After(v.ExpiresAt) {
			delete(s.links, k)
		}
	}
}

// Stop 停止后台清理 goroutine。等待清理 goroutine 退出后返回。
func (s *ShareStore) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
}

// Create 生成新的分享链接并存储。
func (s *ShareStore) Create(filename, absPath string, ttl time.Duration, maxDownloads int, oneTime bool) (*ShareLink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	const maxTokenRetries = 10

	var token string
	for range maxTokenRetries {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return nil, fmt.Errorf("生成 token 失败: %w", err)
		}
		token = hex.EncodeToString(b)
		if _, exists := s.links[token]; !exists {
			break
		}
	}
	if _, exists := s.links[token]; exists {
		return nil, fmt.Errorf("无法生成唯一的分享 token（重试 %d 次后仍冲突）", maxTokenRetries)
	}

	now := time.Now()
	link := &ShareLink{
		Token:        token,
		Filename:     filename,
		AbsPath:      absPath,
		CreatedAt:    now,
		ExpiresAt:    now.Add(ttl),
		MaxDownloads: maxDownloads,
		OneTime:      oneTime,
	}
	if len(s.links) >= maxShareEntries {
		// 先全量清理过期条目
		cleanupNow := time.Now()
		for k, v := range s.links {
			if cleanupNow.After(v.ExpiresAt) {
				delete(s.links, k)
			}
		}
		// 如果清理后仍有空间，直接插入
		if len(s.links) < maxShareEntries {
			s.links[token] = link
			return link, nil
		}
		// 仍满时按创建时间淘汰最旧的 10%
		evictCount := maxShareEntries / 10
		sorted := make([]struct {
			key       string
			createdAt time.Time
		}, 0, len(s.links))
		for k, v := range s.links {
			sorted = append(sorted, struct {
				key       string
				createdAt time.Time
			}{key: k, createdAt: v.CreatedAt})
		}
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].createdAt.Before(sorted[j].createdAt)
		})
		for i := 0; i < evictCount && i < len(sorted); i++ {
			delete(s.links, sorted[i].key)
		}
	}
	s.links[token] = link
	return link, nil
}

// Peek 返回指定 token 的分享链接副本，不修改状态（不计数、不删除）。
func (s *ShareStore) Peek(token string) *ShareLink {
	s.mu.RLock()
	defer s.mu.RUnlock()
	link, ok := s.links[token]
	if !ok {
		return nil
	}
	c := *link
	return &c
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

// ShareCreateResponse 创建/撤销分享链接的响应结构体。
type ShareCreateResponse struct {
	Success      bool   `json:"success"`
	Token        string `json:"token,omitempty"`
	Filename     string `json:"filename,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	MaxDownloads int    `json:"max_downloads,omitempty"`
	OneTime      bool   `json:"one_time,omitempty"`
	Message      string `json:"message,omitempty"`
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
		sendJSONResponse(w, ShareCreateResponse{Success: false, Message: "请求体解析失败"}, http.StatusBadRequest)
		return
	}
	if req.Filename == "" {
		sendJSONResponse(w, ShareCreateResponse{Success: false, Message: "filename 不能为空"}, http.StatusBadRequest)
		return
	}
	remotePath, err := ValidateFilePath(req.Filename)
	if err != nil {
		sendJSONResponse(w, ShareCreateResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}

	fullPath := h.safePath(remotePath)
	if fullPath == "" {
		sendJSONResponse(w, ShareCreateResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	fi, lstatErr := os.Lstat(fullPath)
	if lstatErr != nil {
		if os.IsNotExist(lstatErr) {
			sendJSONResponse(w, ShareCreateResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		} else {
			sendJSONResponse(w, ShareCreateResponse{Success: false, Message: "无法访问文件"}, http.StatusInternalServerError)
		}
		return
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		sendJSONResponse(w, ShareCreateResponse{Success: false, Message: "不支持分享符号链接"}, http.StatusBadRequest)
		return
	}

	// 解析并限制 TTL
	ttl := 24 * time.Hour
	if req.TTL != "" {
		d, ttlErr := time.ParseDuration(req.TTL)
		if ttlErr != nil {
			sendJSONResponse(w, ShareCreateResponse{Success: false, Message: "无效的 TTL 格式"}, http.StatusBadRequest)
			return
		}
		if d <= 0 {
			sendJSONResponse(w, ShareCreateResponse{Success: false, Message: "TTL 必须大于 0"}, http.StatusBadRequest)
			return
		}
		ttl = min(d, maxShareTTL)
	}

	link, err := h.shareStore.Create(req.Filename, fullPath, ttl, req.MaxDownloads, req.OneTime)
	if err != nil {
		sendJSONResponse(w, ShareCreateResponse{Success: false, Message: "创建分享链接失败"}, http.StatusInternalServerError)
		return
	}

	sendJSONResponse(w, ShareCreateResponse{
		Success:      true,
		Token:        link.Token,
		Filename:     link.Filename,
		CreatedAt:    link.CreatedAt.Format(time.RFC3339),
		ExpiresAt:    link.ExpiresAt.Format(time.RFC3339),
		MaxDownloads: link.MaxDownloads,
		OneTime:      link.OneTime,
	}, http.StatusOK)
}

// accessShareHandler 处理 GET /s/{token}，直接流式传输文件内容。
func (h *Handlers) accessShareHandler(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}

	// Peek：先查看链接是否存在且文件有效，不修改状态
	link := h.shareStore.Peek(token)
	if link == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "分享链接无效或已过期"}, http.StatusNotFound)
		return
	}
	// 检查文件是否存在
	if _, err := os.Stat(link.AbsPath); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "分享文件已不存在"}, http.StatusGone)
		return
	}
	// Consume：再消费（递增计数、一次性删除）
	link = h.shareStore.Consume(token)
	if link == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "分享链接已被消费"}, http.StatusConflict)
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
	w.Header().Set("Content-Disposition", formatContentDisposition(filepath.Base(link.Filename)))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", stat.Size()))
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	if _, err := copyWithContext(w, f, r.Context()); err != nil {
		h.logger.Warn("分享文件流式传输中断", "token", token, "error", err)
	}
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
		sendJSONResponse(w, ShareCreateResponse{Success: false, Message: "token 不能为空"}, http.StatusBadRequest)
		return
	}

	if err := h.shareStore.Revoke(token); err != nil {
		sendJSONResponse(w, ShareCreateResponse{Success: false, Message: err.Error()}, http.StatusNotFound)
		return
	}

	sendJSONResponse(w, ShareCreateResponse{Success: true, Message: "分享链接已撤销"}, http.StatusOK)
}
