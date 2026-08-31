// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webrtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// TURN REST API（coturn 标准，draft-uberti-behave-turn-rest-00）短期凭证：
//
//	GET {url}?username=<user>[&service=<svc>]
//	  → 200 {username: "<expiry-ts>:<user>", password: "<base64(HMAC-SHA1)>", ttl: <seconds>}
//
// 响应里的 username/password 是服务端已算好的临时凭据，客户端只需透传到 ICEServer
// （pion 直接把 Username/Credential 下发给 TURN 服务器，coturn 用共享 secret 验签）。
// 与静态凭据（SetTURNServers + SetTURNCredential）并存，REST 优先；REST 拉取失败回落静态。

const (
	// turnRESTFetchTimeout 是单次 REST 凭据拉取超时。打洞首 PC 前惰性拉取会阻塞等它，
	// 上限 2s 避免拉取蔓延拖垮首 PC（REST 端点不可达时快速失败回落）。
	turnRESTFetchTimeout = 2 * time.Second
	// turnRESTRenewThreshold 是续期阈值：剩余 TTL 小于该值（或已过期）时下次 PC 前续期。
	turnRESTRenewThreshold = 60 * time.Second
	// turnRESTFetchBackoff 是拉取失败后的退避窗口（审查 Minor 2）：失败后该窗口内
	// 不重拉（用旧缓存或回落），避免端点故障期间每轮 newPC 阻塞重拉 + Warn 刷屏。
	turnRESTFetchBackoff = 30 * time.Second
	// maxTURNParamLen 是 TURN REST URL/username/service 的长度上限（防注入/误配）。
	maxTURNParamLen = 512
)

// turnRESTTTLUserRegexp 匹配 coturn 短期凭据 username 的 "TTL:user" 格式
// （审查 Minor 4：响应 username 应形如 "3600:api-user"，非该格式拒绝 fail-fast）。
var turnRESTTTLUserRegexp = regexp.MustCompile(`^\d+:`)

// restCredential 是 REST 短期 TURN 凭据（服务端算好的，客户端透传）。
type restCredential struct {
	username  string
	password  string
	expiresAt time.Time
}

var (
	// turnRESTMu 保护 TURN REST 全局配置与缓存（SetTURNRESTURL / ensureTURNRESTCredential）。
	turnRESTMu sync.Mutex
	// turnRESTURL 是 REST 凭证端点（如 https://turn.example.com/turn）；空 = 未配置。
	turnRESTURL string
	// turnRESTUsername / turnRESTService 是 REST API 的认证用户名与可选 service。
	turnRESTUsername string
	turnRESTService  string
	// turnRESTCred 是缓存的短期凭据；nil = 无有效缓存。
	turnRESTCred *restCredential
	// turnRESTFetchCh 非 nil 表示拉取在途（单飞：首次并发 newPC 只拉一次），关闭后完成。
	turnRESTFetchCh chan struct{}
	// turnRESTFetchFailAt 是最近一次拉取失败的时间（审查 Minor 2 退避：失败后
	// turnRESTFetchBackoff 内不重拉，避免端点故障期间每轮 newPC 阻塞重拉 + Warn 刷屏）。
	turnRESTFetchFailAt time.Time
	// turnRESTClient 是拉取 REST 凭据的 http.Client。CheckRedirect 拒绝跨 scheme 重定向
	// （审查 Minor 1：防止 https 端点 302 到非 loopback http，凭据 query 明文上线）。
	turnRESTClient = &http.Client{
		Timeout: turnRESTFetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// 重定向目标必须仍是 https（或 loopback http）——与 SetTURNRESTURL 的
			// scheme 边界一致，防止凭据 query 被带到明文/跨主机端点。
			allowed := req.URL.Scheme == "https" ||
				(req.URL.Scheme == "http" && isLoopbackHost(req.URL.Hostname()))
			if !allowed {
				return fmt.Errorf("webrtc: TURN REST 重定向目标 %s 违反安全边界（仅 https 或 loopback http）", redactURLQuery(req.URL))
			}
			if len(via) >= 5 {
				return errors.New("webrtc: TURN REST 重定向次数过多")
			}
			return nil
		},
	}
)

