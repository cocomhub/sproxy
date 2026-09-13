// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/otp"
)

// ---- 测试装配 ----

// newRealTOTPServer 启动 force_totp=true 的真实 TCP 黑盒测试服务器（httptest.NewServer，
// RemoteAddr 天然回环，U2 首注册可达）。可注入显式 Ring / CredentialStore；限频器注入
// 高阈值防连发干扰（与 register_handler_test.go newRegisterHandlersCfg 同法）。
//
// 返回 URL、cfgPtr、*Handlers（S4 交叉断言走生产 h.getRole(ak)，消除测试私有转写与
// 生产逻辑漂移风险）与 ring（白盒透视登录条目等）。
func newRealTOTPServer(t *testing.T, mod func(*Config), ring *accesskey.Ring, store *accesskey.CredentialStore) (string, *atomic.Pointer[Config], *accesskey.Ring) {
	t.Helper()
	if ring == nil {
		ring = accesskey.NewRing()
	}
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.ChunkSize = 4 << 10
	cfg.LogLevel = "error"
	cfg.Registration.ForceTOTP = true
	if mod != nil {
		mod(cfg)
	}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	opts := RegisterRoutesOpts{
		Mux:             http.NewServeMux(),
		CfgPtr:          &cfgPtr,
		Version:         "totp-e2e",
		BuildAt:         "test",
		Logger:          testLogger(),
		CredentialRing:  ring,
		CredentialStore: store,
		TotpRateLimit:   1000000,
		LoginRateLimit:  1000000,
	}
	h := RegisterRoutes(t.Context(), opts)
	t.Cleanup(func() { _ = h.Close() })
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(ts.Close)
	return ts.URL, &cfgPtr, ring
}

// newRealTOTPServerH 是带 *Handlers 的装配变体（S4 / getRole 断言走生产 h.getRole(ak)）。
// 其余语义与 newRealTOTPServer 完全一致；多数测试仍用 URL 版，仅需要生产 getRole 或
// 白盒 Handlers 字段的测试用本变体。
func newRealTOTPServerH(t *testing.T, mod func(*Config), ring *accesskey.Ring, store *accesskey.CredentialStore) (*Handlers, string, *accesskey.Ring) {
	t.Helper()
	if ring == nil {
		ring = accesskey.NewRing()
	}
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.ChunkSize = 4 << 10
	cfg.LogLevel = "error"
	cfg.Registration.ForceTOTP = true
	if mod != nil {
		mod(cfg)
	}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	opts := RegisterRoutesOpts{
		Mux:             http.NewServeMux(),
		CfgPtr:          &cfgPtr,
		Version:         "totp-e2e-h",
		BuildAt:         "test",
		Logger:          testLogger(),
		CredentialRing:  ring,
		CredentialStore: store,
		TotpRateLimit:   1000000,
		LoginRateLimit:  1000000,
	}
	h := RegisterRoutes(t.Context(), opts)
	t.Cleanup(func() { _ = h.Close() })
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(ts.Close)
	return h, ts.URL, ring
}

// newNoCredentialTOTPClient 构造用于公开端点的显式无凭据客户端（M14：sendNoAuth 短路
// 签名——TOTP 注册/登录/nonce 端点不挂 authMiddleware，带过期配置签名头反而会被拒）。
func newNoCredentialTOTPClient(t *testing.T, url string) *client.FileClient {
	t.Helper()
	return client.NewFileClient(url, client.WithSendNoAuth(true))
}

// totpCodeNow 用 pkg/otp 在当前时刻复算 TOTP 动态码（模拟 GA）。
func totpCodeNow(t *testing.T, base32Secret string) string {
	t.Helper()
	return totpCodeAt(t, base32Secret, time.Now())
}

// totpCodeAt 用 pkg/otp 在指定时刻复算 TOTP 动态码（模拟 GA）。
func totpCodeAt(t *testing.T, base32Secret string, now time.Time) string {
	t.Helper()
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(base32Secret)
	if err != nil {
		t.Fatalf("base32 解码失败: %v", err)
	}
	code, cerr := otp.NewTOTP(secret).Code(now)
	if cerr != nil {
		t.Fatalf("totp code: %v", cerr)
	}
	return code
}

// wrongTotpCode 返回一个与当前时刻真实 code 不同的 6 位码（排除极小概率相同）。
func wrongTotpCode(t *testing.T, base32Secret string) string {
	t.Helper()
	real := totpCodeNow(t, base32Secret)
	for _, c := range []string{"000000", "111111", "222222"} {
		if c != real {
			return c
		}
	}
	return "333333"
}

