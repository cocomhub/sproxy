// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/audit"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// volume_settings_api_test.go 锁定卷详情/设置 HTTP 面的三件事：
//  ① 权限门顺序（可见性 → 归属/admin）与「他人私有卷 404 防枚举」；
//  ② 错误 → 状态码映射（400/405/409/500）；
//  ③ 业务审计在成功与拒绝两条路径都发出，且不含规则全文。

// settingsCtx 构造带 Principal 的请求 ctx（认证面的输出形态）。
func settingsCtx(owner, role string) context.Context {
	return withPrincipal(context.Background(), &Principal{Owner: owner, Role: role})
}

// doSettingsReq 用给定 ctx/body 调 handler。
func doSettingsReq(t *testing.T, h http.Handler, ctx context.Context, method, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequestWithContext(ctx, method, target, nil)
	} else {
		r = httptest.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

// fakeSettingsBackend 是同时实现 SettingsProvider/SettingsWriter 的假外部后端。
type fakeSettingsBackend struct {
	sections   []registry.VolumeSettings
	updateErr  error
	updateCall int
	lastSect   string
}

func (f *fakeSettingsBackend) FS() syncpkg.FS { return fakeSyncFS{} }
func (f *fakeSettingsBackend) Close() error   { return nil }
func (f *fakeSettingsBackend) Settings(context.Context) ([]registry.VolumeSettings, error) {
	return f.sections, nil
}

func (f *fakeSettingsBackend) UpdateSettings(_ context.Context, section string, _ json.RawMessage) error {
	f.updateCall++
	f.lastSect = section
	return f.updateErr
}

// providerOnlyBackend 只实现 SettingsProvider（用于「能力缺失 ⇒ 405」）。
type providerOnlyBackend struct{}

func (providerOnlyBackend) FS() syncpkg.FS { return fakeSyncFS{} }
func (providerOnlyBackend) Close() error   { return nil }
func (providerOnlyBackend) Settings(context.Context) ([]registry.VolumeSettings, error) {
	return nil, nil
}

// newSettingsTestHandlers 构造只有卷集的最小 Handlers（不经 RegisterRoutes，聚焦门禁逻辑）。
func newSettingsTestHandlers(t *testing.T, name string, be registry.ExternalBackend) *Handlers {
	t.Helper()
	h := &Handlers{}
	h.volSet = registry.NewSet(nil, nil, nil, nil, "")
	if err := h.volSet.AddExternalVolume(volume.Volume{Name: name, Type: "secretdata"}, be); err != nil {
		t.Fatalf("AddExternalVolume: %v", err)
	}
	return h
}

// settingsRoutes 返回被测的路由集合（详情 / 设置读 / 设置写）。
func settingsRoutes(h *Handlers) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/volumes/{name}/detail", h.volumeDetailHandler)
	mux.HandleFunc("GET /api/volumes/{name}/settings", h.volumeSettingsGetHandler)
	mux.HandleFunc("PUT /api/volumes/{name}/settings/{section}", h.volumeSettingsPutHandler)
	return mux
}

