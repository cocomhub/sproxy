// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package accesskey 是凭据 Ring（AK→多 SK 权威表）与信封加密核心。
//
// 数据模型：一个 AK（Access Key）可挂载多条 SK 条目（SKEntry），每条持有独立的
// SK 密钥字节、生命周期（CreatedAt/ExpiresAt）、状态与元信息。物理层调用方（auth
// 验签 / hub.Authenticator / 派生密钥）通过 Ring 的统一快照查询获取"当前活跃"条目，
// 从而实现 SK 的运行时滚动（renew）与多条目共存，无需重启进程。
package accesskey

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Kind 描述 SK 条目的密钥形态（规格 5 的预留枚举）。
type Kind string

const (
	// KindPlain 明文 SK（初始凭据 / 兼容旧客户端）。
	KindPlain Kind = "plain"
	// KindSecretWrap 由信封加密包裹的 SK（renew 产物，4A 主路径）。
	KindSecretWrap Kind = "secret_wrap"
	// KindTOTPWrap TOTP 包裹的 SK（4B 预留枚举值，未实现）。
	KindTOTPWrap Kind = "totp_wrap"
)

// Status 描述 SK 条目的生命周期状态。
type Status string

const (
	// StatusActive 激活，可被使用。
	StatusActive Status = "active"
	// StatusExpired 已过期（达到 ExpiresAt，aliveLocked 判否）。
	StatusExpired Status = "expired"
	// StatusDisabled 已禁用（管理员显式禁用，aliveLocked 判否）。
	StatusDisabled Status = "disabled"
)

// sentinel 哨兵错误：调用方（auth / 验证 / 管理端点）据此精确区分失败类型。
var (
	// ErrNotFound 条目（AK 或 SK）不存在（404 语义）。
	ErrNotFound = errors.New("accesskey: entry not found")
	// ErrExpired 条目存在但已过期（或禁用），不可使用。
	ErrExpired = errors.New("accesskey: entry expired")
	// ErrDuplicate 条目 ID 重复。
	ErrDuplicate = errors.New("accesskey: duplicate entry id")
	// ErrInvalidSecret SK 非 32 字节（AES-256 密钥长度）。
	ErrInvalidSecret = errors.New("accesskey: invalid secret length")
	// ErrInvalidAK AK 非法（空串等）。
	ErrInvalidAK = errors.New("accesskey: invalid access key")
)

// Meta 是 SK 条目的附加元信息（审计 / 展示用）。
type Meta struct {
	// Type 条目类型，如 "renew"、"initial"（4B 可扩展）。
	Type string
	// IP 创建条目时的客户端来源 IP（审计线索）。
	IP string
}

// SKEntry 是单个 SK 凭据条目。
type SKEntry struct {
	// ID 唯一 ID，形如 sk-<12hex>（创建时由 newEntryID 生成）。
	ID string
	// SK 32 字节 AES-256 密钥字节。
	SK []byte
	// Kind 密钥形态（plain / secret_wrap / totp_wrap）。
	Kind Kind
	// WrapKeyID 当 Kind 为 secret_wrap 时，指明包裹该 SK 的信封密钥（AK）的 ID。
	WrapKeyID string
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// ExpiresAt 过期时间；零值表示永久有效。
	ExpiresAt time.Time
	// Status 生命周期状态（active / expired / disabled）。
	Status Status
	// Meta 附加元信息（类型 / 来源 IP）。
	Meta Meta
}

// Key 是单个 AK（Access Key）及其挂载的全部 SK 条目。
//
// 注意：Mesh 段不存字段。Mesh 由 AK 字符串派生（本包提供 ParseMesh 纯函数，
// 语义与 pkg/tunnel.AccessKeyMesh 一致），避免在两处各存一份导致漂移。
type Key struct {
	// AK Access Key 标识，形如 sk[-<mesh>]-<16hex>。
	AK string
	// Owner 该 AK 的归属者（租户 / 用户）。
	Owner string
	// Entries 该 AK 挂载的全部 SK 条目。
	Entries []SKEntry
}

// EntryIDLen 是 SKEntry.ID 中 hex 段长度（12 hex = 6 字节随机，共 6B 熵）。
const EntryIDLen = 12

// isHexChar 判断 c 是否为（小写或大写）十六进制字符（ParseMesh 用）。
// 与 pkg/tunnel.isHexString 的 hexChars 一致（含 ABCDEF），避免含大写 hex 的
// AK（管理导入 / 手工编辑产物）在两端解析出不同 mesh 导致派生参数不一致。
func isHexChar(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// newEntryID 生成 sk-<12hex> 的唯一条目 ID（6 字节 crypto/rand 熵）。
// crypto/rand 失败返回包装错误（调用方据此拒绝写入，不再复用其他哨兵）。
func newEntryID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("accesskey: generate entry id: %w", err)
	}
	return "sk-" + hex.EncodeToString(b), nil
}

// KeyPair 是一对静态凭据（AK + 64-hex SK hex 字符串），用于从静态名单装配 Ring。
// 不依赖任何上层包（hub/server），凭据域自包含。SK 必须是 32 字节（64 hex）。
type KeyPair struct {
	Key    string
	Secret string // 64-hex SK（hex 编码的 32 字节）
}

// NewRingFromKeyPairs 从 []KeyPair 构造 Ring（每条 AK 一条 plain alive 条目，
// Meta{Type:"initial"}）。SK 非 32 字节 / AK 非法（如空）的条目跳过；返回的
// ring 可用作 http/auth 与 hub.Authenticator 的共享凭据源。
func NewRingFromKeyPairs(pairs []KeyPair) *Ring {
	ring := NewRing()
	for _, k := range pairs {
		sk, err := hex.DecodeString(k.Secret)
		if err != nil || len(sk) != 32 {
			continue
		}
		if err := ring.UpsertAK(k.Key, ""); err != nil {
			continue
		}
		_, _ = ring.AddKey(k.Key, sk, WithMeta(Meta{Type: "initial"}))
	}
	return ring
}

// ParseMesh 从 SproxySig AccessKey 提取 mesh 段，语义与 pkg/tunnel.AccessKeyMesh
// 一致（AK 形如 sk[-<mesh>]-<16hex>）：
//   - sk-<mesh>-<hex>（mesh 可含连字符）→ mesh
//   - sk-<hex>（无 mesh 段）→ ""
//   - 其他格式 → ""
//
// 本包为消除对 pkg/tunnel 的依赖（避免循环依赖）内嵌复制了该实现；
// 两端派生参数一致性依赖此单一实现与 tunnel 保持同步。
func ParseMesh(ak string) string {
	if !strings.HasPrefix(ak, "sk-") {
		return ""
	}
	rest := strings.TrimPrefix(ak, "sk-")
	// 末尾必须为 -<16 hex>（mesh 段可含连字符，故取最后一个 '-'）。
	idx := strings.LastIndex(rest, "-")
	if idx <= 0 || idx+17 != len(rest) {
		return ""
	}
	hexPart := rest[idx+1:]
	if len(hexPart) != 16 {
		return ""
	}
	for i := 0; i < len(hexPart); i++ {
		if !isHexChar(hexPart[i]) {
			return ""
		}
	}
	return rest[:idx]
}
