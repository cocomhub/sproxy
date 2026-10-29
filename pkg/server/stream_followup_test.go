// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// stream_followup_test.go：真实 secretdata 外部卷的上传校验回归（#763 流式路径）。
// 现状 bug：writeExternalSettle 用 teeReadSeeker 算 serverChecksum，但 teeReadSeeker.ReadAt
// 透传不 tee，而 secretdata 流式路径全程 ReadAt 两遍读 → hasher 收不到字节 → serverChecksum
// 为空摘要 → 与客户端 ExpectedChecksum 比对失败 → 400。本测试用**真实** secretdata 后端
// （extFS 单遍 Read 无法暴露该 bug）复现。

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/secretdata"
)

// newSecretdataExternalEnv 构造 Handlers + 一个**真实 secretdata** 外部卷（vault）。
func newSecretdataExternalEnv(t *testing.T) *Handlers {
	t.Helper()
	inner := syncpkg.NewLocalFS(filepath.Join(t.TempDir(), "inner"), nil)
	be, err := secretdata.NewBackend(context.Background(),
		volume.Volume{Name: "vault", Type: volume.TypeSecretdata}, inner,
		secretdata.Options{Secret: []byte("test-secret-key-32bytes-abcdefgh"), TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("secretdata.NewBackend: %v", err)
	}
	root, rerr := storage.OpenRoot(t.TempDir())
	if rerr != nil {
		t.Fatalf("OpenRoot: %v", rerr)
	}
	vs := registry.NewSet(
		[]volume.Volume{{Name: "main"}},
		map[string]*storage.Root{"main": root}, nil, nil, "main",
	)
	if aerr := vs.AddExternalVolume(volume.Volume{Name: "vault", Type: volume.TypeSecretdata}, be); aerr != nil {
		t.Fatalf("AddExternalVolume: %v", aerr)
	}
	tenants := storage.NewTenantCache(root)
	t.Cleanup(func() { _ = tenants.Close() })
	t.Cleanup(func() { _ = root.Close() })
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(&Config{})
	return &Handlers{volSet: vs, logger: testLogger(), tenants: tenants, cfgPtr: &cfgPtr,
		checksumStores: make(map[string]checksum.ChecksumStoreIface)}
}

// TestExternalUpload_SecretdataChecksumOK 上传到真实 secretdata 外部卷，客户端提供正确
// checksum → 必须成功（当前 bug 下 serverChecksum 为空 → 400）。
func TestExternalUpload_SecretdataChecksumOK(t *testing.T) {
	t.Parallel()
	h := newSecretdataExternalEnv(t)
	content := "hello world\n"
	res, werr := h.fileService().WriteFile(context.Background(), files.WriteFileInput{
		Owner:            "alice",
		RemotePath:       "movie.bin",
		ExplicitVol:      "vault",
		ExpectedChecksum: testutil.SHA256Hex([]byte(content)),
		ClientSize:       int64(len(content)),
	}, strings.NewReader(content))
	if werr != nil {
		t.Fatalf("WriteFile(secretdata, 正确 checksum) 失败: %v", werr)
	}
	if res.Checksum != testutil.SHA256Hex([]byte(content)) {
		t.Fatalf("返回 checksum=%q 期望 %q", res.Checksum, testutil.SHA256Hex([]byte(content)))
	}
}

// TestWriteTarget_SecretdataChecksumOK 统一写内核（#765）外部分支（S3/DAV 路径）写真实
// secretdata：Verify + 正确 checksum 必须成功——与 writeExternalSettle 共用
// feedChecksumRewindable（独立一遍算 checksum + 回绕），故多遍 ReadAt 消费下亦正确。
func TestWriteTarget_SecretdataChecksumOK(t *testing.T) {
	t.Parallel()
	h := newSecretdataExternalEnv(t)
	content := "s3-like object body\n"
	res, werr := h.fileService().WriteTarget(context.Background(), files.WriteTargetInput{
		Owner:            "alice",
		RemotePath:       "user/obj.bin",
		Volume:           "vault",
		Verify:           true,
		ExpectedChecksum: testutil.SHA256Hex([]byte(content)),
		ClientSize:       int64(len(content)),
	}, strings.NewReader(content))
	if werr != nil {
		t.Fatalf("WriteTarget(外部 secretdata): %v", werr)
	}
	if res.Checksum != testutil.SHA256Hex([]byte(content)) {
		t.Fatalf("checksum=%q 期望 %q", res.Checksum, testutil.SHA256Hex([]byte(content)))
	}
}
