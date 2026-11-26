// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/audit"
	"github.com/cocomhub/sproxy/pkg/state"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// volume_settings_api_test.go 锁定卷详情/设置 HTTP 面的三件事：
//  ① 权限门顺序（可见性 → 归属/admin）与「他人私有卷 404 防枚举」；
//  ② 错误 → 状态码映射（400/405/409/500）；
//  ③ 业务审计在成功与拒绝两条路径都发出，且不含规则全文。

// settingsCtx 构造带 Principal **与 actor** 的请求 ctx（认证面的输出形态）。
// 两者都要：owner 经 `ownerFromRequest`（= ActorFrom）取值，角色经 PrincipalFrom 取值；
// 生产侧 authMiddleware 同时写入二者，测试必须镜像——只写 Principal 会让 owner 恒为空
// （退化成 anonymous），使「自己的用户卷」判据假通过。
func settingsCtx(owner, role string) context.Context {
	ctx := withPrincipal(context.Background(), &Principal{Owner: owner, Role: role})
	return withActor(ctx, owner)
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
	// registry.Set 内部为每个卷懒建 TenantCache，缓存着**租户子根**（独立 os.Root，无
	// finalizer）：不关时请求路径懒建的租户根会持有 <卷根>/<owner>/ 目录句柄，
	// Windows 上 t.TempDir() 的 RemoveAll 报 `being used by another process`。
	t.Cleanup(func() { _ = h.volSet.Close() })

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
		{"payload invalid → 400", fmt.Errorf("%w: 缺 max_size", registry.ErrSettingsPayloadInvalid), http.StatusBadRequest},
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

// ---- 审查修复回归 ----

// TestVolumeSettings_PrivateUserVolumeOfOthersIs404 锁定防枚举：他人 owner-only 用户卷
// 必须 404（不是 403/200），与 deleteUserVolumeHandler 同源判据。
func TestVolumeSettings_PrivateUserVolumeOfOthersIs404(t *testing.T) {
	t.Parallel()
	h := newSettingsTestHandlers(t, "alice-vault", &fakeSettingsBackend{sections: []registry.VolumeSettings{{
		Key: "block_policy", Value: json.RawMessage(`{"rules":[]}`),
	}}})
	// 把该卷登记成 alice 的用户卷（owner-only ACL：Mode=allow + 单 owner 白名单）
	uv := &UserVolumeStore{root: t.TempDir(), ownerLocks: map[string]*sync.Mutex{}}
	if err := uv.Create("alice", UserVolume{
		Name: "alice-vault", Type: "secretdata",
		ACL: volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{"alice": {}}},
	}); err != nil {
		t.Fatalf("Create user volume: %v", err)
	}
	h.userVolumes = uv
	// 重新登记带 owner-only ACL 的卷描述
	_ = h.volSet.RemoveExternalVolume("alice-vault")
	if err := h.volSet.AddExternalVolume(volume.Volume{
		Name: "alice-vault", Type: "secretdata",
		ACL: volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{"alice": {}}},
	}, &fakeSettingsBackend{}); err != nil {
		t.Fatalf("AddExternalVolume: %v", err)
	}

	// bob 看不到 ⇒ 读与写都 404
	for _, tc := range []struct {
		method, target string
		body           []byte
	}{
		{http.MethodGet, "/api/volumes/alice-vault/detail", nil},
		{http.MethodGet, "/api/volumes/alice-vault/settings", nil},
		{http.MethodPut, "/api/volumes/alice-vault/settings/block_policy", []byte(`{"rules":[]}`)},
	} {
		rr := doSettingsReq(t, settingsRoutes(h), settingsCtx("bob", string(accesskey.RoleUser)), tc.method, tc.target, tc.body)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s %s: code=%d want 404（防枚举）body=%s", tc.method, tc.target, rr.Code, rr.Body.String())
		}
	}
	// alice 自己能写
	rr := doSettingsReq(t, settingsRoutes(h), settingsCtx("alice", string(accesskey.RoleUser)),
		http.MethodPut, "/api/volumes/alice-vault/settings/block_policy", []byte(`{"rules":[]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("owner 写自己的用户卷应 200，得到 %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestVolumeSettings_LocalVolumeReturnsEmptySettings 锁定本地卷（无外部后端）也返回 200 +
// 空 settings（不是 404）：卷表里每个可见卷都有「详情」按钮，点开应是「暂无可调配置」。
func TestVolumeSettings_LocalVolumeReturnsEmptySettings(t *testing.T) {
	t.Parallel()
	// 本地卷登记为**存储卷**（无 External 后端）——卷表会列出它，点详情应是「暂无可调配置」。
	root := t.TempDir()
	r, rerr := storage.OpenRoot(root)
	if rerr != nil {
		t.Fatalf("OpenRoot: %v", rerr)
	}
	t.Cleanup(func() { _ = r.Close() })
	h := &Handlers{}
	h.volSet = registry.NewSet([]volume.Volume{{Name: "main", Type: "local"}}, nil, nil, nil, "main")
	// registry.Set 内部为每个卷懒建 TenantCache，缓存着**租户子根**（独立 os.Root，无
	// finalizer）：不关时请求路径懒建的租户根会持有 <卷根>/<owner>/ 目录句柄，
	// Windows 上 t.TempDir() 的 RemoveAll 报 `being used by another process`。
	t.Cleanup(func() { _ = h.volSet.Close() })

	if err := h.volSet.RegisterStorageVolume(localVolStub{name: "main", root: r}); err != nil {
		t.Fatalf("RegisterStorageVolume: %v", err)
	}
	rr := doSettingsReq(t, settingsRoutes(h), settingsCtx("alice", string(accesskey.RoleUser)),
		http.MethodGet, "/api/volumes/main/detail", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("本地卷详情应 200，得到 %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Settings []registry.VolumeSettings `json:"settings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(got.Settings) != 0 {
		t.Fatalf("本地卷应无设置分区，得到 %+v", got.Settings)
	}
}

