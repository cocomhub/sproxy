// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// list_view_source_test.go 钉住 §15.2 步骤 2/3 的核心：**list 可经卷视图列举**，且与实时扫描路径
// **结果等价**（排序前逐条一致：名称/大小/修改时间/卷归属/checksum/目录去重）。
//
// 覆盖：
//   - 等价性（视图 vs 实时扫描，同一夹具）；
//   - 在途临时文件不对外可见（视图路径同样过滤）；
//   - 能力不支持（装配层门控关闭）⇒ 回落**实时扫描**（步骤 3 起不再走索引，零回归）；
//   - ?volume= 过滤。

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// viewListAdapter 把「卷集 + lister」组合为 VolumeSet + VolumeViewLister（模拟装配层适配器）。
type viewListAdapter struct {
	VolumeSet
	lister VolumeViewLister
}

func (v viewListAdapter) ListDirView(ctx context.Context, volName, owner, subdir string) ([]ExternalEntry, bool, error) {
	return v.lister.ListDirView(ctx, volName, owner, subdir)
}

// viewListSet 给测试卷集加装 files.VolumeViewLister（模拟装配层 ListDirView）。
type viewListSet struct {
	*registry.Set
	env *dirsEnv
	// calls 记录被调用的次数（证明视图路径真的被走到，而不是静默回落实时扫描）。
	calls int
	// unsupported=true 模拟「缓存被 disable」⇒ 能力不可用，必须回落实时扫描。
	unsupported bool
}

func (v *viewListSet) ListDirView(_ context.Context, volName, owner, subdir string) ([]ExternalEntry, bool, error) {
	v.calls++
	if v.unsupported {
		return nil, false, nil
	}
	// 外部卷：返回 fake 条目（模拟装配层经外部卷视图列举）。
	if ext, ok := v.env.externalVols[volName]; ok && ext != nil {
		ext.gotRel.Store(subdir)
		if ext.listErr != nil {
			return nil, true, ext.listErr
		}
		if ext.tree != nil {
			return ext.tree[subdir], true, nil // 未列出的目录 = 空目录
		}
		return ext.entries, true, nil
	}
	userRel := userBucket
	if subdir != "" {
		userRel += "/" + strings.Trim(subdir, "/")
	}
	tnt := v.env.volumeTenant(volName, owner)
	if tnt == nil || tnt.Root() == nil {
		return nil, true, os.ErrNotExist
	}
	entries, err := tnt.Root().ReadDir(userRel)
	if err != nil {
		return nil, true, err
	}
	out := make([]ExternalEntry, 0, len(entries))
	for _, e := range entries {
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		out = append(out, ExternalEntry{
			Name: e.Name(), IsDir: e.IsDir(), Size: info.Size(), MTime: info.ModTime().UnixNano(),
		})
	}
	return out, true, nil
}

// viewEnv 构造「多卷 + 可选卷视图列举」的测试环境（default 卷落在 e.root）。
func viewEnv(t *testing.T, unsupported bool) *dirsEnv {
	t.Helper()
	env := newDirsEnv(t)
	env.enableVolumes(t, "default")
	env.enableWriteDefaults()
	env.viewList = &viewListSet{env: env, unsupported: unsupported}
	env.rebuild()
	return env
}

