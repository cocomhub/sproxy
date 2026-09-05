// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// 凭据管理领域 API（/api/credentials* 客户端侧）。
//
// 与服务端 pkg/server/credentials_handler.go 构成双向契约：
//   - renew 用「当前本端 SK」签名（签名命中条目）；响应 wrapped_secret 用
//     credentialWrapKey(签名命中条目SK, ak, wrapContext) 派生信封解开（任务 5 契约）。
//   - 新 SK 立即生效（服务端多 SK 共存，旧 SK 仍可用）；TTL 由服务端控制，客户端无
//     ttl 参数。
//   - wrap context 与服务端 credentialWrapContext 同拼法：CredentialWrapContextPrefix
//     （mesh 非空追加 "#"+mesh）。mesh 由 AK 派生（accesskey.ParseMesh）。

// CredentialWrapContextPrefix 是凭据 wrap context 的固定前缀（值收归
// pkg/accesskey.WrapContextCredentials——唯一事实源，M5；本名作别名引用），
// 与服务端 pkg/server.credentialWrapContext 保持同一常量：
//   - 服务端：credentialWrapContext + ["#"+mesh]
//   - 客户端：CredentialWrapContextPrefix + ["#"+mesh]
//
// 任何一端改动必须全部同步，否则旧 SK 解不开服务端包裹的新 SK（renew 全部失败）。
const CredentialWrapContextPrefix = accesskey.WrapContextCredentials

// CredentialWrapContext 计算 wrap context（prefix + [#mesh]）——mesh 由 AK 派生：
// 空 mesh 保持前缀不带井号；非空追加 "#"+mesh。与 accesskey ParseMesh + 服务端
// credentialWrapKey 同拼法（双向契约）；cmd/web 测试复算闭环用。
func CredentialWrapContext(ak string) string {
	mesh := accesskey.ParseMesh(ak)
	if mesh == "" {
		return CredentialWrapContextPrefix
	}
	return CredentialWrapContextPrefix + "#" + mesh
}

// wrapContextFor 计算 wrap context（prefix + [#mesh]）。
func wrapContextFor(ak string) string {
	return CredentialWrapContext(ak)
}

// RenewResult 是 RenewAccessKey 的解包结果：新 SK 已从 wrapped_secret 解开并立即可用。
type RenewResult struct {
	// AK 被轮换的 AccessKey。
	AK string
	// NewSecret 新 SK 的 32B 字节（hex 编码后即 access_key_secret 配置值）。
	NewSecret []byte
	// SKID 新 SK 条目的 ID（sk-<12hex>），应回填 access_key_id 配置。
	SKID string
	// ExpiresAt 新 SK 的有效期（服务端 TTL 控制，zero 表示永久）。
	ExpiresAt time.Time
}

// renewCredentialResponse 与服务端 renewCredentialResponse 的 json tag 对齐。
type renewCredentialResponse struct {
	AK            string                   `json:"ak"`
	SKID          string                   `json:"sk_id"`
	Kind          accesskey.Kind           `json:"kind"`
	WrapKeyAK     string                   `json:"wrap_key_ak"`
	ExpiresAt     time.Time                `json:"expires_at"`
	WrappedSecret *accesskey.WrappedSecret `json:"wrapped_secret"`
}

// SKInfo 是 ListAccessKeys 解包后的条目摘要。
type SKInfo struct {
	SKID     string
	Created  time.Time
	Expires  time.Time
	Status   accesskey.Status
	MetaType string
	// Decrypted 解开后的 SK 字节；未能解开（未持有该条目 SK）时为 nil。
	Decrypted []byte
	// Masked 表示该条目无法解开（调用方未持有其 SK，仅持有能解自身条目的密钥）。
	Masked bool
}

// AddAKResult 是 AddAK 的结果。
type AddAKResult struct {
	AK   string
	SKID string
	// Secret 仅当服务端未显式接收 secret（生成并单次回传）时非空。安全警示（S49）：
	// 该字段是本端新 AK 的初始凭据，仅打印给用户，严禁写日志。
	Secret string
}

// AKSummary 是 ListAKs 的条目摘要（admin），json tag 对齐服务端 akSummary。
type AKSummary struct {
	AK      string `json:"ak"`
	Owner   string `json:"owner"`
	SKCount int    `json:"sk_count"`
	AliveSK int    `json:"alive_sk"`
}

// ErrNoCredentials 表示未配置 access_key_secret（renew 等签名操作无法进行）。
var ErrNoCredentials = errors.New("未配置 access_key_secret（SproxySig 凭据）")