// TestVolumeSettings_AuditResultContract 锁定审计 result 取值走契约（success/denied/error），
// 不是自造的 ok/failed —— 否则审计控制台把成功显示成红「失败」、result=success 筛不到。
func TestVolumeSettings_AuditResultContract(t *testing.T) {
	t.Parallel()
	h := newSettingsTestHandlers(t, "av2", &fakeSettingsBackend{})
	attachTestAudit(t, h)
	doSettingsReq(t, settingsRoutes(h), settingsCtx("boss", string(accesskey.RoleAdmin)),
		http.MethodPut, "/api/volumes/av2/settings/block_policy", []byte(`{"rules":[]}`))
	doSettingsReq(t, settingsRoutes(h), settingsCtx("alice", string(accesskey.RoleUser)),
		http.MethodPut, "/api/volumes/av2/settings/block_policy", []byte(`{"rules":[]}`))

	seen, err := h.auditSvc.Query(context.Background(), audit.EntryFilter{Action: auditActionVolumeSettingsUpdate})
	if err != nil {
		t.Fatalf("查审计: %v", err)
	}
	if len(seen) < 2 {
		t.Fatalf("应有成功 + 拒绝两条，得到 %d", len(seen))
	}
	for _, e := range seen {
		switch string(e.Result) {
		case "success", "denied", "error":
		default:
			t.Fatalf("审计 result=%q 不在契约内（success/denied/error）", e.Result)
		}
	}
}

