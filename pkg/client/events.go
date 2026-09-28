// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// FileEvent 是 /api/events SSE 推送的文件变更事件（对齐服务端 fileEvent JSON）。
type FileEvent struct {
	Cursor uint64 `json:"cursor"`
	Action string `json:"action"`
	Owner  string `json:"owner"`
	Rel    string `json:"rel"`
	Size   int64  `json:"size,omitempty"`
}

// WatchEventsOptions 是 WatchEvents 的配置项。
type WatchEventsOptions struct {
	// Owner 订阅的 owner（事件流按 owner 隔离；空 = 匿名/当前凭据 owner）。
	Owner string
	// LastEventID 初始游标（重连回放起点；0 = 从当前时刻开始，不回放历史）。
	LastEventID uint64
	// MaxBackoff 断线重连最大退避间隔（默认 30s）。
	MaxBackoff time.Duration
}

// WatchEvents 建立 /api/events SSE 长连接并持续消费文件变更事件。
//
// 事件通过回调 onEvent 逐条投递（nil 则忽略）；连接断开自动指数退避重连
// （1s→MaxBackoff 封顶），重连时携带 Last-Event-ID 头实现游标回放（不丢事件）。
// ctx 取消返回 nil（正常退出）。认证失败（401/403）与不可重试错误返回 error（不重连）。
//
// 注意：本方法**阻塞**直到 ctx 取消或连接终结；调用方应在新 goroutine 中执行。
// 返回的 *http.Response 在调用返回后已关闭（仅供错误诊断）。
func (c *FileClient) WatchEvents(ctx context.Context, opts WatchEventsOptions, onEvent func(FileEvent)) error {
	if onEvent == nil {
		onEvent = func(FileEvent) { /* 无监听者：丢弃事件 */ }
	}
	maxBackoff := opts.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 30 * time.Second
	}
	baseDelay := time.Second

	// 当前游标（重连推进）。
	var cursor atomic.Uint64
	cursor.Store(opts.LastEventID)

	backoff := baseDelay
	for {
		lastID := cursor.Load()
		err := c.watchEventsOnce(ctx, opts.Owner, lastID, onEvent, &cursor)
		if err == nil {
			return nil // 正常终结
		}
		// ctx 取消是优雅退出（非错误）：读侧因 resp.Body 关闭返回 EOF，
		// 这里统一把「请求上下文已取消」映射为 nil。
		if ctx.Err() != nil {
			return nil
		}
		// 不可重试错误（认证/协议）直接返回。
		if isFatalEventsError(err) {
			return err
		}
		// 指数退避重连。
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// watchEventsOnce 建立一次 SSE 连接并消费到断开/取消。
// 返回 nil 表示正常终结（ctx 取消）；返回错误表示本次连接异常（调用方决定重连）。
func (c *FileClient) watchEventsOnce(ctx context.Context, owner string, lastID uint64, onEvent func(FileEvent), cursor *atomic.Uint64) error {
	apiPath, headers := eventStreamRequest(owner, lastID)
	resp, err := c.doRequest(ctx, http.MethodGet, apiPath, nil, headers)
	if err != nil {
		return fmt.Errorf("建立事件流连接: %w", err)
	}
	// ctx 取消时关闭响应体：http.Response.Body 的 Read 不受请求 ctx 取消控制，
	// 不关闭会永久阻塞在 br.ReadString（长连接无数据时）。
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			resp.Body.Close()
		case <-done:
		}
	}()
	defer close(done)
	defer resp.Body.Close()

	if err := checkEventStreamStatus(resp); err != nil {
		return err
	}

	br := bufio.NewReader(resp.Body)
	parser := &sseParser{cursor: cursor, onEvent: onEvent}
	for {
		done, herr := readEventLine(ctx, br, parser)
		if herr != nil {
			return herr
		}
		if done {
			return nil
		}
	}
}

// readEventLine 读取一行事件并交给 sseParser 处理。
// 返回 done=true 表示 ctx 已取消（优雅终结）；herr 非 nil 表示读取出错。
func readEventLine(ctx context.Context, br *bufio.Reader, parser *sseParser) (bool, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		if err == io.EOF && ctx.Err() != nil {
			return true, nil
		}
		if err == io.EOF {
			return false, fmt.Errorf("事件流断开: %w", err)
		}
		return false, fmt.Errorf("读事件流: %w", err)
	}
	if err := parser.handleLine(strings.TrimRight(line, "\r\n")); err != nil {
		return false, err
	}
	select {
	case <-ctx.Done():
		return true, nil
	default:
	}
	return false, nil
}

// eventStreamRequest 组装事件流的请求路径与头（Last-Event-ID 续传游标）。
func eventStreamRequest(owner string, lastID uint64) (string, http.Header) {
	headers := make(http.Header)
	headers.Set("Accept", "text/event-stream")
	if lastID > 0 {
		headers.Set("Last-Event-ID", strconv.FormatUint(lastID, 10))
	}
	apiPath := "/api/events"
	if owner != "" {
		apiPath += "?owner=" + url.QueryEscape(owner)
	}
	return apiPath, headers
}

// checkEventStreamStatus 校验事件流响应状态：认证失败 401/403 与其它非 200。
func checkEventStreamStatus(resp *http.Response) error {
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("事件流认证失败 (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("事件流请求失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

// sseParser 累积 SSE 事件字段，事件块结束时派发回调并推进游标。
type sseParser struct {
	evID      uint64
	dataLines []string
	cursor    *atomic.Uint64
	onEvent   func(FileEvent)
}

// handleLine 处理一行 SSE 字段并维护解析状态。
func (p *sseParser) handleLine(line string) error {
	switch {
	case line == "":
		// 空行 = 事件块结束，flush 当前累积。
		if err := p.flush(); err != nil {
			return err
		}
		p.evID = 0
	case strings.HasPrefix(line, ":"):
		// 注释/心跳行（忽略）。
	case strings.HasPrefix(line, "id:"):
		if v, err := strconv.ParseUint(strings.TrimSpace(line[len("id:"):]), 10, 64); err == nil {
			p.evID = v
		}
	case strings.HasPrefix(line, "data:"):
		p.dataLines = append(p.dataLines, strings.TrimSpace(line[len("data:"):]))
	}
	return nil
}

// flush 结束当前事件块：有 data 且 id 非空才派发（SSE 规范：无 id 不推进游标）。
func (p *sseParser) flush() error {
	if len(p.dataLines) == 0 {
		return nil
	}
	data := strings.Join(p.dataLines, "\n")
	p.dataLines = nil
	if p.evID == 0 {
		return nil // 无 id 的事件忽略（SSE 规范：无 id 不推进游标）
	}
	var ev FileEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return fmt.Errorf("解析事件 JSON: %w", err)
	}
	ev.Cursor = p.evID
	p.cursor.Store(p.evID)
	p.onEvent(ev)
	return nil
}

// isFatalEventsError 判定事件流错误是否不可重试（认证/协议错误直接返回，不重连）。
func isFatalEventsError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "认证失败") || strings.Contains(msg, "HTTP 4")
}
