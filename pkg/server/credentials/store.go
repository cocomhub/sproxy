// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// store.go 是凭据 Ring 的 **StateStore 存储后端**（statestore.md §5.1 P0）：
// 双读单写（StateStore.Get 优先、未命中回退旧 <meta>/credentials.json；Save 恒写
// StateStore 新路径），可选静态加密（secure 非 nil 时 StateStore 值为密文）。
//
// 磁盘/值字节与既有 accesskey.CredentialStore 的 {version, keys} JSON 逐字一致。

package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/state"
)

// stateBackedCredentialStore 是凭据 Ring 的 StateStore 适配（statestore.md §5.1 P0）：
// 实现 accesskey.CredentialStorer，Load/Save 委托 StateStore.Get/Put（key 见
// credentialRingKey），磁盘字节与既有 credentialsFile（{version, keys}）JSON 逐字一致。
//
// 双读单写（零回归铁律，statestore.md §5.1）：
//   - Load：StateStore.Get 优先；ErrKeyNotFound → 回退读旧 <meta>/credentials.json
//     （legacyPath，迁移前存量零丢失）；旧文件也不存在 → (nil, nil)（U3 零凭据启动）；
//   - Save：恒写 StateStore 新路径（首写即完成迁移；旧 meta 文件不再改写）。
//
// secure 非 nil（credential_store.encrypt=true 时由装配层注入）：StateStore 值为密文字节，
// Vault/aesgcm 语义与 EncryptingStorer 一致（StateStore 之上做字节级加解密）。
type stateBackedCredentialStore struct {
	st         state.StateStore
	key        string
	legacyPath string // 旧 <meta>/credentials.json（回退读）；空 = 不回退
	secure     accesskey.SecureStorer
}

var _ accesskey.CredentialStorer = (*stateBackedCredentialStore)(nil)

// newStateBackedCredentialStore 构造 StateStore 后端凭据 store。
func newStateBackedCredentialStore(st state.StateStore, key, legacyPath string, secure accesskey.SecureStorer) *stateBackedCredentialStore {
	return &stateBackedCredentialStore{st: st, key: key, legacyPath: legacyPath, secure: secure}
}

// stateBackedCredentialsFile 是凭据快照的磁盘格式（与 accesskey.CredentialStore 的
// credentialsFile 结构同形：version=1 + keys）。accesskey 包的该结构未导出，故在本层
// 声明等价结构（与 accesskey/encrypting_storer.go 的 encryptedCredentialsFile 同款裁定）。
type stateBackedCredentialsFile struct {
	Version int             `json:"version"`
	Keys    []accesskey.Key `json:"keys"`
}

// stateLooksLikePlaintextJSON 是明文凭据 JSON 嗅探（与 accesskey 包内
// looksLikePlaintextJSON 同语义：首字节 '{' 且含 "keys"）——供加密态 Load 解密失败时
// 定向诊断「文件仍为明文未迁移」。仅影响错误文案，不影响 fail-closed 语义。
func stateLooksLikePlaintextJSON(data []byte) bool {
	if len(data) == 0 || data[0] != '{' {
		return false
	}
	return bytes.Contains(data, []byte(`"keys"`))
}

// Load 读凭据快照（StateStore 优先；未命中回退旧 meta 文件）。
// 值损坏（JSON 解析失败 / 结构非法）返回 error（fail-closed，凭据是权威，不静默重建）。
func (s *stateBackedCredentialStore) Load() ([]accesskey.Key, error) {
	data, found, err := s.readCredentialsData()
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil // U3：零凭据启动
	}
	if s.secure != nil {
		data, err = s.decryptCredentials(data)
		if err != nil {
			return nil, err
		}
	}
	var f stateBackedCredentialsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("credentials store: 解析失败（文件损坏，拒绝覆盖）: %w", err)
	}
	return f.Keys, nil
}

// readCredentialsData 读取凭据快照原始字节：StateStore.Get 优先；ErrKeyNotFound 回退
// 读旧 <meta>/credentials.json（迁移前存量）。found=false 表示确实无凭据数据
// （StateStore 未命中且无 legacy 可读）——调用方据此返回 (nil, nil)（U3 零凭据启动），
// 不再进入解析。
func (s *stateBackedCredentialStore) readCredentialsData() ([]byte, bool, error) {
	ctx := context.Background()
	data, err := s.st.Get(ctx, s.key)
	if err != nil {
		if !errors.Is(err, state.ErrKeyNotFound) {
			return nil, false, fmt.Errorf("credentials store: StateStore 读取失败: %w", err)
		}
		// 回退读旧 meta（迁移前存量）。
		if s.legacyPath != "" {
			data, err = os.ReadFile(s.legacyPath)
			if err != nil {
				if os.IsNotExist(err) {
					return nil, false, nil // U3：零凭据启动
				}
				return nil, false, fmt.Errorf("credentials store: 读取旧 %s 失败: %w", s.legacyPath, err)
			}
			return data, true, nil
		}
		return nil, false, nil
	}
	return data, true, nil
}

// decryptCredentials 解密凭据快照字节（secure 已装配时）。解密失败按数据形态给定向
// 诊断：明文 JSON（未迁移）vs 密文被篡改 / master key 不匹配——两者都 fail-closed。
func (s *stateBackedCredentialStore) decryptCredentials(data []byte) ([]byte, error) {
	pt, derr := s.secure.Decrypt(data)
	if derr != nil {
		if stateLooksLikePlaintextJSON(data) {
			return nil, fmt.Errorf("credentials store: 解密失败——文件仍为明文 JSON 未迁移（credential_store.encrypt=true 开启前既有凭据文件需先迁移为密文；fail-closed 拒绝，不静默重建）: %w", derr)
		}
		return nil, fmt.Errorf("credentials store: 解密失败——密文被篡改 / master key 不匹配（fail-closed 拒绝，不静默重建）: %w", derr)
	}
	return pt, nil
}

// Save 全量快照写 StateStore（序列化格式与 credentialstore.go 一致：{version, keys}）。
// secure 非 nil 时先加密再写（StateStore 值为密文）。
func (s *stateBackedCredentialStore) Save(keys []accesskey.Key) error {
	ctx := context.Background()
	f := stateBackedCredentialsFile{Version: 1, Keys: keys}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("credentials store: 序列化失败: %w", err)
	}
	if s.secure != nil {
		data, err = s.secure.Encrypt(data)
		if err != nil {
			return fmt.Errorf("credentials store: 加密失败: %w", err)
		}
	}
	if err := s.st.Put(ctx, s.key, data); err != nil {
		return fmt.Errorf("credentials store: StateStore 写入失败: %w", err)
	}
	return nil
}
