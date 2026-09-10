// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// auth_config_e2e_test.go — 登录/注册 + 存储配置 + 卷下拉 Web UI 真交互 E2E
// （PR-F2 任务 5）。
//
// 无凭据模型红线：首次注册会令凭据 ring 非空，此后该 server 实例的匿名 loopback 请求
// 一律 401——凭据类用例必须自包含（独立 server 实例 + 全部交互经已建立的签名会话）。
// 签名链路的判据统一为「后续请求的 Authorization 头 = SproxySig v=2 ...」。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/mxschmitt/playwright-go"
)

// TestAuth_RegisterTOTP_AndLogin UI 注册（TOTP）+ 算码登录 + 签名请求（自包含 server）。
func TestAuth_RegisterTOTP_AndLogin(t *testing.T) {
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		// TOTP 分支：注册只下发 otpauth_uri/base32_secret（无 sk），须走 TOTP 登录。
		c.Registration.ForceTOTP = true
	})
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	if err := page.Locator("#login-btn").Click(); err != nil {
		t.Fatalf("click login-btn: %v", err)
	}
	if err := waitLoc(page, "#login-modal", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("登录弹窗未打开: %v", err)
	}
	// 切注册面板并填 owner。
	if err := page.Locator("#register-tab").Click(); err != nil {
		t.Fatalf("click register-tab: %v", err)
	}
	if err := page.Locator("#register-owner").Fill("e2e-owner"); err != nil {
		t.Fatalf("fill register-owner: %v", err)
	}

	// 注册：POST /api/credentials/register → {ak, owner, admin, otpauth_uri, base32_secret}。
	resp, err := page.ExpectResponse("**/api/credentials/register", func() error {
		return page.Locator("#do-register-btn").Click()
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("未观察到 POST /api/credentials/register（注册未接线？）: %v", err)
	}
	if got := resp.Status(); got != http.StatusOK {
		t.Fatalf("注册 status = %d, want 200", got)
	}
	var reg struct {
		AK           string `json:"ak"`
		Owner        string `json:"owner"`
		Admin        bool   `json:"admin"`
		OTPAuthURI   string `json:"otpauth_uri"`
		Base32Secret string `json:"base32_secret"`
	}
	if jerr := resp.JSON(&reg); jerr != nil {
		t.Fatalf("解析注册响应: %v", jerr)
	}
	if !strings.HasPrefix(reg.AK, "ak-") {
		t.Errorf("ak = %q, want 前缀 ak-", reg.AK)
	}
	if reg.Base32Secret == "" {
		t.Error("base32_secret 为空（TOTP 注册未生效）")
	}
	if !reg.Admin {
		t.Error("首个注册者应为 admin")
	}

	// DOM：注册结果展示 AK + base32 + 客户端 QR 渲染。
	if werr := waitLoc(page, "#register-result", playwright.WaitForSelectorStateVisible, 8000); werr != nil {
		t.Fatalf("注册结果区未显示: %v", werr)
	}
	resultTxt, _ := page.Locator("#register-result").InnerText()
	if !strings.Contains(resultTxt, "AccessKey") {
		t.Errorf("注册结果缺 AccessKey 文案:\n%s", resultTxt)
	}
	if !strings.Contains(resultTxt, reg.Base32Secret) {
		t.Errorf("注册结果未展示 base32_secret:\n%s", resultTxt)
	}
	if cnt, _ := page.Locator("#qr-register svg").Count(); cnt < 1 {
		t.Errorf("QR svg 数 = %d, want >= 1（客户端二维码未渲染）", cnt)
	}

	// 登录：切登录面板 → 填 AK + 同进程即时算的 TOTP 码 → 提交。
	code := totpCodeFromBase32(t, reg.Base32Secret)
	if tabErr := page.Locator("#login-tab").Click(); tabErr != nil {
		t.Fatalf("click login-tab: %v", tabErr)
	}
	if akErr := page.Locator("#login-ak").Fill(reg.AK); akErr != nil {
		t.Fatalf("fill login-ak: %v", akErr)
	}
	if codeErr := page.Locator("#login-code").Fill(code); codeErr != nil {
		t.Fatalf("fill login-code: %v", codeErr)
	}
	// 登录成功后 applyWebLoginKeys → refreshList，该请求必须带 SproxySig 签名头
	// （凭据生效后可能走隧道模式 → 以「任一带签名外发请求」为判据）。
	rec := recordSignedRequests(page)
	base := rec.count()
	if clickErr := page.Locator("#do-login-btn").Click(); clickErr != nil {
		t.Fatalf("click do-login-btn: %v", clickErr)
	}
	if seen := rec.waitIncrease(base, 10000); seen == "" {
		t.Fatalf("登录后未观察到带 SproxySig v=2 签名的请求（登录/签名链路未接通？）")
	}

	// DOM：sessionStorage 三键写入且弹窗关闭。
	akVal, err := page.Evaluate("sessionStorage.getItem('sproxy_access_key')")
	if err != nil {
		t.Fatalf("读取 sessionStorage ak: %v", err)
	}
	if akVal != reg.AK {
		t.Errorf("sessionStorage sproxy_access_key = %v, want %q", akVal, reg.AK)
	}
	secretVal, _ := page.Evaluate("sessionStorage.getItem('sproxy_access_key_secret')")
	if s, ok := secretVal.(string); !ok || s == "" {
		t.Errorf("sessionStorage sproxy_access_key_secret 为空或非字符串: %v", secretVal)
	}
	idVal, _ := page.Evaluate("sessionStorage.getItem('sproxy_access_key_id')")
	if s, ok := idVal.(string); !ok || s == "" {
		t.Errorf("sessionStorage sproxy_access_key_id 为空或非字符串: %v", idVal)
	}
	if vis, _ := page.Locator("#login-modal").IsVisible(); vis {
		t.Error("登录成功后 #login-modal 应关闭")
	}
}

