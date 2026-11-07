// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_volume_dimension_test.go 锁定评审 R13 的**卷维度**（`Entry.Volume`）：
//   - 此前该字段从未赋值、读面也没有该维度 ⇒ 取证时无法回答「这件事发生在哪个卷」；
//   - 现在语义为：`AuditEvent.Volume` 显式 > ctx 绑定（`?volume=` 中间件 / DAV/S3 解析点）
//     > **单卷部署**的文件/卷域事件补默认卷名 > 空（多卷下不猜默认卷，避免记错归属）。
//
// 每个用例都断言**为什么**：多卷不猜 = 防止自动放置落到非默认卷时记错卷（这是数据正确性，
// 不是风格问题）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// volumeDimSet 构造只含**卷元数据**（无根/后端）的卷集：审计维度解析只读 Name/Default。
func volumeDimSet(names ...string) *registry.Set {
	vols := make([]volume.Volume, 0, len(names))
	for _, n := range names {
		vols = append(vols, volume.Volume{Name: n})
	}
	return registry.NewSet(vols, nil, nil, nil, names[0])
}

// auditVolumeOf 读回最近一条审计事件的卷维度（走 `/api/audit` 同一读入口）。
func auditVolumeOf(t *testing.T, h *Handlers, f audit.EntryFilter) []string {
	t.Helper()
	evs := h.readAuditEvents(context.Background(), f, 50, false)
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Volume)
	}
	return out
}

// TestAuditVolumeMiddleware_BindsQueryParam 请求携带 `?volume=` ⇒ ctx 可见（写面零改动即可带卷）。
func TestAuditVolumeMiddleware_BindsQueryParam(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	var got string
	hnd := h.auditVolumeMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = auditVolumeFrom(r.Context())
	}))

	hnd.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/upload?volume=v2", nil))
	if got != "v2" {
		t.Fatalf("显式 volume 参数应绑定进 ctx, got %q", got)
	}

	got = "sentinel"
	hnd.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/upload", nil))
	if got != "" {
		t.Fatalf("无参数时不得凭空绑定卷, got %q", got)
	}
}

// TestAuditVolume_RecordedFromCtx ctx 绑定 ⇒ 条目带卷。
func TestAuditVolume_RecordedFromCtx(t *testing.T) {
	t.Parallel()
	h := &Handlers{volSet: volumeDimSet("v1", "v2")}
	attachTestAudit(t, h)

	ctx := withAuditVolume(context.Background(), "v2")
	h.RecordAudit(ctx, AuditEvent{Action: "file.delete", ObjectType: "file", Object: "a.txt", Result: "success"})

	got := auditVolumeOf(t, h, audit.EntryFilter{Action: "file.delete"})
	if len(got) != 1 || got[0] != "v2" {
		t.Fatalf("条目应带 ctx 绑定的卷 v2, got %v", got)
	}
}

// TestAuditVolume_ExplicitWinsOverCtx 显式 Volume 优先（跨卷/后台任务语义）。
func TestAuditVolume_ExplicitWinsOverCtx(t *testing.T) {
	t.Parallel()
	h := &Handlers{volSet: volumeDimSet("v1", "v2")}
	attachTestAudit(t, h)

	ctx := withAuditVolume(context.Background(), "v2")
	h.RecordAudit(ctx, AuditEvent{
		Action: "file.copy", ObjectType: "file", Object: "a.txt",
		Volume: "v1", Result: "success",
	})

	got := auditVolumeOf(t, h, audit.EntryFilter{Action: "file.copy"})
	if len(got) != 1 || got[0] != "v1" {
		t.Fatalf("显式 Volume 应优先于 ctx, got %v", got)
	}
}

// TestAuditVolume_SingleVolumeFallback 单卷部署：文件域事件无绑定时补唯一卷名（无歧义）。
func TestAuditVolume_SingleVolumeFallback(t *testing.T) {
	t.Parallel()
	h := &Handlers{volSet: volumeDimSet("only")}
	attachTestAudit(t, h)

	h.RecordAudit(context.Background(), AuditEvent{Action: "file.write", ObjectType: "file", Object: "a.txt", Result: "success"})

	got := auditVolumeOf(t, h, audit.EntryFilter{Action: "file.write"})
	if len(got) != 1 || got[0] != "only" {
		t.Fatalf("单卷部署应补唯一卷名, got %v", got)
	}
}

