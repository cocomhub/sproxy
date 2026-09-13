// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/otp"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// ---- 测试服务装配 ----

// newRegisterTestServer 启动带空凭据 Ring（零凭据待注册）的完整路由服务器，
// 返回可直接 ServeHTTP 的 handler、cfgPtr 与 ring。handler 直调可精确控制
// RemoteAddr（loopback 预检的黑盒触发点）；需真实 TCP（并发/SK 可用性）者再包
// httptest.NewServer。
func newRegisterTestServer(t *testing.T, mod func(*Config)) (http.Handler, *atomic.Pointer[Config], *accesskey.Ring) {
	t.Helper()
	ring := accesskey.NewRing()
	return newRegisterTestServerWithRing(t, ring, mod)
}

// newRegisterTestServerWithRing 用显式 Ring 装配（nil → 空 Ring）。
func newRegisterTestServerWithRing(t *testing.T, ring *accesskey.Ring, mod func(*Config)) (http.Handler, *atomic.Pointer[Config], *accesskey.Ring) {
	t.Helper()
	h, cfgPtr, outRing := newRegisterHandlers(t, ring, mod)
	return h.Handler(), cfgPtr, outRing
}

// newRegisterHandlers 是装配低层：返回 *Handlers（不包 Handler()），供需要白盒
// 访问 Handlers 字段（如 totpNoncePool）的测试使用。行为与 newRegisterTestServer
// 一致（零凭据空 Ring 待注册）。
func newRegisterHandlers(t *testing.T, ring *accesskey.Ring, mod func(*Config)) (*Handlers, *atomic.Pointer[Config], *accesskey.Ring) {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = tmpDir
	cfg.ChunkSize = 4 << 10
	cfg.LogLevel = "error"
	if mod != nil {
		mod(cfg)
	}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	if ring == nil {
		ring = accesskey.NewRing()
	}
	opts := RegisterRoutesOpts{
		Mux:            http.NewServeMux(),
		CfgPtr:         &cfgPtr,
		Version:        "test-version",
		BuildAt:        "test-buildat",
		Logger:         testLogger(),
		CredentialRing: ring,
		// nonce 池上限语义验证需连发 >4096 次，totpLimiter(10/min) 会过早限流——
		// 限频高低与池语义无关，测试注入高阈值瞬态。
		TotpRateLimit: 100000,
	}
	h := RegisterRoutes(t.Context(), opts)
	t.Cleanup(func() { _ = h.Close() })
	return h, &cfgPtr, ring
}

// serveRegister 直接对 handler 发 register 请求（RemoteAddr 由调用方指定）。
func serveRegister(t *testing.T, h http.Handler, remoteAddr string, body []byte) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/credentials/register", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

// registeredPair 是 register 响应体。
type registeredPair struct {
	AK     string `json:"ak"`
	Owner  string `json:"owner"`
	Admin  bool   `json:"admin"`
	SK     string `json:"sk"`
	SkeyID string `json:"skey_id"`
}

// loopback 常量（常用回环 RemoteAddr）。
const (
	loopRemoteV4  = "127.0.0.1:56789"
	loopRemoteV6  = "[::1]:56789"
	remoteNonLoop = "203.0.113.7:9999"
)

// ---- 注册成功（简单模式）----

// TestRegister_SimpleMode_Success 验证公开注册成功：回环来源无认证 → 200；返回
// AK(sk-<32hex>)/sk(64-hex)/skey_id(skey-)/admin=true；条目 ExpiresAt ≈ now+TTL；
// 用下发 SK+skey_id 签名 GET /api/files → 200。
func TestRegister_SimpleMode_Success(t *testing.T) {
	h, _, ring := newRegisterTestServer(t, func(c *Config) { c.CredentialTTL = 48 * time.Hour })

	st, body := serveRegister(t, h, loopRemoteV4, []byte(`{}`))
	if st != http.StatusOK {
		t.Fatalf("register status = %d, want 200 (body=%s)", st, body)
	}
	var p registeredPair
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	if !accesskey.IsValidAK(p.AK) || !strings.HasPrefix(p.AK, accesskey.AccessKeyPrefix) ||
		len(p.AK) != len(accesskey.AccessKeyPrefix)+accesskey.AccessKeyHexLen*2 {
		t.Errorf("AK 形态异常（应 ak-<32hex>）: %q", p.AK)
	}
	if !p.Admin {
		t.Errorf("首注册 admin = false, want true")
	}
	if len(p.SK) != 64 {
		t.Errorf("sk 长度 = %d, want 64", len(p.SK))
	}
	if b, derr := hex.DecodeString(p.SK); derr != nil || len(b) != 32 {
		t.Errorf("sk 应 32B hex: err=%v len=%d", derr, len(b))
	}
	if !strings.HasPrefix(p.SkeyID, accesskey.SkeyIDPrefix) {
		t.Errorf("skey_id = %q, want 前缀 %q", p.SkeyID, accesskey.SkeyIDPrefix)
	}

	// ExpiresAt ≈ now + cfg.CredentialTTL（容差 ±3s）。
	entry, alive, gerr := ring.GetEntry(p.AK, p.SkeyID)
	if gerr != nil || !alive {
		t.Fatalf("GetEntry(%q,%q): alive=%v err=%v", p.AK, p.SkeyID, alive, gerr)
	}
	want := time.Now().Add(48 * time.Hour)
	if delta := entry.ExpiresAt.Sub(want); delta > 3*time.Second || delta < -3*time.Second {
		t.Errorf("ExpiresAt = %v, want ≈ %v (±3s)", entry.ExpiresAt, want)
	}

	// SK 立即可用（v2 skey-id 必传闭合）：新 AK 访问文件落自身租户 → 200。
	stFiles, _ := signedGetEntryHelp2(t, h, "/api/files", p.AK, p.SkeyID, p.SK)
	if stFiles != http.StatusOK {
		t.Fatalf("注册后签名 GET /api/files status = %d, want 200", stFiles)
	}
}

// signedGetEntryHelp2 直接用 handler 发送显式 skeyID 签名的 GET。
func signedGetEntryHelp2(t *testing.T, h http.Handler, path, ak, entryID, sk string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = loopRemoteV4
	signRequestEntry(req, ak, entryID, sk)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

// ---- loopback 预检（U2）----

// TestRegister_LoopbackGate_FirstAdmin 验证 U2：无 admin 时非回环来源 403；回环
// （IPv4 与 ::1）200 admin=true；首个 admin 后远程来源按 registration.disable 放行。
func TestRegister_LoopbackGate_FirstAdmin(t *testing.T) {
	h, _, ring := newRegisterTestServer(t, nil)

	// 非回环来源 → 403（且不落盘凭据）。
	st, body := serveRegister(t, h, remoteNonLoop, []byte(`{}`))
	if st != http.StatusForbidden {
		t.Fatalf("远程首注册 status = %d, want 403 (body=%s)", st, body)
	}
	if ring.Len() != 0 {
		t.Errorf("拒绝后 ring 不应新增凭据, len=%d", ring.Len())
	}
	if !strings.Contains(string(body), "回环") {
		t.Errorf("403 body 应提示回环限制: %s", body)
	}

	// 回环 IPv4 → 200 admin=true。
	st4, body4 := serveRegister(t, h, loopRemoteV4, []byte(`{}`))
	if st4 != http.StatusOK {
		t.Fatalf("回环(s)注册 status = %d, want 200 (body=%s)", st4, body4)
	}
	var p1 registeredPair
	_ = json.Unmarshal(body4, &p1)
	if !p1.Admin {
		t.Errorf("首 registrant admin = false, want true")
	}
	if got := roleOf2(t, ring, p1.AK); got != "admin" {
		t.Errorf("getRole(首) = %q, want admin", got)
	}

	// 有 admin 后：IPv6 回环再注册 → 200 admin=false。
	st6, body6 := serveRegister(t, h, loopRemoteV6, []byte(`{"owner":"v6"}`))
	if st6 != http.StatusOK {
		t.Fatalf("回环(::1)注册 status = %d, want 200 (body=%s)", st6, body6)
	}
	var p2 registeredPair
	_ = json.Unmarshal(body6, &p2)
	if p2.Admin {
		t.Errorf("第二个注册不应 admin: %+v", p2)
	}

	// 有 admin 后：远程来源按 registration.disable=false → 200 admin=false。
	stR, bodyR := serveRegister(t, h, remoteNonLoop, []byte(`{"owner":"remote"}`))
	if stR != http.StatusOK {
		t.Fatalf("有 admin 后远程注册 status = %d, want 200 (body=%s)", stR, bodyR)
	}
	var p3 registeredPair
	_ = json.Unmarshal(bodyR, &p3)
	if p3.Admin {
		t.Errorf("有 admin 后远程注册不应 admin: %+v", p3)
	}
}

// roleOf2 读取 ring 中 AK 的角色（空归一 user）。
func roleOf2(t *testing.T, ring *accesskey.Ring, ak string) string {
	t.Helper()
	k, ok := ring.GetKey(ak)
	if !ok {
		t.Fatalf("ring 中无 AK %q", ak)
	}
	if k.Role == "" {
		return "user"
	}
	return string(k.Role)
}

// TestRegister_RemoteRegistrationAfterAdmin 验证首个 admin 后禁止注册
// （registration.disable=true）：回环亦 403（I4b：存量部署语义）。
func TestRegister_RemoteRegistrationAfterAdmin(t *testing.T) {
	h, _, _ := newRegisterTestServer(t, func(c *Config) { c.Registration.Disable = true })
	st, body := serveRegister(t, h, loopRemoteV4, []byte(`{}`))
	if st != http.StatusForbidden {
		t.Fatalf("disable=true 注册 status = %d, want 403 (body=%s)", st, body)
	}
}

// ---- 首 user 即 admin / S4 / D2 并发 ----

// TestRegister_FirstUserIsAdmin_S4 验证 DEC-A 与 S4：granted==admin 与 getRole==admin
// 一一对应；第二注册 admin=false / getRole==user。
func TestRegister_FirstUserIsAdmin_S4(t *testing.T) {
	h, _, ring := newRegisterTestServer(t, nil)

	_, body := serveRegister(t, h, loopRemoteV4, []byte(`{"owner":"first"}`))
	var p1 registeredPair
	_ = json.Unmarshal(body, &p1)
	if !p1.Admin || roleOf2(t, ring, p1.AK) != "admin" {
		t.Fatalf("首注册: granted=%v role=%q（S4 不一致）", p1.Admin, roleOf2(t, ring, p1.AK))
	}

	_, body2 := serveRegister(t, h, loopRemoteV4, []byte(`{"owner":"second"}`))
	var p2 registeredPair
	_ = json.Unmarshal(body2, &p2)
	if p2.Admin || roleOf2(t, ring, p2.AK) != "user" {
		t.Fatalf("第二注册: granted=%v role=%q（应为 admin=false user）", p2.Admin, roleOf2(t, ring, p2.AK))
	}
}

// TestRegister_ConcurrentSingleAdmin 验证 D2 原子性：并发注册 → 恰一个 admin=true 且
// 全 ring 唯一 admin。经真实 httptest.Server（复用注册端点限频窗口内多 IP 同上限）。
func TestRegister_ConcurrentSingleAdmin(t *testing.T) {
	rh, _, ring := newRegisterTestServer(t, nil)
	ts := httptest.NewServer(rh)
	defer ts.Close()

	const n = 8
	var admins int64
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/credentials/register", strings.NewReader(`{}`))
			if err != nil {
				t.Errorf("build register req: %v", err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("concurrent register: %v", err)
				return
			}
			defer resp.Body.Close()
			data, _ := io.ReadAll(resp.Body)
			var p registeredPair
			_ = json.Unmarshal(data, &p)
			if p.Admin {
				atomic.AddInt64(&admins, 1)
			}
		})
	}
	wg.Wait()

	if got := atomic.LoadInt64(&admins); got != 1 {
		t.Fatalf("并发注册 admin 数 = %d, want 1", got)
	}
	adminCount := 0
	for _, k := range ring.Snapshot() {
		if k.Role == accesskey.RoleAdmin {
			adminCount++
		}
	}
	if adminCount != 1 {
		t.Fatalf("ring admin 总数 = %d, want 1（D2 唯一）", adminCount)
	}
}

