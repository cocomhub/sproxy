// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
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
	}
	h := RegisterRoutes(t.Context(), opts)
	t.Cleanup(func() { _ = h.Close() })
	return h.Handler(), &cfgPtr, ring
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
	store := NewCredentialStore(tmpDir + "/tenant/meta")
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
