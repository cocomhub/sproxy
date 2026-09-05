// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package accesskey 是凭据 Ring（AK→多 SK 权威表）、信封加密核心，以及 **AK/SK 生成与
// 解析的唯一权威（唯一事实源）**：所有 AK/SK 的生成（GeneratePair / GeneratePairLegacy /
// GenerateID）与解析（ParseMesh / IsValidAK / RandomHexHex）一律收归本包，其它包
// （server / tunnel / client / cmd）禁止自行实现生成或解析逻辑，只能调用本包（参见
// MeshFrom 的委托说明）。
//
// 数据模型：一个 AK（Access Key）可挂载多条 SK 条目（SKEntry），每条持有独立的
// SK 密钥字节、生命周期（CreatedAt/ExpiresAt）、状态与元信息。物理层调用方（auth
// 验签 / hub.Authenticator / 派生密钥）通过 Ring 的统一快照查询获取"当前活跃"条目，
// 从而实现 SK 的运行时滚动（renew）与多条目共存，无需重启进程。
//
// # 权威 AK/SK 格式规格（单一事实源，一切生成/解析/文档以此为唯一口径）
//
//	AK = sk[-<mesh>]-<32hex>       （标准）：sk- 前缀 + 可选 mesh（可含连字符）+ 32 hex 随机段 = 16 字节 = 2^128 熵
//	AK = sk[-<mesh>]-<16hex>       （legacy 兼容）：16 hex 随机段 = 8 字节 = 2^64 熵（旧 8B AK，仅解析层向后兼容）
//	SK = <64hex>                   （恒 64 hex = 32 字节 = AES-256 密钥长度）
//
// 规则：
//   - 前缀白名单：AccessKeyPrefix = "sk-" 是唯一允许前缀；往后如需引入新类型
//     （如 hub-/relay-/totp-/api-）只改动 AllowedAKPrefixes 白名单，解析逻辑不变。
//   - 随机段：标准恒 32 hex（AccessKeyHexLen*2 字符）；解析层同时接受 legacy 16 hex。
//   - mesh 段只允许 [0-9A-Za-z_-] 字符，可含连字符，可为空。
//
// 生成（GeneratePair）一律产 32hex(16B) 标准形态；解析
// （ParseMesh / pkg/tunnel.AccessKeyMesh / IsValidAK）接受标准与 legacy 双兼容，
// 其它形态一律拒绝（返回空 mesh / false）。
package accesskey

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
	// AK Access Key 标识，形如 sk[-<mesh>]-<32hex>（legacy 兼容 sk[-<mesh>]-<16hex>）。
	AK string
	// Owner 该 AK 的归属者（租户 / 用户）。
	Owner string
	// Entries 该 AK 挂载的全部 SK 条目。
	Entries []SKEntry
}

// EntryIDLen 是 SKEntry.ID 中 hex 段长度（12 hex = 6 字节随机，共 6B 熵）。
const EntryIDLen = 12

// KeyPair 是一对静态凭据（AK + 64-hex SK hex 字符串），用于从静态名单装配 Ring。
// 不依赖任何上层包（hub/server），凭据域自包含。SK 必须是 32 字节（64 hex）。
type KeyPair struct {
	Key    string
	Secret string // 64-hex SK（hex 编码的 32 字节）
}

// AccessKeyHexLen 是标准 AK 随机段的字节长度（16 字节 = 32 hex 字符，2^128 熵）。
// 语义：Generation 用 AccessKeyHexLen 字节（32 hex）产随机段；解析层（ParseMesh /
// IsValidAK）以 "随机段 hex 字符数 = AccessKeyHexLen*2（32）" 为标准，同时按
// AccessKeyHexLegacy 兼容旧 8B（16 hex）AK。
const AccessKeyHexLen = 16

// AccessKeyHexLegacy 是 legacy（旧 8B）AK 随机段的字节长度（8 字节 = 16 hex 字符，
// 2^64 熵）。仅解析 / 显式 legacy 生成（GeneratePairLegacy）使用：接受旧凭据（含既有
// 16hex 网格）的 mesh 解析，不参与新标准生成。
const AccessKeyHexLegacy = 8

// AccessKeyPrefix 是 AK 的唯一类型前缀（标准 AK 一律以它开头）。
//
// 这是 4B 类型前缀扩展点：将来如需引入新 AK 类型（如 hub-/relay-/totp-/api-），
// 只需在此追加新前缀常量并把其加入 AllowedAKPrefixes 白名单，解析逻辑（ParseMesh /
// IsValidAK）不需要改动（它们只认白名单，回溯到本常量数组）。
const AccessKeyPrefix = "sk-"

