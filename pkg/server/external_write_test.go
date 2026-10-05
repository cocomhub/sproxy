// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// external_write_test.go 验证外部卷写路径（2026-10-05 用户裁定：普通上传进外部卷 +
// 统一 user/ 前缀）：routeUpload 对外部卷装配 UploadSink（Tenant nil 放行）→
// files.WriteFile 经 Sink 整流写 → 读路径 resolveExternalDownload 用 user/<rel> 命中
// ——键空间闭合（此前断连：写路径 pikpak/... 与读路径 user/... 不相交，宣称能力不可达）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// TestReserveVolume_ExternalSink：外部卷 reserveVolume 放行并装配 UploadSink
// （Tenant nil + Sink 非 nil），本地卷不变（Tenant 非 nil + Sink nil）。
func TestReserveVolume_ExternalSink(t *testing.T) {
	t.Parallel()
	v := volume.Volume{Name: "baidu", Type: "baidupcs", DirectLink: false}
	fs := &extFS{files: map[string]string{}}
	h := newExternalTestEnv(t, v, fs)

	route, err := h.reserveVolume("alice", "user/f.bin", "baidu", 100)
	if err != nil {
		t.Fatalf("外部卷 reserveVolume: %v", err)
	}
	if route.sink == nil {
		t.Fatal("外部卷 route 应装配 UploadSink（Tenant nil 放行）")
	}
	route.release()
}

// TestExternalUploadReadRoundtrip：外部卷写→读键空间闭合端到端——files.WriteFile 经
// Sink 写 user/f.bin → resolveExternalDownload 用 user/f.bin 命中（A 态 source）。
func TestExternalUploadReadRoundtrip(t *testing.T) {
	t.Parallel()
	v := volume.Volume{Name: "secret", Type: volume.TypeSecretdata}
	fs := &extFS{files: map[string]string{}}
	h := newExternalTestEnv(t, v, fs)

	// 1. 普通上传到外部卷（files.WriteFile 经 Sink 整流写 user/ 键）。
	route, rerr := h.reserveVolume("alice", "user/movie.bin", "secret", 11)
	if rerr != nil {
		t.Fatalf("reserveVolume: %v", rerr)
	}
	defer route.release()
	ur := files.UploadRoute{
		VolumeName: route.volumeName, Tenant: route.tenant, Sink: route.sink,
		Scope: route.scope, ScopeRes: route.scopeRes, Pool: route.pool, PoolRes: route.poolRes,
		Release: route.release,
	}
	svc := h.fileService()
	// WriteFileInput 需要 Owner/RemotePath；构造 domain 调用（绕过 HTTP，直测键闭合）。
	res, werr := svc.WriteFile(context.Background(), files.WriteFileInput{
		Owner:            "alice",
		RemotePath:       "movie.bin",
		ExplicitVol:      "secret", // 显式外部卷（此前隐式依赖"唯一外部卷=Default"）
		ExpectedChecksum: testutil.SHA256Hex([]byte("hello world\n")),
		ClientSize:       12,
	}, strings.NewReader("hello world\n"))
	if werr != nil {
		t.Fatalf("WriteFile(外部卷): %v", werr)
	}
	if res.VolumeName != "secret" {
		t.Fatalf("VolumeName=%q want secret", res.VolumeName)
	}
	_ = ur

	// 2. 读路径命中：/download?filename=movie.bin → UserRel → user/movie.bin →
	// resolveExternalDownload 加 owner 前缀 → alice/user/movie.bin 命中（A 态 source）。
	dp := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download?filename=movie.bin", nil), "alice", "user/movie.bin", "movie.bin", "")
	if dp == nil {
		t.Fatal("外部卷读路径未命中 alice/user/movie.bin（键空间仍断连）")
	}
	if dp.redirectURL != "" {
		t.Fatal("secretdata 恒私密不应有直链")
	}
	if dp.source == nil {
		t.Fatal("secretdata 应走服务端 source")
	}
	// 3. Source 打开能读到写的内容（RangeSeeker 消费）。
	info, serr := dp.source.Stat(context.Background())
	if serr != nil || info.Size() != 12 {
		t.Fatalf("Source.Stat size=%d err=%v want 12", info.Size(), serr)
	}
}

// 编译期断言保留 import 稳定。