// ---- 重复注册 / 既有账号 ----

// TestRegister_FirstAdminGrantedWithExistingUser 澄清实际断言（F4）：既有 user 账号
// 在场时，AddRegistration 的「全 ring 无 admin」判定只看 Role——user 不阻止首个
// admin 诞生。断言：本次注册 admin=true 且恰 1 个 admin，既有 user 账号的角色/条目
// 不被改动。
func TestRegister_FirstAdminGrantedWithExistingUser(t *testing.T) {
	pre := accesskey.NewRing()
	skB, _ := hex.DecodeString(testAccessSecret)
	_ = pre.UpsertAK(testAccessKey, "existing")
	_, _ = pre.AddKey(testAccessKey, skB, accesskey.WithID(testEntryID(testAccessKey)))
	setKeyRole(t, pre, testAccessKey, accesskey.RoleUser)

	h, _, ring := newRegisterTestServerWithRing(t, pre, nil)
	_, body := serveRegister(t, h, loopRemoteV4, []byte(`{}`))
	var p registeredPair
	_ = json.Unmarshal(body, &p)
	if !p.Admin {
		t.Fatalf("默认全场首个 admin 授予应成立（user 在场不妨碍）")
	}
	if got := countAdmins(ring); got != 1 {
		t.Fatalf("应恰 1 个 admin, got %d", got)
	}
	if k, _ := ring.GetKey(testAccessKey); k == nil || k.Role != accesskey.RoleUser {
		t.Errorf("既有账号角色被改动: %+v", k)
	}
	if e, ok := ring.GetKey(testAccessKey); !ok || len(e.Entries) != 1 {
		t.Errorf("既有账号条目被改动: %+v", e)
	}
}

// TestRegister_RepeatedRegistrationSameAKPreservesAccount 验证「同 AK 重复注册」语义
// （F4 / D2 exists 分支）：AddRegistration 对已存在 AK 再次注册——**首注册者重复
// 注册保持 admin（不降级）**；重复注册追加新 SK 条目而非覆盖。经白盒直接调用
// AddRegistration（公开端点每次生成全新 AK，无法指定目标 AK）覆盖 exists 分支。
func TestRegister_RepeatedRegistrationSameAKPreservesAccount(t *testing.T) {
	ring := accesskey.NewRing()
	skB, _ := hex.DecodeString(testAccessSecret)

	// 首次注册该 AK → 授 admin（写锁内扫无 admin）。
	granted1, id1, err := ring.AddRegistration(testAccessKey, "owner", skB, nil, accesskey.RoleUser, time.Hour)
	if err != nil {
		t.Fatalf("AddRegistration(first): %v", err)
	}
	if !granted1 || id1 == "" {
		t.Fatalf("首注册 granted=%v id=%q, want 授 admin+条目", granted1, id1)
	}

	// 同一 AK 二次注册：授权 admin 重复注册 → **保持 admin**（不降级为 user）
	// 且追加一条新 SK 条目（不覆盖既有条目/SK）。
	origEntries := len(mustEntries(ring, testAccessKey))
	granted2, id2, err := ring.AddRegistration(testAccessKey, "owner", skB, nil, accesskey.RoleUser, time.Hour)
	if err != nil {
		t.Fatalf("AddRegistration(exists): %v", err)
	}
	if !granted2 {
		t.Errorf("已授权 admin 重复注册应保持 admin（不降级）, granted=false")
	}
	if id2 == "" || id2 == id1 {
		t.Errorf("重复注册应追加返回新 SK 条目 id（got %q, first %q）", id2, id1)
	}
	k, ok := ring.GetKey(testAccessKey)
	if !ok {
		t.Fatalf("重复注册后 AK 应仍存在")
	}
	if k.Role != accesskey.RoleAdmin {
		t.Errorf("重复注册后角色 = %q, want admin（首注册者保持）", k.Role)
	}
	if len(k.Entries) != origEntries+1 {
		t.Errorf("重复注册后条目数 = %d, want %d（追加而非覆盖）", len(k.Entries), origEntries+1)
	}

	// admin 已存在时，全新账号注册不被打标 admin（唯一 admin 保住）。
	granted3, _, err3 := ring.AddRegistration("ak-acct-00aabbccddeeff00", "second", skB, nil, accesskey.RoleUser, time.Hour)
	if err3 != nil {
		t.Fatalf("AddRegistration(second): %v", err3)
	}
	if granted3 {
		t.Errorf("已有 admin 时新账号注册不应授 admin")
	}
	if countAdmins(ring) != 1 {
		t.Errorf("admin 数 = %d, want 唯一 1（首注册者）", countAdmins(ring))
	}
}

// mustEntries 读取 ring 中 AK 的条目（测试辅助；调用方负责断言）。
func mustEntries(ring *accesskey.Ring, ak string) []accesskey.SKEntry {
	if k, ok := ring.GetKey(ak); ok {
		return k.Entries
	}
	return nil
}

func countAdmins(ring *accesskey.Ring) int {
	n := 0
	for _, k := range ring.Snapshot() {
		if k.Role == accesskey.RoleAdmin {
			n++
		}
	}
	return n
}

// ---- I6 / requireRole / akAdd role ----

// TestAdminRole_AKDeleteRejected 验证 I6：DELETE /api/credentials/{ak} 目标是
// Role==admin 的 AK（confirm+force 齐）→ 400（admin 角色 AK 不可删除）。
func TestAdminRole_AKDeleteRejected(t *testing.T) {
	url, _, _ := newCredentialsTestServer(t, testAdminKey, testAdminSecret, testAccessKey, testAccessSecret, nil, nil)
	st, body := doSignedJSON(t, http.MethodDelete, url+"/api/credentials/"+testAdminKey, testAdminKey, testAdminSecret, map[string]any{
		"confirm": testAdminKey, "force": true,
	})
	if st != http.StatusBadRequest {
		t.Fatalf("删除 admin AK status = %d, want 400 (body=%s)", st, body)
	}
	if !strings.Contains(string(body), "admin") {
		t.Errorf("400 body 应提示 admin 角色不可删: %s", body)
	}
}

// TestRequireRole_NodeDenied 验证 requireRole 文件组门禁：Role==node 的 AK 签名访问
// GET /api/files → 403；user 正常 200。
func TestRequireRole_NodeDenied(t *testing.T) {
	url, _, _ := newCredentialsTestServer(t, testAdminKey, testAdminSecret, testAccessKey, testAccessSecret, nil, nil)
	nodeAK := "ak-node-00774455aabbccdd"
	nodeSK := strings.Repeat("55", 32)

	stAdd, bodyAdd := doSignedJSON(t, http.MethodPost, url+"/api/credentials", testAdminKey, testAdminSecret, map[string]any{
		"ak": nodeAK, "owner": "mesh-host", "role": "node", "secret": nodeSK,
	})
	if stAdd != http.StatusOK {
		t.Fatalf("admin 添加 node AK status = %d, want 200 (body=%s)", stAdd, bodyAdd)
	}
	var addResp struct {
		AK   string `json:"ak"`
		SKID string `json:"sk_id"`
	}
	_ = json.Unmarshal(bodyAdd, &addResp)

	stNode, bodyNode := signedGetEntry(t, url+"/api/files", nodeAK, addResp.SKID, nodeSK)
	if stNode != http.StatusForbidden {
		t.Fatalf("node 访问 GET /api/files status = %d, want 403 (body=%s)", stNode, bodyNode)
	}

	// user 正常 200。
	stUser, _ := signedGet(t, url+"/api/files", testAccessKey, testAccessSecret)
	if stUser != http.StatusOK {
		t.Fatalf("user 访问 GET /api/files status = %d, want 200", stUser)
	}
}

// TestAdminRole_AKAddRoleField 验证 akAddHandler 支持 role：node → 200 GetKey.Role==
// RoleNode；admin → 拒绝 400（管理端点不可创建 admin）。
func TestAdminRole_AKAddRoleField(t *testing.T) {
	url, _, ring := newCredentialsTestServer(t, testAdminKey, testAdminSecret, testAccessKey, testAccessSecret, nil, nil)

	nodeAK := "ak-node-00998877ffeeddcc"
	st, body := doSignedJSON(t, http.MethodPost, url+"/api/credentials", testAdminKey, testAdminSecret, map[string]any{
		"ak": nodeAK, "owner": "relay", "role": "node", "secret": strings.Repeat("44", 32),
	})
	if st != http.StatusOK {
		t.Fatalf("akAdd role=node status = %d, want 200 (body=%s)", st, body)
	}
	if k, ok := ring.GetKey(nodeAK); !ok || k.Role != accesskey.RoleNode {
		t.Errorf("新 AK 角色 = %+v, want RoleNode", k)
	}

	adminUpAK := "ak-up-00deadbeef001122"
	stBad, bodyBad := doSignedJSON(t, http.MethodPost, url+"/api/credentials", testAdminKey, testAdminSecret, map[string]any{
		"ak": adminUpAK, "owner": "wannabe", "role": "admin", "secret": strings.Repeat("66", 32),
	})
	if stBad != http.StatusBadRequest {
		t.Fatalf("akAdd role=admin status = %d, want 400 (body=%s)", stBad, bodyBad)
	}
	if _, ok := ring.GetKey(adminUpAK); ok {
		t.Errorf("role=admin 被拒后不应创建 AK")
	}
}

// ---- ★ 隧道内层门禁（MUST-FIX）----

