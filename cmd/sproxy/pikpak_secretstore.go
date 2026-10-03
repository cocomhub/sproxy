// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume/ext/pikpak"
	"github.com/cocomhub/sproxy/pkg/volume/secretdata"
	"github.com/cocomhub/sproxy/pkg/volume/secrets"
)

// pikpakMasterSecret 是 pikpak 账号凭据加密卷的主密钥名（默认 secrets 卷内）。
const pikpakMasterSecret = "pikpak-master"

// pikpakEncryptedSecretStore 构造 pikpak 账号凭据的**加密存储**（C5，一次到位）：
//   - 主密钥放默认 secrets 卷（<storageRoot>/secrets 的 pikpak-master：Manager.Create
//     生成随机 32B 密钥，恰一次、重启复用——Manager 的设计即密钥管理）；
//   - 凭据以任意 JSON 内容写入加密卷 <secretRoot>（secretdata/shardseal 加密落盘），
//     经 pikpak.FSSecretStore 呈现（名称校验 + 内容任意）。
//
// storageRoot 为卷根；secretRoot 为空 = <storageRoot>/pikpak-secrets（config
// pikpak.secrets_dir 覆盖）。装配失败 fail-closed——refresh_token 是永久账号接管凭据，
// 绝不静默退回明文落盘。
func pikpakEncryptedSecretStore(ctx context.Context, storageRoot, secretRoot string, log *slog.Logger) (pikpak.SecretStore, error) {
	// 1. 主密钥：默认 secrets 卷（<storageRoot>/secrets，0700 目录）。
	secRoot := filepath.Join(storageRoot, "secrets")
	if err := os.MkdirAll(secRoot, 0o700); err != nil {
		return nil, fmt.Errorf("pikpak secret store: mkdir secrets dir: %w", err)
	}
	mgr := secrets.NewManager(secretsLocalFS(storageRoot), "default-secrets", true)
	masterKey, err := mgr.Read(ctx, pikpakMasterSecret)
	if errors.Is(err, fs.ErrNotExist) {
		masterKey, err = mgr.Create(ctx, pikpakMasterSecret) // 生成随机 32B 密钥并落盘
	}
	if err != nil {
		return nil, fmt.Errorf("pikpak secret store: 主密钥 %q 获取/生成失败: %w", pikpakMasterSecret, err)
	}
	// 2. 凭据加密卷：secretdata（shardseal 加密），默认落 <storageRoot>/pikpak-secrets。
	if secretRoot == "" {
		secretRoot = filepath.Join(storageRoot, "pikpak-secrets")
	}
	if mkdirErr := os.MkdirAll(secretRoot, 0o700); mkdirErr != nil {
		return nil, fmt.Errorf("pikpak secret store: mkdir encrypted root: %w", mkdirErr)
	}
	encFS, err := secretdata.NewFS(syncpkg.NewLocalFS(secretRoot, nil), secretdata.Options{Secret: masterKey})
	if err != nil {
		return nil, fmt.Errorf("pikpak secret store: 加密卷装配失败: %w", err)
	}
	if log != nil {
		log.Info("pikpak account credentials 加密卷就绪", "root", secretRoot)
	}
	return pikpak.NewFSSecretStore(encFS), nil
}
