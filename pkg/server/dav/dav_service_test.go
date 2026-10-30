// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dav

// dav_service_test.go 覆盖 WebDAV 挂载面（Service/Handler）的装配与请求分支：
//   - New 保存依赖；
//   - bindVolumeForAudit 的显式卷 / 默认卷 / 未装配卷集合；
//   - Handler 的卷可用性拒绝（未装配/未知/ACL/默认卷禁用/租户不可用）与 user 桶创建失败；
//   - 经 Handler 的 WebDAV 全流程（PROPFIND/MKCOL/PUT/GET/MOVE/COPY/DELETE）；
//   - /dav 前缀剥离、路径规范化（.. 不出 user 桶）与方法不支持。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// newTestRoot 在临时目录打开一个存储根（t.Cleanup 关闭）。
func newTestRoot(t *testing.T) (*storage.Root, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot(%s): %v", dir, err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root, dir
}

// newTestTenant 在 root 下打开 owner 的租户（预建 meta 桶，与生产默认卷同布局）。
func newTestTenant(t *testing.T, root *storage.Root, owner string) *storage.Tenant {
	t.Helper()
	tnt, err := storage.OpenTenant(root, owner, storage.WithMetaBucket())
	if err != nil {
		t.Fatalf("OpenTenant(%s): %v", owner, err)
	}
	t.Cleanup(func() { _ = tnt.Root().Close() })
	return tnt
}

// newVolumeSet 构造含若干本地卷的运行时卷集合（每卷独立临时根）。
func newVolumeSet(t *testing.T, names ...string) *registry.Set {
	t.Helper()
	roots := make(map[string]*storage.Root, len(names))
	vols := make([]volume.Volume, 0, len(names))
	for _, name := range names {
		root, _ := newTestRoot(t)
		roots[name] = root
		vols = append(vols, volume.Volume{Name: name})
	}
	if len(vols) == 0 {
		return registry.NewSet(nil, nil, nil, nil, "")
	}
	set := registry.NewSet(vols, roots, nil, nil, names[0])
	t.Cleanup(func() { _ = set.Close() })
	return set
}

// handlerFixture 是 Handler 测试的最小装配（默认卷 + 指定 owner 的租户）。
type handlerFixture struct {
	svc        *Service
	root       *storage.Root
	dir        string
	tenant     *storage.Tenant
	auditNames []string
}

// newHandlerFixture 构造 Handler 测试装配：默认卷恒允许，全部请求主体固定为 owner。
func newHandlerFixture(t *testing.T, owner string, metaPolicy MetaPolicy) *handlerFixture {
	t.Helper()
	root, dir := newTestRoot(t)
	tnt := newTestTenant(t, root, owner)
	f := &handlerFixture{root: root, dir: dir, tenant: tnt}
	f.svc = New(Deps{
		Logger:           testLogger(),
		OwnerFromRequest: func(*http.Request) string { return owner },
		TenantFor: func(o string) *storage.Tenant {
			if o == owner {
				return tnt
			}
			return nil
		},
		DefaultVolumeAllows: func(string) bool { return true },
		Meta:                metaPolicy,
		WithAuditVolume: func(ctx context.Context, name string) context.Context {
			f.auditNames = append(f.auditNames, name)
			return ctx
		},
	})
	return f
}

// doDavRequest 直接驱动 Service.Handler（不经过网络；无监听端口）。
func doDavRequest(t *testing.T, svc *Service, method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	svc.Handler(rec, req)
	return rec
}

// TestNew_StoresDeps 钉住构造函数保留依赖（Handler 依赖全部经 Deps 注入）。
func TestNew_StoresDeps(t *testing.T) {
	t.Parallel()
	var called bool
	svc := New(Deps{
		OwnerFromRequest: func(*http.Request) string {
			called = true
			return "alice"
		},
	})
	if svc == nil {
		t.Fatal("New 不应返回 nil")
	}
	if got := svc.deps.OwnerFromRequest(httptest.NewRequest(http.MethodGet, "/dav/", nil)); got != "alice" || !called {
		t.Fatalf("Deps 未按注入保存: got=%q called=%v", got, called)
	}
}

