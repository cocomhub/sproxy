// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// fingerprintPrefix 是身份指纹的展示前缀，形如 "sha256:<64 hex>"。
	fingerprintPrefix = "sha256:"
	// sha256HexLen 是 SHA-256 摘要的十六进制长度。
	sha256HexLen = 64
	// identityFilePerm 是身份文件的权限（仅属主可读写）。
	identityFilePerm = 0600
	// identityFileDirPerm 是身份文件所在目录的权限。
	identityFileDirPerm = 0700
	// identitySigDomain 是身份签名的域分离前缀，防止跨协议/跨消息重放。
	identitySigDomain = "sproxy-identity-v1"
	// ed25519PublicKeyLen 是 Ed25519 公钥长度（32B）。
	ed25519PublicKeyLen = ed25519.PublicKeySize
	// ed25519SignatureLen 是 Ed25519 签名长度（64B）。
	ed25519SignatureLen = ed25519.SignatureSize
)

var (
	// ErrIdentityFileCorrupt 表示身份文件损坏或内容非法。
	// 加载失败时 fail-closed 返回错误，绝不静默重建覆盖用户文件。
	ErrIdentityFileCorrupt = errors.New("identity: 身份文件损坏")
)

// Identity 表示隧道节点长时身份密钥对（Ed25519）。
//
// 指纹 = SHA-256(公钥) 的 hex（64 字符），展示格式 "sha256:<hex>"。
// 身份是独立于共享密钥 + HMAC 认证之外的额外身份层，用于对端公钥指纹 pinning（防 MITM）。
//
// 选择 Ed25519（而非 X25519）的原因：pinning 必须是"真正持有身份私钥的证明"
// （proof of possession）——握手时对端用身份私钥对"双方临时 ECDH 公钥"签名，
// 校验方验签后确认对端持有私钥，再对身份公钥指纹做 pinning 匹配。X25519 只能
// 做 ECDH 无法签名，故身份密钥选用 Ed25519；身份公钥与 ECDH 临时公钥同为 32B，
// 在握手帧中通过第二阶段身份扩展帧结构（[标志][公钥][签名]）与第一阶段临时
// ECDH 公钥天然区分。
type Identity struct {
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
}

// identityFile 是 Identity 的磁盘持久化格式（JSON，UTF-8）。
// 私钥为 Ed25519 32B seed 的 hex；公钥为 32B 的 hex（加载时校验与私钥派生一致）。
type identityFile struct {
	PrivateKey string `json:"private_key"`
	PublicKey  string `json:"public_key"`
}

// GenerateIdentity 生成一个新的 Ed25519 身份密钥对。
func GenerateIdentity() (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generate: %w", err)
	}
	return &Identity{privateKey: priv, publicKey: pub}, nil
}

// PublicKey 返回身份公钥（32 字节，副本）。
func (id *Identity) PublicKey() []byte {
	if id == nil || id.publicKey == nil {
		return nil
	}
	return append([]byte(nil), id.publicKey...)
}

// Fingerprint 返回身份指纹，格式 "sha256:<64 hex>"。
func (id *Identity) Fingerprint() string {
	if id == nil || id.publicKey == nil {
		return ""
	}
	return FingerprintFromPublicKey(id.publicKey)
}

// FingerprintFromPublicKey 计算公钥的指纹：SHA-256(公钥)，格式 "sha256:<64 hex>"。
func FingerprintFromPublicKey(pub []byte) string {
	sum := sha256.Sum256(pub)
	return fingerprintPrefix + hex.EncodeToString(sum[:])
}

// Sign 用身份私钥对 msg 签名（proof of possession）。msg 应包含域分离前缀与握手上下文。
func (id *Identity) Sign(msg []byte) []byte {
	if id == nil || id.privateKey == nil {
		return nil
	}
	return ed25519.Sign(id.privateKey, msg)
}

// Verify 用身份公钥验证 msg 的签名。
func (id *Identity) Verify(msg, sig []byte) bool {
	if id == nil || id.publicKey == nil {
		return false
	}
	return ed25519.Verify(id.publicKey, msg, sig)
}