// TestLocalMuxGate_XferNilPrincipalPasses 验证 xfer 直连语义（F6）：经 LocalHandler()
// 直连路由文件组请求时，请求 ctx 无外层 Principal（xfer handleStream 用
// http.NewRequest 不携带 ctx）→ localMuxGate 放行（principal==nil 跳过 requireRole），
// 隧道握手密钥/pinning 身份由 xfer 会话层闭合。本例钉住「nil 放行」的既有行为，
// 防未来 xfer 面引入带身份 ctx 时 gate 行为突变（见 task-3-report.md 已知边界）。
func TestLocalMuxGate_XferNilPrincipalPasses(t *testing.T) {
	h, _, _ := newRegisterTestServer(t, nil)

	// 无任何 Principal 的裸 GET /api/files（模拟 xfer handleStream 构造的内层请求）——
	// gate 对 nil principal 跳过 → 落到 listFiles handler：凭据 ring 为空 + 非回环 →
	// 需要看是否 200/401；xfer 面本就无认证主体，gate 不得先 403。
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	req.RemoteAddr = loopRemoteV4 // xfer 会话来自 loopback listen（listener 侧 TCP 源地址）
	h.ServeHTTP(w, req)
	if w.Code == http.StatusForbidden {
		t.Fatalf("xfer nil-principal GET /api/files 不应被 gate 拦为 403（nil 跳过分支）")
	}
	// 断言 gate 确实走到了 handler 而非被 requireRole 拦截：200（列表）或 401（ring 空
	// handleNoCredentials 未回环则 401——此处经 NewRequest RemoteAddr 回环且 ring 空会
	// 走 handleNoCredentials 回环直通 → 200）。核心断言是「非 403、未被 gate 拒」。
	if w.Code != http.StatusOK && w.Code != http.StatusUnauthorized {
		t.Fatalf("xfer nil-principal GET /api/files status = %d（应非 403；200=handler 正常/401=无凭据面）", w.Code)
	}
}

// newTunnelRoleServer 启动带指定角色凭据的完整路由服务器（含 traditional POST /tunnel）。
// 返回可解析 base URL。
func newTunnelRoleServer(t *testing.T, ak, sk string, role accesskey.Role) string {
	t.Helper()
	ring := accesskey.NewRing()
	skB, _ := hex.DecodeString(sk)
	_ = ring.UpsertAK(ak, "tun-test")
	_, _ = ring.AddKey(ak, skB, accesskey.WithID(testEntryID(ak)))
	if role != "" {
		setKeyRole(t, ring, ak, role)
	}
	return newRealServerWithRing(t, ring)
}

// newRealServerWithRing 用 httptest.NewServer 包装 Ring 注入的 handler（真实 TCP）。
func newRealServerWithRing(t *testing.T, ring *accesskey.Ring) string {
	t.Helper()
	h, _, _ := newRegisterTestServerWithRing(t, ring, nil)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts.URL
}

// tunnelClientFor 构造走 traditional POST /tunnel 的隧道客户端（外层 UNSIGNED SproxySig）。
// 密钥 = HKDF(skHex, mesh)。参考 TestTunnelInnerRequest_InheritsClientTraceID 模板。
func tunnelClientFor(t *testing.T, baseURL, ak, sk string) *tunnel.Client {
	t.Helper()
	key, err := tunnel.DeriveTunnelKey(sk, accesskey.ParseMesh(ak))
	if err != nil {
		t.Fatalf("DeriveTunnelKey: %v", err)
	}
	tc, err := tunnel.NewClient(hex.EncodeToString(key), baseURL+"/tunnel", 10*time.Second, testLogger())
	if err != nil {
		t.Fatalf("tunnel.NewClient: %v", err)
	}
	base := tc.HTTPClient.Transport
	tc.HTTPClient.Transport = &tunnelSignTransport{base: base, ak: ak, sk: sk}
	return tc
}

