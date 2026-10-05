// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// 多 target 副本底层 FS（任务 9d）。容器自包含（blob 不变），SecretdataFS 只需把对底层
// 的读写经 replicaFS 转发即可获得多副本：写复制到全部 target（写全部成功才算成功，任一
// 失败回滚已写子集）、读主 target 失败自动回退副本、删除同 Multi 删。复制发生在容器
// blob 级（每文件合成元/分块/parity 各写一次到全部 target），不改变 secretdata 逻辑。

import (
	"bytes"
	"context"
	"fmt"
	"io"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// NewFSMultiplicas 构造多副本 secretdata FS。primary 是主底层卷 FS（逻辑卷 0），replicas
// 是副本底层卷 FS（写入复制到全部 target、读主失败回退副本、删除 Multi 删）。replicas
// 为空时退化为单卷（零行为回归）。Options.Targets 记录配置的副本卷名（装配元数据），
// 实际的底层副本映射由本函数的 replicas 承担。
func NewFSMultiplicas(primary syncpkg.FS, replicas []syncpkg.FS, opts Options) (*SecretdataFS, error) {
	inner := primary
	if len(replicas) > 0 {
		all := make([]syncpkg.FS, 0, 1+len(replicas))
		all = append(all, primary)
		all = append(all, replicas...)
		inner = newReplicaFS(all)
	}
	return NewFS(inner, opts)
}

// replicaFS 是多底层卷的副本视图：写复制到全部、读主失败回退副本、删全删。实现 sync.FS。
type replicaFS struct {
	targets []syncpkg.FS // targets[0] = 主 target；其余为副本
}

var _ syncpkg.FS = (*replicaFS)(nil)

func newReplicaFS(all []syncpkg.FS) *replicaFS { return &replicaFS{targets: all} }

// ListDir/Stat/Rename/MakeDir：主 target 视图（副本布局与主一致，任意 target 均可列出）。
func (r *replicaFS) ListDir(ctx context.Context, path string) ([]syncpkg.Entry, error) {
	return r.targets[0].ListDir(ctx, path)
}

func (r *replicaFS) Stat(ctx context.Context, path string) (*syncpkg.Entry, error) {
	return r.targets[0].Stat(ctx, path)
}

func (r *replicaFS) Rename(ctx context.Context, from, to string) error {
	return r.targets[0].Rename(ctx, from, to)
}

func (r *replicaFS) MakeDir(ctx context.Context, path string) error {
	return r.targets[0].MakeDir(ctx, path)
}

// OpenRead 主 target 优先；主失败（缺失/损坏）回退副本。全部失败返回首个错误。
func (r *replicaFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	var lastErr error
	for _, fs := range r.targets {
		rc, err := fs.OpenRead(ctx, path)
		if err == nil {
			return rc, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// WriteFile 把同一内容写入全部 target（读一次、逐 target 写）。任一 target 写失败 →
// best-effort 回滚已写 target（保持副本间一致：写全部成功才算成功），并返回错误。
func (r *replicaFS) WriteFile(ctx context.Context, path string, reader io.Reader, size, mtime int64) error {
	data, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("replica: 读待写内容失败: %w", err)
	}
	var written []int
	for i, fs := range r.targets {
		if werr := fs.WriteFile(ctx, path, bytes.NewReader(data), int64(len(data)), mtime); werr != nil {
			for _, j := range written {
				_ = r.targets[j].Delete(ctx, path) // best-effort 回滚已写副本
			}
			return fmt.Errorf("replica: 写副本 target[%d] %s 失败（已回滚）: %w", i, path, werr)
		}
		written = append(written, i)
	}
	return nil
}

// Delete 副本同删（全部 target best-effort 删，返回主 target 结果——主删成功即视为删除）。
func (r *replicaFS) Delete(ctx context.Context, path string) error {
	for i := 1; i < len(r.targets); i++ {
		_ = r.targets[i].Delete(ctx, path)
	}
	return r.targets[0].Delete(ctx, path)
}

// IsLocalVolume 封装卷委派（syncpkg.LocalVolume，用户裁定 2026-10-05）：multiplicas
// 副本视图委派主 target——**任一副本外部即外部**（targets[0] 即主 target；多 target
// 本地加密卷 = 内部 → 转存走用户配额）。漏委派会让多副本本地卷被误报外部（配额绕过）。
func (r *replicaFS) IsLocalVolume() bool {
	if len(r.targets) == 0 {
		return false
	}
	if lv, ok := r.targets[0].(syncpkg.LocalVolume); ok {
		return lv.IsLocalVolume()
	}
	return false
}