// TestBindVolumeForAudit_SelectsVolumeName 覆盖审计卷名绑定：显式卷优先、缺省取默认卷、
// 未装配卷集合不追加空名。
func TestBindVolumeForAudit_SelectsVolumeName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		volumes  *registry.Set
		explicit string
		want     string
	}{
		{"显式卷优先", newVolumeSet(t, "vdef", "v2"), "v2", "v2"},
		{"缺省取默认卷", newVolumeSet(t, "vdef"), "", "vdef"},
		{"未装配卷集合", nil, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			svc := New(Deps{
				Volumes: tc.volumes,
				WithAuditVolume: func(ctx context.Context, name string) context.Context {
					got = append(got, name)
					return ctx
				},
			})
			req := httptest.NewRequest(http.MethodGet, "/dav/", nil)
			_ = svc.bindVolumeForAudit(req, tc.explicit)
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("审计卷名 = %v, want [%q]", got, tc.want)
			}
		})
	}
}

// TestHandler_RejectsUnavailableVolumes 覆盖 Handler 的卷门禁：未装配卷集合、未知卷、
// ACL 拒绝、默认卷禁用、显式卷无本地根 —— 均须 fail-closed，绝不能落到默认租户。
func TestHandler_RejectsUnavailableVolumes(t *testing.T) {
	t.Parallel()

	allowRoot, _ := newTestRoot(t)
	denyRoot, _ := newTestRoot(t)
	aclSet := registry.NewSet(
		[]volume.Volume{
			{Name: "vdef"},
			{Name: "vallow", ACL: volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{"alice": {}}}},
			{Name: "vdeny", ACL: volume.ACL{Mode: volume.ModeDeny, Owners: map[string]struct{}{"alice": {}}}},
			{Name: "vnoRoot"},
		},
		map[string]*storage.Root{"vdef": allowRoot, "vallow": allowRoot, "vdeny": denyRoot},
		nil, nil, "vdef",
	)
	t.Cleanup(func() { _ = aclSet.Close() })

	tests := []struct {
		name     string
		volumes  *registry.Set
		target   string
		allowDef bool
		wantCode int
		wantText string
	}{
		{"未装配卷集合", nil, "/dav/?volume=v1", true, http.StatusBadRequest, "卷集合未装配"},
		{"未知卷", aclSet, "/dav/?volume=missing", true, http.StatusForbidden, "卷不可用"},
		{"ACL 拒绝", aclSet, "/dav/?volume=vdeny", true, http.StatusForbidden, "卷不可用"},
		{"显式卷无本地根", aclSet, "/dav/?volume=vnoRoot", true, http.StatusBadRequest, "卷不存在"},
		{"默认卷禁用", nil, "/dav/", false, http.StatusForbidden, "卷不可用"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := New(Deps{
				Logger:              testLogger(),
				OwnerFromRequest:    func(*http.Request) string { return "alice" },
				Volumes:             tc.volumes,
				DefaultVolumeAllows: func(string) bool { return tc.allowDef },
				WithAuditVolume:     func(ctx context.Context, _ string) context.Context { return ctx },
			})
			rec := doDavRequest(t, svc, "PROPFIND", tc.target, "", nil)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body=%q)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantText) {
				t.Fatalf("body = %q, want 含 %q", rec.Body.String(), tc.wantText)
			}
		})
	}
}

// TestHandler_AnonymousOwner 未认证请求（OwnerFromRequest 空）落到 anonymous 租户，根可列举。
// 本包不承担认证（authMiddleware 在装配层）；此处钉住「空 owner 仍可服务其匿名租户」语义。
func TestHandler_AnonymousOwner(t *testing.T) {
	t.Parallel()
	root, dir := newTestRoot(t)
	tnt := newTestTenant(t, root, storage.AnonymousOwner)
	var audits []string
	svc := New(Deps{
		Logger:           testLogger(),
		OwnerFromRequest: func(*http.Request) string { return "" },
		TenantFor: func(o string) *storage.Tenant {
			if o == "" {
				return tnt
			}
			return nil
		},
		DefaultVolumeAllows: func(string) bool { return true },
		WithAuditVolume: func(ctx context.Context, name string) context.Context {
			audits = append(audits, name)
			return ctx
		},
	})
	if rec := doDavRequest(t, svc, "PROPFIND", "/dav/", "", map[string]string{"Depth": "1"}); rec.Code != http.StatusMultiStatus {
		t.Fatalf("匿名租户 PROPFIND = %d body=%q", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, storage.AnonymousOwner, "user")); err != nil {
		t.Fatalf("匿名租户 user 桶应预建: %v", err)
	}
	if len(audits) != 1 || audits[0] != "" {
		t.Fatalf("未装配卷集合时审计卷名为空, got %v", audits)
	}
}

