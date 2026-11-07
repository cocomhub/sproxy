// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
)

// register_direct_test.go 钉住 **presign 直传对象登记**（2026-10-14 P0 收口）：
// 直传内容不经本进程 ⇒ 登记必须把它纳入与上传**同一套**记账与元数据：
//   - 双账本（owner Scope + 卷容量池）按声明 size 记账（此前完全不可见 = 容量/配额旁路）；
//   - 本地卷写完整性 sidecar + checksum 台账（读侧校验与列表比对的来源）；
//   - 存在性/size 不符一律 fail-closed（404/409），绝不按猜的数字记账。
//
// 变异：去掉 size 校验 ⇒ 409 用例红；去掉 Commit ⇒ 配额记账用例红（Usage 仍 0）。

// directUploadEnv 建最小写面装配 + 直传目标对象（内容已「直传」落盘）。
func directUploadEnv(t *testing.T, payload []byte) (*dirsEnv, string) {
	t.Helper()
	e := writeTargetEnv(t)
	root := e.tenantFor("alice").Root()
	if merr := root.MkdirAll("user", 0o755); merr != nil {
		t.Fatalf("建 user 目录: %v", merr)
	}
	f, oerr := root.OpenFile("user/direct.bin", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if oerr != nil {
		t.Fatalf("预置直传对象: %v", oerr)
	}
	if _, werr := f.Write(payload); werr != nil {
		_ = f.Close()
		t.Fatalf("写入直传对象: %v", werr)
	}
	if cerr := f.Close(); cerr != nil {
		t.Fatalf("关闭直传对象: %v", cerr)
	}
	sum := sha256.Sum256(payload)
	return e, hex.EncodeToString(sum[:])
}

// TestRegisterDirectUpload_AccountsQuotaAndSidecar：登记成功 ⇒ 配额按 size 记账 + sidecar +
// checksum 台账（与上传同口径）。
func TestRegisterDirectUpload_AccountsQuotaAndSidecar(t *testing.T) {
	t.Parallel()
	payload := []byte("direct-upload-payload-20b") // 25B
	e, sha := directUploadEnv(t, payload)
	res, err := e.svc.RegisterDirectUpload(context.Background(), DirectUploadInput{
		Owner: "alice", RemotePath: "direct.bin", Size: int64(len(payload)), SHA256: sha,
	})
	if err != nil {
		t.Fatalf("RegisterDirectUpload: %v", err)
	}
	if res.Size != int64(len(payload)) || res.Checksum != sha {
		t.Fatalf("res=%+v want size=%d checksum=%s", res, len(payload), sha)
	}
	// **双账本（owner 侧）**：此前直传对象对配额完全不可见 ⇒ 登记后必须按 size 记账。
	sc := e.quotaScopeFor("alice", "user/direct.bin")
	if sc == nil {
		t.Fatal("quotaScopeFor 应返回 user 桶 Scope（dirsEnv 单卷默认装配）")
	}
	if got := sc.Usage(); got != int64(len(payload)) {
		t.Fatalf("直传登记后 owner 配额 Usage=%d want %d（容量/配额旁路未关闭）", got, len(payload))
	}
	// sidecar 内置（本地卷）：读侧逐分块校验的来源。
	if _, serr := e.tenantFor("alice").Root().Stat("meta/direct.bin.meta"); serr != nil {
		t.Fatalf("登记应生成 sidecar: %v", serr)
	}
	// checksum 台账。
	if cs := e.checksum["alice"]; cs == nil {
		t.Fatal("checksum 台账缺失")
	}
}

// TestRegisterDirectUpload_SizeMismatch409：声明 size 与实测不符 ⇒ 409（fail-closed，不记错账）。
func TestRegisterDirectUpload_SizeMismatch409(t *testing.T) {
	t.Parallel()
	payload := []byte("abcdefghij")
	e, sha := directUploadEnv(t, payload)
	_, err := e.svc.RegisterDirectUpload(context.Background(), DirectUploadInput{
		Owner: "alice", RemotePath: "direct.bin", Size: int64(len(payload)) + 7, SHA256: sha,
	})
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusConflict {
		t.Fatalf("size 不符应 409, got %v", err)
	}
	if !strings.Contains(he.Message, "大小与声明不符") {
		t.Fatalf("错误文案应可归因: %v", he.Message)
	}
	// 拒绝后不得记账（未动账本）。
	if sc := e.quotaScopeFor("alice", "user/direct.bin"); sc != nil && sc.Usage() != 0 {
		t.Fatalf("拒绝登记后配额不应变化, Usage=%d", sc.Usage())
	}
}

// TestRegisterDirectUpload_Missing404：对象不存在（直传未完成）⇒ 404。
func TestRegisterDirectUpload_Missing404(t *testing.T) {
	t.Parallel()
	e := writeTargetEnv(t)
	sum := sha256.Sum256([]byte("x"))
	_, err := e.svc.RegisterDirectUpload(context.Background(), DirectUploadInput{
		Owner: "alice", RemotePath: "nope.bin", Size: 1, SHA256: hex.EncodeToString(sum[:]),
	})
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusNotFound {
		t.Fatalf("对象不存在应 404, got %v", err)
	}
}

// TestRegisterDirectUpload_BadParams400：size 负 / sha256 非 64 位十六进制 ⇒ 400。
func TestRegisterDirectUpload_BadParams400(t *testing.T) {
	t.Parallel()
	e := writeTargetEnv(t)
	good := strings.Repeat("a", 64)
	cases := []DirectUploadInput{
		{Owner: "alice", RemotePath: "x.bin", Size: -1, SHA256: good},
		{Owner: "alice", RemotePath: "x.bin", Size: 1, SHA256: "short"},
		{Owner: "alice", RemotePath: "x.bin", Size: 1, SHA256: strings.Repeat("z", 64)},
		{Owner: "alice", RemotePath: "x.bin", Size: 1, SHA256: ""},
	}
	for i, in := range cases {
		_, err := e.svc.RegisterDirectUpload(context.Background(), in)
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != http.StatusBadRequest {
			t.Fatalf("case %d 应 400, got %v", i, err)
		}
	}
}

// TestIsSHA256Hex：形状矩阵（大小写不敏感；长度/字符集严格）。
func TestIsSHA256Hex(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in string
		ok bool
	}{
		{strings.Repeat("0", 64), true},
		{strings.Repeat("abcdef09", 8), true},
		{strings.ToUpper(strings.Repeat("abcdef09", 8)), true},
		{strings.Repeat("0", 63), false},
		{strings.Repeat("0", 65), false},
		{strings.Repeat("g", 64), false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsSHA256Hex(c.in); got != c.ok {
			t.Fatalf("IsSHA256Hex(%q)=%v want %v", c.in, got, c.ok)
		}
	}
}
