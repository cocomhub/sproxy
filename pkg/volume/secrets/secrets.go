// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package secrets 是 secret 文件管理封装卷（设计 docs/designs/2026-10-01-secret-volume.md
// §6.1）：底层指向任意卷的 `secrets/` 目录（可以是 local/baidupcs/s3/webdav，
// 也可以是 secret_data 加密卷——嵌套封装）。寻址 `secrets://<卷名>/<name>`，
// 默认卷可省略。管理 API：创建（随机 32B hex 密钥）/ 列 / 选默认 / 读取。
//
// secrets 卷只存普通文件（secret 文件本身不加密）；本地盘创建时权限 0600
// （ssh 密钥式），外部盘（baidupcs/s3）无权限要求（能拿到 = 有权限）。
package secrets

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// ErrNotFound 是读取/删除不存在的 secret 的哨兵错误。
var ErrNotFound = errors.New("secrets: secret 不存在")

// Manager 是 secrets 卷管理句柄：底层任意 sync.FS + 卷名寻址。
// 线程安全：底层 FS 自身保证（LocalFS os 调用 / 外部后端并发安全）。
type Manager struct {
	fs    syncpkg.FS
	name  string
	local bool
}

// NewManager 构造 secrets 管理句柄。
// fs：底层卷的 secrets/ 目录视图（sync.FS）；name：卷名（寻址前缀）；
// local：底层是否为本地盘（决定 0600 权限语义）。
func NewManager(fs syncpkg.FS, name string, local bool) *Manager {
	return &Manager{fs: fs, name: name, local: local}
}

// Name 返回卷名（寻址 `secrets://<name>/` 前缀）。
func (m *Manager) Name() string { return m.name }

// Create 创建（或覆盖）一个 secret：生成随机 32B hex 密钥（64 字符）写入
// `secrets/<name>`（本地 0600）。返回生成的密钥字节。
func (m *Manager) Create(ctx context.Context, name string) ([]byte, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, "/\\") {
		return nil, fmt.Errorf("secrets: 非法 secret 名 %q（不能为空、不能含路径分隔符）", name)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("secrets: 随机密钥生成失败: %w", err)
	}
	key := []byte(hex.EncodeToString(buf))
	if err := m.fs.WriteFile(ctx, name, bytes.NewReader(key), int64(len(key)), 0); err != nil {
		return nil, fmt.Errorf("secrets: 写入 %q 失败: %w", name, err)
	}
	if m.local {
		// 本地盘权限 0600（ssh 密钥式）：底层为 LocalFS 时直接落盘后收紧权限。
		// 外部盘（baidupcs/s3）无权限要求——无法也无需 chmod。
		if err := localChmod(m.fs, name); err != nil {
			return nil, fmt.Errorf("secrets: 本地 secret %q 权限收紧失败: %w", name, err)
		}
	}
	return key, nil
}

// Read 读取 secret 内容（密钥字节）。
func (m *Manager) Read(ctx context.Context, name string) ([]byte, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, "/\\") {
		return nil, fmt.Errorf("secrets: 非法 secret 名 %q", name)
	}
	rc, err := m.fs.OpenRead(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("secrets: 打开 %q 失败: %w", name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("secrets: 读 %q 失败: %w", name, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("secrets: secret %q 内容为空（损坏）", name)
	}
	return data, nil
}

// List 列出卷内全部 secret 名（排序）。
func (m *Manager) List(ctx context.Context) ([]string, error) {
	entries, err := m.fs.ListDir(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("secrets: 列目录失败: %w", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir {
			continue
		}
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out, nil
}

// Exists 探测 secret 是否存在。
func (m *Manager) Exists(ctx context.Context, name string) (bool, error) {
	ent, err := m.fs.Stat(ctx, strings.TrimSpace(name))
	if err != nil {
		return false, err
	}
	return ent != nil, nil
}

// SelectDefault 返回默认 secret 名：显式 defaultSecret 非空优先，否则全局默认
// （首个存在的 secret；无 secret 返回错误）。
func (m *Manager) SelectDefault(ctx context.Context, defaultSecret string) (string, error) {
	if strings.TrimSpace(defaultSecret) != "" {
		return strings.TrimSpace(defaultSecret), nil
	}
	names, err := m.List(ctx)
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", fmt.Errorf("secrets: 卷 %q 无 secret 可用（先 Create）", m.name)
	}
	return names[0], nil
}

// ---- ExternalBackend 适配 ----

// backend 是 secrets 卷的 registry.ExternalBackend 实现：持 Manager + sync.FS 视图。
type backend struct {
	mgr *Manager
	fs  syncpkg.FS
}

func (b *backend) FS() syncpkg.FS { return b.fs }
func (b *backend) Close() error   { return nil }

var _ registry.ExternalBackend = (*backend)(nil)

// NewBackend 按卷描述构造 secrets 外部后端（V3 plugin）：
//   - v.Extra["target"]：底层卷名（必填；local 指本地默认卷 secrets/ 目录）；
//   - 底层寻址：targetFS 是**底层卷的 secrets/ 目录视图**——本地卷用
//     sync.NewLocalFS(<targetRoot>/secrets)；外部卷（baidupcs/s3/secretdata 等）
//     由装配层把底层后端 FS 的 `secrets/` 前缀视图传入（本函数不建前缀，由装配
//     按 target 类型决定——见 cmd/sproxy/secret_register.go）。
//
// 说明：本函数签名设计为接收**已就绪的 secrets/ 视图 FS**（构造解耦，便于装配层
// 递归解析 secrets→secretdata 嵌套），故 Extra 只消费 target/root 元数据用于错误
// 文案与默认目录；实际 FS 由装配层经 NewManagerWithFS 注入。
func NewBackend(ctx context.Context, v volume.Volume, fs syncpkg.FS) (registry.ExternalBackend, error) {
	if v.Type == "" || v.Type == volume.TypeLocal {
		return nil, fmt.Errorf("secrets backend: 卷 %q 类型 %q 不是外部 secrets 卷", v.Name, v.Type)
	}
	if fs == nil {
		return nil, fmt.Errorf("secrets backend: 卷 %q 底层 FS 未注入", v.Name)
	}
	mgr := NewManager(fs, v.Name, isLocalTarget(v))
	return &backend{mgr: mgr, fs: fs}, nil
}

// isLocalTarget 判定底层目标是否为本地盘（决定 0600 权限语义）：Extra.target 为空或
// "local" 视为本地；baidupcs/s3/webdav/secretdata 等外部目标返回 false。
func isLocalTarget(v volume.Volume) bool {
	t, _ := v.Extra["target"].(string)
	return strings.TrimSpace(t) == "" || t == "local"
}

// localChmod 对本地 secret 文件收紧权限为 0600。
// 底层为 sync.LocalFS 时直接 chmod（见 local_chmod.go）；其它 FS（外部）不支持——忽略。
func localChmod(fs syncpkg.FS, name string) error {
	lfs, ok := fs.(*syncpkg.LocalFS)
	if !ok {
		return nil // 外部盘无权限语义
	}
	return chmodPath(lfs, name)
}

// ManagerOfExternal 从 ExternalBackend 反取 secrets.Manager（装配层辅助）。
// 仅当 backend 是 secrets 卷适配器时返回非 nil。
type managerUnwrapper interface{ SecretsManager() *Manager }

func ManagerOfExternal(be registry.ExternalBackend) *Manager {
	if u, ok := be.(managerUnwrapper); ok {
		return u.SecretsManager()
	}
	return nil
}

// SecretsManager 暴露内部 Manager（供装配层反取）。
func (b *backend) SecretsManager() *Manager { return b.mgr }
