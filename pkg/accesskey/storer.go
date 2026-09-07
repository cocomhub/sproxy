// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

// CredentialStorer 是凭据 Ring 快照的持久化抽象（core 域自包含，只依赖 Key）。
// 实现约定：
//   - Load：文件不存在返回 (nil, nil)（首次启动）；数据损坏返回 error
//     （fail-closed，不静默重建，防止用空凭据表运行）；
//   - Save：原子写（临时文件 + rename），失败返回 error（调用方记
//     credential_persist_error）。
//
// 宿主（pkg/server.CredentialStore）与外部 KMS 插件均实现本接口；
// 注册表装配见 plugin.go。
type CredentialStorer interface {
	Load() ([]Key, error)
	Save(keys []Key) error
}

// SecureStorer 是静态存储加密抽象（对整份凭据文件的字节级加解密）。
// 4C-2（KMS 加密实现）将提供真正的加密实现；本任务仅默认明文装配。
type SecureStorer interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

// PlainStorer 是 SecureStorer 的明文默认实现（未开启加密时装配）：
// Encrypt / Decrypt 均原样返回输入（深拷贝，避免返回入参切片被调用方改写污染）。
type PlainStorer struct{}

// 编译期断言：PlainStorer 满足 SecureStorer（防签名漂移，与
// pkg/server/credentialstore.go 的 `var _ accesskey.CredentialStorer` 同款模式）。
var _ SecureStorer = PlainStorer{}

// Encrypt 原样返回输入（深拷贝）。
func (PlainStorer) Encrypt(p []byte) ([]byte, error) {
	return append([]byte(nil), p...), nil
}

// Decrypt 原样返回输入（深拷贝）。
func (PlainStorer) Decrypt(c []byte) ([]byte, error) {
	return append([]byte(nil), c...), nil
}