// TestVolumeSettings_InvalidPayloadIs400 锁定客户端载荷非法 → 400（不是 500），
// 且 500 不回内部错误原文。
func TestVolumeSettings_InvalidPayloadIs400(t *testing.T) {
	t.Parallel()
	be := &fakeSettingsBackend{updateErr: registry.ErrSettingsPayloadInvalid}
	h := newSettingsTestHandlers(t, "p1", be)
	rr := doSettingsReq(t, settingsRoutes(h), settingsCtx("boss", string(accesskey.RoleAdmin)),
		http.MethodPut, "/api/volumes/p1/settings/block_policy", []byte(`{`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("载荷非法应 400，得到 %d body=%s", rr.Code, rr.Body.String())
	}
}

// localVolStub 是本地卷（存储卷）的最小白名单实现：只用 Root 的 FS 视图与卷名。
type localVolStub struct {
	name string
	root *storage.Root
}

func (l localVolStub) Name() string      { return l.name }
func (l localVolStub) Kind() volume.Kind { return volume.KindStorage }
func (l localVolStub) FS() syncpkg.FS    { return nil }
func (l localVolStub) Close() error      { return nil }

// TestEnsureSettingsStoreInjected_LazyForRuntimeVolumes 锁定运行期新建的用户卷也能拿到
// 持久化后端（启动时的 AttachVolumeSettingsStores 早于该卷存在 ⇒ 必须惰性补注入，
// 否则用户新建卷后打开详情永远 read_only，直到重启）。
func TestEnsureSettingsStoreInjected_LazyForRuntimeVolumes(t *testing.T) {
	t.Parallel()
	b := &fakeStoreSetterBackend{}
	h := newSettingsTestHandlers(t, "late", b)
	h.SetStateStore(state.NewLocalStateStore(t.TempDir(), nil))

	if b.setCalls != 0 {
		t.Fatalf("初始不应注入，得到 %d", b.setCalls)
	}
	h.ensureSettingsStoreInjected("late", b)
	if b.setCalls != 1 || b.store == nil {
		t.Fatalf("应按需注入一次，得到 calls=%d store=%v", b.setCalls, b.store)
	}
	// 幂等：已注入后不重复
	h.ensureSettingsStoreInjected("late", b)
	if b.setCalls != 1 {
		t.Fatalf("已注入后不应重复注入，得到 %d", b.setCalls)
	}
}

// fakeStoreSetterBackend 记录注入调用（并可选成功/失败）。
type fakeStoreSetterBackend struct {
	fakeSettingsBackend
	setCalls int
	store    registry.SettingsStore
}

func (f *fakeStoreSetterBackend) SetSettingsStore(s registry.SettingsStore) error {
	f.setCalls++
	f.store = s
	return nil
}

// TestAttachVolumeSettingsStores_LocalFallback 锁定默认单机部署（未配 state_store）时，
// 卷设置仍能持久化：装配层用本地 StateStore 兜底。否则「Web UI 动态调整并持久化」这条
// 交付能力只在 mongo/集群下可用，与文档/验收不符。
func TestAttachVolumeSettingsStores_LocalFallback(t *testing.T) {
	t.Parallel()
	b := &fakeStoreSetterBackend{}
	h := newSettingsTestHandlers(t, "localpersist", b)
	// h.stateStore 为 nil（默认 local 单机未装配 StateStore）
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	h.cfgPtr = &cfgPtr
	AttachVolumeSettingsStores(h)
	if b.store == nil {
		t.Fatal("未装配 StateStore 时应回落本地 StateStore（否则设置分区恒只读）")
	}
	if _, err := b.store.Save(context.Background(), "localpersist", json.RawMessage(`{"rules":[]}`), 0); err != nil {
		t.Fatalf("本地 StateStore 应可写: %v", err)
	}
}

// TestSettingsStoreInjectedCache_KeyedByBackend 防止包级注入缓存在测试间串味：
// 缓存按「卷名 + 后端实例」区分（同名不同实例必须重新注入）。
func TestSettingsStoreInjectedCache_KeyedByBackend(t *testing.T) {
	t.Parallel()
	b1 := &fakeStoreSetterBackend{}
	b2 := &fakeStoreSetterBackend{}
	h := newSettingsTestHandlers(t, "dup-name", b1)
	h.SetStateStore(state.NewLocalStateStore(t.TempDir(), nil))
	h.ensureSettingsStoreInjected("dup-name", b1)
	h.ensureSettingsStoreInjected("dup-name", b2)
	if b2.setCalls != 1 {
		t.Fatalf("同名不同后端实例必须各自注入，得到 %d", b2.setCalls)
	}
}

// TestForgetVolumeSettings_ClearsStateAndCache 锁定卷删除时清理持久化设置与注入缓存
// （安全审查 MEDIUM）：设置只按卷名持久化，若删除后残留，同名重建的卷会静默继承旧规则
// ——规则含路径语义，属于跨身份的残留状态。
func TestForgetVolumeSettings_ClearsStateAndCache(t *testing.T) {
	t.Parallel()
	b := &fakeStoreSetterBackend{}
	h := newSettingsTestHandlers(t, "recycled", b)
	st := state.NewLocalStateStore(t.TempDir(), nil)
	h.SetStateStore(st)

	h.ensureSettingsStoreInjected("recycled", b) // 建立注入缓存 + 注入 store
	if b.store == nil {
		t.Fatal("应先注入 store")
	}
	if _, err := b.store.Save(context.Background(), "recycled", json.RawMessage(`{"rules":[]}`), 0); err != nil {
		t.Fatalf("预置持久化规则: %v", err)
	}

	h.forgetVolumeSettings(context.Background(), "recycled")

	raw, ver, err := b.store.Load(context.Background(), "recycled")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if raw != nil || ver != 0 {
		t.Fatalf("卷删除后不应残留设置，得到 (%s,%d)", raw, ver)
	}
	// 注入缓存也要清：同名重建会用**新后端实例**，不该复用旧的「已注入」标记
	if _, done := settingsStoreInjected.Load(settingsInjectKey("recycled", b)); done {
		t.Fatal("删除后应清除该卷的注入缓存")
	}
}

// TestRefreshVolume_ReloadsBlockPolicy 锁定 refresh 入口**顺带重载卷设置**（审查 P1-2）：
// 多实例共享 StateStore 时，A 保存规则后 B 仍用旧规则；`POST /api/volumes/refresh?volume=X`
// 是全仓既有的运维收敛通道，应在这里把规则重新载入（不新增 API）。
func TestRefreshVolume_ReloadsBlockPolicy(t *testing.T) {
	t.Parallel()
	b := &reloadableBackend{}
	h := newSettingsTestHandlers(t, "reload-me", b)
	h.SetStateStore(state.NewLocalStateStore(t.TempDir(), nil))

	if err := h.volumeSettingsRefresh(context.Background(), "reload-me"); err != nil {
		t.Fatalf("volumeSettingsRefresh: %v", err)
	}
	if b.reloadCalls != 1 {
		t.Fatalf("refresh 应调用严格重载一次，得到 %d", b.reloadCalls)
	}
}

// reloadableBackend 实现**严格重载**（SettingsReloader），并记录调用次数。
type reloadableBackend struct {
	fakeSettingsBackend
	reloadCalls int
}

func (b *reloadableBackend) ReloadSettings(context.Context) error {
	b.reloadCalls++
	return nil
}

// TestRefreshVolume_ReloadFailureSurfaces 锁定重载**失败必须可见**（审查 Important）：
// `SetSettingsStore` 在载入失败时只告警返回 nil（挂载期不能阻断），若 refresh 走它做「重载」，
// 多实例收敛实际失败时仍回「已重载」成功回执。故 refresh 必须走能返回错误的显式重载路径。
func TestRefreshVolume_ReloadFailureSurfaces(t *testing.T) {
	t.Parallel()
	b := &failingReloadBackend{}
	h := newSettingsTestHandlers(t, "reload-fail", b)
	h.SetStateStore(state.NewLocalStateStore(t.TempDir(), nil))

	err := h.volumeSettingsRefresh(context.Background(), "reload-fail")
	if err == nil {
		t.Fatal("后端重载失败时 refresh 必须返回错误（不得回「已重载」）")
	}
}

// failingReloadBackend 只实现 SettingsProvider（模拟「重载不可用」的后端）。
type failingReloadBackend struct{ fakeSettingsBackend }

func (f *failingReloadBackend) ReloadSettings(context.Context) error {
	return errors.New("state store unavailable")
}

// TestVolumeSettingsRefresh_UnsupportedMeansNoReload 锁定「包装器转发但 inner 未实现」
// 的情形：必须视为**无重载语义**（返回 ErrUnsupported 供调用方按「不支持」描述），
// 绝不能落回吞错的 Setter 兼容路径（安全审查）。
func TestVolumeSettingsRefresh_UnsupportedMeansNoReload(t *testing.T) {
	t.Parallel()
	// 只实现 Provider+Writer+Setter（无 Reloader）：capacity 包装器的 ReloadSettings 返回
	// ErrUnsupported ⇒ h.volumeSettingsRefresh 应返回 ErrUnsupported（调用方据此判「不支持」）。
	b := &setterOnlyBackend{}
	h := newSettingsTestHandlers(t, "no-reload", b)
	h.SetStateStore(state.NewLocalStateStore(t.TempDir(), nil))
	// inner 无严格重载语义（专用哨兵）⇒ 结论 not_supported，**不回退** Setter 吞错路径
	// （安全审查：不支持就不能伪装成「已重载」，也不该走失败不可见的注入）。
	outcome, err := h.volumeSettingsRefreshOutcome(context.Background(), "no-reload", b)
	if err != nil || outcome != settingsReloadNotSupported {
		t.Fatalf("应为 not_supported 且无错误，得到 %v err=%v", outcome, err)
	}
	if b.setCalls != 0 {
		t.Fatalf("不应回退 Setter 吞错路径，得到 %d 次", b.setCalls)
	}

	// **真实失败绝不回退**（核心：否则重载失败被当成成功）
	f := &failingReloadBackend{}
	h2 := newSettingsTestHandlers(t, "reload-fail2", f)
	h2.SetStateStore(state.NewLocalStateStore(t.TempDir(), nil))
	if err := h2.volumeSettingsRefresh(context.Background(), "reload-fail2"); err == nil {
		t.Fatal("真实重载失败必须返回错误（不得回退兼容路径掩盖）")
	}
}

// setterOnlyBackend 实现 Setter 但不实现 Reloader，并模拟 capacity 包装器的
// 「恒实现 + ErrUnsupported」形态。
type setterOnlyBackend struct {
	fakeSettingsBackend
	setCalls int
}

func (b *setterOnlyBackend) SetSettingsStore(registry.SettingsStore) error {
	b.setCalls++
	return nil
}

func (b *setterOnlyBackend) ReloadSettings(context.Context) error {
	return fmt.Errorf("capacity: backend 未实现 ReloadSettings: %w", registry.ErrSettingsReloadNotImplemented)
}

// failingReloadBackend 模拟 inner 的真实重载失败（不得回退兼容路径）。

// ---- 安全审查（2026-11-27）：重载三态、专用哨兵、审计结果 ----

// TestVolumeSettingsRefresh_ThreeState 锁定 refresh 重载的**三态**结论：
// reloaded / not_supported / failed。文案与审计必须基于结果，而不是「包装器是否实现接口」
// （包装器恒实现 ⇒ 断言恒真 ⇒ 不支持的后端也会被说成「已重载」）。
func TestVolumeSettingsRefresh_ThreeState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		be   registry.ExternalBackend
		want settingsReloadOutcome
	}{
		{"reloader ok", &reloadableBackend{}, settingsReloaded},
		{"reloader NotSupported", &notSupportedReloadBackend{}, settingsReloadNotSupported},
		{"reloader real failure", &failingReloadBackend{}, settingsReloadFailed},
		// inner 恰好返回通用 ErrUnsupported（语义不同）也必须是 failed，不得被当成「不支持」
		{"reloader generic ErrUnsupported", &genericUnsupportedReloadBackend{}, settingsReloadFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newSettingsTestHandlers(t, "v", &plainSettingsBackend{})
			got, err := h.volumeSettingsRefreshOutcome(context.Background(), "v", c.be)
			if got != c.want {
				t.Fatalf("outcome=%v want %v (err=%v)", got, c.want, err)
			}
			if c.want == settingsReloadFailed && err == nil {
				t.Fatal("failed 必须带错误")
			}
			if c.want != settingsReloadFailed && err != nil {
				t.Fatalf("非 failed 不应带错误: %v", err)
			}
		})
	}
}