// TestVolumeSettings_PermissionMatrix 锁定：系统卷 admin 可写、非 admin 403、所有人可读。
func TestVolumeSettings_PermissionMatrix(t *testing.T) {
	t.Parallel()
	be := &fakeSettingsBackend{sections: []registry.VolumeSettings{{
		Key: "block_policy", Label: "分块规则", Value: json.RawMessage(`{"rules":[],"version":0}`),
	}}}
	tests := []struct {
		name   string
		owner  string
		role   string
		method string
		target string
		body   []byte
		want   int
	}{
		{"admin writes system volume", "boss", string(accesskey.RoleAdmin), http.MethodPut,
			"/api/volumes/sysvault/settings/block_policy", []byte(`{"rules":[]}`), http.StatusOK},
		{"user writes system volume → 403", "alice", string(accesskey.RoleUser), http.MethodPut,
			"/api/volumes/sysvault/settings/block_policy", []byte(`{"rules":[]}`), http.StatusForbidden},
		{"reader writes system volume → 403", "bob", string(accesskey.RoleReader), http.MethodPut,
			"/api/volumes/sysvault/settings/block_policy", []byte(`{"rules":[]}`), http.StatusForbidden},
		{"user reads detail → 200", "alice", string(accesskey.RoleUser), http.MethodGet,
			"/api/volumes/sysvault/detail", nil, http.StatusOK},
		{"user reads settings → 200", "alice", string(accesskey.RoleUser), http.MethodGet,
			"/api/volumes/sysvault/settings", nil, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSettingsTestHandlers(t, "sysvault", be)
			rr := doSettingsReq(t, settingsRoutes(h), settingsCtx(tc.owner, tc.role), tc.method, tc.target, tc.body)
			if rr.Code != tc.want {
				t.Fatalf("code=%d want %d body=%s", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

// TestVolumeSettings_UnknownVolume404 锁定不存在卷恒 404（读与写一致，不泄露存在性）。
func TestVolumeSettings_UnknownVolume404(t *testing.T) {
	t.Parallel()
	h := newSettingsTestHandlers(t, "exists", &fakeSettingsBackend{})
	ctx := settingsCtx("alice", string(accesskey.RoleUser))
	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/api/volumes/ghost/detail"},
		{http.MethodGet, "/api/volumes/ghost/settings"},
	} {
		rr := doSettingsReq(t, settingsRoutes(h), ctx, tc.method, tc.target, nil)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s %s: code=%d want 404", tc.method, tc.target, rr.Code)
		}
	}
}

// TestVolumeSettings_StatusMapping 锁定错误 → 状态码映射。
func TestVolumeSettings_StatusMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"version conflict → 409", registry.ErrSettingsVersionConflict, http.StatusConflict},
		{"unsupported section → 405", registry.ErrSettingsSectionUnsupported, http.StatusMethodNotAllowed},
		{"validation → 400", errors.New("policy: 规则 0 的 max_size 超过硬上限"), http.StatusBadRequest},
		{"other → 500", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSettingsTestHandlers(t, "v1", &fakeSettingsBackend{updateErr: tc.err})
			rr := doSettingsReq(t, settingsRoutes(h), settingsCtx("boss", string(accesskey.RoleAdmin)),
				http.MethodPut, "/api/volumes/v1/settings/block_policy", []byte(`{"rules":[]}`))
			if rr.Code != tc.want {
				t.Fatalf("code=%d want %d body=%s", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

// TestVolumeSettings_MissingWriterIs405 锁定后端未实现 SettingsWriter ⇒ 405。
func TestVolumeSettings_MissingWriterIs405(t *testing.T) {
	t.Parallel()
	h := newSettingsTestHandlers(t, "ro", providerOnlyBackend{})
	rr := doSettingsReq(t, settingsRoutes(h), settingsCtx("boss", string(accesskey.RoleAdmin)),
		http.MethodPut, "/api/volumes/ro/settings/block_policy", []byte(`{"rules":[]}`))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d want 405 body=%s", rr.Code, rr.Body.String())
	}
}

// TestVolumeSettings_DetailShape 锁定详情回包结构（volume + capabilities + settings）。
func TestVolumeSettings_DetailShape(t *testing.T) {
	t.Parallel()
	be := &fakeSettingsBackend{sections: []registry.VolumeSettings{{
		Key: "block_policy", Label: "分块规则", Value: json.RawMessage(`{"rules":[],"version":2}`),
	}}}
	h := newSettingsTestHandlers(t, "vault", be)
	rr := doSettingsReq(t, settingsRoutes(h), settingsCtx("alice", string(accesskey.RoleUser)),
		http.MethodGet, "/api/volumes/vault/detail", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Volume       map[string]any            `json:"volume"`
		Capabilities []string                  `json:"capabilities"`
		Settings     []registry.VolumeSettings `json:"settings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析回包: %v body=%s", err, rr.Body.String())
	}
	if got.Volume["name"] != "vault" {
		t.Fatalf("volume.name=%v", got.Volume["name"])
	}
	if len(got.Settings) != 1 || got.Settings[0].Key != "block_policy" {
		t.Fatalf("settings=%+v", got.Settings)
	}
	if !slicesContains(got.Capabilities, "settings") {
		t.Fatalf("capabilities=%v 应含 settings", got.Capabilities)
	}
}

// TestVolumeSettings_NoProviderEmptyList 锁定后端不支持 settings ⇒ 详情返回空列表（不报错）。
func TestVolumeSettings_NoProviderEmptyList(t *testing.T) {
	t.Parallel()
	h := newSettingsTestHandlers(t, "plain", providerOnlyBackend{})
	// providerOnly 实现了 Provider 但返回 nil ⇒ 列表为空（等价于「无可调配置」）
	rr := doSettingsReq(t, settingsRoutes(h), settingsCtx("alice", string(accesskey.RoleUser)),
		http.MethodGet, "/api/volumes/plain/settings", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Settings []registry.VolumeSettings `json:"settings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(got.Settings) != 0 {
		t.Fatalf("应为空列表，得到 %+v", got.Settings)
	}
}

// TestVolumeSettings_AuditOnBothOutcomes 锁定成功与拒绝都记业务审计，且不含规则全文。
func TestVolumeSettings_AuditOnBothOutcomes(t *testing.T) {
	t.Parallel()
	h := newSettingsTestHandlers(t, "av", &fakeSettingsBackend{sections: []registry.VolumeSettings{{
		Key: "block_policy", Value: json.RawMessage(`{"rules":[]}`),
	}}})
	attachTestAudit(t, h)

	body := []byte(`{"rules":[{"path_regex":"^secret/","mode":"single","max_size":"1MiB"}]}`)
	doSettingsReq(t, settingsRoutes(h), settingsCtx("boss", string(accesskey.RoleAdmin)),
		http.MethodPut, "/api/volumes/av/settings/block_policy", body)
	doSettingsReq(t, settingsRoutes(h), settingsCtx("alice", string(accesskey.RoleUser)),
		http.MethodPut, "/api/volumes/av/settings/block_policy", body)

	seen, err := h.auditSvc.Query(context.Background(), audit.EntryFilter{Action: auditActionVolumeSettingsUpdate})
	if err != nil {
		t.Fatalf("查审计: %v", err)
	}
	var okN, deniedN int
	for _, e := range seen {
		if strings.Contains(e.Detail, "path_regex") {
			t.Fatalf("审计不得含规则全文: %q", e.Detail)
		}
		if string(e.Result) == "denied" {
			deniedN++
		} else {
			okN++
		}
	}
	if okN == 0 || deniedN == 0 {
		t.Fatalf("成功=%d 拒绝=%d（两者都应记录）", okN, deniedN)
	}
}

// TestVolumeSettings_PutRejectsOversizeBody 锁定请求体上限（防大 payload 打内存）。
func TestVolumeSettings_PutRejectsOversizeBody(t *testing.T) {
	t.Parallel()
	h := newSettingsTestHandlers(t, "big", &fakeSettingsBackend{})
	big := bytes.Repeat([]byte("a"), (1<<20)+16)
	rr := doSettingsReq(t, settingsRoutes(h), settingsCtx("boss", string(accesskey.RoleAdmin)),
		http.MethodPut, "/api/volumes/big/settings/block_policy", big)
	if rr.Code != http.StatusBadRequest && rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限请求应被拒，code=%d", rr.Code)
	}
}

// fakeSyncFS 是满足 sync.FS 的空实现（本文件只测 HTTP 门禁，不触达卷内容）。
type fakeSyncFS struct{}

func (fakeSyncFS) ListDir(context.Context, string) ([]syncpkg.Entry, error) { return nil, nil }
func (fakeSyncFS) Stat(context.Context, string) (*syncpkg.Entry, error) {
	return nil, nil
}
func (fakeSyncFS) OpenRead(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}
func (fakeSyncFS) WriteFile(context.Context, syncpkg.LogicalKey, string, io.Reader, int64, int64) error {
	return errors.New("not implemented")
}
func (fakeSyncFS) Rename(context.Context, string, string) error { return errors.New("not implemented") }
func (fakeSyncFS) Delete(context.Context, string) error         { return errors.New("not implemented") }
func (fakeSyncFS) MakeDir(context.Context, string) error        { return errors.New("not implemented") }

// TestVolumeSettingsRoutes_Registered 锁定三条路由已注册进装配路由表，
// 且既有两段字面路由（export/user）不被 `{name}` 通配吞掉（Go 1.22 ServeMux 字面段优先）。
func TestVolumeSettingsRoutes_Registered(t *testing.T) {
	t.Parallel()
	want := []string{
		"GET /api/volumes/{name}/detail",
		"GET /api/volumes/{name}/settings",
		"PUT /api/volumes/{name}/settings/{section}",
	}
	got := map[string]bool{}
	for _, r := range fileDomainRoutes() {
		got[r.Method+" "+r.Path] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("路由未注册: %s", w)
		}
	}
	// 既有字面路由仍在（没有被通配覆盖删除）
	for _, k := range []string{"GET /api/volumes/export", "GET /api/volumes/user", "POST /api/volumes/refresh"} {
		if !got[k] {
			t.Errorf("既有路由丢失: %s", k)
		}
	}
}
