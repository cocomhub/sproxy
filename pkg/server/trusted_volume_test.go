// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// trusted_volume_test.go 钉住可信卷装配：外部卷上传源/转存目标默认 Wrap（写后生成
// .meta），trusted_volume.disable=true 时关闭（零回归）。断言铁律：正例落到真实
// 副作用（外部卷文件旁存在可反序列化校验的 .meta；关闭后无 .meta）。

import (
	"bytes"
	"context"
	"io"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// fakeExternalFSBackend 是最小 ExternalBackend（内存 FS 包装），用于装配层 Wrap 断言。
type fakeExternalFSBackend struct{ fs syncpkg.FS }

func (f *fakeExternalFSBackend) FS() syncpkg.FS { return f.fs }
func (f *fakeExternalFSBackend) Close() error   { return nil }

var _ registry.ExternalBackend = (*fakeExternalFSBackend)(nil)

// newTrustedHandlers 构造带 volSet + cfgPtr 的 Handlers（仅装配 externalSinkFor 所需字段）。
func newTrustedHandlers(t *testing.T, disable bool) (*Handlers, *syncpkg.LocalFS) {
	t.Helper()
	inner := syncpkg.NewLocalFS(t.TempDir(), nil)
	vs := registry.NewSet(nil, nil, nil, nil, "")
	be := &fakeExternalFSBackend{fs: inner}
	if err := vs.AddExternalVolume(volume.Volume{Name: "ext", Type: "fake", ACL: volume.ACL{Mode: volume.ModeDeny}}, be); err != nil {
		t.Fatalf("AddExternalVolume: %v", err)
	}
	cfg := Default()
	cfg.TrustedVolume.Disable = disable
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	return &Handlers{cfgPtr: &cfgPtr, volSet: vs}, inner
}

// TestExternalSinkFor_TrustedWrap 默认（disable=false）外部卷上传源 Wrap：
// WriteFile 后数据旁生成 .meta（隐藏 + 可校验）。
func TestExternalSinkFor_TrustedWrap(t *testing.T) {
	t.Parallel()
	h, inner := newTrustedHandlers(t, false)
	sink := h.externalSinkFor("alice", "ext")
	if sink == nil {
		t.Fatal("externalSinkFor 应返回 sink")
	}
	ctx := context.Background()
	data := bytes.Repeat([]byte("trusted upload 可信上传内容 "), 100)
	// 域侧 rel = user/uploaded.bin（剥桶后经 ResolveOwnerPath 计算外部卷键）。
	if err := sink.WriteFile(ctx, "user/uploaded.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("sink.WriteFile: %v", err)
	}
	// 外部卷键：ModeDeny（共享）→ ResolveOwnerPath 加 owner 前缀 → alice/user/uploaded.bin。
	// 数据文件旁 .meta 应存在（trusted 装饰器写后生成）。
	e, err := inner.Stat(ctx, "alice/user/uploaded.bin.meta")
	if err != nil || e == nil {
		t.Fatalf("可信卷上传应生成 .meta: %v %v", e, err)
	}
	rc, rerr := inner.OpenRead(ctx, "alice/user/uploaded.bin.meta")
	if rerr != nil {
		t.Fatalf("OpenRead meta: %v", rerr)
	}
	raw, _ := io.ReadAll(rc)
	rc.Close()
	fm, uerr := meta.Unmarshal(raw)
	if uerr != nil {
		t.Fatalf("meta 反序列化: %v", uerr)
	}
	if fm.Size != int64(len(data)) {
		t.Fatalf("meta Size = %d, want %d", fm.Size, len(data))
	}
}

// TestExternalSinkFor_Disabled  trusted_volume.disable=true → 不 Wrap：无 .meta（零回归）。
func TestExternalSinkFor_Disabled(t *testing.T) {
	t.Parallel()
	h, inner := newTrustedHandlers(t, true)
	sink := h.externalSinkFor("alice", "ext")
	if sink == nil {
		t.Fatal("externalSinkFor 应返回 sink")
	}
	ctx := context.Background()
	data := []byte("plain upload")
	if err := sink.WriteFile(ctx, "user/plain.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("sink.WriteFile: %v", err)
	}
	if e, err := inner.Stat(ctx, "alice/user/plain.bin.meta"); err != nil || e != nil {
		t.Fatalf("disable 下不应生成 .meta: %v %v", e, err)
	}
}

// TestTrustedDisabled 开关读取：缺省 false（功能默认启用），显式 true 关闭。
func TestTrustedDisabled(t *testing.T) {
	t.Parallel()
	// 缺省 Default()：TrustedVolume.Disable 零值 false。
	h := &Handlers{}
	cfg := Default()
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	h.cfgPtr = &cfgPtr
	if h.trustedDisabled() {
		t.Fatal("缺省 trusted_volume.disable 应为 false（功能默认启用）")
	}
	cfg2 := Default()
	cfg2.TrustedVolume.Disable = true
	cfgPtr.Store(cfg2)
	if !h.trustedDisabled() {
		t.Fatal("显式 disable=true 应报告关闭")
	}
}
