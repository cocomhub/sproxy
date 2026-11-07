// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// default_policy_test.go —— secretdata 卷的**默认分块策略按底层本地性分流**（2026-10-21
// 用户裁定：默认行为统一可靠；本地卷 4MiB blocklet、外部/网盘卷 256KiB blocklet）。
//
// 背景：外部卷（百度网盘）实测 64KiB blocklet 的随机读 range 1.37s vs 默认 4MiB 的 10.8s
// （差 8 倍）；**256KiB 是最平衡档**——单段 256KiB（网盘 ~0.5s/段，首帧/seek 足够流畅）、
// 3GB meta 仅 1.8MiB（64KiB 档为 7.1MiB，挂载解密慢 4 倍）、解密 ~0.07ms 可忽略；
// 且 blocklet **不影响上传**（上传/落盘单位是「块」= 1MB-200MB random，与 blocklet 无关）。
// 本地卷 4MiB 与 256KiB 差异毫秒级（page cache + 零网络开销）。用户裁决
// 「本地默认最终也会归档外部卷，用 4MiB 人工指定即可」⇒ 默认对外部卷用 256KiB 是统一可靠
// 的默认，本地卷保持 4MiB（避免本地小文件 meta/CPU 白增）。
//
// 本文件锁定 NewFS 在 opts.Block 未显式配置时：
//   - 底层是本地卷（IsLocalVolume()==true）→ DefaultBlockPolicy()（4MiB blocklet）；
//   - 底层是外部/网盘卷 → ExternalDefaultBlockPolicy()（256KiB blocklet）；
//   - 显式 opts.Block（用户 extra.block_policy 或代码构造）→ 尊重，不覆盖。

import (
	"context"
	"io"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// localMarkerFS 报告自身为本地卷。
type localMarkerFS struct {
	*syncpkg.LocalFS
}

func (f *localMarkerFS) IsLocalVolume() bool { return true }

// externalMarkerFS 报告自身为外部卷（底层 LocalFS 提供数据）。
type externalMarkerFS struct {
	*syncpkg.LocalFS
}

func (f *externalMarkerFS) IsLocalVolume() bool { return false }

func newDefaultPolicyFS(t *testing.T, inner syncpkg.FS) *SecretdataFS {
	t.Helper()
	fs, err := NewFS(inner, Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return fs
}

// TestDefaultPolicy_LocalKeeps4MiB：底层本地卷 + 未显式配置 ⇒ 默认 4MiB blocklet。
func TestDefaultPolicy_LocalKeeps4MiB(t *testing.T) {
	t.Parallel()
	fs := newDefaultPolicyFS(t, &localMarkerFS{LocalFS: syncpkg.NewLocalFS(t.TempDir(), nil)})
	if fs.opts.Block.BlockletMax != 4<<20 {
		t.Fatalf("本地卷默认 blocklet_max=%d，应为 4MiB（4<<20）", fs.opts.Block.BlockletMax)
	}
}

// TestDefaultPolicy_ExternalUses256KiB：底层外部/网盘卷 + 未显式配置 ⇒ 默认 256KiB blocklet。
func TestDefaultPolicy_ExternalUses256KiB(t *testing.T) {
	t.Parallel()
	fs := newDefaultPolicyFS(t, &externalMarkerFS{LocalFS: syncpkg.NewLocalFS(t.TempDir(), nil)})
	if fs.opts.Block.BlockletMax != 256<<10 {
		t.Fatalf("外部卷默认 blocklet_max=%d，应为 256KiB（256<<10）", fs.opts.Block.BlockletMax)
	}
}

// plainFS 是**未实现** LocalVolume 接口的 FS（手写 7 方法转发，不含 IsLocalVolume）。
// 用于锁「未自述本地性 ⇒ 按外部处理」这一默认分支（审核 P3-4：localMarker/externalMarker
// 都嵌了 *LocalFS 从而实现了接口，ok=false 分支此前无覆盖）。
type plainFS struct{ in syncpkg.FS }

func (p plainFS) ListDir(ctx context.Context, s string) ([]syncpkg.Entry, error) {
	return p.in.ListDir(ctx, s)
}
func (p plainFS) Stat(ctx context.Context, s string) (*syncpkg.Entry, error) {
	return p.in.Stat(ctx, s)
}
func (p plainFS) OpenRead(ctx context.Context, s string) (io.ReadCloser, error) {
	return p.in.OpenRead(ctx, s)
}
func (p plainFS) WriteFile(ctx context.Context, k syncpkg.LogicalKey, s string, r io.Reader, size, mt int64) error {
	return p.in.WriteFile(ctx, k, s, r, size, mt)
}
func (p plainFS) Rename(ctx context.Context, a, b string) error { return p.in.Rename(ctx, a, b) }
func (p plainFS) Delete(ctx context.Context, s string) error    { return p.in.Delete(ctx, s) }
func (p plainFS) MakeDir(ctx context.Context, s string) error   { return p.in.MakeDir(ctx, s) }

// TestDefaultPolicy_NoLocalVolumeSelfReportTreatedAsExternal：底层**未实现** LocalVolume
// （无 IsLocalVolume 自述）⇒ 按**外部**处理（256KiB）——安全方向：装饰器链上若有未知包装，
// 最坏也只是用了外部档位（仅网盘/本地性能档位差异，无正确性后果）。
func TestDefaultPolicy_NoLocalVolumeSelfReportTreatedAsExternal(t *testing.T) {
	t.Parallel()
	inner := plainFS{in: syncpkg.NewLocalFS(t.TempDir(), nil)}
	if _, ok := any(inner).(syncpkg.LocalVolume); ok {
		t.Fatal("测试夹具不应实现 LocalVolume（否则本分支测不到）")
	}
	fs := newDefaultPolicyFS(t, inner)
	if fs.opts.Block.BlockletMax != 256<<10 {
		t.Fatalf("未自述本地性默认 blocklet_max=%d，应为 256KiB（按外部处理）", fs.opts.Block.BlockletMax)
	}
}

// TestDefaultPolicy_ExplicitWins：显式 opts.Block（用户配置）不被本地性分流覆盖。
func TestDefaultPolicy_ExplicitWins(t *testing.T) {
	t.Parallel()
	inner := &externalMarkerFS{LocalFS: syncpkg.NewLocalFS(t.TempDir(), nil)}
	fs, err := NewFS(inner, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 1 << 20, Max: 4 << 20, BlockletMode: "fixed", BlockletMin: 256 << 10, BlockletMax: 1 << 20},
		TempDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if fs.opts.Block.BlockletMax != 1<<20 {
		t.Fatalf("显式 blocklet_max=%d 应被保留（1<<20），却被本地性分流覆盖", fs.opts.Block.BlockletMax)
	}
}
