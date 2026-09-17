// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// user_volumes_e2e_test.go — 用户卷 Web UI 真交互 E2E（Playwright + Chromium）。
//
// 每用例走「控件操作 → 捕获并断言网络请求（POST/GET/DELETE /api/volumes/user）→
// 断言变化后的 DOM（用户卷行出现 / 消失 / 错误提示）」三要素，杜绝 false-green。
// dialog（confirm 删除确认）用例在点击前装 OnDialog，否则 Playwright 默认
// auto-dismiss 会让点击 inert。
//
// 装配前提（与 cmd/sproxy 用户卷装配对齐）：
//   - RegisterRoutes 不装配用户卷 store（SetUserVolumeStore 是 cmd/sproxy 的装配职责）
//     → 本文件经 testServerCfgWithHandlers 拿 h 后注入 h.SetUserVolumeStore(store)；
//   - registry backend 需已注册（create handler 用 registry.NewBackend 试构造）
//     → 本文件注册唯一 fake backend 类型（sync.Once 防重复 panic；并发测试唯一类型名）。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/mxschmitt/playwright-go"
)

// ---- fake 后端（e2e 专用，规避 baidupcs 注册依赖 + 并发冲突）----

// fakeBackendType 是 e2e 注册的后端类型名：与 UI 下拉写死选项一致（app.js 的
// userVolumes.createUserVolumeFormHtml(['baidupcs'])）。e2e 进程独立（不跑 cmd/sproxy），
// 注册同名 fake backend 使 UI 真实链路（选 baidupcs → 创建）可走通。
const fakeBackendType = "baidupcs"

var registerFakeBackendOnce sync.Once

// registerFakeBackend 注册 e2e fake 后端（sync.Once：RegisterBackend 重复注册 panic）。
// fake FS 是最小内存 sync.FS（只实现接口形状，UI 用例不驱动同步）。
func registerFakeBackend() {
	registerFakeBackendOnce.Do(func() {
		registry.RegisterBackend(fakeBackendType, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
			return &fakeExternalBackend{}, nil
		})
	})
}

// fakeExternalBackend 是 e2e ExternalBackend：FS 为最小内存 sync.FS；Close 无资源。
type fakeExternalBackend struct{}

func (b *fakeExternalBackend) FS() syncpkg.FS { return &fakeFS{} }

func (b *fakeExternalBackend) Close() error { return nil }

// fakeFS 是最小 sync.FS 实现（接口形状；UI 用例不驱动同步引擎）。
type fakeFS struct{}

func (f *fakeFS) ListDir(_ context.Context, _ string) ([]syncpkg.Entry, error) { return nil, nil }
func (f *fakeFS) Stat(_ context.Context, _ string) (*syncpkg.Entry, error)     { return nil, nil }
func (f *fakeFS) OpenRead(_ context.Context, _ string) (io.ReadCloser, error)  { return nil, nil }
func (f *fakeFS) WriteFile(_ context.Context, _ string, _ io.Reader, _ int64, _ int64) error {
	return nil
}
func (f *fakeFS) Rename(_ context.Context, _, _ string) error { return nil }
func (f *fakeFS) Delete(_ context.Context, _ string) error    { return nil }
func (f *fakeFS) MakeDir(_ context.Context, _ string) error   { return nil }

// ---- e2e 装配：用户卷 store + fake backend ----

// userVolumeE2EServer 启动带用户卷装配的 e2e 服务（RegisterRoutes + SetUserVolumeStore + fake backend）。
func userVolumeE2EServer(t *testing.T) (string, func()) {
	t.Helper()
	baseURL, h, cfg, cleanup := testServerCfgWithHandlers(t, nil)
	t.Cleanup(cleanup)
	registerFakeBackend()
	h.SetUserVolumeStore(server.NewUserVolumeStore(cfg.StorageRoot))
	return baseURL, func() {}
}

// openVolumesPanel 打开 /ui/ → 点 #stats-btn（监控弹窗）→ 点 #volumes-tab（卷面板含用户卷区）。
func openVolumesPanel(page playwright.Page, baseURL string) (playwright.Locator, error) {
	if _, gerr := page.Goto(baseURL+"/ui/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)}); gerr != nil {
		return nil, gerr
	}
	if err := page.Locator("#stats-btn").Click(); err != nil {
		return nil, err
	}
	if err := waitLoc(page, "#stats-modal", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		return nil, err
	}
	if err := page.Locator("#volumes-tab").Click(); err != nil {
		return nil, err
	}
	if err := waitLoc(page, "#volumes-panel", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		return nil, err
	}
	// 用户卷区骨架（创建表单 + 列表容器）渲染后即交互。
	if err := waitLoc(page, "#user-volumes-list", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		return nil, err
	}
	return page.Locator("#user-volumes-list"), nil
}