// listFileInfos 以固定排序取一次列表（便于逐条比较）。
func listFileInfos(t *testing.T, env *dirsEnv, subdir string) []FileInfo {
	t.Helper()
	res, err := env.svc.List(t.Context(), ListQuery{Owner: "alice", Subdir: subdir})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// 目录条目 Volume 为空、文件带卷名，按 (IsDir,Name,Volume) 排序稳定比较。
	sort.Slice(res.Files, func(i, j int) bool {
		a, b := res.Files[i], res.Files[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Volume < b.Volume
	})
	return res.Files
}

// TestList_ViewSourceEquivalentToScan 钉住「视图路径与实时扫描路径结果等价」。
// 变异：视图路径漏掉在途临时文件过滤 / 目录去重 / checksum 键 ⇒ 对齐失败。
func TestList_ViewSourceEquivalentToScan(t *testing.T) {
	t.Parallel()
	env := viewEnv(t, false)

	body1 := []byte("alpha")
	body2 := []byte("β-bravo")
	env.upload(t, "alice", "docs/a.txt", body1, sha256Hex(body1), 0)
	env.upload(t, "alice", "docs/b.txt", body2, sha256Hex(body2), 0)
	env.upload(t, "alice", "root.txt", []byte("r"), sha256Hex([]byte("r")), 0)
	if _, merr := env.svc.MakeDir(context.Background(), "alice", "docs/empty", ""); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	// 在途临时文件：实时扫描路径本来就跳过；视图路径必须同样跳过。
	if err := env.tenants["alice"].Root().MkdirAll("user/docs", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	f, ferr := env.tenants["alice"].Root().OpenFile("user/docs/.inflight-0123456789abcdef-upl.part", os.O_CREATE|os.O_WRONLY, 0o600)
	if ferr != nil {
		t.Fatalf("OpenFile inflight: %v", ferr)
	}
	_ = f.Close()

	// ① 实时扫描路径（关闭视图能力）。
	env.viewList = &viewListSet{env: env, unsupported: true}
	env.rebuild()
	indexed := listFileInfos(t, env, "docs")
	// ② 视图路径。
	viewSet := &viewListSet{env: env, unsupported: false}
	env.viewList = viewSet
	env.rebuild()
	viewed := listFileInfos(t, env, "docs")
	if viewSet.calls == 0 {
		t.Fatal("视图路径未被调用（能力未生效，比较退化为「扫描 vs 扫描」）")
	}

	if len(indexed) != len(viewed) {
		t.Fatalf("条目数不一致：索引 %d / 视图 %d\nindex=%+v\nview=%+v", len(indexed), len(viewed), indexed, viewed)
	}
	for i := range indexed {
		if indexed[i] != viewed[i] {
			t.Fatalf("第 %d 条不一致：index=%+v view=%+v", i, indexed[i], viewed[i])
		}
	}
	// 在途文件两条路径都不可见。
	for _, f := range viewed {
		if f.Name == ".inflight-0123456789abcdef-upl.part" {
			t.Fatalf("视图路径不应暴露在途临时文件: %+v", f)
		}
	}
	// 根目录也要一致（含 root.txt）。
	env.viewList = &viewListSet{env: env, unsupported: true}
	env.rebuild()
	idxRoot := listFileInfos(t, env, "")
	env.viewList = &viewListSet{env: env, unsupported: false}
	env.rebuild()
	viewRoot := listFileInfos(t, env, "")
	if len(idxRoot) != len(viewRoot) {
		t.Fatalf("根目录条目数不一致：扫描 %d / 视图 %d (%+v vs %+v)", len(idxRoot), len(viewRoot), idxRoot, viewRoot)
	}
}

// TestList_ViewSourceUnsupportedFallsBack 钉住门控：能力不可用（缓存关闭）⇒ 实时扫描（零回归）。
func TestList_ViewSourceUnsupportedFallsBack(t *testing.T) {
	t.Parallel()
	env := viewEnv(t, true)
	body := []byte("fallback")
	env.upload(t, "alice", "docs/x.txt", body, sha256Hex(body), 0)
	files := listFileInfos(t, env, "docs")
	if len(files) != 1 || files[0].Name != "x.txt" || files[0].Checksum != sha256Hex(body) {
		t.Fatalf("回落实时扫描应列出 x.txt 且带 checksum，got %+v", files)
	}
}

// TestList_ViewSourceVolumeFilter 钉住 ?volume= 过滤在视图路径同样生效。
func TestList_ViewSourceVolumeFilter(t *testing.T) {
	t.Parallel()
	env := viewEnv(t, false)
	body := []byte("v")
	env.upload(t, "alice", "docs/v.txt", body, sha256Hex(body), 0)

	res, err := env.svc.List(t.Context(), ListQuery{Owner: "alice", Subdir: "docs", VolName: "default"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Files) != 1 || res.Files[0].Volume != "default" {
		t.Fatalf("?volume=default 应只列该卷文件，got %+v", res.Files)
	}
	// 未知卷 = 404（与既有语义一致，视图路径不改这条前置校验）。
	if _, err = env.svc.List(t.Context(), ListQuery{Owner: "alice", Subdir: "docs", VolName: "nope"}); err == nil {
		t.Fatal("未知卷应返回 404 错误")
	}
}