// SaveIdentity 将身份原子持久化到 path（唯一临时文件 + rename），权限 0600。
// 自动创建父目录（0700）。
// 安全权衡：os.Chmod 在 Windows 上是 no-op，身份文件权限 0600 依赖 os.CreateTemp
// 默认权限与路径隔离（XDG 用户配置目录），不把权限位当唯一安全边界（fail-closed 逻辑兜底）。
func SaveIdentity(id *Identity, path string) error {
	if id == nil || id.privateKey == nil || id.publicKey == nil {
		return fmt.Errorf("identity: 空身份")
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, identityFileDirPerm); err != nil {
			return fmt.Errorf("identity: 创建目录 %s: %w", dir, err)
		}
	}
	data, err := json.Marshal(identityFile{
		PrivateKey: hex.EncodeToString(id.privateKey.Seed()),
		PublicKey:  hex.EncodeToString(id.publicKey),
	})
	if err != nil {
		return fmt.Errorf("identity: 序列化: %w", err)
	}
	// 唯一临时文件名（非固定 .tmp），避免崩溃残留固定名文件。
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("identity: 创建临时文件: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // rename 成功后 Remove 为 no-op
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("identity: 写入临时文件: %w", err)
	}
	if err := tmp.Chmod(identityFilePerm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("identity: 设置临时文件权限: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("identity: 关闭临时文件: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("identity: 替换身份文件: %w", err)
	}
	return nil
}

// LoadIdentity 从 path 加载身份。文件不存在时返回 *os.PathError（可用 os.IsNotExist 判断）；
// 文件损坏/内容非法时返回 ErrIdentityFileCorrupt（fail-closed，不静默重建）。
func LoadIdentity(path string) (*Identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f identityFile
	if err = json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%w: JSON 解析失败: %v", ErrIdentityFileCorrupt, err)
	}
	seed, err := hex.DecodeString(f.PrivateKey)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%w: 私钥格式非法", ErrIdentityFileCorrupt)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	// 显式两值断言：errcheck(check-type-assertions) 要求检查类型断言结果，避免类型异常时 panic。
	pubAny := priv.Public()
	pub, ok := pubAny.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%w: 私钥派生公钥类型异常: %T", ErrIdentityFileCorrupt, pubAny)
	}
	// 校验存盘公钥与私钥派生一致（防文件损坏导致私钥与公钥不配对）。
	if f.PublicKey != "" {
		pubHex := hex.EncodeToString(pub)
		if !strings.EqualFold(f.PublicKey, pubHex) {
			return nil, fmt.Errorf("%w: 公钥与私钥不匹配", ErrIdentityFileCorrupt)
		}
	}
	return &Identity{privateKey: priv, publicKey: pub}, nil
}

// LoadOrCreateIdentity 加载身份；文件不存在时生成并保存新身份。
// 文件已存在但损坏时返回 ErrIdentityFileCorrupt（不覆盖）。
// 注意：生产 CLI 路径未直接使用本函数（sclient identity generate 显式生成；
// factory 懒加载用 LoadIdentityOptional），保留供库使用方需要"自动创建"语义时调用。
func LoadOrCreateIdentity(path string) (*Identity, error) {
	id, err := LoadIdentity(path)
	if err == nil {
		return id, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	id, err = GenerateIdentity()
	if err != nil {
		return nil, err
	}
	if err := SaveIdentity(id, path); err != nil {
		return nil, err
	}
	return id, nil
}

// ParseFingerprint 归一化指纹输入：接受纯 64 hex、带 "sha256:" 前缀（大小写不敏感）、
// 大写 hex、首尾空白。返回规范化小写 "sha256:<64 hex>"。
func ParseFingerprint(s string) (string, error) {
	s = strings.TrimSpace(s)
	// 大小写不敏感剥离前缀：接受 "sha256:" / "SHA256:" / "Sha256:"。
	if len(s) >= len(fingerprintPrefix) && strings.EqualFold(s[:len(fingerprintPrefix)], fingerprintPrefix) {
		s = s[len(fingerprintPrefix):]
	}
	if len(s) != sha256HexLen {
		return "", fmt.Errorf("指纹长度非法: 期望 %d 个 hex 字符, 实际 %d", sha256HexLen, len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("指纹含非十六进制字符: %w", err)
	}
	return fingerprintPrefix + strings.ToLower(s), nil
}

// FingerprintMatches 恒时比较两个指纹（均已归一化为 "sha256:<64 hex>"）。
func FingerprintMatches(gotFP, wantFP string) bool {
	if len(gotFP) != len(wantFP) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(gotFP), []byte(wantFP)) == 1
}

// pinContains 报告 peerFP 是否命中 peerFingerprints 列表中的任一指纹（逐项归一化后恒时比较）。
func pinContains(peerFingerprints []string, peerFP string) bool {
	for _, fp := range peerFingerprints {
		norm, err := ParseFingerprint(fp)
		if err != nil {
			continue
		}
		if FingerprintMatches(peerFP, norm) {
			return true
		}
	}
	return false
}