// TestUserVolumesE2E_CreateListDelete 全链路：打开卷面板 → 创建用户卷（填 name/type/extra）
// → 列表出现新卷 → 删除（confirm 确认）→ 列表消失。断言落网络请求 + DOM 变化。
func TestUserVolumesE2E_CreateListDelete(t *testing.T) {
	t.Parallel()
	baseURL, _ := userVolumeE2EServer(t)

	page, stop := pageFixture(t)
	defer stop()
	acceptDialog(page, "")

	if _, err := openVolumesPanel(page, baseURL); err != nil {
		t.Fatalf("打开卷面板失败: %v", err)
	}

	// 创建：填表单 → 点创建 → 捕获 POST /api/volumes/user。
	volName := "e2e-vol-" + randSuffix()
	extraJSON := `{"bduss":"e2e-test-bduss"}`
	req, reqErr := page.ExpectRequest("**/api/volumes/user", func() error {
		if err := page.Locator("#uv-name").Fill(volName); err != nil {
			return err
		}
		if _, selErr := page.Locator("#uv-type").SelectOption(playwright.SelectOptionValues{Values: &[]string{fakeBackendType}}); selErr != nil {
			return selErr
		}
		if err := page.Locator("#uv-extra").Fill(extraJSON); err != nil {
			return err
		}
		return page.Locator("#uv-create-btn").Click()
	}, playwright.PageExpectRequestOptions{Timeout: playwright.Float(8000)})
	if reqErr != nil {
		t.Fatalf("创建未触发 POST /api/volumes/user: %v", reqErr)
	}
	if got := req.Method(); got != "POST" {
		t.Fatalf("创建请求 method = %s, want POST", got)
	}
	var body struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	requestJSON(t, req, &body)
	if body.Name != volName || body.Type != fakeBackendType {
		t.Fatalf("创建请求体 name/type = %q/%q, want %q/%q", body.Name, body.Type, volName, fakeBackendType)
	}

	// 列表出现新卷（等待 GET /api/volumes/user 刷新 + DOM 行出现）。
	if err := waitLoc(page, "#user-volumes-list tr", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("创建后列表应出现行（%s）: %v", volName, err)
	}
	rowText, err := page.Locator("#user-volumes-list").InnerText()
	if err != nil {
		t.Fatalf("读取用户卷列表文本: %v", err)
	}
	if !strings.Contains(rowText, volName) {
		t.Fatalf("列表应包含 %q, got: %s", volName, rowText)
	}

	// 删除：点该卷删除按钮 → confirm 确认 → 捕获 DELETE /api/volumes/user?name=。
	delReq, err := page.ExpectRequest("**/api/volumes/user?name=*", func() error {
		return page.Locator(`[data-action="delete-user-volume"][data-name="` + volName + `"]`).Click()
	}, playwright.PageExpectRequestOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("删除未触发 DELETE /api/volumes/user: %v", err)
	}
	if got := delReq.Method(); got != "DELETE" {
		t.Fatalf("删除请求 method = %s, want DELETE", got)
	}
	if !strings.Contains(delReq.URL(), "name="+volName) {
		t.Fatalf("删除 URL 应含 name=%s, got %s", volName, delReq.URL())
	}

	// 列表消失（等待刷新后行移除）。
	if err := waitLoc(page, "#user-volumes-list tr", playwright.WaitForSelectorStateHidden, 8000); err != nil {
		t.Fatalf("删除后列表行应消失: %v", err)
	}
}

// TestUserVolumesE2E_BadType 未注册 type → 服务端 400 → 错误提示显示（#uv-create-msg）。
func TestUserVolumesE2E_BadType(t *testing.T) {
	t.Parallel()
	baseURL, _ := userVolumeE2EServer(t)

	page, stop := pageFixture(t)
	defer stop()

	if _, err := openVolumesPanel(page, baseURL); err != nil {
		t.Fatalf("打开卷面板失败: %v", err)
	}

	// 创建未注册 type → POST 400 → msg 显示错误。
	resp, respErr := page.ExpectResponse("**/api/volumes/user", func() error {
		if err := page.Locator("#uv-name").Fill("bad-type-vol"); err != nil {
			return err
		}
		// select 选项只有 baidupcs（已注册）——未注册 type 需注入 option 后设 value
		// （HTMLSelectElement 直接设不存在的 value 会回退为空 → 前端「卷名与类型必填」拦截，
		// 故必须 append 匹配 option）。
		_, eErr := page.Locator("#uv-type").Evaluate(`(el) => {
			const opt = document.createElement('option');
			opt.value = 'nosuchtype';
			opt.text = 'nosuchtype';
			el.appendChild(opt);
			el.value = 'nosuchtype';
			el.dispatchEvent(new Event('change'));
		}`, nil)
		if eErr != nil {
			return eErr
		}
		if err := page.Locator("#uv-extra").Fill(`{"bduss":"x"}`); err != nil {
			return err
		}
		return page.Locator("#uv-create-btn").Click()
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(8000)})
	if respErr != nil {
		t.Fatalf("创建未触发 POST /api/volumes/user: %v", respErr)
	}
	if got := resp.Status(); got != http.StatusBadRequest {
		t.Fatalf("未注册 type 创建 status = %d, want 400", got)
	}
	// 错误提示显示（#uv-create-msg 含「type 未注册」字样）。
	if err := waitLoc(page, "#uv-create-msg", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("错误提示未显示: %v", err)
	}
	msgText, err := page.Locator("#uv-create-msg").InnerText()
	if err != nil {
		t.Fatalf("读取错误提示: %v", err)
	}
	if !strings.Contains(msgText, "请求失败") {
		t.Fatalf("错误提示应含「请求失败」（jsonRequest 非 2xx 抛错），got: %s", msgText)
	}
}

// randSuffix 生成短随机后缀（卷名唯一，防并发测试冲突）。
func randSuffix() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "x"
	}
	return hex.EncodeToString(b)
}

// _ 编译期断言：fakeExternalBackend 满足 registry.ExternalBackend（防签名漂移）。
var _ registry.ExternalBackend = (*fakeExternalBackend)(nil)

// 占位避免 fmt 未使用（backend 工厂参数）。
var _ = fmt.Sprintf