// SetTURNRESTURL 设置 TURN REST API 短期凭证端点（coturn 标准，与静态凭据并存，REST 优先）。
// url 形如 https://turn.example.com/turn；username 是 REST API 的认证用户名；
// service 可选（透传给服务端，如用于区分 realm/service）。
//
// 安全边界（fail-closed）：URL 默认强推 https；明文 http 仅限 loopback（本机调试），
// 非 loopback 的 http 拒绝（否则凭据与 TURN 流量可被中间人读取，对齐 sync_remotes
// 明文校验模式）。url 传空串清空 REST 配置与缓存（供测试复位/CLI 停用）。
// 校验失败返回 error 且不修改当前配置（保持之前的值）。
func SetTURNRESTURL(urlStr, username, service string) error {
	if strings.TrimSpace(urlStr) == "" {
		turnRESTMu.Lock()
		turnRESTURL = ""
		turnRESTUsername = ""
		turnRESTService = ""
		turnRESTCred = nil
		turnRESTFetchFailAt = time.Time{} // 清空退避（配置变更后允许立即重拉）
		turnRESTMu.Unlock()
		return nil
	}
	if len(urlStr) > maxTURNParamLen || len(username) > maxTURNParamLen || len(service) > maxTURNParamLen {
		return fmt.Errorf("webrtc: TURN REST 参数过长（上限 %d 字符）", maxTURNParamLen)
	}
	u, err := url.Parse(strings.TrimSpace(urlStr))
	if err != nil {
		return fmt.Errorf("webrtc: TURN REST URL 非法: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("webrtc: TURN REST URL scheme %q 无效，仅允许 https（http 仅限 loopback）", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("webrtc: TURN REST URL 缺少 host: %q", urlStr)
	}
	// 明文 http 仅限 loopback（本机调试）：远程端点用 http 会把 TURN 临时凭据明文上线，
	// 且 TURN 数据面本身可被窃听——对齐 sync_remotes 明文校验模式（安全审查 MEDIUM）。
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("webrtc: TURN REST URL 使用明文 http 且非 loopback（凭据将明文上线；远程端点请用 https，本机调试可用 http://127.0.0.1）: %q", urlStr)
	}
	if username == "" {
		return fmt.Errorf("webrtc: TURN REST username 不能为空")
	}

	turnRESTMu.Lock()
	turnRESTURL = u.String()
	turnRESTUsername = username
	turnRESTService = service
	turnRESTCred = nil                // 配置变更后旧缓存作废（新端点/新凭据）
	turnRESTFetchFailAt = time.Time{} // 清空退避（新配置允许立即拉取）
	turnRESTMu.Unlock()
	return nil
}

// fetchTURNRESTCredential 向 REST 端点发 GET（coturn 惯例）拉取短期凭据。
// 成功返回透传凭据（含 expiresAt）；失败返回 error（调用方降级回落，不 panic）。
func fetchTURNRESTCredential() (*restCredential, error) {
	turnRESTMu.Lock()
	base, user, svc := turnRESTURL, turnRESTUsername, turnRESTService
	turnRESTMu.Unlock()
	if base == "" {
		return nil, errors.New("webrtc: TURN REST 未配置")
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("webrtc: 解析 TURN REST URL 失败: %w", err)
	}
	// 审查 Minor 3：合并 base URL 已有的 query（如 ?realm=..）而非整段覆盖。
	q := u.Query()
	q.Set("username", user)
	if svc != "" {
		q.Set("service", svc)
	}
	u.RawQuery = q.Encode()

	// ctx 超时与 turnRESTClient.Timeout 双重兜底（ctx 用于 req 级取消，Timeout 用于
	// 整个请求含响应体读取）。
	ctx, cancel := context.WithTimeout(context.Background(), turnRESTFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("webrtc: 构造 TURN REST 请求失败: %w", err)
	}
	resp, err := turnRESTClient.Do(req)
	if err != nil {
		// 审查 Minor 5：日志/错误剥离 query（不带 username 等参数），避免凭据落日志。
		return nil, fmt.Errorf("webrtc: 拉取 TURN REST 凭据失败（%s）: %w", redactURLQuery(u), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("webrtc: TURN REST 响应非 2xx: %d", resp.StatusCode)
	}
	var cr struct {
		Username string `json:"username"`
		Password string `json:"password"`
		TTL      int64  `json:"ttl"`
	}
	// 限制响应体大小（防恶意端点返回超大 body 撑爆内存）。
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&cr); err != nil {
		return nil, fmt.Errorf("webrtc: 解析 TURN REST 响应失败: %w", err)
	}
	// 审查 Minor 4：校验响应 username 为 coturn TTL:user 格式、ttl 设上限（防溢出/
	// 恶意值导致每次 newPC 重拉或撑大 ALLOCATE）。
	if cr.Username == "" || cr.Password == "" || cr.TTL <= 0 {
		return nil, errors.New("webrtc: TURN REST 响应缺少 username/password/ttl")
	}
	if !turnRESTTTLUserRegexp.MatchString(cr.Username) {
		return nil, fmt.Errorf("webrtc: TURN REST 响应 username %q 非 coturn TTL:user 格式", redactCredential(cr.Username))
	}
	const maxTURNRESTTTL = 24 * 60 * 60 // 24h：TURN 临时凭据 TTL 上限（防溢出/恶意值）
	if cr.TTL > maxTURNRESTTTL {
		cr.TTL = maxTURNRESTTTL
	}
	return &restCredential{
		username:  cr.Username,
		password:  cr.Password,
		expiresAt: time.Now().Add(time.Duration(cr.TTL) * time.Second),
	}, nil
}

