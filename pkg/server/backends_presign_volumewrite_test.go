// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// backends_presign_volumewrite_test.go 钉住「**卷上 PUT 直传默认禁**」不变式（#44① (b)）。
//
// 缺口：`POST /api/backends/{type}/presign?method=PUT` 返回签名 URL 后，客户端**直传对象存储**
// ——服务端看不到那次写。`.../presign/complete` 只是**事后登记**（#44① (a) 已实现：经统一写内核
// 结算双账本 + meta）⇒ 客户端完全可以 PUT 后**不调 complete**，在卷内留下**未记账对象**：
// ① owner 配额永不收敛（周期对账只收敛卷池）② 无 sidecar/台账。签发期的封印/占用守卫拦不住它
// （它们只判断「该不该签」，无法阻止直传本身）。
//
// ⇒ 卷上**默认禁** PUT 直传；需按卷显式开启 `extra.allow_presign: true`（默认禁 + 显式豁免）。
// GET（读）不受限：本条只针对「服务端看不到的写」。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// presignVolWriteSet 构造只含一个卷（type=presignVolTestType）的卷集；extra 为卷级配置。
func presignVolWriteSet(extra map[string]any) *registry.Set {
	return registry.NewSet(
		[]volume.Volume{{Name: "v1", Type: presignVolTestType, Extra: extra}},
		nil, nil, nil, "v1",
	)
}

// presignVolWriteMux 构造只挂 presign 端点的 mux（含卷集）。
// presignVolTestType 是本文件**自有**的测试后端类型（不复用 backends_presign_test.go 的
// presignVolTestType：RegisterBackend 对重复注册 panic，多个并行用例各注册一次会互相炸）。
const presignVolTestType = "backends-presign-vol-test"

// presignVolTestOnce 保证本类型只注册一次（并行用例共享）。
var presignVolTestOnce sync.Once

func presignVolWriteMux(t *testing.T, extra map[string]any) *http.ServeMux {
	t.Helper()
	presignVolTestOnce.Do(func() {
		registry.RegisterBackend(presignVolTestType, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
			return &presignTestBackend{}, nil
		})
	})

	h := &Handlers{volSet: presignVolWriteSet(extra)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/backends/{type}/presign", h.backendPresignHandler)
	return mux
}

// TestBackendsPresign_VolumePutDeniedByDefault 卷上 PUT 直传**默认拒绝**（未开 allow_presign）。
func TestBackendsPresign_VolumePutDeniedByDefault(t *testing.T) {
	t.Parallel()
	mux := presignVolWriteMux(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/backends/"+presignVolTestType+"/presign?path=dir/a.txt&method=PUT", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("卷上 PUT 直传默认必须 403（未修必红：会签发 URL ⇒ 可绕过配额记账），得到 %d", rec.Code)
	}
}

// TestBackendsPresign_VolumePutAllowedWithOptIn 显式开启 allow_presign ⇒ 放行（默认禁 + 显式豁免）。
func TestBackendsPresign_VolumePutAllowedWithOptIn(t *testing.T) {
	t.Parallel()
	mux := presignVolWriteMux(t, map[string]any{"allow_presign": true})

	req := httptest.NewRequest(http.MethodPost, "/api/backends/"+presignVolTestType+"/presign?path=dir/a.txt&method=PUT", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("显式开启后应放行，得到 %d（body=%s）", rec.Code, rec.Body.String())
	}
}

// TestBackendsPresign_VolumeGetUnaffected **对照组**：GET（读）不受本守卫影响——本条只针对
// 「服务端看不到的写」；若 GET 也被拒，说明守卫判据选错了位置（丧失区分力）。
func TestBackendsPresign_VolumeGetUnaffected(t *testing.T) {
	t.Parallel()
	mux := presignVolWriteMux(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/backends/"+presignVolTestType+"/presign?path=dir/a.txt&method=GET", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET 读路径不应受写守卫影响，得到 %d", rec.Code)
	}
}
