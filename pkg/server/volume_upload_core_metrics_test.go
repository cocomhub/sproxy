// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_upload_core_metrics_test.go 钉住「上传内核路径可观测」的**用户可见契约**：
// /metrics 输出 `sproxy_volume_upload_core_total{volume,mode}`（mode ∈ used/skipped_oversize/
// degraded/failed/legacy_mode/no_deps），数据源是外部后端（当前 baidupcs）的上传内核路径计数。
//
// 为什么需要：护栏/降级落地后，生产必须能看出「内核在不在跑、为何没跑」；此前只有一条 WARN 日志。

import (
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

func TestVolumeUploadCoreMetrics_Rendered(t *testing.T) {
	t.Parallel()
	ts, h := newTestServerWithMetrics(t)

	// 快照语义：抓取时注入最新累计值（不是事件累加）。
	h.metrics.SetVolumeUploadCore("baidu", 3, 1, 2, 5, 0, 0)

	body := metricsBody(t, ts)
	if !strings.Contains(body, "# TYPE sproxy_volume_upload_core_total counter") {
		t.Fatalf("缺少 TYPE 行:\n%s", body)
	}
	for _, want := range []string{
		`sproxy_volume_upload_core_total{volume="baidu",mode="used"} 3`,
		`sproxy_volume_upload_core_total{volume="baidu",mode="skipped_oversize"} 1`,
		`sproxy_volume_upload_core_total{volume="baidu",mode="degraded"} 2`,
		`sproxy_volume_upload_core_total{volume="baidu",mode="failed"} 5`,
		`sproxy_volume_upload_core_total{volume="baidu",mode="legacy_mode"} 0`,
		`sproxy_volume_upload_core_total{volume="baidu",mode="no_deps"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("缺少 %q:\n%s", want, body)
		}
	}
}

// fakeCoreCountsBackend 只实现被探测的方法（嵌入接口即可，无需补齐 ExternalBackend 全部方法）。
type fakeCoreCountsBackend struct {
	registry.ExternalBackend
	used, skip, deg, fail, legacy, nodeps int64
}

func (f fakeCoreCountsBackend) UploadCoreStatsCounts() (int64, int64, int64, int64, int64, int64) {
	return f.used, f.skip, f.deg, f.fail, f.legacy, f.nodeps
}

func TestUploadCoreCountsOf_ProviderDetection(t *testing.T) {
	t.Parallel()

	// ① 实现者 ⇒ ok=true 且值透传
	used, skip, deg, fail, legacy, nodeps, ok := uploadCoreCountsOf(fakeCoreCountsBackend{used: 7, skip: 1, deg: 2, fail: 3, legacy: 4, nodeps: 5})
	if !ok {
		t.Fatal("实现者应被识别")
	}
	if used != 7 || skip != 1 || deg != 2 || fail != 3 || legacy != 4 || nodeps != 5 {
		t.Fatalf("计数透传不符: used=%d skip=%d deg=%d fail=%d legacy=%d nodeps=%d", used, skip, deg, fail, legacy, nodeps)
	}

	// ② nil 后端 ⇒ ok=false（本地卷/未实现后端不产生噪声）
	if _, _, _, _, _, _, okNil := uploadCoreCountsOf(nil); okNil {
		t.Fatal("nil 后端不应被识别")
	}

	// ③ 未实现能力的后端 ⇒ ok=false
	if _, _, _, _, _, _, okPlain := uploadCoreCountsOf(fakePlainBackend{}); okPlain {
		t.Fatal("未实现能力的后端不应被识别")
	}
}

// fakePlainBackend 是不实现上传内核计数的后端（仅嵌入接口占位）。
type fakePlainBackend struct{ registry.ExternalBackend }