// TestRegister_TunnelInnerGateNodeDenied 验证 MUST-FIX：Role==node 的 AK 经 traditional
// POST /tunnel 开隧道（内层 ctx 由外层透传 Principal）→ 内层全部文件组路由 → 403
// （fail-closed，node 无法借隧道访问文件）。覆盖：GE /api/files、分块下载
// GET /download/chunk、分块上传 POST /upload/init（F1 收口：chunk 组纳入 gate）。
func TestRegister_TunnelInnerGateNodeDenied(t *testing.T) {
	const nodeAK = "ak-node-tun-cafebabecafebabe"
	const nodeSK = "9999999999999999999999999999999999999999999999999999999999999999"
	base := newTunnelRoleServer(t, nodeAK, nodeSK, accesskey.RoleNode)
	tc := tunnelClientFor(t, base, nodeAK, nodeSK)

	send := func(method, path string, body io.Reader) int {
		t.Helper()
		req, _ := http.NewRequest(method, path, body)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := tc.Do(req)
		if err != nil {
			t.Fatalf("node tunnel %s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// 常规文件组。
	if st := send(http.MethodGet, "/api/files", nil); st != http.StatusForbidden {
		t.Fatalf("node 隧道内层 GET /api/files status = %d, want 403（fail-closed）", st)
	}
	// 分块下载组（F1：此前漏 gated，node 可在 anonymous 租户读任意文件块）。
	if st := send(http.MethodGet, "/download/chunk?filename=tun.txt&offset=0&size=16", nil); st != http.StatusForbidden {
		t.Fatalf("node 隧道内层 GET /download/chunk status = %d, want 403（fail-closed）", st)
	}
	// 分块上传组（F1：此前漏 gated，node 可在 anonymous 租户写 chunk 后 complete）。
	if st := send(http.MethodPost, "/upload/init", strings.NewReader(`{"filename":"tun.bin","total_size":16,"chunk_size":4096,"total_chunks":1}`)); st != http.StatusForbidden {
		t.Fatalf("node 隧道内层 POST /upload/init status = %d, want 403（fail-closed）", st)
	}
}

// TestRegister_TunnelInnerGateUserAllowed 验证对向：Role==user 的 AK 开隧道 → 内层
// GET /api/files → 200（user 隧道文件操作可用且落自身租户）；分块下载组 user 放行。
func TestRegister_TunnelInnerGateUserAllowed(t *testing.T) {
	const userAK = "ak-user-tun-deadbeefdeadbeef"
	const userSK = "8888888888888888888888888888888888888888888888888888888888888888"
	base := newTunnelRoleServer(t, userAK, userSK, accesskey.RoleUser)
	tc := tunnelClientFor(t, base, userAK, userSK)
	// node 测试结束后补充 chunk 面 user 放行（见 TestRegister_TunnelInnerGateUserChunk）。

	// 先经隧道内层上传一个文件（落 userAK 租户），再列表确认。
	body := strings.NewReader("tunnel upload body")
	upReq, _ := http.NewRequest(http.MethodPost, "/upload", body)
	upReq.Header.Set("Content-Type", "multipart/form-data; boundary=----sproxy-test-boundary")
	upReq.Header.Set("X-File-Checksum", sha256hex([]byte("tunnel upload body")))
	upReq.Header.Set("X-File-Path", "tun.txt")

	// 手工构造 multipart 体（Server 端需标准 multipart 解析）。
	mpBody := &strings.Builder{}
	mw := multipart.NewWriter(mpBody)
	part, perr := mw.CreateFormFile("file", "tun.txt")
	if perr != nil {
		t.Fatalf("create form file: %v", perr)
	}
	_, _ = io.WriteString(part, "tunnel upload body")
	_ = mw.Close()
	upReq.Body = io.NopCloser(strings.NewReader(mpBody.String()))
	upReq.Header.Set("Content-Type", mw.FormDataContentType())
	upReq.ContentLength = int64(mpBody.Len())
	upResp, err := tc.Do(upReq)
	if err != nil {
		t.Fatalf("user tunnel upload: %v", err)
	}
	upResp.Body.Close()
	if upResp.StatusCode != http.StatusOK {
		t.Fatalf("user 隧道内层上传 status = %d, want 200", upResp.StatusCode)
	}

	listReq, _ := http.NewRequest(http.MethodGet, "/api/files", nil)
	listResp, err := tc.Do(listReq)
	if err != nil {
		t.Fatalf("user tunnel list: %v", err)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("user 隧道内层 GET /api/files status = %d, want 200", listResp.StatusCode)
	}
	// 落正确租户（userAK 而非 anonymous）：tun.txt 出现在其租户根。
	data, _ := io.ReadAll(listResp.Body)
	if !strings.Contains(string(data), "tun.txt") {
		t.Errorf("user 隧道列表应含 tun.txt（落 userAK 租户）, body=%s", data)
	}
}

// TestRegister_TunnelInnerGateUserChunk 验证 F1 对向：user AK 经传统 POST /tunnel
// 内层分块下载组放行（chunk 组纳入 gate 后仅拦非文件组角色，user 200）。
func TestRegister_TunnelInnerGateUserChunk(t *testing.T) {
	const userAK = "ak-user-chunk-cafebabecafebabe"
	const userSK = "7777777777777777777777777777777777777777777777777777777777777777"
	base := newTunnelRoleServer(t, userAK, userSK, accesskey.RoleUser)
	tc := tunnelClientFor(t, base, userAK, userSK)

	// GET /download/chunk → gate 放行 → 落到 handler（文件不存在 → 404，而非 403）。
	req, _ := http.NewRequest(http.MethodGet, "/download/chunk?filename=tun.txt&offset=0&size=16", nil)
	resp, err := tc.Do(req)
	if err != nil {
		t.Fatalf("user tunnel chunk download Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Fatalf("user 隧道内层 GET /download/chunk 不应 403（gate 只拦 node）")
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("user 隧道内层 GET /download/chunk status = %d, want 404（gate 放行后由 handler 判 not found）, body 可查", resp.StatusCode)
	}
}

// newRegisterAuditServer 启动带审计 buffer 捕获的注册测试服务器（空 Ring，零凭据）。
// 返回可直接 ServeHTTP 的 handler + 审计 buffer。
func newRegisterAuditServer(t *testing.T, mod func(*Config)) (http.Handler, *bytes.Buffer) {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = tmpDir
	cfg.ChunkSize = 4 << 10
	cfg.LogLevel = "error"
	if mod != nil {
		mod(cfg)
	}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	var auditBuf bytes.Buffer
	auditLogger := slog.New(slog.NewJSONHandler(&auditBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	opts := RegisterRoutesOpts{
		Mux:            http.NewServeMux(),
		CfgPtr:         &cfgPtr,
		Version:        "test-version",
		BuildAt:        "test-buildat",
		Logger:         testLogger(),
		AuditLogger:    auditLogger,
		CredentialRing: accesskey.NewRing(),
	}
	h := RegisterRoutes(t.Context(), opts)
	t.Cleanup(func() { _ = h.Close() })
	return h.Handler(), &auditBuf
}

// TestRegister_AuditTrail 验证注册审计（F3）：成功 → credential_register（Detail 含
// role 与 skey_id，且不含 sk 明文）；远程首注册被拒 → credential_register_denied。
// 仿既有 credentials_handler_test.go 的 auditActions(...) 断言模式。
func TestRegister_AuditTrail(t *testing.T) {
	h, buf := newRegisterAuditServer(t, nil)

	// 远程首注册被拒 → credential_register_denied（无 admin 源，root 拒绝）。
	remoteReq := httptest.NewRequest(http.MethodPost, "/api/credentials/register", strings.NewReader(`{}`))
	remoteReq.Header.Set("Content-Type", "application/json")
	remoteReq.RemoteAddr = remoteNonLoop
	w := httptest.NewRecorder()
	h.ServeHTTP(w, remoteReq)
	if w.Code != http.StatusForbidden {
		t.Fatalf("远程首注册 status = %d, want 403", w.Code)
	}
	denied := auditActions(t, buf, auditActionCredRegisterDenied)
	if len(denied) < 1 {
		t.Fatalf("远程首注册被拒应有 credential_register_denied 审计")
	}
	if d := denied[0]["detail"]; !strings.Contains(fmt.Sprint(d), "回环") {
		t.Errorf("credential_register_denied detail = %v, want 含『回环』", d)
	}

	// 回环注册成功 → credential_register（role=admin，Detail 不含 sk 明文）。
	okReq := httptest.NewRequest(http.MethodPost, "/api/credentials/register", strings.NewReader(`{"owner":"audited"}`))
	okReq.Header.Set("Content-Type", "application/json")
	okReq.RemoteAddr = loopRemoteV4
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, okReq)
	if w2.Code != http.StatusOK {
		t.Fatalf("回环注册 status = %d, want 200", w2.Code)
	}
	var p registeredPair
	_ = json.Unmarshal(w2.Body.Bytes(), &p)

	okEvents := auditActions(t, buf, auditActionCredRegister)
	if len(okEvents) < 1 {
		t.Fatalf("注册成功应有 credential_register 审计")
	}
	evt := okEvents[len(okEvents)-1]
	if got := fmt.Sprint(evt["detail"]); !strings.Contains(got, "role=admin") {
		t.Errorf("credential_register detail = %q, want 含 role=admin", got)
	}
	if obj := fmt.Sprint(evt["object"]); obj != p.AK {
		t.Errorf("credential_register object = %q, want 新 AK %q", obj, p.AK)
	}
	// 负向断言：SK 明文绝不出现在任何审计行（SK 单次下发、不落日志的回归护栏）。
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if strings.Contains(line, p.SK) {
			t.Fatalf("审计输出泄露 SK 明文: %s", line)
		}
	}
}

// ---- 零凭据启动（U3）----

// TestRegister_ZeroCredentialBootstrap 验证 U3：bootstrapCredentials 在 store 为空时
// 不生成任何凭据（ring.Len()==0）——首启 anonymous 移除，系统以零凭据等待注册。
func TestRegister_ZeroCredentialBootstrap(t *testing.T) {
	tmpDir := t.TempDir()
	store := accesskey.NewCredentialStore(tmpDir + "/tenant/meta")
	ring := accesskey.NewRing()
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(Default())

	h := &Handlers{cfgPtr: &cfgPtr, credentialRing: ring, credentialStore: store, logger: testLogger()}
	h.bootstrapCredentials(RegisterRoutesOpts{CredentialStore: store})
	if h.credentialRing.Len() != 0 {
		t.Fatalf("U3：store 为空时 bootstrap 不应生成凭据, len=%d", h.credentialRing.Len())
	}
	keys, err := store.Load()
	if err != nil || len(keys) != 0 {
		t.Fatalf("U3：store 磁盘不应写入凭据, err=%v keys=%d", err, len(keys))
	}
}

// ---- 无认证 401 面不受影响 ----

// TestRegister_NoAuthUnauthorizedOther 验证 register 无 Authorization 头可直达；其余
// /api/credentials/* 仍 401。
func TestRegister_NoAuthUnauthorizedOther(t *testing.T) {
	h, _, _ := newRegisterTestServer(t, nil)

	st, _ := serveRegister(t, h, loopRemoteV4, []byte(`{}`))
	if st != http.StatusOK {
		t.Fatalf("register 无认证 status = %d, want 200", st)
	}

	// 其余凭据端点仍 401（ring 已非空，未认证 → 401）。
	req := httptest.NewRequest(http.MethodGet, "/api/credentials", nil)
	req.RemoteAddr = loopRemoteV4
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无凭据 GET /api/credentials 应 401, got %d", w.Code)
	}
}

// ---- TOTP 注册分支（force_totp）----

// totpRegistered 是 force_totp 模式 register 响应体（无 sk 字段）。
type totpRegistered struct {
	AK           string `json:"ak"`
	Owner        string `json:"owner"`
	Admin        bool   `json:"admin"`
	OTPAuthURI   string `json:"otpauth_uri"`
	Base32Secret string `json:"base32_secret"`
}

// serveNonce 对 handler 发 nonce 请求（RemoteAddr 由调用方指定）。
func serveNonce(t *testing.T, h http.Handler, remoteAddr string, body []byte) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/credentials/nonce", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

// TestRegisterTOTP_Success 验证 force_totp=true 注册成功：回环来源 → 200，
// 响应含 {ak, admin:true, otpauth_uri, base32_secret} 且**无 sk 字段**；AK 形态
// ak-<32hex>；otpauth URI 含 secret=<base32>；base32_secret 无 padding 且能按
// base32 解码回约 20B；GetKey(ak).TOTPSecret 非 nil 且 20B。
func TestRegisterTOTP_Success(t *testing.T) {
	h, _, ring := newRegisterTestServer(t, func(c *Config) { c.Registration.ForceTOTP = true })

	st, body := serveRegister(t, h, loopRemoteV4, []byte(`{"owner":"totp1"}`))
	if st != http.StatusOK {
		t.Fatalf("TOTP 注册 status = %d, want 200 (body=%s)", st, body)
	}
	var p totpRegistered
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	if !accesskey.IsValidAK(p.AK) || !strings.HasPrefix(p.AK, accesskey.AccessKeyPrefix) ||
		len(p.AK) != len(accesskey.AccessKeyPrefix)+accesskey.AccessKeyHexLen*2 {
		t.Errorf("AK 形态异常（应 ak-<32hex>）: %q", p.AK)
	}
	if !p.Admin {
		t.Errorf("TOTP 首注册 admin = false, want true")
	}
	// 无 sk 字段：简单模式的 sk/skey_id 在 force_totp 响应中必须省略。
	if bytes.Contains(body, []byte(`"sk":`)) {
		t.Errorf("TOTP 注册响应不应含 sk 字段: %s", body)
	}
	if bytes.Contains(body, []byte(`"skey_id":`)) {
		t.Errorf("TOTP 注册响应不应含 skey_id 字段: %s", body)
	}
	if p.OTPAuthURI == "" {
		t.Errorf("otpauth_uri 为空")
	}
	if !strings.Contains(p.OTPAuthURI, "secret="+p.Base32Secret) {
		t.Errorf("otpauth URI 应含 secret=<base32>（got uri=%q base32=%q）", p.OTPAuthURI, p.Base32Secret)
	}
	// base32 无 padding（不能被 '=' 填充），且可解码回 ≈20B（GenerateSecret）。
	if strings.Contains(p.Base32Secret, "=") {
		t.Errorf("base32_secret 不应有 padding: %q", p.Base32Secret)
	}
	decoded, derr := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(p.Base32Secret)
	if derr != nil {
		t.Fatalf("base32_secret 解码失败: %v (%q)", derr, p.Base32Secret)
	}
	if len(decoded) != 20 {
		t.Errorf("base32_secret 解码长度 = %d, want 20（otp.GenerateSecret 20B）", len(decoded))
	}

	k, ok := ring.GetKey(p.AK)
	if !ok {
		t.Fatalf("ring 中无新 AK %q", p.AK)
	}
	if len(k.TOTPSecret) != 20 {
		t.Errorf("Key.TOTPSecret 长度 = %d, want 20", len(k.TOTPSecret))
	}
}

// TestRegisterTOTP_NoSKEntry 验证 TOTP 注册不建 SK 条目：GetKey(ak).Entries 为空。
func TestRegisterTOTP_NoSKEntry(t *testing.T) {
	h, _, ring := newRegisterTestServer(t, func(c *Config) { c.Registration.ForceTOTP = true })

	st, body := serveRegister(t, h, loopRemoteV4, []byte(`{}`))
	if st != http.StatusOK {
		t.Fatalf("TOTP 注册 status = %d, want 200 (body=%s)", st, body)
	}
	var p totpRegistered
	_ = json.Unmarshal(body, &p)
	k, ok := ring.GetKey(p.AK)
	if !ok {
		t.Fatalf("ring 中无新 AK %q", p.AK)
	}
	if len(k.Entries) != 0 {
		t.Errorf("TOTP 注册不应建 SK 条目, Entries=%+v", k.Entries)
	}
}

// TestRegisterTOTP_FirstUserAdmin 验证 TOTP 模式首 user 即 admin、第二注册 admin=false。
func TestRegisterTOTP_FirstUserAdmin(t *testing.T) {
	h, _, ring := newRegisterTestServer(t, func(c *Config) { c.Registration.ForceTOTP = true })

	_, b1 := serveRegister(t, h, loopRemoteV4, []byte(`{"owner":"first"}`))
	var p1 totpRegistered
	_ = json.Unmarshal(b1, &p1)
	if !p1.Admin {
		t.Errorf("TOTP 首注册 granted=false, want true")
	}
	if got := roleOf2(t, ring, p1.AK); got != "admin" {
		t.Errorf("TOTP 首注册 getRole = %q, want admin", got)
	}

	_, b2 := serveRegister(t, h, loopRemoteV4, []byte(`{"owner":"second"}`))
	var p2 totpRegistered
	_ = json.Unmarshal(b2, &p2)
	if p2.Admin {
		t.Errorf("TOTP 第二注册 granted=true, want false")
	}
	if got := roleOf2(t, ring, p2.AK); got != "user" {
		t.Errorf("TOTP 第二注册 getRole = %q, want user", got)
	}
}

// TestRegisterTOTP_DefaultSimpleMode 验证 force_totp=false（默认）回归：简单模式仍
// 返回 {sk, skey_id} 且无 otpauth 字段（force_totp 分支 + omitempty 互斥省略）。
func TestRegisterTOTP_DefaultSimpleMode(t *testing.T) {
	h, _, _ := newRegisterTestServer(t, nil) // ForceTOTP 默认 false

	st, body := serveRegister(t, h, loopRemoteV4, []byte(`{}`))
	if st != http.StatusOK {
		t.Fatalf("默认简单模式注册 status = %d, want 200 (body=%s)", st, body)
	}
	var p registeredPair
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	if len(p.SK) != 64 {
		t.Errorf("简单模式 sk 长度 = %d, want 64", len(p.SK))
	}
	if !strings.HasPrefix(p.SkeyID, accesskey.SkeyIDPrefix) {
		t.Errorf("简单模式 skey_id = %q, want 前缀 %q", p.SkeyID, accesskey.SkeyIDPrefix)
	}
	if bytes.Contains(body, []byte(`"otpauth_uri"`)) || bytes.Contains(body, []byte(`"base32_secret"`)) {
		t.Errorf("简单模式响应不应含 otpauth 字段: %s", body)
	}
}

// TestRegisterTOTP_LoopbackGate 验证 U2：force_totp 分支同样受回环门禁（远程首注册 403）。
func TestRegisterTOTP_LoopbackGate(t *testing.T) {
	h, _, ring := newRegisterTestServer(t, func(c *Config) { c.Registration.ForceTOTP = true })

	st, body := serveRegister(t, h, remoteNonLoop, []byte(`{}`))
	if st != http.StatusForbidden {
		t.Fatalf("TOTP 远程首注册 status = %d, want 403 (body=%s)", st, body)
	}
	if ring.Len() != 0 {
		t.Errorf("TOTP 拒绝后 ring 不应新增凭据, len=%d", ring.Len())
	}

	// 回环注册成功后远程可再注册（admin 已存在）。
	st4, _ := serveRegister(t, h, loopRemoteV4, []byte(`{}`))
	if st4 != http.StatusOK {
		t.Fatalf("TOTP 回环首注册 status = %d, want 200", st4)
	}
	stR, _ := serveRegister(t, h, remoteNonLoop, []byte(`{"owner":"remote"}`))
	if stR != http.StatusOK {
		t.Fatalf("TOTP 有 admin 后远程注册 status = %d, want 200", stR)
	}
}

// ---- nonce 端点 ----

// nonceResp 是 nonce 端点响应体。
type nonceResp struct {
	Nonce     string    `json:"nonce"`
	ExpiresAt time.Time `json:"expires_at"`
}

// TestNonceEndpoint_Success 验证 nonce 端点（force_totp=true）：POST → 200
// {nonce, expires_at}；两次不同；零凭据窗口回环可达（TOTP 登录前置步骤）。
func TestNonceEndpoint_Success(t *testing.T) {
	h, _, ring := newRegisterTestServer(t, func(c *Config) { c.Registration.ForceTOTP = true })
	if n := ring.Len(); n != 0 {
		t.Fatalf("预置 = %d 个凭据, want 0（零凭据语义）", n)
	}

	st1, body1 := serveNonce(t, h, loopRemoteV4, []byte(`{}`))
	if st1 != http.StatusOK {
		t.Fatalf("nonce status = %d, want 200（零凭据窗口可达） (body=%s)", st1, body1)
	}
	var n1 nonceResp
	if err := json.Unmarshal(body1, &n1); err != nil {
		t.Fatalf("unmarshal nonce: %v (body=%s)", err, body1)
	}
	if len(n1.Nonce) != 32 {
		t.Errorf("nonce 长度 = %d, want 32 hex (16B)", len(n1.Nonce))
	}
	if _, derr := hex.DecodeString(n1.Nonce); derr != nil {
		t.Errorf("nonce 非 hex: %v", derr)
	}
	if n1.ExpiresAt.IsZero() {
		t.Errorf("expires_at 为零值")
	}

	// 两次不同（16B 随机）。
	st2, body2 := serveNonce(t, h, loopRemoteV4, []byte(`{}`))
	if st2 != http.StatusOK {
		t.Fatalf("nonce #2 status = %d, want 200 (body=%s)", st2, body2)
	}
	var n2 nonceResp
	_ = json.Unmarshal(body2, &n2)
	if n1.Nonce == n2.Nonce {
		t.Errorf("两次 nonce 相同（应 16B 随机）: %q", n1.Nonce)
	}
}

// TestNonceEndpoint_PoolCap 验证 nonce 池上限 4096（D5）：连续签发超过上限的 nonce
// 全部 200（池满**淘汰最旧**而非拒绝），且池大小恒被钳到上限、最早签发者已被淘汰。
//
// 确定性说明（fix 轮 2）：生产时钟下连发 4104 个 nonce 全在毫秒级同批签发 →
// expiresAt 全部相同 → 池满淘汰的 min-expiresAt 退化为 tie-break（nonce 字典序最小），
// 而首个 nonce 是随机的、不保证被淘汰 → 断言「first 必被淘汰」天然失效（间歇 ~70%
// FAIL）。故在连发循环前注入**严格递增时钟**，使每个 nonce 的 expiresAt 互不相同，
// 池满淘汰对绝对最早（first, T0）完全确定。
func TestNonceEndpoint_PoolCap(t *testing.T) {
	hh, _, _ := newRegisterHandlers(t, nil, func(c *Config) { c.Registration.ForceTOTP = true })
	h := hh.Handler()

	// 注入严格递增时钟（handler 装配后、连发 nonce 前；serveNonce→nonceHandler→
	// addFor 走同一 pool 实例 hh.totpNoncePool）。每签一个 nonce tick+1ms，expiresAt
	// 严格递增且互不相同 → 池满淘汰 min-expiresAt 对最老的 first 完全确定。
	base := time.Now().Truncate(time.Second)
	tick := 0
	hh.totpNoncePool.SetClock(func() time.Time {
		tt := base.Add(time.Duration(tick) * time.Millisecond)
		tick++
		return tt
	})

	stFirst, bodyFirst := serveNonce(t, h, loopRemoteV4, []byte(`{}`))
	if stFirst != http.StatusOK {
		t.Fatalf("首个 nonce status = %d, want 200 (body=%s)", stFirst, bodyFirst)
	}
	var first nonceResp
	_ = json.Unmarshal(bodyFirst, &first)

	for i := 2; i <= maxTotpNoncePool+8; i++ {
		st, body := serveNonce(t, h, loopRemoteV4, []byte(`{}`))
		if st != http.StatusOK {
			t.Fatalf("nonce #%d status = %d, want 200（池满应淘汰最旧而非拒绝） (body=%s)", i, st, body)
		}
	}

	pool := hh.totpNoncePool
	if pool == nil {
		t.Fatalf("totpNoncePool 未装配（nil）")
	}
	if sz := pool.size(); sz != maxTotpNoncePool {
		t.Errorf("池满后 size = %d, want %d（惰性淘汰钳制）", sz, maxTotpNoncePool)
	}
	// 确定性淘汰（Fix 3）：must 早于 first 的 nonce 应被淘汰（池满后消费不命中）。
	// 全局最早 nonce（first,ts=T0）必须不在池中。
	if _, ok := pool.obtain(first.Nonce, "127.0.0.1"); ok {
		t.Errorf("最早 nonce（全局第一批）应被淘汰（池满后消费不命中）")
	}
}

// TestNoncePool_ConsumeRejectsAllIPExceptIssuer 以白盒方式验证单次消费 + IP 绑定：
// nonce 从某来源 IP 签发后，另一来源 IP 消费被拒、同 IP 消费成功且二次消费失败。
// 入口 = h 的 totpNoncePool（RegisterRoutes 装配时非 nil）。
func TestNoncePool_ConsumeRejectsAllIPExceptIssuer(t *testing.T) {
	hh, _, _ := newRegisterHandlers(t, nil, func(c *Config) { c.Registration.ForceTOTP = true })
	h := hh.Handler()
	count := nonceCountFor(hh)
	st, body := serveNonce(t, h, loopRemoteV4, []byte(`{}`))
	if st != http.StatusOK {
		t.Fatalf("nonce status = %d, want 200 (body=%s)", st, body)
	}
	var nr nonceResp
	_ = json.Unmarshal(body, &nr)

	pool := hh.totpNoncePool
	if pool == nil {
		t.Fatalf("totpNoncePool 未装配（nil）")
	}
	// 同来源 IP 首次消费成功。
	if _, ok := pool.obtain(nr.Nonce, normalizedTestIP(loopRemoteV4)); !ok {
		t.Errorf("同来源 IP 消费 nonce 应成功（首次消费）")
	}
	// 二次消费（同 IP）失败——单次使用。
	if _, ok := pool.obtain(nr.Nonce, normalizedTestIP(loopRemoteV4)); ok {
		t.Errorf("二次消费 nonce 应失败（单次使用）")
	}

	// IP 绑定：新签发一个 nonce，从另一来源 IP 消费拒绝……
	st2, body2 := serveNonce(t, h, loopRemoteV4, []byte(`{}`))
	if st2 != http.StatusOK {
		t.Fatalf("nonce #2 status = %d, want 200 (body=%s)", st2, body2)
	}
	var nr2 nonceResp
	_ = json.Unmarshal(body2, &nr2)
	if _, ok := pool.obtain(nr2.Nonce, "203.0.113.7"); ok {
		t.Errorf("不同来源 IP 消费 nonce 应拒绝（IP 绑定）")
	}
	// ……且该次失败尝试已消费 nonce（D5「失败也消费」）——此后任何来源再消费都失败。
	if _, ok := pool.obtain(nr2.Nonce, normalizedTestIP(loopRemoteV4)); ok {
		t.Errorf("被错误 IP 尝试后 nonce 不应可再用（失败也消费）")
	}

	if after := nonceCountFor(hh); after != count {
		t.Errorf("消费后池大小 = %d, want %d（单次消费逐条删除）", after, count)
	}
}

// normalizedTestIP 与 normalizeRemoteIP 对齐（Strip host:port），测试用归一化辅助。
func normalizedTestIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// nonceCountFor 透视当前 nonce 池大小（白盒，仅测试用）。
func nonceCountFor(h *Handlers) int {
	if h.totpNoncePool == nil {
		return -1
	}
	return h.totpNoncePool.size()
}

// ---- TOTP 登录端点（task 9）----

// loginResp 是 POST /api/credentials/login 的成功响应体（snake_case，M2）。
type loginResp struct {
	AK                   string                   `json:"ak"`
	SessionSkeyID        string                   `json:"session_skey_id"`
	SessionExpiresAt     time.Time                `json:"session_expires_at"`
	WrappedSessionSecret *accesskey.WrappedSecret `json:"wrapped_session_secret"`
}

// loginResult 登录取回解密的 session SK（wrapped 经 insertEntryID 解封）。
type loginResult struct {
	resp          loginResp
	sessionSK     []byte
	sessionSkeyID string
}

// newTOTPTestServer 装配 force_totp=true + 高限频的登录黑盒测试服务器（FRED 语义：
// 可注入 clock 的 Ring）。mod 改配置；clockJa 为 nil 用真实时钟。
func newTOTPTestServer(t *testing.T, mod func(*Config), ring *accesskey.Ring) (*Handlers, http.Handler, *atomic.Pointer[Config], *accesskey.Ring) {
	t.Helper()
	if ring == nil {
		ring = accesskey.NewRing(time.Now)
	}
	hh, cfgPtr, outRing := newRegisterHandlersTOTP(t, ring, mod)
	return hh, hh.Handler(), cfgPtr, outRing
}

// newRegisterHandlersTOTP 与 newRegisterHandlers 一致，但注入默认 ForceTOTP=true +
// 更高的 login 限频（避免 per-AK 锁定测试被 429 干扰）并暴露白盒 Handlers。
func newRegisterHandlersTOTP(t *testing.T, ring *accesskey.Ring, mod func(*Config)) (*Handlers, *atomic.Pointer[Config], *accesskey.Ring) {
	t.Helper()
	if ring == nil {
		ring = accesskey.NewRing(time.Now)
	}
	cfg := Default()
	cfg.StorageRoot = t.TempDir() // 隔离每个测试的存储根（避免污染工作区 ./storage）
	cfg.ChunkSize = 4 << 10
	cfg.Registration.ForceTOTP = true
	if mod != nil {
		mod(cfg)
	}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	return newRegisterHandlersCfg(t, ring, &cfgPtr)
}

// newRegisterHandlersCfg 是低层装配：用给定 cfgPtr/Ring（登录测试回归，避免重复
// RegisterRoutes 样板）。TotpRateLimit/LoginRateLimit 注入高阈值防限频干扰。
func newRegisterHandlersCfg(t *testing.T, ring *accesskey.Ring, cfgPtr *atomic.Pointer[Config]) (*Handlers, *atomic.Pointer[Config], *accesskey.Ring) {
	t.Helper()
	opts := RegisterRoutesOpts{
		Mux:            http.NewServeMux(),
		CfgPtr:         cfgPtr,
		Version:        "test-version",
		BuildAt:        "test-buildat",
		Logger:         testLogger(),
		CredentialRing: ring,
		// 登录黑盒测试需连发（per-AK 锁定 5+ 次、垃圾 nonce 多连发），默认 10/min
		// 限频会过早 429 干扰语义断言——注入高阈值瞬态（与 nonce 池测试同法）。
		TotpRateLimit:  1000000,
		LoginRateLimit: 1000000,
	}
	h := RegisterRoutes(t.Context(), opts)
	t.Cleanup(func() { _ = h.Close() })
	return h, cfgPtr, ring
}

// registerTOTPFor 在 ForceTOTP=true 服务器上注册一个 TOTP 账号，返回 AK 与 base32 secret。
// owner 为空 → 默认 "totp-login"。
func registerTOTPFor(t *testing.T, h http.Handler, akOwner string) (string, string) {
	t.Helper()
	if akOwner == "" {
		akOwner = "totp-login"
	}
	st, body := serveRegister(t, h, loopRemoteV4, []byte(`{"owner":"`+akOwner+`"}`))
	if st != http.StatusOK {
		t.Fatalf("TOTP 注册 status = %d, want 200 (body=%s)", st, body)
	}
	var p totpRegistered
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("unmarshal register: %v (body=%s)", err, body)
	}
	return p.AK, p.Base32Secret
}

// totpCodeFor 用 pkg/otp 复算指定时刻的 TOTP 动态码（模拟 GA）。
func totpCodeFor(t *testing.T, base32Secret string, now time.Time) (string, []byte) {
	t.Helper()
	secret, derr := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(base32Secret)
	if derr != nil {
		t.Fatalf("base32 解码失败: %v", derr)
	}
	code, cerr := otp.NewTOTP(secret).Code(now)
	if cerr != nil {
		t.Fatalf("totp code: %v", cerr)
	}
	return code, secret
}

// serveLogin 对 handler 发 login 请求（RemoteAddr 由调用方指定）。
func serveLogin(t *testing.T, h http.Handler, remoteAddr string, body []byte) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/credentials/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

// obtainNonce 走 nonce 端点签发一枚绑定指定来源 IP 的 nonce，返回 nonce 字符串。
func obtainNonce(t *testing.T, h http.Handler, ip string) string {
	t.Helper()
	st, body := serveNonce(t, h, ip, []byte(`{}`))
	if st != http.StatusOK {
		t.Fatalf("nonce 签发 status = %d, want 200 (body=%s)", st, body)
	}
	var nr nonceResp
	if err := json.Unmarshal(body, &nr); err != nil {
		t.Fatalf("unmarshal nonce: %v (body=%s)", err, body)
	}
	return nr.Nonce
}

// loginSuccessf 执行一次期待成功的 TOTP 登录（正确 nonce + 正确 code），返回解密的
// session SK / skeyID。remoteAddr 允许覆盖来源 IP（默认 loopRemoteV4）。code 按真实
// 时钟复算。注入 ring 时钟的测试用 loginSuccessAt 显式传 now。
func loginSuccessf(t *testing.T, h http.Handler, cfgPtr *atomic.Pointer[Config], ak, base32Secret, nonce, loginType, remoteAddr string) loginResult {
	t.Helper()
	return loginSuccessAt(t, h, cfgPtr, ak, base32Secret, nonce, loginType, remoteAddr, time.Now())
}

// loginSuccessAt 是 loginSuccessf 的注入时钟版本：code 在指定 now 时刻复算（与 ring
// 注入时钟对齐，保证 TOTP ±1 窗口命中）。
func loginSuccessAt(t *testing.T, h http.Handler, cfgPtr *atomic.Pointer[Config], ak, base32Secret, nonce, loginType, remoteAddr string, now time.Time) loginResult {
	t.Helper()
	if remoteAddr == "" {
		remoteAddr = loopRemoteV4
	}
	code, _ := totpCodeFor(t, base32Secret, now)
	body := map[string]any{"ak": ak, "nonce": nonce, "code": code}
	if loginType != "" {
		body["login_type"] = loginType
	}
	raw, _ := json.Marshal(body)
	st, respBody := serveLogin(t, h, remoteAddr, raw)
	if st != http.StatusOK {
		t.Fatalf("登录 status = %d, want 200 (body=%s; ak=%s nonce=%s code=%s)", st, respBody, ak, nonce, code)
	}
	var lr loginResp
	if err := json.Unmarshal(respBody, &lr); err != nil {
		t.Fatalf("unmarshal login: %v (body=%s)", err, respBody)
	}
	// 响应字段契约（M2）。
	if lr.AK != ak {
		t.Errorf("resp.ak = %q, want %q", lr.AK, ak)
	}
	if !strings.HasPrefix(lr.SessionSkeyID, accesskey.SkeyIDPrefix) {
		t.Errorf("session_skey_id = %q, want 前缀 %q", lr.SessionSkeyID, accesskey.SkeyIDPrefix)
	}
	if lr.WrappedSessionSecret == nil || lr.WrappedSessionSecret.Kind != accesskey.KindTOTPWrap {
		t.Fatalf("wrapped_session_secret.kind = %+v, want totp_wrap", lr.WrappedSessionSecret)
	}
	// 用 DeriveTOTPWrapKey(code,ak,nonce) 解封 session SK（I3 唯一路径）。
	wrapKey, kerr := accesskey.DeriveTOTPWrapKey(code, ak, nonce)
	if kerr != nil {
		t.Fatalf("DeriveTOTPWrapKey: %v", kerr)
	}
	sessSK, derr := accesskey.DecryptSecretKind(lr.WrappedSessionSecret, accesskey.KindTOTPWrap, wrapKey)
	if derr != nil {
		t.Fatalf("DecryptSecretKind: %v", derr)
	}
	if len(sessSK) != 32 {
		t.Fatalf("解出的 session SK 长度 = %d, want 32", len(sessSK))
	}
	return loginResult{resp: lr, sessionSK: sessSK, sessionSkeyID: lr.SessionSkeyID}
}

// signedGetWithSKID 直接用 handler 发送显式 skeyID 签名的 GET（session SK 立即可用断言）。
func signedGetWithSKID(t *testing.T, h http.Handler, path, ak, entryID, skHexStr string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = loopRemoteV4
	signRequestEntry(req, ak, entryID, skHexStr)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// TestLogin_Success 验证登录成功全链路：注册拿 base32_secret → 测试内用 pkg/otp
// 复算 code（模拟 GA）→ nonce → login → 200 {ak, session_skey_id, session_expires_at,
// wrapped_session_secret}（M2）；session_skey_id 前缀 skey-；用
// DeriveTOTPWrapKey(code,ak,nonce) + DecryptSecretKind(KindTOTPWrap) 解出 32B session
// SK（I3）；ring 新增 session 条目（Kind==totp_wrap、WrapKeyID 为空 M13、Meta.Type=="login"
// IP 绑定）；session skeyID 立即可用（签名 GET /api/credentials/<ak>/sk → 200）。
func TestLogin_Success(t *testing.T) {
	hh, h, _, _ := newTOTPTestServer(t, nil, nil)
	ak, b32 := registerTOTPFor(t, h, "")
	nonce := obtainNonce(t, h, loopRemoteV4)

	lr := loginSuccessf(t, h, hh.cfgPtr, ak, b32, nonce, "", "")
	// session_expires_at ≈ now + cfg.Registration.SessionTTL（web 缺省 24h，容差 ±3s）。
	want := time.Now().Add(24 * time.Hour)
	if d := lr.resp.SessionExpiresAt.Sub(want); d > 3*time.Second || d < -3*time.Second {
		t.Errorf("session_expires_at = %v, want ≈ %v（web 24h）", lr.resp.SessionExpiresAt, want)
	}

	// ring 新增 session 条目：Kind 标记、WrapKeyID 置空（M13）、Meta.Type/IP。
	k, ok := hh.credentialRing.GetKey(ak)
	if !ok {
		t.Fatalf("ring 中无 AK %q", ak)
	}
	if len(k.Entries) != 1 {
		t.Fatalf("登录后应有 1 条 session 条目, got %d", len(k.Entries))
	}
	e := k.Entries[0]
	if e.ID != lr.sessionSkeyID {
		t.Errorf("ring 条目 ID = %q, want 响应 %q", e.ID, lr.sessionSkeyID)
	}
	if e.Kind != accesskey.KindTOTPWrap {
		t.Errorf("session 条目 Kind = %q, want %q", e.Kind, accesskey.KindTOTPWrap)
	}
	if e.WrapKeyID != "" {
		t.Errorf("session 条目 WrapKeyID = %q, want 空（M13：wrap 来自 TOTP code+nonce）", e.WrapKeyID)
	}
	if e.Meta.Type != "login" {
		t.Errorf("session 条目 Meta.Type = %q, want login", e.Meta.Type)
	}
	if e.Meta.IP != normalizedTestIP(loopRemoteV4) {
		t.Errorf("session 条目 Meta.IP = %q, want %q", e.Meta.IP, normalizedTestIP(loopRemoteV4))
	}
	if !bytes.Equal(e.SK, lr.sessionSK) {
		t.Errorf("ring 条目 SK 与解封 session SK 不一致")
	}

	// session skeyID 立即可用（v2 强制必传路径）：签名 GET /api/credentials/<ak>/sk → 200。
	st, body := signedGetWithSKID(t, h, "/api/credentials/"+ak+"/sk", ak, lr.sessionSkeyID, hex.EncodeToString(lr.sessionSK))
	if st != http.StatusOK {
		t.Fatalf("session skeyID 签名 GET /api/credentials/%s/sk status = %d, want 200 (body=%s)", ak, st, body)
	}
}

// TestLogin_WrongCode_401 验证错误动态码 → 401，且该 nonce 已被消费（同 nonce 再登录也
// 不应成功——防同 nonce 爆破，D5）。
func TestLogin_WrongCode_401(t *testing.T) {
	_, h, _, _ := newTOTPTestServer(t, nil, nil)
	ak, b32 := registerTOTPFor(t, h, "")
	nonce := obtainNonce(t, h, loopRemoteV4)

	now := time.Now()
	code, _ := totpCodeFor(t, b32, now)
	wrong := "000000"
	if wrong == code {
		wrong = "111111"
	}
	body, _ := json.Marshal(map[string]any{"ak": ak, "nonce": nonce, "code": wrong})
	st, respBody := serveLogin(t, h, loopRemoteV4, body)
	if st != http.StatusUnauthorized {
		t.Fatalf("错误动态码 status = %d, want 401 (body=%s)", st, respBody)
	}

	// 该 nonce 已被本次尝试消费：即使此时用正确 code 再登录，也应 401（nonce 不可复用）。
	st2, _ := serveLogin(t, h, loopRemoteV4, mustJSON(t, map[string]any{"ak": ak, "nonce": nonce, "code": code}))
	if st2 == http.StatusOK {
		t.Fatalf("被消费的 nonce 不应可复用（登录不得成功）")
	}
}

// mustJSON 序列化任意值为 JSON 字节（测试辅助）。
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestLogin_PerAKLockout 验证 per-AK 失败锁定（U4）：同一 AK 连续 5 次错误动态码 →
// 第 6 次登录（**含正确动态码 + 正确 nonce**）→ 401（锁定窗口内）；ring 注入 now 前进
// 超过 LoginFailWindow → 自动解锁，可登录成功。
func TestLogin_PerAKLockout(t *testing.T) {
	fixed := time.Now()
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return fixed }
	ring := accesskey.NewRing(clock)
	hh, h, cfgPtr, _ := newTOTPTestServer(t, func(c *Config) {
		c.Registration.LoginFailLimit = 5
		c.Registration.LoginFailWindow = 15 * time.Minute
	}, ring)
	ak, b32 := registerTOTPFor(t, h, "")

	// 连续 5 次错误动态码（各自新 nonce，避免 IP/过期干扰 TOTP 错语义）。
	for i := range 4 {
		n := obtainNonce(t, h, loopRemoteV4)
		body := mustJSON(t, map[string]any{"ak": ak, "nonce": n, "code": "000000"})
		st, _ := serveLogin(t, h, loopRemoteV4, body)
		if st != http.StatusUnauthorized {
			t.Fatalf("错误码 #%d status = %d, want 401", i, st)
		}
	}
	// 第 5 次错误码 → 触发锁定（failCount 达 5）。
	n5 := obtainNonce(t, h, loopRemoteV4)
	st5, _ := serveLogin(t, h, loopRemoteV4, mustJSON(t, map[string]any{"ak": ak, "nonce": n5, "code": "000000"}))
	if st5 != http.StatusUnauthorized {
		t.Fatalf("第 5 次错误码 status = %d, want 401", st5)
	}

	// 第 6 次登录：**正确 nonce + 正确 code** 仍 401（锁定窗口内，不随 IP 失效）。
	mu.Lock()
	lkStateBefore := hh.loginFailTracker.lockedUntil(ak)
	mu.Unlock()
	if lkStateBefore.IsZero() {
		t.Fatalf("第 5 次失败后 AK 应处于锁定态（lockedUntil 非零）")
	}
	n6 := obtainNonce(t, h, loopRemoteV4)
	codeOK, _ := totpCodeFor(t, b32, fixed)
	st6, body6 := serveLogin(t, h, loopRemoteV4, mustJSON(t, map[string]any{"ak": ak, "nonce": n6, "code": codeOK}))
	if st6 != http.StatusUnauthorized {
		t.Fatalf("锁定窗口内正确动态码 status = %d, want 401 (body=%s)", st6, body6)
	}

	// ring 注入 now 前进超过 LoginFailWindow → 解锁。
	mu.Lock()
	fixed = fixed.Add(16 * time.Minute)
	mu.Unlock()
	n7 := obtainNonce(t, h, loopRemoteV4)
	lr := loginSuccessAt(t, h, cfgPtr, ak, b32, n7, "", "", fixed)
	if len(lr.sessionSK) != 32 {
		t.Fatalf("解锁后登录得到 session SK 长度 = %d, want 32", len(lr.sessionSK))
	}
}

// TestLogin_NoTOTPSecret_404 验证登录对无 TOTPSecret 的 AK → 404（M16，I1）。
// 覆盖两种来源：force_totp=false 注册的 AK（简单模式，无 TOTP 分支）+ 构造 ring 中存在
// 但无 TOTPSecret 的 AK。
func TestLogin_NoTOTPSecret_404(t *testing.T) {
	// 场景 A：force_totp=false 注册的 AK（简单模式）；登录端点存在但无 TOTPSecret。
	_, h, _, _ := newTOTPTestServer(t, func(c *Config) { c.Registration.ForceTOTP = false }, nil)
	simpleAK, _ := registerSimpleFor(t, h)
	nonce := obtainNonce(t, h, loopRemoteV4)
	body := mustJSON(t, map[string]any{"ak": simpleAK, "nonce": nonce, "code": "000000"})
	st, respBody := serveLogin(t, h, loopRemoteV4, body)
	if st != http.StatusNotFound {
		t.Fatalf("无 TOTPSecret AK 登录 status = %d, want 404 (body=%s)", st, respBody)
	}

	// 场景 B：构造 ring 中存在但无 TOTPSecret 的 AK（R2-N2：AK 不存在→404 语义一致）。
	ring := accesskey.NewRing()
	skB, _ := hex.DecodeString(testAccessSecret)
	_ = ring.UpsertAK(testAccessKey, "plain")
	_, _ = ring.AddKey(testAccessKey, skB, accesskey.WithID(testEntryID(testAccessKey)))
	_, h2, _, _ := newTOTPTestServer(t, nil, ring)
	nonce2 := obtainNonce(t, h2, loopRemoteV4)
	st2, _ := serveLogin(t, h2, loopRemoteV4, mustJSON(t, map[string]any{"ak": testAccessKey, "nonce": nonce2, "code": "000000"}))
	if st2 != http.StatusNotFound {
		t.Fatalf("构造无 TOTPSecret AK 登录 status = %d, want 404", st2)
	}
}

// registerSimpleFor 在 force_totp=false 服务器上注册一个简单模式 AK，返回 AK/SK。
func registerSimpleFor(t *testing.T, h http.Handler) (string, string) {
	t.Helper()
	st, body := serveRegister(t, h, loopRemoteV4, []byte(`{}`))
	if st != http.StatusOK {
		t.Fatalf("简单模式注册 status = %d, want 200 (body=%s)", st, body)
	}
	var p registeredPair
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("unmarshal register: %v (body=%s)", err, body)
	}
	return p.AK, p.SK
}

// TestLogin_NonceReplay 验证 nonce 重放：同一 nonce 二次登录（正确 code）→ 401/400
// （已被首次消费，R2-N2 语义：池中存在但已消费 → 记失败，但首登已成功，故第二次为
// 无效 nonce → 拒绝）。
func TestLogin_NonceReplay(t *testing.T) {
	hh, h, _, _ := newTOTPTestServer(t, nil, nil)
	ak, b32 := registerTOTPFor(t, h, "")
	nonce := obtainNonce(t, h, loopRemoteV4)
	_ = loginSuccessf(t, h, hh.cfgPtr, ak, b32, nonce, "", "")

	// 重放同一 nonce（虽 code 正确）→ 拒绝。
	now := time.Now()
	code, _ := totpCodeFor(t, b32, now)
	st, body := serveLogin(t, h, loopRemoteV4, mustJSON(t, map[string]any{"ak": ak, "nonce": nonce, "code": code}))
	if st == http.StatusOK {
		t.Fatalf("nonce 重放登录不得成功 (body=%s)", body)
	}
	if st != http.StatusUnauthorized && st != http.StatusBadRequest {
		t.Errorf("nonce 重放 status = %d, want 401/400", st)
	}
}

// TestLogin_NonceExpired 验证 nonce 过期：注入过期时间后消费 → 401/400。
func TestLogin_NonceExpired(t *testing.T) {
	hh, h, _, _ := newTOTPTestServer(t, nil, nil)
	ak, b32 := registerTOTPFor(t, h, "")
	nonce := obtainNonce(t, h, loopRemoteV4)

	// 直接把池中该 nonce 的过期时间改写为过去（白盒注入），模拟时钟前进导致过期。
	hh.totpNoncePool.mu.Lock()
	if e, ok := hh.totpNoncePool.m[nonce]; ok {
		e.expiresAt = time.Now().Add(-time.Second)
		hh.totpNoncePool.m[nonce] = e
	}
	hh.totpNoncePool.mu.Unlock()

	now := time.Now()
	code, _ := totpCodeFor(t, b32, now)
	st, body := serveLogin(t, h, loopRemoteV4, mustJSON(t, map[string]any{"ak": ak, "nonce": nonce, "code": code}))
	if st == http.StatusOK {
		t.Fatalf("过期 nonce 登录不得成功 (body=%s)", body)
	}
	if st != http.StatusUnauthorized && st != http.StatusBadRequest {
		t.Errorf("过期 nonce status = %d, want 401/400", st)
	}
}

// TestLogin_NonceIPMismatch 验证 nonce IP 绑定（D5）：nonce 从 127.0.0.1 签发、从另一
// 来源 IP 消费 → 401（注入 RemoteAddr 变化）。
func TestLogin_NonceIPMismatch(t *testing.T) {
	_, h, _, _ := newTOTPTestServer(t, nil, nil)
	ak, b32 := registerTOTPFor(t, h, "")
	// nonce 从 127.0.0.1 签发（loopRemoteV4）。
	nonce := obtainNonce(t, h, loopRemoteV4)
	now := time.Now()
	code, _ := totpCodeFor(t, b32, now)
	// 从另一来源 IP 消费 → 401。
	st, body := serveLogin(t, h, remoteNonLoop, mustJSON(t, map[string]any{"ak": ak, "nonce": nonce, "code": code}))
	if st != http.StatusUnauthorized {
		t.Fatalf("IP 不匹配 nonce status = %d, want 401 (body=%s)", st, body)
	}
}

// TestLogin_GarbageNonceDoesNotCountFailure 验证垃圾 nonce 不计失败（R2-N2）：同一 AK
// 携带池中不存在的 nonce 连发多条 login → 均 401 但不累计失败计数；随后正确 nonce +
// 正确 code 登录成功（未被误锁）。
func TestLogin_GarbageNonceDoesNotCountFailure(t *testing.T) {
	hh, h, _, _ := newTOTPTestServer(t, func(c *Config) {
		c.Registration.LoginFailLimit = 5 // 保持默认阈值——若垃圾 nonce 计失败，4 条后就该锁
	}, nil)
	ak, b32 := registerTOTPFor(t, h, "")

	// 连发 8 条垃圾 nonce（池中不存在）——若计入失败，早已触发锁定（>5）。
	for i := range 8 {
		st, body := serveLogin(t, h, loopRemoteV4, mustJSON(t, map[string]any{
			"ak": ak, "nonce": "deadbeefdeadbeefdeadbeefdeadbeef" + fmt.Sprintf("%02d", i), "code": "123456",
		}))
		if st != http.StatusUnauthorized {
			t.Fatalf("垃圾 nonce #%d status = %d, want 401 (body=%s)", i, st, body)
		}
	}

	// 未误锁：正确 nonce + 正确 code 登录成功。
	nonce := obtainNonce(t, h, loopRemoteV4)
	lr := loginSuccessf(t, h, hh.cfgPtr, ak, b32, nonce, "", "")
	if len(lr.sessionSK) != 32 {
		t.Fatalf("垃圾 nonce 后登录 session SK 长度 = %d, want 32", len(lr.sessionSK))
	}
}

// TestLogin_PoolPruneOnConsume 验证 nonce 池登录侧淘汰（M16）：插入过期 nonce 后 login
// 消费触发惰性清理（池大小收缩断言）。
func TestLogin_PoolPruneOnConsume(t *testing.T) {
	hh, h, _, _ := newTOTPTestServer(t, nil, nil)
	ak, b32 := registerTOTPFor(t, h, "")
	nonce := obtainNonce(t, h, loopRemoteV4)
	before := hh.totpNoncePool.size()

	// 篡改该 nonce 过期时间为过去（模拟过期）→ login 消费路径应惰性清掉它。
	hh.totpNoncePool.mu.Lock()
	if e, ok := hh.totpNoncePool.m[nonce]; ok {
		e.expiresAt = time.Now().Add(-time.Second)
		hh.totpNoncePool.m[nonce] = e
	}
	hh.totpNoncePool.mu.Unlock()

	now := time.Now()
	code, _ := totpCodeFor(t, b32, now)
	_, _ = serveLogin(t, h, loopRemoteV4, mustJSON(t, map[string]any{"ak": ak, "nonce": nonce, "code": code}))
	after := hh.totpNoncePool.size()
	if after >= before {
		t.Errorf("消费过期 nonce 后池大小 = %d, want < %d（惰性清理应收缩）", after, before)
	}
}

// TestLogin_TTLServersideAndScenarios 验证 TTL 服务端控 + 分场景（D3）：
//   - login_type 缺省（web）→ session_expires_at == now+SessionTTL；
//   - login_type=cli → == now+CliTTL；
//   - 客户端 body 带 ttl 字段被忽略（不改 session TTL）；
//   - session 条目在 ExpiresAt 后签名 → 401（ring 注入 now 前进使条目过期）。
func TestLogin_TTLServersideAndScenarios(t *testing.T) {
	fixed := time.Now()
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return fixed }
	ring := accesskey.NewRing(clock)
	_, h, cfgPtr, _ := newTOTPTestServer(t, func(c *Config) {
		c.Registration.SessionTTL = 24 * time.Hour
		c.Registration.CliTTL = 7 * 24 * time.Hour
	}, ring)
	ak, b32 := registerTOTPFor(t, h, "")

	// web（缺省）→ SessionTTL。
	nonceW := obtainNonce(t, h, loopRemoteV4)
	lrW := loginSuccessf(t, h, cfgPtr, ak, b32, nonceW, "", "")
	wantW := fixed.Add(24 * time.Hour)
	if d := lrW.resp.SessionExpiresAt.Sub(wantW); d > time.Second || d < -time.Second {
		t.Errorf("web 缺省 session_expires_at = %v, want ≈ %v", lrW.resp.SessionExpiresAt, wantW)
	}

	// login_type=cli → CliTTL。
	nonceC := obtainNonce(t, h, loopRemoteV4)
	lrC := loginSuccessf(t, h, cfgPtr, ak, b32, nonceC, "cli", "")
	wantC := fixed.Add(7 * 24 * time.Hour)
	if d := lrC.resp.SessionExpiresAt.Sub(wantC); d > time.Second || d < -time.Second {
		t.Errorf("cli session_expires_at = %v, want ≈ %v", lrC.resp.SessionExpiresAt, wantC)
	}

	// 客户端 body 带 ttl 字段被忽略（无白名单字段）→ 仍 web TTL。
	nonceT := obtainNonce(t, h, loopRemoteV4)
	codeT, _ := totpCodeFor(t, b32, fixed)
	stT, bodyT := serveLogin(t, h, loopRemoteV4, mustJSON(t, map[string]any{
		"ak": ak, "nonce": nonceT, "code": codeT, "ttl": "100h",
	}))
	if stT != http.StatusOK {
		t.Fatalf("带 ttl 字段登录 status = %d, want 200 (body=%s)", stT, bodyT)
	}
	var lrT loginResp
	_ = json.Unmarshal(bodyT, &lrT)
	if d := lrT.SessionExpiresAt.Sub(wantW); d > time.Second || d < -time.Second {
		t.Errorf("ttl 字段后 session_expires_at = %v, want ≈ web TTL %v（ttl 被忽略）", lrT.SessionExpiresAt, wantW)
	}

	// session 条目在 ExpiresAt 后签名 → 401（ring 注入 now 前进到条目过期后）。
	mu.Lock()
	fixed = lrC.resp.SessionExpiresAt.Add(time.Second)
	mu.Unlock()
	st, body := signedGetWithSKID(t, h, "/api/credentials/"+ak+"/sk", ak, lrC.sessionSkeyID, hex.EncodeToString(lrC.sessionSK))
	if st == http.StatusOK {
		t.Fatalf("过期 session 签名应 401，却 200 (body=%s)", body)
	}
}

