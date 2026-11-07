// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_wrapper_nested_test.go 提供「config 声明嵌套封装卷（`type: <wrapper 名>` +
// `extra.target=<卷>/<子目录>`）」的测试装配夹具，与生产装配顺序/形态一致。
//
// 方案甲：用户面 target 语法在**装配层翻译**出封装声明词汇（`extra.source` +
// `extra.wrapper`，见 pkg/server/enclosure/nested.go；**Type 不变**），底层由 enclosure 机制装配——故夹具需注册一个
// **同名封装处理层**（`enclosure.RegisterWrapper`）让翻译生效，后端即「区域视图上的
// secretdata」（与过渡期的 SubFS 装配物理落点等价）。
//
// 被 volume_wrapper_delete_quota_test.go（删除链路配额）与 volume_wrapper_root_list_test.go
// （根目录列举）共用。

import (
	"context"
	"os"
	"sync"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/enclosure"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/secretdata"
)

// nestedSecretType 是测试用封装处理层类型名（避开生产 "secretdata"，防污染共享注册表）。
const nestedSecretType = "nested-secretdata-test"

// nestedSecretWrapper 把「区域视图上的 secretdata」注册为封装处理层（与生产
// secretdataWrapper 同构，密钥固定、临时目录每次新建）。
type nestedSecretWrapper struct{}

func (nestedSecretWrapper) Name() string { return nestedSecretType }

// nestedTempDirs 记录本测试进程为 nested 封装创建的所有显式 TempDir。
//
// secretdata 对**显式配置的 TempDir 不清理（调用方所有）**，而 Build 没有 *testing.T，
// 故由测试侧登记 + 在 cleanupNestedTempDirs 里统一清理（否则每次运行残留一个目录，
// 实测 %TEMP% 累积 648 个 nested-secret-tmp*）。
var nestedTempDirs struct {
	mu   sync.Mutex
	dirs []string
}

// cleanupAllNestedTempDirs 删除本测试进程创建的所有 nested 临时目录。
// 由 pkg/server 的 TestMain 在全部测试结束后调用（不能在单个测试的 t.Cleanup 里做：
// 其它并行测试可能仍在使用这些目录）。
func cleanupAllNestedTempDirs() {
	nestedTempDirs.mu.Lock()
	dirs := nestedTempDirs.dirs
	nestedTempDirs.dirs = nil
	nestedTempDirs.mu.Unlock()
	for _, d := range dirs {
		_ = os.RemoveAll(d)
	}
}

func (nestedSecretWrapper) Build(ctx context.Context, v volume.Volume, src syncpkg.FS, _ map[string]any) (registry.ExternalBackend, error) {
	tmp, terr := os.MkdirTemp("", "nested-secret-tmp-*")
	if terr != nil {
		return nil, terr
	}
	nestedTempDirs.mu.Lock()
	nestedTempDirs.dirs = append(nestedTempDirs.dirs, tmp)
	nestedTempDirs.mu.Unlock()
	return secretdata.NewBackend(ctx, v, src, secretdata.Options{
		Secret: []byte("nested-delete-quota-key-01"), MetaPadBytes: 4096, TempDir: tmp,
	})
}

var registerNestedSecretOnce sync.Once

func registerNestedSecretWrapper() {
	registerNestedSecretOnce.Do(func() { enclosure.RegisterWrapper(nestedSecretWrapper{}) })
}

// newNestedSecretWrapperHandlers 装配「config 声明嵌套封装卷」的测试 Handlers：
// main 本地卷（容量 mainCap，0 = 不限）+ 卷 store + config 声明的封装卷 vault
// （type=nestedSecretType，target=main/videos）——经装配期**翻译**落到 enclosure 底座
// （封印注入 + 容量池继承 + 后端补装），与生产装配路径同形。
//
// extra 追加额外的本地卷（如跨卷 move 的目标卷）——不传则形态与既有一致（调用方零改动）。
func newNestedSecretWrapperHandlers(t *testing.T, mainCap int64, extra ...VolumeConfig) *Handlers {
	t.Helper()
	registerNestedSecretWrapper()

	mainDir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = mainDir
	cfg.Volumes = append([]VolumeConfig{
		{Name: "main", Root: mainDir, VolCapacity: ByteSize(mainCap)},
		{Name: "vault", Type: nestedSecretType, Extra: map[string]any{"target": "main/videos"}},
	}, extra...)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	h.SetUserVolumeStore(NewUserVolumeStore(mainDir))
	// 装配期只登记元数据 + 池（推迟装配）；后端补装与生产 setupServerCore 同层。
	if aerr := enclosure.AttachAll(context.Background(), h.volSet, testLogger(), enclosure.AttachOptions{}); aerr != nil {
		t.Fatalf("AttachAll: %v", aerr)
	}
	return h
}