// TestAuth_SaveKeysSigns auth-bar 保存 AK/SK → 后续请求带签名（自包含 server）。
func TestAuth_SaveKeysSigns(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	// Go 侧先注册（简单分支返回 sk），使 ring 非空——页面须靠 auth-bar 保存凭据才能访问。
	resp, err := http.Post(baseURL+"/api/credentials/register", "application/json",
		strings.NewReader(`{"owner":"bar-owner"}`))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, want 200", resp.StatusCode)
	}
	var reg struct {
		AK string `json:"ak"`
		SK string `json:"sk"`
	}
	if jerr := json.NewDecoder(resp.Body).Decode(&reg); jerr != nil {
		t.Fatalf("解析注册响应: %v", jerr)
	}
	if reg.AK == "" || reg.SK == "" {
		t.Fatalf("简单注册应返回 ak 与 sk，实际 ak=%q sk 长度=%d", reg.AK, len(reg.SK))
	}

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	if err := waitLoc(page, "#accessKey", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("auth-bar 未渲染: %v", err)
	}

	if err := page.Locator("#accessKey").Fill(reg.AK); err != nil {
		t.Fatalf("fill accessKey: %v", err)
	}
	if err := page.Locator("#token").Fill(reg.SK); err != nil {
		t.Fatalf("fill token: %v", err)
	}
	if err := page.Locator("#save-access-btn").Click(); err != nil {
		t.Fatalf("click save-access-btn: %v", err)
	}

	// DOM：saveAccessKeys 写入 sessionStorage 三键（id 空）。
	akVal, _ := page.Evaluate("sessionStorage.getItem('sproxy_access_key')")
	if akVal != reg.AK {
		t.Errorf("sessionStorage sproxy_access_key = %v, want %q", akVal, reg.AK)
	}
	secretVal, _ := page.Evaluate("sessionStorage.getItem('sproxy_access_key_secret')")
	if secretVal != reg.SK {
		t.Errorf("sessionStorage sproxy_access_key_secret = %v, want 注册下发的 sk", secretVal)
	}
	idVal, _ := page.Evaluate("sessionStorage.getItem('sproxy_access_key_id')")
	if idVal != "" {
		t.Errorf("手动保存后的 sproxy_access_key_id = %v, want 空", idVal)
	}

	// 网络：点刷新 → 必带 SproxySig v=2 签名头（直连 GET /api/files 或隧道 POST /tunnel）。
	rec := recordSignedRequests(page)
	base := rec.count()
	if rerr := page.Locator("#refresh-btn").Click(); rerr != nil {
		t.Fatalf("click refresh-btn: %v", rerr)
	}
	if seen := rec.waitIncrease(base, 10000); seen == "" {
		t.Fatalf("刷新未观察到带 SproxySig v=2 签名的请求（保存的凭据未参与签名）")
	}

	// 渲染：不再停留在 401 错误态。
	waitTextGone(t, page, "#file-list", "请求失败", 8000)
}