// redactURLQuery 返回 URL 的 scheme://host/path（剥离 query，防凭据落日志）。
func redactURLQuery(u *url.URL) string {
	if u == nil {
		return ""
	}
	cu := *u
	cu.RawQuery = ""
	cu.Fragment = ""
	return cu.String()
}

// redactCredential 返回凭据的脱敏形式（保留前缀 + 掩码，防完整值落日志）。
func redactCredential(v string) string {
	if len(v) <= 8 {
		return "***"
	}
	return v[:4] + "***"
}

// ensureTURNRESTCredential 返回当前有效的 REST TURN 凭据（供 defaultConfig 组装 TURN 条目）。
//   - REST 未配置 → nil（不拉取）；
//   - 缓存有效（剩余 TTL > 阈值）→ 直接返回；
//   - 缓存缺失/过期/低 TTL → 惰性拉取（单飞：并发首次只拉一次），成功缓存并返回；
//   - 拉取失败 → 保留仍有效的旧缓存（未过期则继续用）+ 退避（turnRESTFetchBackoff 内
//     不重拉）+ 日志 + 返回当前凭据（nil 则调用方回落静态/仅 STUN，不 panic）。
func ensureTURNRESTCredential() *restCredential {
	turnRESTMu.Lock()
	if turnRESTURL == "" {
		turnRESTMu.Unlock()
		return nil
	}
	// 缓存有效（剩余 TTL > 阈值）→ 直接返回（不重拉）。
	if c := turnRESTCred; c != nil && time.Until(c.expiresAt) > turnRESTRenewThreshold {
		turnRESTMu.Unlock()
		return c
	}
	// 退避（审查 Minor 2）：最近一次拉取失败后 turnRESTFetchBackoff 内不重拉——
	// 若旧缓存仍有效（TTL 内）则继续用，否则回落（返回 nil 或旧缓存）。
	if !turnRESTFetchFailAt.IsZero() && time.Since(turnRESTFetchFailAt) < turnRESTFetchBackoff {
		turnRESTMu.Unlock()
		return turnRESTCred // 可能是旧缓存（TTL 内）或 nil（已过期）
	}
	// 有拉取在途（单飞）：等待其完成并返回其结果（成功或失败都不再重拉，
	// 避免失败时的惊群重复拉取）。
	if turnRESTFetchCh != nil {
		ch := turnRESTFetchCh
		turnRESTMu.Unlock()
		<-ch
		turnRESTMu.Lock()
		c := turnRESTCred // 可能是成功缓存或 nil（失败）
		turnRESTMu.Unlock()
		return c
	}
	// 自己拉取（单飞入口）。
	ch := make(chan struct{})
	turnRESTFetchCh = ch
	turnRESTMu.Unlock()

	cred, err := fetchTURNRESTCredential()

	turnRESTMu.Lock()
	turnRESTFetchCh = nil
	close(ch)
	if err != nil {
		// 审查 Minor 2：保留仍有效的旧缓存（未过期则下次 PC 继续用），并记录失败
		// 时间启用退避（避免端点故障期间每轮重拉 + Warn 刷屏）。
		turnRESTFetchFailAt = time.Now()
		if turnRESTCred == nil || time.Until(turnRESTCred.expiresAt) <= 0 {
			slog.Warn("webrtc: 拉取 TURN REST 短期凭据失败且无有效缓存，本次回落静态凭据/仅 STUN", "error", err)
		} else {
			slog.Warn("webrtc: 拉取 TURN REST 短期凭据失败，沿用旧缓存（剩余 TTL 内）", "error", err)
		}
	} else {
		turnRESTFetchFailAt = time.Time{} // 拉取成功清除退避
		turnRESTCred = cred
	}
	c := turnRESTCred
	turnRESTMu.Unlock()
	return c
}

// isLoopbackHost 判断主机名是否为 loopback（IPv4/IPv6 loopback 或 localhost）。
// 用于 TURN REST 明文 http 的安全边界：仅 loopback 允许明文（本机调试）。
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
