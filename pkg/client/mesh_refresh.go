// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// MeshTargetTTL 是 mesh 服务解析缓存的新鲜窗口。过期后下一次 Resolve 触发重新
// 拉取 /api/hub/services，使 relay 节点下线/重上线的变化被 mesh 感知。
const MeshTargetTTL = 3 * time.Second

// MeshResolveTimeout 是单次服务解析的网络超时，防止 hub 无响应拖住连接建立。
const MeshResolveTimeout = 5 * time.Second

// ErrMeshServiceUnavailable 报告服务当前不可用（节点离线或未宣告）。
func ErrMeshServiceUnavailable(service string) error {
	return fmt.Errorf("mesh 服务 %q 当前不可用（节点离线或未宣告）", service)
}

// InsecureHTTPClient 返回跳过 TLS 证书验证的 http.Client（仅自签证书开发/测试环境；
// 生产环境应使用受信 CA 或把自签 CA 加入 RootCAs，而非关闭校验）。
// Timeout 取 60s 对齐 HubSignaler 长轮询（单次 poll 60s > 服务端 PollTimeout 25s）。
func InsecureHTTPClient() *http.Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	}
	return &http.Client{Timeout: 60 * time.Second, Transport: tr}
}

// MeshSignalToken 返回信令 Bearer token：显式 flagToken 优先，否则复用 svcAuthToken。
// hub 的 /api/signal/* 走 authMiddleware（校验 auth_token），与 MeshServices /
// RelayStream 的认证一致；relay start --token 是另一套 relay 注册 token，不混用。
func MeshSignalToken(flagToken, svcAuthToken string) string {
	if flagToken != "" {
		return flagToken
	}
	return svcAuthToken
}

// MeshAccessKey 返回 SproxySig 认证 AccessKey：显式 flag 优先，否则配置值。
func MeshAccessKey(flagKey, cfgKey string) string {
	if flagKey != "" {
		return flagKey
	}
	return cfgKey
}

// MeshAccessKeySecret 返回 SproxySig 认证 AccessKeySecret：显式 flag 优先，否则配置值。
func MeshAccessKeySecret(flagKey, cfgKey string) string {
	if flagKey != "" {
		return flagKey
	}
	return cfgKey
}

// MeshRelayToken 返回自动注册用的 relay_token：显式 relayToken > 配置 relayToken
// （svcRelayToken）> 回落 MeshSignalToken（--token → auth_token）。
func MeshRelayToken(flagRelayToken, svcRelayToken, flagToken, svcAuthToken string) string {
	if flagRelayToken != "" {
		return flagRelayToken
	}
	if svcRelayToken != "" {
		return svcRelayToken
	}
	return MeshSignalToken(flagToken, svcAuthToken)
}

// MeshTargetRefresher 按需解析 mesh 目标，带 TTL 缓存与单飞（single-flight）刷新。
//
// 并发安全设计：所有缓存字段由 mu 保护；刷新期间**不持有 mu 做网络调用**——
// 承担刷新的 goroutine 置位后解锁再请求，等待者在 done 通道上等待，完成后重新
// 抢锁读取最终状态。这样 TTL 内并发调用只打一次 hub（单飞），且不引入「锁内
// 做 I/O」死锁风险。
type MeshTargetRefresher struct {
	svc     *FileClient
	service string
	ttl     time.Duration
	now     func() time.Time // 可注入时钟（测试用）

	mu             sync.Mutex
	target         *MeshService
	lastRefresh    time.Time
	refreshing     bool
	done           chan struct{}
	refreshErr     error
	lastFailedNode string
}

// NewMeshTargetRefresher 创建 refresher。
func NewMeshTargetRefresher(svc *FileClient, service string) *MeshTargetRefresher {
	return &MeshTargetRefresher{svc: svc, service: service, ttl: MeshTargetTTL, now: time.Now}
}

// SetClock 注入替代时钟（测试用）。
func (r *MeshTargetRefresher) SetClock(now func() time.Time) { r.now = now }

// SetTTL 覆盖缓存新鲜窗口（测试用；生产用 MeshTargetTTL 默认）。
func (r *MeshTargetRefresher) SetTTL(ttl time.Duration) { r.ttl = ttl }

// Service 返回服务名。
func (r *MeshTargetRefresher) Service() string { return r.service }

// Resolve 返回服务当前目标。缓存新鲜（<TTL）直接返回副本；否则触发一次刷新，
// 并发调用共享同一刷新（单飞）。服务不在列表返回 ErrMeshServiceUnavailable。
func (r *MeshTargetRefresher) Resolve(ctx context.Context) (*MeshService, error) {
	r.mu.Lock()
	if r.target != nil && r.now().Sub(r.lastRefresh) < r.ttl {
		t := *r.target
		r.mu.Unlock()
		return &t, nil
	}
	if r.refreshing {
		done := r.done
		r.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.refreshErr != nil {
			return nil, r.refreshErr
		}
		if r.target != nil {
			t := *r.target
			return &t, nil
		}
		return nil, ErrMeshServiceUnavailable(r.service)
	}
	// 本 goroutine 承担刷新：置位后立即解锁，绝不在锁内做网络调用。
	r.refreshing = true
	r.refreshErr = nil
	r.done = make(chan struct{})
	r.mu.Unlock()

	fetchCtx, cancel := context.WithTimeout(ctx, MeshResolveTimeout)
	svcs, err := r.svc.MeshServices(fetchCtx)
	cancel()

	r.mu.Lock()
	r.refreshing = false
	close(r.done) // 等待者唤醒后先抢锁再读最终状态，无竞态
	if err != nil {
		r.refreshErr = fmt.Errorf("查询 mesh 服务失败: %w", err)
		r.mu.Unlock()
		return nil, r.refreshErr
	}
	// 多候选 failover：优先选择最近未失败的节点，避免死节点永久遮蔽健康副本
	// （旧实现恒取 node-ID 字典序首个）。
	var first, candidate *MeshService
	for i := range svcs {
		if svcs[i].Name != r.service {
			continue
		}
		t := svcs[i]
		if first == nil {
			first = &t
		}
		if candidate == nil && t.Node != r.lastFailedNode {
			candidate = &t
		}
	}
	if candidate == nil {
		candidate = first // 全部候选都是最近失败节点：回退到首个
	}
	if candidate != nil {
		t := *candidate
		r.target = &t
		r.lastRefresh = r.now()
		r.mu.Unlock()
		return &t, nil
	}
	r.target = nil
	r.lastRefresh = time.Time{}
	r.mu.Unlock()
	return nil, ErrMeshServiceUnavailable(r.service)
}

// Invalidate 使缓存过期：dial 失败后调用，并记录失败节点让下一次 Resolve 跳过它。
func (r *MeshTargetRefresher) Invalidate(failedNode string) {
	r.mu.Lock()
	r.target = nil
	r.lastRefresh = time.Time{}
	r.refreshErr = nil
	r.lastFailedNode = failedNode
	r.mu.Unlock()
}