// RenewAccessKey 轮换当前 AK 的 SK：用当前本端 SK 签名 POST /renew，用同一 SK 解开
// 服务端包裹的新 SK，并把本端凭据切换为新 SK + 新 entryID（renew 后立即可用）。
//
// 契约：
//   - wrap key = DeriveWrapKey(当前本端 SK, ak, "sproxy-credentials/v1[#mesh]")；
//     与服务端 credentialWrapKey 同拼法（mesh 从 AK 派生）。解不开（GCM auth 失败 /
//     kind 不符 / nonce/ciphertext 畸形）时返回错误，不误认成功。
//   - 成功后本端 accessKeySecret / accessKeyID 立即回填为响应值（多 SK 共存：
//     旧 SK 服务端仍可用，本端优先用新 SK 签名——服务端按 entryID 精确定位）。
func (c *FileClient) RenewAccessKey(ctx context.Context) (*RenewResult, error) {
	if c.accessKey == "" || c.accessKeySecret == "" {
		return nil, fmt.Errorf("%w: renew 需要 access_key 与 access_key_secret", ErrNoCredentials)
	}
	// access_key_secret 必须是合法 64-hex（32 字节）；非法则解 wrap 必然失败，直接报错
	// 而非等 GCM 校验失败再暴露（避免 must32 静默吞错路径）。
	if _, derr := hex.DecodeString(c.accessKeySecret); derr != nil {
		return nil, fmt.Errorf("本端 access_key_secret 非法（非 64-hex）: %w", ErrNoCredentials)
	}
	ak := c.accessKey
	urlPath := "/api/credentials/" + ak + "/renew"
	var resp renewCredentialResponse
	if err := c.doJSON(ctx, http.MethodPost, urlPath, struct{}{}, &resp); err != nil {
		return nil, fmt.Errorf("renew 失败: %w", err)
	}
	if resp.WrappedSecret == nil {
		return nil, fmt.Errorf("renew 失败: 响应缺少 wrapped_secret")
	}
	newSK, err := c.decryptWrappedSecret(ak, resp.WrappedSecret)
	if err != nil {
		return nil, err
	}
	res := &RenewResult{
		AK:        ak,
		NewSecret: newSK,
		SKID:      resp.SKID,
		ExpiresAt: resp.ExpiresAt,
	}
	// 回填本端凭据：新 SK 立即可用（服务端已多 SK 共存，旧 SK 仍可用）。
	c.accessKeySecret = hex.EncodeToString(newSK)
	c.accessKeyID = resp.SKID
	return res, nil
}

// decryptWrappedSecret 解开服务端包裹的 SK（wrap key 由本端旧 SK 派生）。
func (c *FileClient) decryptWrappedSecret(ak string, env *accesskey.WrappedSecret) ([]byte, error) {
	skBytes, err := hex.DecodeString(c.accessKeySecret)
	if err != nil || len(skBytes) != 32 {
		return nil, fmt.Errorf("本端 access_key_secret 非法（非 64-hex 32 字节）: %w", accesskey.ErrInvalidSecret)
	}
	wk, err := accesskey.DeriveWrapKey(skBytes, ak, wrapContextFor(ak))
	if err != nil {
		return nil, fmt.Errorf("派生信封密钥失败: %w", err)
	}
	newSK, err := accesskey.DecryptSecret(env, wk)
	if err != nil {
		return nil, fmt.Errorf("解不开 wrapped_secret（本端 SK 与包裹密钥不匹配或信封损坏）: %w", err)
	}
	return newSK, nil
}