// AllowedAKPrefixes 是允许的 AK 类型前缀白名单（当前唯一允许 sk-）。
// 新增 AK 类型前缀时把新前缀常量并入本数组即可，无需求改任何解析函数。
var AllowedAKPrefixes = []string{AccessKeyPrefix}

// meshCharset 是 mesh 段的允许字符集（字母数字 + 下划线 + 连字符）。
const meshCharset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-"

// GeneratePair 生成一对 AccessKey/AccessKeySecret：
//   - AccessKey（公开标识）= sk-<mesh>-<32hex>（mesh 为空时 sk-<32hex>）
//   - AccessKeySecret（本地密钥）= 32B 随机 hex（64 hex chars）
//
// r 传 nil 时用 crypto/rand（生产路径）；测试可注入确定性 reader。与客户端
// pkg/client 的 access_key/access_key_secret 配置及服务端 pkg/server 的 access_keys
// 配置对应；sclient `trust ak add` 未显式指定 AK 时用本函数在本地生成一对
// （原 cmd generateAccessKeyPair 删除后内联，唯一事实源）。
func GeneratePair(r io.Reader, mesh string) (ak, sk string, err error) {
	if r == nil {
		r = rand.Reader
	}
	akBytes := make([]byte, AccessKeyHexLen) // 16 字节 = 32 hex 随机段（标准形态）
	if _, err := io.ReadFull(r, akBytes); err != nil {
		return "", "", fmt.Errorf("accesskey: generate ak: %w", err)
	}
	skBytes := make([]byte, 32)
	if _, err := io.ReadFull(r, skBytes); err != nil {
		return "", "", fmt.Errorf("accesskey: generate sk: %w", err)
	}
	return meshHex(mesh) + hex.EncodeToString(akBytes), hex.EncodeToString(skBytes), nil
}

// meshHex 把 mesh 拼进 AK 前缀："sk-"（mesh 为空）或 "sk-<mesh>-"（mesh 非空）。
// 生成路径的 mesh 拼法（与解析层取最后一个 '-' 的语义互逆；AccessKeyPrefix 含尾 '-'）。
func meshHex(mesh string) string {
	if mesh == "" {
		return AccessKeyPrefix // => "sk-"
	}
	return AccessKeyPrefix + mesh + "-"
}

// RandomHexHex 生成 n 字节 crypto/rand 随机数的 hex 编码（供本包内生成/Prefix+Random 组装用）。
// 其它包若要"随机段+前缀"拼接，应使用本包导出的生成函数（GeneratePair / GenerateID /
// GeneratePairLegacy），而非自行调用 rand。失败返回包装错误。
func RandomHexHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("accesskey: random: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// GenerateID 生成 sk-<12hex> 的随机凭据 ID 段（等价 newEntryID 的导出入口；ext 子模块
// 可经此获得权威 ID 段，无需自行内联生成）。
func GenerateID(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	b := make([]byte, EntryIDLen/2) // 6 字节 = 12 hex
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("accesskey: generate id: %w", err)
	}
	return "sk-" + hex.EncodeToString(b), nil
}

// GeneratePairLegacy 生成 legacy（旧 8B）形态的 AK 对：sk[-<mesh>]-<16hex>（随机段
// AccessKeyHexLegacy 字节 = 16 hex）。仅用于导出/回填既有 16hex 兼容凭据，或解析层
// 双兼容语料的确定性构造；常规新凭据一律用 GeneratePair（标准 32hex）。
func GeneratePairLegacy(r io.Reader, mesh string) (ak, sk string, err error) {
	if r == nil {
		r = rand.Reader
	}
	akBytes := make([]byte, AccessKeyHexLegacy)
	if _, err := io.ReadFull(r, akBytes); err != nil {
		return "", "", fmt.Errorf("accesskey: generate legacy ak: %w", err)
	}
	skBytes := make([]byte, 32)
	if _, err := io.ReadFull(r, skBytes); err != nil {
		return "", "", fmt.Errorf("accesskey: generate legacy sk: %w", err)
	}
	return meshHex(mesh) + hex.EncodeToString(akBytes), hex.EncodeToString(skBytes), nil
}

// WrapContextCredentials 是凭据信封加密的 wrap context 固定前缀（唯一事实源，M5）。
//
// 服务端（pkg/server.credentialWrapContext）与客户端（pkg/client.CredentialWrapContextPrefix）
// 分别以别名引用本常量；任何一端改动必须全部同步，否则旧 SK 解不开服务端包裹的新 SK
// （renew 全部失败）。实际派生用 `WrapContextCredentials + "#" + mesh`（mesh 为空保持
// 无井号），使不同 mesh 派生不同信封密钥（spec 7.4 明令 wrapKey(旧SK, mesh)）。
const WrapContextCredentials = "sproxy-credentials/v1"

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

