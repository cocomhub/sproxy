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
	"time"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/storage"
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
func pikpakEncryptedSecretStore(ctx context.Context, volRoot *storage.Root, storageRoot, secretRoot string, log *slog.Logger) (pikpak.SecretStore, error) {
	// 1. 主密钥：默认 secrets 卷（<storageRoot>/secrets，0700 目录）。
	secRoot := filepath.Join(storageRoot, "secrets")
	if err := os.MkdirAll(secRoot, 0o700); err != nil {
		return nil, fmt.Errorf("pikpak secret store: mkdir secrets dir: %w", err)
	}
	// 主密钥是**密钥材料**，必须与默认 secrets 卷同口径存储：卷根可用时经 `storage.Root`
	// （at-rest 加密卷上密文落盘 + 封印 + 历史明文一次性迁移），否则回落裸 LocalFS
	// （无卷语义可依附的场景，如独立目录/未装配测试）。口径必须与 `ensureDefaultSecretsVolume`
	// 一致——两个写入者用不同形态写同一目录会让对方读到「密文当密钥」（永久性损坏）。
	mgr := pikpakMasterKeyManager(volRoot, storageRoot, log)
	// 主密钥首次创建需**原子**（F3）：双进程并发首装（服务端首启 + CLI 首个 add）各生成
	// 不同随机密钥会 last-writer-wins 分裂、既有加密凭据不可解。用 O_EXCL 锁文件把
	// read-or-create 串行化（跨进程互斥；遗留锁超时删除后重试）。
	lockPath := filepath.Join(secRoot, ".pikpak-master.lock")
	var masterKey []byte
	serr := withExclusiveLock(lockPath, func() error {
		k, rerr := mgr.Read(ctx, pikpakMasterSecret)
		if errors.Is(rerr, fs.ErrNotExist) {
			k, rerr = mgr.Create(ctx, pikpakMasterSecret) // 生成随机 32B 密钥并落盘
		}
		if rerr != nil {
			return fmt.Errorf("主密钥 %q 获取/生成失败: %w", pikpakMasterSecret, rerr)
		}
		masterKey = k
		return nil
	})
	if serr != nil {
		return nil, fmt.Errorf("pikpak secret store: %w", serr)
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

// pikpakMasterKeyManager 构造主密钥的 secrets 管理器：卷根可用 → 经 Root 的视图（含 0600
// 回调）；否则回落裸 LocalFS（与 `secretsFSFor` 的「独立目录」分支同口径）。
func pikpakMasterKeyManager(volRoot *storage.Root, storageRoot string, log *slog.Logger) *secrets.Manager {
	if volRoot != nil {
		fs := server.NewRootSecretsFS(volRoot, "secrets", log)
		opts := []secrets.ManagerOption{}
		if c, ok := fs.(interface{ ChmodSecret(name string) error }); ok {
			opts = append(opts, secrets.WithLocalChmod(c.ChmodSecret))
		}
		return secrets.NewManager(fs, "default-secrets", true, opts...)
	}
	return secrets.NewManager(secretsLocalFS(storageRoot), "default-secrets", true)
}

// withExclusiveLock 用 O_CREATE|O_EXCL 锁文件串行化跨进程临界区（Windows 上 O_EXCL
// 同样语义）。fn 失败原样返回；正常路径删除锁文件。持锁者崩溃遗留锁 → 10s 超时后删
// 除重试（首次创建是罕见一次性事件，此启发足够；合法持锁 <1s）。
func withExclusiveLock(lockPath string, fn func() error) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, oerr := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if oerr == nil {
			f.Close()
			err := fn()
			_ = os.Remove(lockPath)
			return err
		}
		if !errors.Is(oerr, fs.ErrExist) {
			return oerr // 非 EEXIST：目录权限等真实错误，直接失败
		}
		if time.Now().After(deadline) {
			_ = os.Remove(lockPath) // 遗留锁（持锁者崩溃）→ 删除重试
			continue
		}
		time.Sleep(50 * time.Millisecond)
	}
}