// hexEncode 返回给定字节的 hex 字符串。
func hexEncode(b []byte) string {
	return fmt.Sprintf("%x", b)
}

// registerRespH 是 register 响应统一解析结构（简单/TOTP 分支公共字段）。
type registerRespH struct {
	AK           string `json:"ak"`
	Admin        bool   `json:"admin"`
	Base32Secret string `json:"base32_secret"`
}

// doRegisterToURL 用裸 HTTP 对整个 register 端点发请求（httptest 天然回环 RemoteAddr）。
func doRegisterToURL(t *testing.T, url, owner string) (int, registerRespH) {
	t.Helper()
	body := "{}"
	if owner != "" {
		body = `{"owner":"` + owner + `"}`
	}
	resp, err := http.Post(url+"/api/credentials/register", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("register POST: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out registerRespH
	_ = json.Unmarshal(data, &out) // 非 JSON 响应（如 403）→ out 保持零值
	return resp.StatusCode, out
}

// registerHTTP 是 doRegisterToURL 的并发安全/无 t.Helper 版本：返回 error 而非内部
// t.Fatalf，供子 goroutine 安全调用（子 goroutine 内零 Fatal 路径，汇总到主 goroutine）。
func registerHTTP(url, owner string) (int, registerRespH, error) {
	body := "{}"
	if owner != "" {
		body = `{"owner":"` + owner + `"}`
	}
	resp, err := http.Post(url+"/api/credentials/register", "application/json", strings.NewReader(body))
	if err != nil {
		return 0, registerRespH{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out registerRespH
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out, nil
}

// ---- 1. TOTP 注册→登录→session 全链路 ----

// TestTOTPLogin_FullChain 是 4B-2 的端到端黑盒收口（补 task⑨ 未覆盖的真实客户端
// SDK 层）：
//
//	register（force_totp=true，回环）拿 ak+base32_secret
//	→ pkg/otp 复算 TOTP 动态码（模拟 GA）
//	→ POST /api/credentials/nonce 签 nonce
//	→ POST /api/credentials/login 用 (ak, nonce, code) 拿 session 三件套
//	→ accesskey.DeriveTOTPWrapKey(code, ak, nonce) + DecryptSecretKind(KindTOTPWrap)
//	  解出明文 session SK（client.LoginTOTP 内部完成）
//	→ 用真实 FileClient（WithAccessKey(ak, hex(sessionSK)) + WithAccessKeyID(skeyID)）
//	  请求 GET /api/files → 200（空文件表；全套走 ConfigSigner 验签 → RingAuthenticator）。
//
// 与 task⑨ handler 级测试（serveLogin/signedGetWithSKID）互补：本测试只依赖
// pkg/client + pkg/accesskey + pkg/otp 的公开面，不经任何 server 内测试 helper，
// 验证「登录端点签发 → 客户端解封 → 会话请求 200」这条完整链不被装配细节偏差破坏。
func TestTOTPLogin_FullChain(t *testing.T) {
	hh, url, _ := newRealTOTPServerH(t, nil, nil, nil)
	noAuth := newNoCredentialTOTPClient(t, url)

	reg, err := noAuth.RegisterTOTP(context.Background(), "full-chain")
	if err != nil {
		t.Fatalf("RegisterTOTP: %v", err)
	}
	if reg.AK == "" || reg.Base32Secret == "" {
		t.Fatalf("注册响应缺字段: %+v", reg)
	}

	nonce, err := noAuth.RequestTOTPNonce(context.Background())
	if err != nil {
		t.Fatalf("RequestTOTPNonce: %v", err)
	}
	if nonce.Nonce == "" {
		t.Fatalf("nonce 为空")
	}

	login, err := noAuth.LoginTOTP(context.Background(), reg.AK, nonce.Nonce, totpCodeNow(t, reg.Base32Secret), "")
	if err != nil {
		t.Fatalf("LoginTOTP: %v", err)
	}
	if len(login.SessionSK) != 32 || login.SessionSkeyID == "" {
		t.Fatalf("session 凭据异常: skid=%q sklen=%d", login.SessionSkeyID, len(login.SessionSK))
	}

	sess := client.NewFileClient(url,
		client.WithAccessKey(reg.AK, hexEncode(login.SessionSK)),
		client.WithAccessKeyID(login.SessionSkeyID),
	)
	files, lerr := sess.List(context.Background())
	if lerr != nil {
		t.Fatalf("session 会话 GET /api/files: %v", lerr)
	}
	if len(files) != 0 {
		t.Errorf("新 AK 根目录文件表应为空, got %d 项", len(files))
	}

	// S4：响应 admin（= AddRegistration granted）与生产 getRole(ak) 交叉断言一致（Fix 2）。
	// FullChain 主链保持 URL-only 装配，S4 的 getRole 走 hh.getRole 而非测试私有转写。
	if !reg.Admin {
		t.Errorf("首注册 granted=false, want true")
	}
	if got := hh.getRole(reg.AK); got != "admin" {
		t.Errorf("getRole(%q) = %q, want admin（S4 一致性：granted=true ↔ 生产 getRole==admin）", reg.AK, got)
	}
}

// TestTOTPLogin_FullChain_Subdir 验证会话凭据下子目录列表可达（含 subdir 查询串的
// 签名 canonical）：Mkdir + List(subdir) → 200。
func TestTOTPLogin_FullChain_Subdir(t *testing.T) {
	url, _, _ := newRealTOTPServer(t, nil, nil, nil)
	noAuth := newNoCredentialTOTPClient(t, url)
	reg, err := noAuth.RegisterTOTP(context.Background(), "subdir-chain")
	if err != nil {
		t.Fatalf("RegisterTOTP: %v", err)
	}
	nonce, err := noAuth.RequestTOTPNonce(context.Background())
	if err != nil {
		t.Fatalf("RequestTOTPNonce: %v", err)
	}
	login, err := noAuth.LoginTOTP(context.Background(), reg.AK, nonce.Nonce, totpCodeNow(t, reg.Base32Secret), "")
	if err != nil {
		t.Fatalf("LoginTOTP: %v", err)
	}
	sess := client.NewFileClient(url,
		client.WithAccessKey(reg.AK, hexEncode(login.SessionSK)),
		client.WithAccessKeyID(login.SessionSkeyID),
	)
	if err := sess.Mkdir(context.Background(), "totp-dir"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if files, err := sess.List(context.Background(), "totp-dir"); err != nil {
		t.Fatalf("List(subdir) 应 200: %v", err)
	} else if len(files) != 0 {
		t.Errorf("新子目录文件表应为空, got %d 项", len(files))
	}
}

// ---- 2. 公开端点豁免 / 错误签名 / U2 回环门禁（e2e 级）----

// TestTOTPLogin_RegisterPublicExempt_BadSignature 验证 register 是公开端点：携带
// **错误签名** Authorization 头（ring 中不存在的随机 AK/SK）的 register 请求仍可达
// （200）——register 不挂 authMiddleware，错误签名头被忽略而非 401（公开豁免断言）。
func TestTOTPLogin_RegisterPublicExempt_BadSignature(t *testing.T) {
	url, _, _ := newRealTOTPServer(t, nil, nil, nil)
	req, err := http.NewRequest(http.MethodPost, url+"/api/credentials/register", strings.NewReader(`{"owner":"signed-wrong"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	signRequestEntry(req, "ak-zz-totpwrong00000000", testEntryID("ak-zz-totpwrong00000000"), strings.Repeat("ab", 32))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("register with bad signature: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("register（错误签名）status = %d, want 200（公开豁免） (body=%s)", resp.StatusCode, data)
	}
}

// TestTOTP_E2E_LoopbackFirstRegOK 验证真实 TCP（httptest.RemoteAddr=127.0.0.1）
// 回环首注册 → 200 admin=true（U2 正向 e2e 断言）。
func TestTOTP_E2E_LoopbackFirstRegOK(t *testing.T) {
	url, _, _ := newRealTOTPServer(t, nil, nil, nil)
	st, p := doRegisterToURL(t, url, "")
	if st != http.StatusOK {
		t.Fatalf("回环首注册 status = %d, want 200", st)
	}
	if !p.Admin {
		t.Errorf("回环首注册 admin=false, want true")
	}
	if p.Base32Secret == "" {
		t.Errorf("回环首注册 base32_secret 为空")
	}
}

// TestTOTP_RemoteFirstRegistration_Forbidden 验证 U2 负向 e2e 形态：远程来源首注册
// （无 admin）→ 403。真实 TCP 下无法伪造 RemoteAddr，改为 handler 直发注入
// remoteNonLoop 来源（复用 register_handler_test.go 基建），断言拒绝且不落盘凭据。
func TestTOTP_RemoteFirstRegistration_Forbidden(t *testing.T) {
	h, _, ring := newRegisterTestServer(t, func(c *Config) { c.Registration.ForceTOTP = true })
	st, body := serveRegister(t, h, remoteNonLoop, []byte(`{}`))
	if st != http.StatusForbidden {
		t.Fatalf("远程首注册 status = %d, want 403（body=%s）", st, body)
	}
	if ring.Len() != 0 {
		t.Errorf("拒绝后 ring 不应新增凭据, len=%d", ring.Len())
	}
	if !strings.Contains(string(body), "回环") {
		t.Errorf("403 body 应提示回环限制: %s", body)
	}
}

// ---- 3. 注册错误路径 / 角色 ----

// TestRegisterTOTP_Disabled 验证 registration.disable=true → register 403（I4b）：
// 已有 admin 的存量部署语义（回环亦 403）。
func TestRegisterTOTP_Disabled(t *testing.T) {
	url, _, _ := newRealTOTPServer(t, func(c *Config) { c.Registration.Disable = true }, nil, nil)
	st, _ := doRegisterToURL(t, url, "")
	if st != http.StatusForbidden {
		t.Fatalf("disable=true 注册 status = %d, want 403", st)
	}
}

// TestRegisterTOTP_FirstIsAdmin 验证 TOTP 首注册 granted==admin 且 getRole==admin
// （S4 交叉断言，getRole 走生产 *Handlers.getRole(ak)——Fix 2 消除测试私有转写漂移）；
// 第二注册 admin=false 且 getRole==user。
func TestRegisterTOTP_FirstIsAdmin(t *testing.T) {
	hh, url, _ := newRealTOTPServerH(t, nil, nil, nil)

	st1, p1 := doRegisterToURL(t, url, "first")
	if st1 != http.StatusOK {
		t.Fatalf("首注册 status = %d, want 200", st1)
	}
	if !p1.Admin {
		t.Errorf("首注册 granted=false, want true")
	}
	if got := hh.getRole(p1.AK); got != "admin" {
		t.Errorf("getRole(首) = %q, want admin（S4 一致性：granted=true ↔ 生产 getRole==admin）", got)
	}

	st2, p2 := doRegisterToURL(t, url, "second")
	if st2 != http.StatusOK {
		t.Fatalf("第二注册 status = %d, want 200", st2)
	}
	if p2.Admin {
		t.Errorf("第二注册 granted=true, want false")
	}
	if got := hh.getRole(p2.AK); got != "user" {
		t.Errorf("getRole(第二) = %q, want user（S4 一致性）", got)
	}
}

// TestRegisterTOTP_ConcurrentFirstAdmin 验证 D2 原子性：并发双注册 → 恰一个 admin
// （AddRegistration 写锁内原子判定，杜绝并发双 admin 与竞态落盘）。
// 注意：注册端点限频器 registerLimiter 在 RegisterRoutes 硬编码 5/min，无 opts 覆盖
// 点。并发双注册仅需 2 个并发 request，远低于阈值，不会触发 429——D2 原子性由
// AddRegistration 写锁保证（service 侧）。为最大确定性仍串行化本测试的并发发送？——
// 不：并发语义正是被测对象。8 个并发请求瞬时全部落在 5/min 窗口内时部分请求会
// 429。因此并发注册数收敛到 2（避免 429 干扰断言），仍验证「恰一 admin」。
func TestRegisterTOTP_ConcurrentFirstAdmin(t *testing.T) {
	url, _, ring := newRealTOTPServer(t, nil, nil, nil)

	const n = 2 // 并发双注册（D2）：恰一 admin；>5 并发会误触 5/min 注册限频 → 429
	results := make([]registerRespH, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// 子 goroutine 零 Fatal：registerHTTP 返回 error，由主 goroutine 汇总
			// t.Fatalf（Fix 3——doRegisterToURL 内含 t.Helper/t.Fatalf 快失败分支，
			// 并发内绝不可用）。
			st, p, rerr := registerHTTP(url, fmt.Sprintf("conc-%d", i))
			if rerr != nil {
				errs[i] = rerr
				return
			}
			if st != http.StatusOK {
				errs[i] = fmt.Errorf("status=%d", st)
				return
			}
			results[i] = p
		}(i)
	}
	wg.Wait()

	admins := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("并发注册 #%d: %v", i, errs[i])
		}
		if results[i].Admin {
			admins++
		}
	}
	if admins != 1 {
		t.Fatalf("并发注册 admin 数 = %d, want 1", admins)
	}
	// 全 ring 唯一 admin（D2）。
	adminCount := 0
	for _, k := range ring.Snapshot() {
		if k.Role == accesskey.RoleAdmin {
			adminCount++
		}
	}
	if adminCount != 1 {
		t.Fatalf("ring admin 总数 = %d, want 1", adminCount)
	}
}

// ---- 4. 登录错误路径 ----

// TestTOTPLogin_WrongCode 验证错误动态码 → 401。
func TestTOTPLogin_WrongCode(t *testing.T) {
	url, _, _ := newRealTOTPServer(t, nil, nil, nil)
	noAuth := newNoCredentialTOTPClient(t, url)
	reg, err := noAuth.RegisterTOTP(context.Background(), "wrong")
	if err != nil {
		t.Fatalf("RegisterTOTP: %v", err)
	}
	nonce, err := noAuth.RequestTOTPNonce(context.Background())
	if err != nil {
		t.Fatalf("RequestTOTPNonce: %v", err)
	}
	_, err = noAuth.LoginTOTP(context.Background(), reg.AK, nonce.Nonce, wrongTotpCode(t, reg.Base32Secret), "")
	if err == nil {
		t.Fatalf("错误动态码登录必须失败")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("错误动态码错误应含 HTTP 401, got %v", err)
	}
}

// TestTOTPLogin_NonceReplay 验证 nonce 单次消费（D5）：同一 nonce 二次登录（正确 code）
// → 拒绝（401），不得成功。
func TestTOTPLogin_NonceReplay(t *testing.T) {
	url, _, _ := newRealTOTPServer(t, nil, nil, nil)
	noAuth := newNoCredentialTOTPClient(t, url)
	reg, err := noAuth.RegisterTOTP(context.Background(), "replay")
	if err != nil {
		t.Fatalf("RegisterTOTP: %v", err)
	}
	nonce, err := noAuth.RequestTOTPNonce(context.Background())
	if err != nil {
		t.Fatalf("RequestTOTPNonce: %v", err)
	}
	code := totpCodeNow(t, reg.Base32Secret)
	if _, err := noAuth.LoginTOTP(context.Background(), reg.AK, nonce.Nonce, code, ""); err != nil {
		t.Fatalf("首次登录: %v", err)
	}
	if _, err := noAuth.LoginTOTP(context.Background(), reg.AK, nonce.Nonce, code, ""); err == nil {
		t.Fatalf("nonce 重放登录必须失败（单次消费）")
	}
}

// TestLogin_UnknownLoginType_400 验证未知 login_type → 400（M8/M16）。
func TestTOTPLogin_UnknownLoginType_400(t *testing.T) {
	url, _, _ := newRealTOTPServer(t, nil, nil, nil)
	noAuth := newNoCredentialTOTPClient(t, url)
	reg, err := noAuth.RegisterTOTP(context.Background(), "lt")
	if err != nil {
		t.Fatalf("RegisterTOTP: %v", err)
	}
	nonce, err := noAuth.RequestTOTPNonce(context.Background())
	if err != nil {
		t.Fatalf("RequestTOTPNonce: %v", err)
	}
	_, err = noAuth.LoginTOTP(context.Background(), reg.AK, nonce.Nonce, totpCodeNow(t, reg.Base32Secret), "mobile")
	if err == nil {
		t.Fatalf("未知 login_type 登录必须失败")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("未知 login_type 错误应含 HTTP 400, got %v", err)
	}
}

// ---- 5. session 过期的签名 401 ----

// TestSessionExpiry_Request401 验证 session 条目过期后签名 → 401（ring now 注入前进到
// ExpiresAt 之后）。用真实 FileClient 会话（ConfigSigner 完整签名路径）请求 GET /api/files。
//
// 注意：ring 时钟前进后 RingAuthenticator 用 `time.Now()` 验签普通请求——但签名过期
// （sproxysig.Verify 用 time.Now 判 Exp）不影响本场景：session 条目过期由 GetEntry 的
// aliveLocked（ring 注入时钟）判定 → 条目不可用 → 401，先于任何签名时间窗检查。
func TestSessionExpiry_Request401(t *testing.T) {
	fixed := time.Now()
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return fixed }
	ring := accesskey.NewRing(clock)
	url, _, _ := newRealTOTPServer(t, nil, ring, nil)
	noAuth := newNoCredentialTOTPClient(t, url)
	reg, err := noAuth.RegisterTOTP(context.Background(), "expiry")
	if err != nil {
		t.Fatalf("RegisterTOTP: %v", err)
	}
	nonce, err := noAuth.RequestTOTPNonce(context.Background())
	if err != nil {
		t.Fatalf("RequestTOTPNonce: %v", err)
	}
	code := totpCodeAt(t, reg.Base32Secret, fixed)
	login, err := noAuth.LoginTOTP(context.Background(), reg.AK, nonce.Nonce, code, "")
	if err != nil {
		t.Fatalf("LoginTOTP: %v", err)
	}
	sess := client.NewFileClient(url,
		client.WithAccessKey(reg.AK, hexEncode(login.SessionSK)),
		client.WithAccessKeyID(login.SessionSkeyID),
	)
	if _, err := sess.List(context.Background()); err != nil {
		t.Fatalf("session 未过期 GET /api/files 应 200: %v", err)
	}
	// ring now 前进到 session ExpiresAt 之后 → 条目非存活 → 签名 401。
	mu.Lock()
	fixed = login.SessionExpiresAt.Add(time.Second)
	mu.Unlock()
	if _, err := sess.List(context.Background()); err == nil {
		t.Fatalf("过期 session 签名 GET /api/files 应 401")
	} else if !strings.Contains(err.Error(), "401") {
		t.Errorf("过期 session 错误应含 HTTP 401, got %v", err)
	}
}

// ---- 6. M16：无 TOTPSecret 登录 404 + 持久化重启（R2-N5）----

// TestLogin_NoTOTPSecret_404 验证无 TOTPSecret 的 AK 登录 → 404（M16，I1）：ring 中
// 存在但无 TOTPSecret 的 AK（经本 test 构造）走完整 login 端点（有效 nonce）→ 404。
//
// 场景：简单模式注册（force_totp=false）产生的 AK 无 TOTPSecret；其登录端点仍存在。
// 但本文件 newRealTOTPServer 默认 ForceTOTP=true 只影响 register 分支——登录端点对
// 无 TOTPSecret AK 的 404 判定与注册模式无关。此处复用 handler 级既有覆盖（task⑨
// TestLogin_NoTOTPSecret_404 已断言 404 + 两场景），本文件仅补「经真实 HTTP 链路」
// 的窄断言：用简单模式注册一个 AK，然后走完整 login（无 TOTPSecret）→ 404。
func TestTOTPLogin_NoTOTPSecret_404(t *testing.T) {
	url, _, _ := newRealTOTPServer(t, func(c *Config) { c.Registration.ForceTOTP = false }, nil, nil)
	// 简单模式注册：响应含 sk/skey_id，无 base32_secret/TOTPSecret。
	resp, err := http.Post(url+"/api/credentials/register", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("简单模式注册 POST: %v", err)
	}
	var simple struct {
		AK string `json:"ak"`
	}
	if derr := json.NewDecoder(resp.Body).Decode(&simple); derr != nil {
		resp.Body.Close()
		t.Fatalf("解析简单注册响应: %v", derr)
	}
	resp.Body.Close()
	if simple.AK == "" {
		t.Fatalf("简单注册响应缺 AK")
	}
	// 该 AK 无 TOTPSecret → 完整登录（有效 nonce + 任意 code）→ 404，且响应体为
	// 固定文案 `{"error":"not found"}`（M16；handler register_handler.go 887 行唯一
	// 404 分支 sendJSONResponse(w, map[string]any{"error": "not found"}, 404)）。
	noAuth := newNoCredentialTOTPClient(t, url)
	nonce, err := noAuth.RequestTOTPNonce(context.Background())
	if err != nil {
		t.Fatalf("RequestTOTPNonce: %v", err)
	}
	_, err = noAuth.LoginTOTP(context.Background(), simple.AK, nonce.Nonce, "000000", "")
	if err == nil {
		t.Fatalf("无 TOTPSecret 登录必须失败")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("无 TOTPSecret 登录错误应含 HTTP 404, got %v", err)
	}

	// Fix 1（Minor 1）：直接发裸 HTTP login（有效 nonce）读响应体，断言固定 404 文案。
	nonce2, err := noAuth.RequestTOTPNonce(context.Background())
	if err != nil {
		t.Fatalf("RequestTOTPNonce(#2): %v", err)
	}
	loginBody, _ := json.Marshal(map[string]any{"ak": simple.AK, "nonce": nonce2.Nonce, "code": "000000"})
	lresp, lerr := http.Post(url+"/api/credentials/login", "application/json", strings.NewReader(string(loginBody)))
	if lerr != nil {
		t.Fatalf("login POST: %v", lerr)
	}
	defer lresp.Body.Close()
	ldata, _ := io.ReadAll(lresp.Body)
	if lresp.StatusCode != http.StatusNotFound {
		t.Fatalf("无 TOTPSecret login 裸 HTTP status = %d, want 404 (body=%s)", lresp.StatusCode, ldata)
	}
	var ldec struct {
		Error string `json:"error"`
	}
	if jerr := json.Unmarshal(ldata, &ldec); jerr != nil {
		t.Fatalf("404 响应体非 JSON: %v (%s)", jerr, ldata)
	}
	if ldec.Error != "not found" {
		t.Errorf("404 响应体 error = %q, want %q（M16 固定文案）", ldec.Error, "not found")
	}
}

// TestAdminRole_TOTPPersistAfterRestart 验证 TOTP 账号持久化闭环（R2-N5）：
//   - store.Save → 新 Ring + Replace → getRole==admin（Role 持久化重启后仍 admin）；
//   - 且用重启后 Key.TOTPSecret 复算 code 走完整登录端点 → 200（TOTP secret 持久化，
//     登录闭环）。
func TestAdminRole_TOTPPersistAfterRestart(t *testing.T) {
	tmpDir := t.TempDir()
	store := accesskey.NewCredentialStore(filepath.Join(tmpDir, "anonymous", "meta"))
	ring1 := accesskey.NewRing()
	url1, _, _ := newRealTOTPServer(t, nil, ring1, store)
	noAuth1 := newNoCredentialTOTPClient(t, url1)
	reg, err := noAuth1.RegisterTOTP(context.Background(), "persist")
	if err != nil {
		t.Fatalf("RegisterTOTP(v1): %v", err)
	}

	// 注册已持久化（register 内 persistCredentials；store 非 nil）。
	keys, err := store.Load()
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("重启后 store 应有 1 个 key, got %d", len(keys))
	}

	// 模拟重启：新 Ring + Replace（bootstrapCredentials 等价重载）。
	ring2 := accesskey.NewRing()
	if rerr := ring2.Replace(keys); rerr != nil {
		t.Fatalf("ring2.Replace: %v", rerr)
	}
	k2, ok := ring2.GetKey(reg.AK)
	if !ok {
		t.Fatalf("重启后 AK %q 不存在", reg.AK)
	}
	if len(k2.TOTPSecret) != 20 {
		t.Fatalf("重启后 TOTPSecret 长度 = %d, want 20", len(k2.TOTPSecret))
	}
	// getRole 走生产方法（Fix 2）：先经 bootstrapCredentials 等价装配出新服务器
	// （newRealTOTPServer 内部 RegisterRoutes→bootstrapCredentials 从 store 载入 ring2
	// 快照重建），再用 h.getRole 断言重启后角色仍 admin。
	hh2, url2, _ := newRealTOTPServerH(t, nil, ring2, store)
	if got := hh2.getRole(reg.AK); got != "admin" {
		t.Errorf("重启后 getRole(生产 h.getRole) = %q, want admin", got)
	}

	// 用重启后 secret 的 base32 形式复算 code 走完整登录端点 → 200（R2-N5 闭环）。
	b32 := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(k2.TOTPSecret)
	noAuth2 := newNoCredentialTOTPClient(t, url2)
	nonce, err := noAuth2.RequestTOTPNonce(context.Background())
	if err != nil {
		t.Fatalf("RequestTOTPNonce(v2): %v", err)
	}
	login, lerr := noAuth2.LoginTOTP(context.Background(), reg.AK, nonce.Nonce, totpCodeNow(t, b32), "")
	if lerr != nil {
		t.Fatalf("重启后完整登录: %v", lerr)
	}
	if len(login.SessionSK) != 32 {
		t.Errorf("重启后登录 session SK 长度 = %d, want 32", len(login.SessionSK))
	}
}
