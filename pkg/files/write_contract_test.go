// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// write_contract_test.go 是写面（`POST /upload`）的**HTTP 契约补钉测试**（D-2 第 3 片的配套）。
//
// 定位：既有 write_test.go 已覆盖写面的主要分支（成功/幂等/冲突/锁/缺 checksum/路径非法/
// 客户端 checksum 不符/路由错误/JSON 可解析），其中幂等与冲突还断言了**文案逐字**。本文件
// 只补钉本次重构**动过形状决定权**的那几处——即原先由 `handleDuplicateFile` 直接写响应头、
// 现在由「域结果 → 处理器写头」的两处：
//
//  1. 幂等命中时 `X-Volume` 仍须存在（多卷装配下，域侧把 home 卷名回传给处理器）；
//  2. 幂等命中时响应体不得带 `X-File-Checksum` **之外**的额外字段/头的漂移。
//
// 验证方法同 read_contract_test.go：**重构前跑一遍、重构后跑一遍**，两次都绿才算「形状未变」。

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestWriteContract_IdempotentKeepsVolumeHeader 钉住「多卷装配 + 自动路由」下幂等命中仍回
// `X-Volume`：写前定位得到 home 卷，幂等分支据此回卷名（历史行为：响应头在调用重复检测**之前**
// 就已设置）。这是本次重构唯一改变"谁决定响应头"的地方，故单列一条。
func TestWriteContract_IdempotentKeepsVolumeHeader(t *testing.T) {
	env := newDirsEnv(t)
	env.enableVolumes(t, "main", "disk2")
	env.enableWriteDefaults()

	const body = "idempotent-with-volume"
	sum := sha256Hex([]byte(body))

	// 首次上传（自动路由）→ 200 且回卷名
	rr := env.upload(t, "alice", "f.txt", []byte(body), sum, 0)
	if rr.Code != http.StatusOK {
		t.Fatalf("首次上传应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	firstVol := rr.Header().Get(headerVolume)
	if firstVol == "" {
		t.Fatalf("首次上传应回 %s 头（多卷装配）", headerVolume)
	}

	// 幂等重传（同名同 checksum）→ 200 且**仍**回同一卷名
	rr = env.upload(t, "alice", "f.txt", []byte(body), sum, 0)
	if rr.Code != http.StatusOK {
		t.Fatalf("幂等重传应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get(headerVolume); got != firstVol {
		t.Fatalf("幂等分支 %s=%q want %q（域侧须把 home 卷回传给处理器）", headerVolume, got, firstVol)
	}
	resp := decodeResp(t, rr)
	if !resp.Success || resp.Message != "文件已上传成功, size: "+strconv.Itoa(len(body)) {
		t.Fatalf("幂等响应=%+v want 幂等成功文案", resp)
	}
	if got := rr.Header().Get(headerFileChecksum); got != sum {
		t.Fatalf("幂等分支 X-File-Checksum=%q want %q", got, sum)
	}
}

// TestWriteContract_MTimeAppliedOnDisk 钉住 `X-File-MTime` 的**落盘**效果（域侧副作用，
// 原实现在 setUploadResponseHeaders 内）：mtime 头必须使目标文件的实际 ModTime 等于该值。
func TestWriteContract_MTimeAppliedOnDisk(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	const body = "mtime-body"
	const mtime = int64(1_700_000_000) * 1e9 // UnixNano
	sum := sha256Hex([]byte(body))

	rr := env.upload(t, "alice", "f.txt", []byte(body), sum, mtime)
	if rr.Code != http.StatusOK {
		t.Fatalf("上传应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	abs := filepath.Join(env.root, "alice", "user", "f.txt")
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatalf("stat 落盘文件: %v", err)
	}
	if got := info.ModTime().Unix(); got != mtime/1e9 {
		t.Fatalf("落盘 mtime=%d want %d（域侧须把 X-File-MTime 应用到文件）", got, mtime/1e9)
	}
}
