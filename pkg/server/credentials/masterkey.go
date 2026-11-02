// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// masterkey.go 是凭据静态加密的**密钥材料来源解析**：master key（aesgcm backend）与
// Vault token（vault backend）。两者都 fail-fast：来源缺失即报错，绝不以空密钥启动后
// 到首次解密才炸。

package credentials

import (
	"fmt"
	"os"
	"strings"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/server/config"
)

// ResolveCredentialMasterKey 解析 credential_store.encrypt=true 装配所需的 32B master
// key（单一事实源 = accesskey 的 LoadMasterKeyFromFile / MasterKeyFromBase64，本层只做
// 读取与来源选择，不写 AES/HKDF）。来源顺序：master_key_file 文件 > 环境变量
// CredentialMasterKeyEnv；两者都无 → error（fail-fast，防启动后解密失败用空凭据表运行）。
//
// 装配层也复用本函数解析同一 master key 加密用户卷 Extra（审查 P2），故导出。
func ResolveCredentialMasterKey(cfg *config.Config) ([]byte, error) {
	if cfg.CredentialStore.MasterKeyFile != "" {
		key, err := accesskey.LoadMasterKeyFromFile(cfg.CredentialStore.MasterKeyFile)
		if err != nil {
			return nil, fmt.Errorf("读取 credential_store.master_key_file 失败: %w", err)
		}
		return key, nil
	}
	if v := os.Getenv(config.CredentialMasterKeyEnv); v != "" {
		key, err := accesskey.MasterKeyFromBase64(v)
		if err != nil {
			return nil, fmt.Errorf("解析环境变量 %s 失败: %w", config.CredentialMasterKeyEnv, err)
		}
		return key, nil
	}
	return nil, fmt.Errorf("credential_store.encrypt=true 需配置 credential_store.master_key_file 或环境变量 %s（base64 编码 32B master key）", config.CredentialMasterKeyEnv)
}

// resolveVaultToken 解析 backend=vault 装配所需的 Vault token（来源顺序：token_file 文件
// （读入后 TrimSpace）> TokenEnv 环境变量 > error fail-fast）。token 值只用于构造
// VaultTransitStorer（HTTP 头），不落日志。
func resolveVaultToken(vc config.VaultConfig) (string, error) {
	if vc.TokenFile != "" {
		data, err := os.ReadFile(vc.TokenFile)
		if err != nil {
			return "", fmt.Errorf("读取 vault token 文件失败: %w", err)
		}
		tok := strings.TrimSpace(string(data))
		tok = strings.TrimPrefix(tok, "\uFEFF") // 清 UTF-8 BOM（Windows 编辑的 token 文件常带，否则 403 难排查）
		if tok != "" {
			return tok, nil
		}
	}
	envName := vc.TokenEnv
	if envName == "" {
		envName = "VAULT_TOKEN"
	}
	if tok := os.Getenv(envName); tok != "" {
		return tok, nil
	}
	return "", fmt.Errorf("credential_store.backend=vault 需配置 token_file 或环境变量 %s", envName)
}