// ListAccessKeys 列出目标 AK 的全部 SK 条目元数据；对每条按「该条目自身 SK」包裹的
// 信封，仅当调用方持有某条 SK（此处：本端 SK 与条目 SK 相同，或为 admin）才能解开。
func (c *FileClient) ListAccessKeys(ctx context.Context, ak string) ([]SKInfo, error) {
	if c.accessKeySecret == "" {
		return nil, ErrNoCredentials
	}
	var resp struct {
		AK    string           `json:"ak"`
		SKs   []skEntrySummary `json:"sk"`
		Total int              `json:"total"`
		Admin bool             `json:"admin"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/credentials/"+ak+"/sk", nil, &resp); err != nil {
		return nil, fmt.Errorf("查询 SK 列表失败: %w", err)
	}
	infos := make([]SKInfo, 0, len(resp.SKs))
	for i := range resp.SKs {
		s := resp.SKs[i]
		info := SKInfo{
			SKID:     s.SKID,
			Created:  s.Created,
			Expires:  s.Expires,
			Status:   s.Status,
			MetaType: s.MetaType,
		}
		if s.WrappedSecret == nil {
			info.Masked = true
			infos = append(infos, info)
			continue
		}
		// 尝试用本端 SK 解开：per-key wrap 用「条目自身 SK」打包，只有持有该条目 SK
		// 才能解（本端 SK == 条目 SK 时为本人条目）。解不开 → masked（不静默丢弃）。
		wk, werr := accesskey.DeriveWrapKey(must32(hex.DecodeString(c.accessKeySecret)), ak, wrapContextFor(ak))
		if werr != nil {
			info.Masked = true
			infos = append(infos, info)
			continue
		}
		if dec, derr := accesskey.DecryptSecret(s.WrappedSecret, wk); derr == nil {
			info.Decrypted = dec
		} else {
			info.Masked = true
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// skEntrySummary 与服务端 skEntrySummary 的 json tag 对齐。
type skEntrySummary struct {
	SKID          string                   `json:"sk_id"`
	Created       time.Time                `json:"created"`
	Expires       time.Time                `json:"expires"`
	Status        accesskey.Status         `json:"status"`
	MetaType      string                   `json:"meta_type"`
	WrappedSecret *accesskey.WrappedSecret `json:"wrapped_secret"`
}

// DeleteSK 删除目标 AK 的单条 SK（本人或 admin）。条目不存在返回 404 语义错误。
func (c *FileClient) DeleteSK(ctx context.Context, ak, skID string) error {
	if err := c.doJSON(ctx, http.MethodDelete, "/api/credentials/"+ak+"/sk/"+skID, nil, &doJSONResp{}); err != nil {
		return fmt.Errorf("删除 SK 失败: %w", err)
	}
	return nil
}

// ExpireSK 设单条 SK 的生效截止时间。until 为 zero 时清空（恢复永久有效）。
func (c *FileClient) ExpireSK(ctx context.Context, ak, skID string, until time.Time) error {
	req := struct {
		Until string `json:"until"`
	}{}
	if !until.IsZero() {
		req.Until = until.Format(time.RFC3339)
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/credentials/"+ak+"/sk/"+skID+"/expire", req, &doJSONResp{}); err != nil {
		return fmt.Errorf("设 SK 过期失败: %w", err)
	}
	return nil
}

// AddAK 新增一条 AK（admin-only）。secret 为 64-hex（32 字节），为空表示由服务端生成。
func (c *FileClient) AddAK(ctx context.Context, ak, owner, secret string) (*AddAKResult, error) {
	req := struct {
		AK     string `json:"ak"`
		Owner  string `json:"owner"`
		Secret string `json:"secret,omitempty"`
	}{AK: ak, Owner: owner, Secret: secret}
	var resp struct {
		AK     string `json:"ak"`
		SKID   string `json:"sk_id"`
		Secret string `json:"secret"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/credentials", req, &resp); err != nil {
		return nil, fmt.Errorf("新增 AK 失败: %w", err)
	}
	return &AddAKResult{AK: resp.AK, SKID: resp.SKID, Secret: resp.Secret}, nil
}

// DeleteAK 删除整个 AK（admin-only + 二次确认：confirm 必须等于目标 AK）。
// force=true 时即使有活跃 SK 也删除（CLI 交互确认后传入）。
func (c *FileClient) DeleteAK(ctx context.Context, ak string, force bool) error {
	req := struct {
		Confirm string `json:"confirm"`
		Force   bool   `json:"force"`
	}{Confirm: ak, Force: force}
	if err := c.doJSON(ctx, http.MethodDelete, "/api/credentials/"+ak, req, &doJSONResp{}); err != nil {
		return fmt.Errorf("删除 AK 失败: %w", err)
	}
	return nil
}

// ListAKs 列出全部 AK（admin-only）。
func (c *FileClient) ListAKs(ctx context.Context) ([]AKSummary, error) {
	var resp struct {
		AKs   []AKSummary `json:"ak"`
		Total int         `json:"total"`
	}
	// AKSummary 的 json tag 在类型上声明（ak/owner/sk_count/alive_sk）。
	if err := c.doJSON(ctx, http.MethodGet, "/api/credentials", nil, &resp); err != nil {
		return nil, fmt.Errorf("查询 AK 列表失败: %w", err)
	}
	return resp.AKs, nil
}

// must32 处理 hex.DecodeString 双返回值（调用方已校验长度；仅内部使用）。
func must32(b []byte, err error) []byte {
	if err != nil {
		return nil
	}
	return b
}
