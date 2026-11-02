// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// wrapper_enclosure_region_test.go 锁定**封装卷足迹区间防误删前置校验**（F1/阶段 2 迁移补回）：
// 声明足迹区间若已存在且含「非本卷内容」（普通目录形态 / 被文件占据），新建与收养都必须
// fail-closed 拒绝——否则删除该封装卷会擦除整个区间（`eraseEnclosureRegion`），最危险形态是
// owner 限定声明（`target: main/<owner>/user/<目录>`，足迹 = 该 owner 的真实数据区）。
//
// 三条装配入口全钉：config 装配（AttachAll = 启动路径）、建卷 API（validateNewEnclosure）、
// 运行期/恢复装配（serverenclosure.AttachEnclosureVolume）。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	serverenclosure "github.com/cocomhub/sproxy/pkg/server/enclosure"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/enclosure"
)

// populateOrdinaryDir 造「普通用户目录」形态（文件 + 子目录混合——旧 adoptableNestedDir 判据中的
// 不可收养形态），返回其绝对路径。
func populateOrdinaryDir(t *testing.T, volumeRoot, rel string) string {
	t.Helper()
	dir := filepath.Join(volumeRoot, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Join(dir, "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "合同.txt"), []byte("既有用户数据"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// assertUserDataIntact 断言既有用户数据未被触碰（拒绝必须是「无副作用」的）。
func assertUserDataIntact(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "合同.txt")); err != nil {
		t.Fatalf("既有用户数据不应被改动: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "archive")); err != nil {
		t.Fatalf("既有用户子目录不应被改动: %v", err)
	}
}

// TestEnclosureRegion_BootRejectsPopulatedOwnerRegion 启动路径（config 装配 + AttachAll）：
// owner 限定声明覆盖既有用户数据目录 → 装配 fail-closed（不带病启动）。
func TestEnclosureRegion_BootRejectsPopulatedOwnerRegion(t *testing.T) {
	t.Parallel()
	registerNestedSecretWrapper()
	mainDir := t.TempDir()
	dir := populateOrdinaryDir(t, mainDir, "alice/user/docs")

	cfg := Default()
	cfg.StorageRoot = mainDir
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: mainDir},
		{Name: "vault", Type: nestedSecretType, Extra: map[string]any{"target": "main/alice/user/docs"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	err := enclosure.AttachAll(context.Background(), h.volSet, testLogger(), enclosure.AttachOptions{})
	if !errors.Is(err, enclosure.ErrRegionOccupied) {
		t.Fatalf("覆盖既有用户数据目录的声明应装配失败（ErrRegionOccupied），got %v", err)
	}
	assertUserDataIntact(t, dir)
}

// TestEnclosureRegion_BootAllowsFreshOrOwnRegion 启动路径正例：空区间 / 仅本卷自建结构 ⇒ 装配通过。
func TestEnclosureRegion_BootAllowsFreshOrOwnRegion(t *testing.T) {
	t.Parallel()
	registerNestedSecretWrapper()
	mainDir := t.TempDir()
	// 本卷自建结构形态（LAYOUT_VERSION + meta 桶 + 随机容器目录）：重启收养必须通过。
	self := filepath.Join(mainDir, "videos")
	if err := os.MkdirAll(filepath.Join(self, "meta"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(self, "aBcDe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(self, "LAYOUT_VERSION"), []byte("2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	cfg.StorageRoot = mainDir
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: mainDir},
		{Name: "vault", Type: nestedSecretType, Extra: map[string]any{"target": "main/videos"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	if err := enclosure.AttachAll(context.Background(), h.volSet, testLogger(), enclosure.AttachOptions{}); err != nil {
		t.Fatalf("空/本卷结构区间应可装配，got %v", err)
	}
	if h.volSet.External("vault") == nil {
		t.Fatal("vault 后端应已挂回卷集")
	}
}

// TestEnclosureRegion_CreateRejectsPopulatedOwnerRegion 建卷 API：400/409 拒绝且**不落盘**、
// 既有数据完好（validateNewEnclosure 在 store.Create 之前拦截 ⇒ 无卷残留）。
func TestEnclosureRegion_CreateRejectsPopulatedOwnerRegion(t *testing.T) {
	t.Parallel()
	h, store := newNestedWrapperAPIHandlers(t)
	mainDir := h.volSet.Root("main").AbsPath()
	dir := populateOrdinaryDir(t, mainDir, "alice/user/docs")

	mux := userVolWrap(h, "alice")
	rec := postUserVolume(t, mux, map[string]any{
		"name": "vault-region", "type": wrapperFSType,
		"extra": map[string]any{"target": "main/alice/user/docs"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("覆盖既有用户数据目录应 409, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if v, err := store.Get("alice", "vault-region"); err != nil || v != nil {
		t.Fatalf("拒绝后不得落盘 store（v=%v err=%v）", v, err)
	}
	if h.volSet.External("vault-region") != nil {
		t.Fatal("拒绝后不得注册运行时卷")
	}
	assertUserDataIntact(t, dir)
}

// TestEnclosureRegion_CreateAllowsEmptyRegion 建卷 API 正例：空区间（不存在）⇒ 200 且可用。
func TestEnclosureRegion_CreateAllowsEmptyRegion(t *testing.T) {
	t.Parallel()
	h, _ := newNestedWrapperAPIHandlers(t)
	mux := userVolWrap(h, "alice")
	rec := postUserVolume(t, mux, map[string]any{
		"name": "vault-fresh", "type": wrapperFSType,
		"extra": map[string]any{"target": "main/alice/user/fresh"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("空区间应 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if h.volSet.External("vault-fresh") == nil {
		t.Fatal("创建成功应挂回运行时卷")
	}
}

// TestEnclosureRegion_AttachEnclosureVolumeRejectsPopulatedRegion 运行期/恢复装配入口
// （serverenclosure.AttachEnclosureVolume，建卷 API 预检之外的**兜底**）：直接调用同样 fail-closed。
//
// 为什么需要兜底：重启恢复路径（cmd/sproxy/user_volume_restore.go）与 config 装配都不经
// validateNewEnclosure——它们是**收养已持久化声明**的入口，恰恰是最需要防误删的路径。
func TestEnclosureRegion_AttachEnclosureVolumeRejectsPopulatedRegion(t *testing.T) {
	t.Parallel()
	h, _ := newNestedWrapperAPIHandlers(t)
	mainDir := h.volSet.Root("main").AbsPath()
	dir := populateOrdinaryDir(t, mainDir, "alice/user/docs")

	tv, nested := serverenclosure.TranslateNestedVolume(volume.Volume{
		Name: "vault-restore", Type: wrapperFSType,
		Extra: map[string]any{"target": "main/alice/user/docs"},
	})
	if !nested {
		t.Fatal("应为嵌套封装声明")
	}
	err := serverenclosure.AttachEnclosureVolume(context.Background(), h.volSet, testLogger(), mainDir, tv)
	if !errors.Is(err, enclosure.ErrRegionOccupied) {
		t.Fatalf("恢复装配应拒绝既有数据区间（ErrRegionOccupied），got %v", err)
	}
	if h.volSet.External("vault-restore") != nil {
		t.Fatal("拒绝后不得注册运行时卷")
	}
	assertUserDataIntact(t, dir)
}

// TestEnclosureAttach_ConcurrentOverlappingDeclarationsSerialized 钉住运行期封装装配的**串行化**
// （2026-10-15 代码审核 B 轮发现）：装配是「**读**卷集校验足迹 → **写**卷集登记」的非原子序列，
// 而「同基底卷上两条无祖先关系的声明不得重叠」这条**互斥占用**不变量正是靠该校验维持。两个并发
// 请求若同时通过校验 ⇒ 两卷登记进同一足迹区间 ⇒ 互斥被破坏（删其一会擦除另一卷数据）。
//
// 判据：N 个并发装配**同一空区间**的声明 ⇒ **恰好 1 个成功**（其余 fail-closed），且最终卷集里
// 只有一个卷占用该区间。去掉 `enclosureAttachMu` 时本用例稳定出现多个成功（`-race` 亦报竞争）。
func TestEnclosureAttach_ConcurrentOverlappingDeclarationsSerialized(t *testing.T) {
	t.Parallel()
	h, _ := newNestedWrapperAPIHandlers(t)
	mainDir := h.volSet.Root("main").AbsPath()

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var okCount int
	errs := make([]error, n)
	names := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("vault-conc-%d", i)
			tv, nested := serverenclosure.TranslateNestedVolume(volume.Volume{
				Name: name, Type: wrapperFSType,
				Extra: map[string]any{"target": "main/alice/user/conc"},
			})
			var err error
			if !nested {
				err = errors.New("应为嵌套封装声明")
			} else {
				err = serverenclosure.AttachEnclosureVolume(context.Background(), h.volSet, testLogger(), mainDir, tv)
			}
			mu.Lock()
			errs[i], names[i] = err, name
			if err == nil {
				okCount++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if okCount != 1 {
		t.Fatalf("并发装配同一足迹区间应**恰好 1 个成功**（互斥占用不变量），got %d；errs=%v", okCount, errs)
	}
	// 最终卷集里只能有一个卷占用该区间（成功者恰好一个，且失败者未登记）。
	registered := 0
	for _, name := range names {
		if h.volSet.External(name) != nil {
			registered++
		}
	}
	if registered != 1 {
		t.Fatalf("卷集里占用该区间的卷应恰好 1 个，got %d", registered)
	}
}