// TestConfig_UpdateMaxStorage 配置面板：PUT /api/config body + 重拉后 input 值双证。
func TestConfig_UpdateMaxStorage(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	if err := page.Locator("#stats-btn").Click(); err != nil {
		t.Fatalf("click stats-btn: %v", err)
	}
	if err := waitLoc(page, "#stats-modal", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("stats-modal 未显示: %v", err)
	}
	// 切配置 tab → showConfig → GET /api/config 渲染面板。
	if err := page.Locator("#config-tab").Click(); err != nil {
		t.Fatalf("click config-tab: %v", err)
	}
	if err := waitLoc(page, "#cfg-max-storage", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("配置面板未渲染 #cfg-max-storage: %v", err)
	}

	if err := page.Locator("#cfg-max-storage").Fill("104857600"); err != nil {
		t.Fatalf("fill cfg-max-storage: %v", err)
	}

	// 更新：PUT /api/config，body {"max_storage_bytes":104857600}。
	req, err := page.ExpectRequest("**/api/config", func() error {
		return page.Locator("#cfg-update-storage").Click()
	}, playwright.PageExpectRequestOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("未观察到 PUT /api/config（更新未接线？）: %v", err)
	}
	if got := req.Method(); got != "PUT" {
		t.Errorf("config update method = %q, want PUT", got)
	}
	var patch struct {
		MaxStorageBytes int64 `json:"max_storage_bytes"`
	}
	requestJSON(t, req, &patch)
	if patch.MaxStorageBytes != 104857600 {
		t.Errorf("PUT body max_storage_bytes = %d, want 104857600", patch.MaxStorageBytes)
	}

	// DOM：showConfig 重拉后输入框回填新值（重拉真实生效）；toast 提示已更新。
	deadline := time.Now().Add(8 * time.Second)
	var gotVal string
	for time.Now().Before(deadline) {
		v, verr := page.Locator("#cfg-max-storage").InputValue()
		if verr == nil && v == "104857600" {
			gotVal = v
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if gotVal != "104857600" {
		v, _ := page.Locator("#cfg-max-storage").InputValue()
		t.Fatalf("重拉后 #cfg-max-storage = %q, want 104857600（showConfig 未重拉/未生效）", v)
	}
	waitTextVisible(t, page, "#toast", "配置已更新", 5000)
}

// TestVolumes_SingleVolumeSelect 单卷（默认 testServer）下卷下拉 option 与 currentVolume() 接线。
func TestVolumes_SingleVolumeSelect(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	// 统一到红线 1（R2）：导航期捕获 initUploadVolumeSelect 触发的 GET /api/volumes，
	// 断言状态码后再断言 option 渲染（数据来自 API 而非静态 DOM）。
	volResp, err := page.ExpectResponse("**/api/volumes", func() error {
		_, gerr := page.Goto(baseURL+"/ui/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)})
		return gerr
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(10000)})
	if err != nil {
		t.Fatalf("未观察到导航触发的 GET /api/volumes: %v", err)
	}
	if got := volResp.Status(); got != http.StatusOK {
		t.Fatalf("GET /api/volumes status = %d, want 200", got)
	}
	// 默认卷 default 被填充。
	if werr := waitLoc(page, "#upload-volume option[value='default']", playwright.WaitForSelectorStateAttached, 8000); werr != nil {
		t.Fatalf("默认卷 option 未填充: %v", werr)
	}

	raw, err := page.Evaluate(`Array.from(document.querySelectorAll('#upload-volume option')).map(o => o.value)`)
	if err != nil {
		t.Fatalf("读取 upload-volume options: %v", err)
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("options 类型 = %T", raw)
	}
	got := make([]string, 0, len(list))
	for _, v := range list {
		got = append(got, v.(string))
	}
	if strings.Join(got, ",") != ",default" {
		t.Errorf("upload-volume options = %v, want [\"\" default]", got)
	}

	// 初始 auto，选择后 setVolumeContext 生效。
	cur, _ := page.Evaluate("currentVolume()")
	if cur != "" {
		t.Fatalf("默认 currentVolume() = %v, want \"\"（auto）", cur)
	}
	if _, serr := page.Locator("#upload-volume").SelectOption(
		playwright.SelectOptionValues{Values: &[]string{"default"}}); serr != nil {
		t.Fatalf("select default 卷: %v", serr)
	}
	cur, _ = page.Evaluate("currentVolume()")
	if cur != "default" {
		t.Fatalf("选择后 currentVolume() = %v, want \"default\"（下拉 change 未接线 setVolumeContext）", cur)
	}
}
