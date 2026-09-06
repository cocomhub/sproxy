// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

// 调用面纪律（4B-2 TOTP 登录）：
//
//   - TOTP 登录只走本文件的 DeriveTOTPWrapKey + KindTOTPWrap 信封这一条路径：
//     DeriveTOTPWrapKey(code, ak, nonce) 派生信封密钥，EncryptSecretKind(KindTOTPWrap, …)
//     / DecryptSecretKind(w, KindTOTPWrap, …) 包裹/解开会话 session SK。
//   - TOTP 登录派发的 session SK 不参与 secret_wrap 包裹链：它既不是由旧 SK
//     经 DeriveWrapKey(WrapContextCredentials#mesh) 包裹的 renew 产物，也不会被
//     admin credentials 流程（renew / sk 列表 per-key wrap）当作新凭据条目挂载。
//     会话 SK 仅在本次登录的短 TTL 内用于签名，过期即丢弃——不得以 KindSecretWrap
//     包裹、不得落入 credentials JSON 的 Entries。
//   - 因此 credentials 一侧的 EncryptSecret / DecryptSecret（默认 KindSecretWrap）
//     不得用来处理 TOTP 登录信封；TOTP 信封必须显式 KindTOTPWrap（DecryptSecret
//     对 KindTOTPWrap 信封也会拒绝，见 wrap_test.go）。
import "crypto/sha256"

// WrapContextTOTP 是 TOTP 登录 session 密钥信封加密的 wrap context 固定前缀
// （唯一事实源，与 WrapContextCredentials 同法收归 accesskey；client/server/JS 引用
// 本常量或与它一致的别名，禁止自行内联字面量漂移）。
//
// 实际派生用 `WrapContextTOTP + "#" + nonce`（nonce 由服务端 nonce 端点一次性签发，
// 见 spec §6.3），使不同 nonce 派生不同信封密钥——TOTP 动态码熵只有 6 位（10^6），
// 由 nonce 唯一性 + 限频 + per-AK 锁定 + 短 session TTL 共同补偿（不能代替密码学强度）。
//
// 与 WrapContextCredentials 刻意不同：TOTP wrap key 的 HKDF secret（wrapKey 第一参）
// 虽同为 32B，但它是 sha256(动态码) 的展开，而非某条 SK；“#"+nonce 追加进 context 也
// 使它与凭据 wrap 路径（"#"+mesh）无法串用。跨 context 复用防护由
// TestDeriveTOTPWrapKey_CrossContext 双向断言。
const WrapContextTOTP = "sproxy-totp/v1"

// DeriveTOTPWrapKey 从 TOTP 动态码 + AK + nonce 派生 TOTP 信封密钥：
//
//	key = wrapKey(sha256(code), ak, WrapContextTOTP+"#"+nonce)
//
// 内部复用现有 wrapKey（HKDF-SHA256，salt=“sproxy-accesskey-wrap/v1\x00”+context，
// info=ak）：6 位动态码 10 字节字符串先经 SHA-256 展为 32B HKDF 输入（输入域 256bit，
// 实际熵仍受 6 位码限制——由 nonce 唯一性 + 限频 + per-AK 锁定 + 短 session TTL 补偿，
// spec §6.3）。返回 32B（AES-256）信封密钥。
//
// 与 admin 流程的 credentials wrap（DeriveWrapKey + WrapContextCredentials#mesh）互为
// 隔离：context 前缀不同且 salt 经 wrapKey 内嵌，TOTP 派生的信封无法被 admin 凭据路径
// 解开、反之亦然（防跨 context 复用）。
func DeriveTOTPWrapKey(code string, ak, nonce string) ([]byte, error) {
	sum := sha256.Sum256([]byte(code))
	return wrapKey(sum[:], ak, WrapContextTOTP+"#"+nonce)
}
