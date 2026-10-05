// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cluster 实现集群出口（mesh 转发 + 凭证下发）：持有数据的节点可能无公网，
// 出口节点（公网可达，mesh node）持**凭证**从持有节点经 mesh 拉数据转发。
//
// 凭证模式（用户裁定 2026-10-05）：持有节点签发**自包含短效凭证**（HMAC 签名 +
// 时效 + scope + 路径范围），出口节点凭凭证访问指定真实卷——下发端控制时效/白名单/
// 范围。凭证替代静态 mesh_readers 指纹绑定（目标节点授权层动态验签）。
package clustercred

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TypeEgress 是集群出口卷的卷后端类型字面量（出口节点装配 `type: egress`）。
// 单一事实源：装配（RegisterBackend）与读路由私密判定共用。
const TypeEgress = "egress"

// ErrCredentialExpired 是凭证过期的哨兵错误。
var ErrCredentialExpired = errors.New("cluster: 凭证已过期")

// ErrCredentialBadSignature 是凭证签名不符的哨兵错误（防篡改/伪造）。
var ErrCredentialBadSignature = errors.New("cluster: 凭证签名不符")

// ErrCredentialScope 是凭证 scope 不符的哨兵错误（read vs 请求操作）。
var ErrCredentialScope = errors.New("cluster: 凭证 scope 不允许该操作")

// ErrCredentialPath 是请求路径超出凭证 path_prefix 范围的哨兵错误。
var ErrCredentialPath = errors.New("cluster: 请求路径超出凭证授权范围")

// ErrCredentialMalformed 是凭证载荷非法（缺字段/坏 JSON）的哨兵错误。
var ErrCredentialMalformed = errors.New("cluster: 凭证载荷非法")

// Credential 是集群出口的自包含短效凭证（用户裁定 2026-10-05）：
// 持有节点签发 → 出口节点持有 → 目标节点验签授权。下发端控制时效/白名单/范围。
//
// 签名：HMAC-SHA256(签发方 SK, canonical(payload))——签发方 SK 只存持有节点本端；
// 目标节点验签需持有同 SK（装配时注入，见 RegisterBackend 的 signKey）。
type Credential struct {
	// Node 是持有节点 mesh node ID（拨号寻址用）。
	Node string `json:"node"`
	// Volume 是持有侧真实卷名（远程访问目标）。
	Volume string `json:"volume"`
	// Owner 是数据归属 owner（目标节点据此限定命名空间）。
	Owner string `json:"owner"`
	// Recipient 是**出口节点**的 xfer 身份指纹（白名单：只对指定出口节点有效；
	// 目标节点授权时校验连接指纹 == Recipient）。
	Recipient string `json:"recipient"`
	// Scope 是授权范围（"read"；复用 mesh scope 归一语义）。
	Scope string `json:"scope"`
	// PathPrefix 是路径范围前缀（可空 = 全卷）；请求 rel 必须以其开头。
	PathPrefix string `json:"path_prefix,omitempty"`
	// IssuedAt / ExpiresAt 是签发/过期 Unix 秒（下发端控时效）。
	IssuedAt  int64 `json:"iat"`
	ExpiresAt int64 `json:"exp"`
	// Sig 是 HMAC-SHA256(签发方 SK, canonical(载荷)) hex。
	Sig string `json:"sig"`
}

// Canonical 构造凭证签名输入串（换行分隔，字段内不含换行故无歧义）。
func (c Credential) Canonical() string {
	parts := []string{
		"sproxy-cluster/credential/v1",
		c.Node,
		c.Volume,
		c.Owner,
		c.Recipient,
		c.Scope,
		c.PathPrefix,
		strconv.FormatInt(c.IssuedAt, 10),
		strconv.FormatInt(c.ExpiresAt, 10),
	}
	return strings.Join(parts, "\n")
}

// sign 计算凭证的 HMAC-SHA256 签名（hex）。
func (c Credential) sign(sk []byte) string {
	mac := hmac.New(sha256.New, sk)
	mac.Write([]byte(c.Canonical()))
	return hex.EncodeToString(mac.Sum(nil))
}

// Sign 由签发方填充签名（NewCredential 后调用；IssuedAt/ExpiresAt 已设）。
func (c *Credential) Sign(sk []byte) {
	c.Sig = c.sign(sk)
}

// Verify 在目标节点验签：签名 + 时效 + scope + 路径范围（下发端控制）。
// 验证通过返回 nil；篡改/过期/scope 不符/路径越界 → 对应哨兵错误。
func (c Credential) Verify(sk []byte, now time.Time) error {
	// 1. 载荷完备性。
	if c.Node == "" || c.Volume == "" || c.Owner == "" || c.Recipient == "" || c.Scope == "" {
		return ErrCredentialMalformed
	}
	if c.ExpiresAt <= c.IssuedAt {
		return ErrCredentialMalformed
	}
	// 2. 签名（防篡改/伪造——目标节点持签发方 SK）。
	if !hmac.Equal([]byte(c.Sig), []byte(c.sign(sk))) {
		return ErrCredentialBadSignature
	}
	// 3. 时效（下发端控制：过期即失效）。
	nowSec := now.Unix()
	if nowSec > c.ExpiresAt {
		return ErrCredentialExpired
	}
	if nowSec < c.IssuedAt {
		return ErrCredentialMalformed
	}
	// 4. scope（read；扩展点：write/rw 时校验请求操作）。
	if c.Scope != "read" {
		return ErrCredentialScope
	}
	return nil
}

// AuthorizedFor 判定目标节点侧的**连接对端指纹**是否被本凭证授权（白名单：
// Recipient 精确匹配——只对签发的出口节点有效）。在 Verify 之后调用。
func (c Credential) AuthorizedFor(peerFingerprint string) error {
	if c.Recipient != strings.TrimSpace(peerFingerprint) {
		return fmt.Errorf("%w: 对端指纹不在凭证白名单", ErrCredentialScope)
	}
	return nil
}

// Authorizes 判定请求 (node, volume, owner, rel) 是否落在凭证授权范围：
// node/volume/owner 精确匹配 + rel 以 PathPrefix 开头（空前缀 = 全卷）。
// 在 Verify 之后调用（签名/时效已验）。
func (c Credential) Authorizes(node, volume, owner, rel string) error {
	if c.Node != node || c.Volume != volume || c.Owner != owner {
		return fmt.Errorf("%w: 目标 (%s/%s/%s) 不在凭证范围", ErrCredentialScope, node, volume, owner)
	}
	if c.PathPrefix != "" && !strings.HasPrefix(rel, c.PathPrefix) {
		return ErrCredentialPath
	}
	return nil
}

// Marshal 把凭证编码为传输串（签发方 → 出口节点，经配置/下发）。
func (c Credential) Marshal() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("cluster: 凭证编码失败: %w", err)
	}
	return string(b), nil
}

// ParseCredential 从传输串解析凭证（出口节点装配时解码；不验签）。
func ParseCredential(encoded string) (Credential, error) {
	var c Credential
	if err := json.Unmarshal([]byte(encoded), &c); err != nil {
		return Credential{}, fmt.Errorf("%w: %v", ErrCredentialMalformed, err)
	}
	return c, nil
}

// NewNonceHex 生成随机 nonce（出口侧请求重放防护备用；16B hex）。
func NewNonceHex() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
	}
	return hex.EncodeToString(b)
}