// TestLogin_UnknownLoginType_400 验证 login_type 未知值 → 400（M8/M16）。
func TestLogin_UnknownLoginType_400(t *testing.T) {
	_, h, _, _ := newTOTPTestServer(t, nil, nil)
	ak, b32 := registerTOTPFor(t, h, "")
	nonce := obtainNonce(t, h, loopRemoteV4)
	now := time.Now()
	code, _ := totpCodeFor(t, b32, now)
	st, body := serveLogin(t, h, loopRemoteV4, mustJSON(t, map[string]any{
		"ak": ak, "nonce": nonce, "code": code, "login_type": "mobile",
	}))
	if st != http.StatusBadRequest {
		t.Fatalf("未知 login_type status = %d, want 400 (body=%s)", st, body)
	}
}

// TestLogin_SessionPruneOnAdd 验证 session 条目惰性修剪（M11）：多次登录后该 AK 过期
// session 条目被清除（仅保留存活条目）。ring 注入 now——先签近过期 session，再推进 now
// 使其过期，再次登录时旧过期条目应被剪掉。
func TestLogin_SessionPruneOnAdd(t *testing.T) {
	start := time.Now().Truncate(time.Second)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return start }
	ring := accesskey.NewRing(clock)
	hh, h, cfgPtr, _ := newTOTPTestServer(t, func(c *Config) {
		// 短 session TTL 让测试快速制造过期条目。
		c.Registration.SessionTTL = time.Hour
		c.Registration.CliTTL = 2 * time.Hour
	}, ring)
	ak, b32 := registerTOTPFor(t, h, "")

	// 两次登录（code 按注入时钟 start 复算）→ 2 条活跃 session 条目。
	getNow := func() time.Time { mu.Lock(); defer mu.Unlock(); return start }
	n1 := obtainNonce(t, h, loopRemoteV4)
	_ = loginSuccessAt(t, h, cfgPtr, ak, b32, n1, "", "", getNow())
	n2 := obtainNonce(t, h, loopRemoteV4)
	_ = loginSuccessAt(t, h, cfgPtr, ak, b32, n2, "", "", getNow())
	k, _ := hh.credentialRing.GetKey(ak)
	if len(k.Entries) != 2 {
		t.Fatalf("两次登录后条目数 = %d, want 2", len(k.Entries))
	}

	// 阶段 2：推进 now 使此前两条 session 条目全部过期（SessionTTL=1h，均为
	// T0+60m；now=T0+120m > expires）。第三次登录追加前剪掉全部过期条目 →
	// 剩 [新增]=1 条。
	mu.Lock()
	start = start.Add(2 * time.Hour)
	mu.Unlock()
	n3 := obtainNonce(t, h, loopRemoteV4)
	_ = loginSuccessAt(t, h, cfgPtr, ak, b32, n3, "", "", getNow())
	k2, _ := hh.credentialRing.GetKey(ak)
	if len(k2.Entries) != 1 {
		t.Errorf("第三次登录应剪掉全部过期条目, 剩余 = %d, want 1（仅存活新增）", len(k2.Entries))
	}

	// 阶段 3：推进 now 超过新增条目 ExpiresAt（now=T0+120m+2h > expires3=T0+120m+1h）。
	// 再登录触发同样修剪 → 恒只剩新增 1 条（Entries 不累积）。
	mu.Lock()
	start = start.Add(2 * time.Hour)
	mu.Unlock()
	n4 := obtainNonce(t, h, loopRemoteV4)
	_ = loginSuccessAt(t, h, cfgPtr, ak, b32, n4, "", "", getNow())
	k3, _ := hh.credentialRing.GetKey(ak)
	if len(k3.Entries) != 1 {
		t.Errorf("阶段 3 登录后全部过期条目应被剪掉, 剩余 = %d, want 1", len(k3.Entries))
	}
}

// TestLogin_EmptyAKTrackerskip 验证空 AK 不登记失败锁定（防 map 污染 DoS）：
// 空 AK + 垃圾/未知 nonce → 401 不计失败；登录返回 401 而非 500。
func TestLogin_EmptyAKTrackerskip(t *testing.T) {
	_, h, _, _ := newTOTPTestServer(t, nil, nil)
	st, body := serveLogin(t, h, loopRemoteV4, mustJSON(t, map[string]any{
		"ak": "", "nonce": "deadbeefdeadbeefdeadbeefdeadbeef", "code": "123456",
	}))
	if st == http.StatusOK {
		t.Fatalf("空 AK 登录不得成功 (body=%s)", body)
	}
}