// TestVolumeSettingsReload_ErrorNotLeakedToAudit 锁定失败原因**分类化**：message 与审计 detail
// 只出现类别，不出现内部 error 文本（可能是路径/驱动细节）。
func TestVolumeSettingsReload_ErrorNotLeakedToAudit(t *testing.T) {
	t.Parallel()
	b := &failingReloadBackend{} // 错误文本含 "state store unavailable"
	h := newSettingsTestHandlers(t, "leak", b)
	h.SetStateStore(state.NewLocalStateStore(t.TempDir(), nil))
	attachTestAudit(t, h)

	r := httptest.NewRequest(http.MethodPost, "/api/volumes/refresh?volume=leak", nil)
	note, detail, auditResult := h.reloadVolumeSettingsNote(r.WithContext(settingsCtx("boss", string(accesskey.RoleAdmin))), "leak")
	if auditResult != AuditResultError {
		t.Fatalf("重载失败时审计 result 应为 error，得到 %q", auditResult)
	}
	for _, s := range []string{note, detail} {
		if strings.Contains(s, "state store unavailable") {
			t.Fatalf("不得把内部错误文本写入回显/审计: %q", s)
		}
		if !strings.Contains(s, "重载失败") {
			t.Fatalf("应给出分类化失败说明: %q", s)
		}
	}
}

// 以下是本组用例的后端替身。
type plainSettingsBackend struct{}

func (plainSettingsBackend) FS() syncpkg.FS { return fakeSyncFS{} }
func (plainSettingsBackend) Close() error   { return nil }
func (plainSettingsBackend) Settings(context.Context) ([]registry.VolumeSettings, error) {
	return nil, nil
}

type notSupportedReloadBackend struct{ plainSettingsBackend }

func (notSupportedReloadBackend) ReloadSettings(context.Context) error {
	return fmt.Errorf("capacity: backend 未实现 ReloadSettings: %w", registry.ErrSettingsReloadNotImplemented)
}

type genericUnsupportedReloadBackend struct{ plainSettingsBackend }

func (genericUnsupportedReloadBackend) ReloadSettings(context.Context) error {
	return fmt.Errorf("inner: %w", syncpkg.ErrUnsupported)
}