// TestAuditVolume_MultiVolumeDoesNotGuess 多卷部署：未绑定时**留空**（自动放置可能落在非默认卷，
// 猜默认卷会记错归属 ⇒ 数据正确性优先）。
func TestAuditVolume_MultiVolumeDoesNotGuess(t *testing.T) {
	t.Parallel()
	h := &Handlers{volSet: volumeDimSet("v1", "v2")}
	attachTestAudit(t, h)

	h.RecordAudit(context.Background(), AuditEvent{Action: "file.write", ObjectType: "file", Object: "a.txt", Result: "success"})

	got := auditVolumeOf(t, h, audit.EntryFilter{Action: "file.write"})
	if len(got) != 1 || got[0] != "" {
		t.Fatalf("多卷且未绑定时应留空（不猜默认卷）, got %v", got)
	}
}

// TestAuditVolume_ConfigEventStaysEmpty 非卷域事件（配置/认证）不得被标到某个卷上。
func TestAuditVolume_ConfigEventStaysEmpty(t *testing.T) {
	t.Parallel()
	h := &Handlers{volSet: volumeDimSet("only")}
	attachTestAudit(t, h)

	h.RecordAudit(context.Background(), AuditEvent{Action: "config_update", ObjectType: "config", Object: "audit", Result: "success"})

	got := auditVolumeOf(t, h, audit.EntryFilter{Action: "config_update"})
	if len(got) != 1 || got[0] != "" {
		t.Fatalf("非卷域事件不得带卷, got %v", got)
	}
}

// TestAuditVolume_ReadFaceFilterAndProjection 读面按卷筛选 + JSON 投影含 volume 字段。
func TestAuditVolume_ReadFaceFilterAndProjection(t *testing.T) {
	t.Parallel()
	h := &Handlers{volSet: volumeDimSet("v1", "v2")}
	attachTestAudit(t, h)

	ctx := context.Background()
	h.RecordAudit(withAuditVolume(ctx, "v1"), AuditEvent{Action: "file.delete", ObjectType: "file", Object: "a", Result: "success"})
	h.RecordAudit(withAuditVolume(ctx, "v2"), AuditEvent{Action: "file.delete", ObjectType: "file", Object: "b", Result: "success"})

	got := auditVolumeOf(t, h, audit.EntryFilter{Volume: "v2"})
	if len(got) != 1 || got[0] != "v2" {
		t.Fatalf("按卷筛选应只返回该卷条目, got %v", got)
	}

	// 投影必须含 volume（Web UI / 导出消费该字段）。
	evs := h.readAuditEvents(ctx, audit.EntryFilter{Volume: "v2"}, 10, false)
	if len(evs) != 1 || evs[0].Volume != "v2" {
		t.Fatalf("读面投影应带 volume, got %+v", evs)
	}
}

// TestAuditVolume_UnknownVolumeNotBound R14 C12：`?volume=` 必须**校验存在性**后才绑定。
// 修复前把原始值直接绑进 ctx ⇒ 单卷部署下可伪造审计归属，并压制「单卷兜底」（真值本可唯一推出）。
func TestAuditVolume_UnknownVolumeNotBound(t *testing.T) {
	t.Parallel()
	h := &Handlers{volSet: volumeDimSet("only")}
	attachTestAudit(t, h)

	var bound string
	hnd := h.auditVolumeMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		bound = auditVolumeFrom(r.Context())
	}))
	hnd.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/upload?volume=ghost", nil))
	if bound != "" {
		t.Fatalf("不存在的卷不得绑定进 ctx（可伪造归属），got %q", bound)
	}

	// 真实卷仍应绑定。
	hnd.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/upload?volume=only", nil))
	if bound != "only" {
		t.Fatalf("存在的卷应绑定, got %q", bound)
	}
}