// MeshFrom 取 AK 的 mesh 段（等价 ParseMesh 的别名入口，供调用方在语义需要
// "委托 tunnel" 语境下使用；两者实现相同）。AK 解析一律收归本包（用户硬约束）。
func MeshFrom(ak string) string { return ParseMesh(ak) }

// ParseMesh 从 SproxySig AccessKey 提取 mesh 段（tunnel.AccessKeyMesh 薄委托指向本
// 实现；AK 形如 sk[-<mesh>]-<hex>，随机段接受标准 32hex 与 legacy 16hex 双兼容）：
//   - sk-<mesh>-<32hex> / sk-<mesh>-<16hex>（mesh 可含连字符）→ mesh
//   - sk-<32hex> / sk-<16hex>（无 mesh 段）→ ""
//   - 其他格式 → ""
//
// 兼容性说明：16hex（legacy 8B AK）与 32hex（标准 16B AK）均正确解析 mesh；
// 其它随机段长度一律拒绝（返回空 mesh）——若拒绝会破坏现存 16hex 凭据（含既有
// 16hex 资源网格）的服务端派生/mesh 审计，故必须保留双兼容。
func ParseMesh(ak string) string {
	for _, p := range AllowedAKPrefixes {
		if after, ok := strings.CutPrefix(ak, p); ok {
			rest := after
			// 末尾必须为 -<hex>（mesh 段可含连字符，故取最后一个 '-'）；
			// hex 段长度 ∈ {16, 32}（legacy 8B / 标准 16B）。
			idx := strings.LastIndex(rest, "-")
			if idx < 0 {
				return ""
			}
			hexPart := rest[idx+1:]
			if _, ok := hexSegmentOK(hexPart); !ok {
				return ""
			}
			return rest[:idx]
		}
	}
	return ""
}

// hexSegmentOK 校验 AK 末尾随机 hex 段：长度 ∈ {16, 32}（标准 32 hex / legacy 16 hex）
// 且有且仅有 hex 字符（isHexChar 判断；允许大写 ABCDEF）。命中时返回 (hexLen, true)。
func hexSegmentOK(s string) (int, bool) {
	var n int
	switch len(s) {
	case AccessKeyHexLen * 2: // 32 hex（标准 16B）
		n = AccessKeyHexLen
	case AccessKeyHexLegacy * 2: // 16 hex（legacy 8B）
		n = AccessKeyHexLegacy
	default:
		return 0, false
	}
	allHex := true
	for i := 0; i < len(s); i++ {
		if !isHexChar(s[i]) {
			allHex = false
			break
		}
	}
	if !allHex {
		return 0, false
	}
	return n, true
}

// IsValidAK 判断 ak 是否为合法 AccessKey：
//   - 前缀 ∈ AllowedAKPrefixes（当前唯一可接受 sk-）——白名单回溯，未来新类型前缀
//     加入白名单即自动放行；
//   - 末尾随机 hex 段为 32hex（标准）或 16hex（legacy）
//   - mesh 段只允许 [0-9A-Za-z_-] 字符（连字符/字母数字/下划线；无 mesh 段时整个
//     rest 即随机段，无需 mesh 校验）。
func IsValidAK(ak string) bool {
	if ak == "" {
		return false
	}
	for _, p := range AllowedAKPrefixes {
		if !strings.HasPrefix(ak, p) {
			continue
		}
		rest := strings.TrimPrefix(ak, p)
		if rest == "" {
			return false
		}
		idx := strings.LastIndex(rest, "-")
		var hexPart, meshPart string
		if idx < 0 {
			// 无 mesh 段：整个 rest 即随机 hex 段。
			hexPart = rest
		} else {
			hexPart = rest[idx+1:]
			meshPart = rest[:idx]
			if meshPart == "" {
				// "sk--<hex>" 双连字符歧义 → 拒绝（空 mesh 应写作 sk-<hex>）。
				return false
			}
		}
		if _, ok := hexSegmentOK(hexPart); !ok {
			return false
		}
		if meshPart == "" {
			return true // 无 mesh 段
		}
		for i := 0; i < len(meshPart); i++ {
			if !strings.ContainsRune(meshCharset, rune(meshPart[i])) {
				return false
			}
		}
		return true
	}
	return false
}