// TestHandler_RejectsUnavailableTenant 租户不可用时返回 400（fail-closed，不回落父根）。
func TestHandler_RejectsUnavailableTenant(t *testing.T) {
	t.Parallel()
	svc := New(Deps{
		Logger:              testLogger(),
		OwnerFromRequest:    func(*http.Request) string { return "alice" },
		DefaultVolumeAllows: func(string) bool { return true },
		TenantFor:           func(string) *storage.Tenant { return nil },
		WithAuditVolume:     func(ctx context.Context, _ string) context.Context { return ctx },
	})
	rec := doDavRequest(t, svc, "PROPFIND", "/dav/", "", nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "卷不存在") {
		t.Fatalf("租户不可用应 400 卷不存在, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// TestHandler_UserBucketCreateFailure user 桶路径被同名文件占用 → MkdirAll 失败 → 500。
func TestHandler_UserBucketCreateFailure(t *testing.T) {
	t.Parallel()
	f := newHandlerFixture(t, "alice", nil)
	if err := os.WriteFile(filepath.Join(f.dir, "alice", "user"), []byte("x"), 0o644); err != nil {
		t.Fatalf("占位 user 文件: %v", err)
	}
	rec := doDavRequest(t, f.svc, "PROPFIND", "/dav/", "", nil)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "创建 user 桶失败") {
		t.Fatalf("user 桶创建失败应 500, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// TestHandler_ExplicitVolume ACL 允许的显式卷可正常服务（tenant 经 Volumes.Tenant 解析）。
func TestHandler_ExplicitVolume(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	set := registry.NewSet(
		[]volume.Volume{
			{Name: "vdef"},
			{Name: "vwork", ACL: volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{"alice": {}}}},
		},
		map[string]*storage.Root{"vdef": root, "vwork": root},
		nil, nil, "vdef",
	)
	t.Cleanup(func() { _ = set.Close() })
	svc := New(Deps{
		Logger:              testLogger(),
		OwnerFromRequest:    func(*http.Request) string { return "alice" },
		Volumes:             set,
		DefaultVolumeAllows: func(string) bool { return true },
		WithAuditVolume:     func(ctx context.Context, _ string) context.Context { return ctx },
	})
	rec := doDavRequest(t, svc, "PROPFIND", "/dav/?volume=vwork", "", map[string]string{"Depth": "1"})
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("显式卷 PROPFIND = %d body=%q", rec.Code, rec.Body.String())
	}
}

// TestHandler_WebDAVCRUDFlow 经 Handler 走完整 WebDAV 流程，并逐项断言 sidecar 联动：
// PUT 建 sidecar、MOVE 迁移、COPY 重建、DELETE 清扫。
func TestHandler_WebDAVCRUDFlow(t *testing.T) {
	t.Parallel()
	f := newHandlerFixture(t, "alice", testMetaPolicy{})

	// 根 PROPFIND（Depth 1）：Handler 预建 user 桶后可列举。
	if rec := doDavRequest(t, f.svc, "PROPFIND", "/dav/", "", map[string]string{"Depth": "1"}); rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND 根 = %d body=%q", rec.Code, rec.Body.String())
	}
	if rec := doDavRequest(t, f.svc, "MKCOL", "/dav/docs", "", nil); rec.Code != http.StatusCreated {
		t.Fatalf("MKCOL = %d body=%q", rec.Code, rec.Body.String())
	}

	const payload = "hello webdav"
	if rec := doDavRequest(t, f.svc, http.MethodPut, "/dav/docs/a.txt", payload, nil); rec.Code != http.StatusCreated {
		t.Fatalf("PUT = %d body=%q", rec.Code, rec.Body.String())
	}
	// PUT 后 sidecar 必须落在租户 meta 桶（写侧联动；否则读路径按旧/缺 meta 处理）。
	if _, err := f.tenant.Root().Stat(meta.MetaPath("user/docs/a.txt")); err != nil {
		t.Fatalf("PUT 后应有 sidecar: %v", err)
	}

	rec := doDavRequest(t, f.svc, http.MethodGet, "/dav/docs/a.txt", "", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != payload {
		t.Fatalf("GET = %d body=%q", rec.Code, rec.Body.String())
	}
	if rec := doDavRequest(t, f.svc, "PROPFIND", "/dav/docs", "", map[string]string{"Depth": "1"}); rec.Code != http.StatusMultiStatus ||
		!strings.Contains(rec.Body.String(), "a.txt") {
		t.Fatalf("PROPFIND docs = %d body=%q", rec.Code, rec.Body.String())
	}

	// MOVE：源 sidecar 迁移到新位置（不留孤儿）。
	host := "http://example.com"
	if rec := doDavRequest(t, f.svc, "MOVE", "/dav/docs/a.txt", "", map[string]string{"Destination": host + "/docs/b.txt"}); rec.Code != http.StatusCreated {
		t.Fatalf("MOVE = %d body=%q", rec.Code, rec.Body.String())
	}
	if _, err := f.tenant.Root().Stat(meta.MetaPath("user/docs/b.txt")); err != nil {
		t.Fatalf("MOVE 后目标应有 sidecar: %v", err)
	}
	if _, err := f.tenant.Root().Stat(meta.MetaPath("user/docs/a.txt")); err == nil {
		t.Fatal("MOVE 后源 sidecar 应随迁不残留")
	}

	// COPY：目标重建 sidecar，源保留。
	if rec := doDavRequest(t, f.svc, "COPY", "/dav/docs/b.txt", "", map[string]string{"Destination": host + "/docs/c.txt"}); rec.Code != http.StatusCreated {
		t.Fatalf("COPY = %d body=%q", rec.Code, rec.Body.String())
	}
	if _, err := f.tenant.Root().Stat(meta.MetaPath("user/docs/c.txt")); err != nil {
		t.Fatalf("COPY 后目标应有 sidecar: %v", err)
	}

	// DELETE：主文件 + sidecar 一并清扫。
	if rec := doDavRequest(t, f.svc, http.MethodDelete, "/dav/docs/c.txt", "", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d body=%q", rec.Code, rec.Body.String())
	}
	if _, err := f.tenant.Root().Stat(meta.MetaPath("user/docs/c.txt")); err == nil {
		t.Fatal("DELETE 后 sidecar 应已清扫")
	}

	// 审计第 6 维：默认卷名已绑定。
	if len(f.auditNames) == 0 || f.auditNames[0] != "" {
		t.Fatalf("默认卷（未传 ?volume=）应绑定空卷名, got %v", f.auditNames)
	}
}

// TestHandler_NormalizesDavPrefixAndTraversal 覆盖 /dav 前缀剥离与 .. 规范化：
// 无尾斜杠的 /dav 映射为根；../.. 被吸收在 user 桶内，绝不越界。
func TestHandler_NormalizesDavPrefixAndTraversal(t *testing.T) {
	t.Parallel()
	f := newHandlerFixture(t, "alice", nil)

	if rec := doDavRequest(t, f.svc, "PROPFIND", "/dav", "", map[string]string{"Depth": "1"}); rec.Code != http.StatusMultiStatus {
		t.Fatalf("/dav（无尾斜杠）PROPFIND = %d body=%q", rec.Code, rec.Body.String())
	}

	if rec := doDavRequest(t, f.svc, http.MethodPut, "/dav/../../escape.txt", "escaped", nil); rec.Code != http.StatusCreated {
		t.Fatalf("含 .. 的 PUT = %d body=%q", rec.Code, rec.Body.String())
	}
	// 越界段被 cleanRel 吸收到 user 桶内：文件必须落在 <tenant>/user/escape.txt。
	got, err := os.ReadFile(filepath.Join(f.dir, "alice", "user", "escape.txt"))
	if err != nil || string(got) != "escaped" {
		t.Fatalf(".. 路径应归一化到 user 桶内: data=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "escape.txt")); err == nil {
		t.Fatal(".. 路径不得逃出 user 桶")
	}
}

// TestHandler_UnsupportedMethod 不支持的方法由 WebDAV 核心拒绝（400），不得触达文件系统。
func TestHandler_UnsupportedMethod(t *testing.T) {
	t.Parallel()
	f := newHandlerFixture(t, "alice", nil)
	rec := doDavRequest(t, f.svc, http.MethodPatch, "/dav/x.txt", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH = %d, want 400", rec.Code)
	}
}
