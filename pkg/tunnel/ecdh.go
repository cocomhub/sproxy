// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

const (
	ecdhPublicKeyLen = 32 // X25519 public key 长度
	sessionKeyLen    = 32 // AES-256 会话密钥长度
	ecdhSalt         = "sproxy-ecdh-salt-v1"

	// identityFlagPresent 表示握手身份扩展中"对端提供了身份公钥"。
	// 身份扩展帧结构：[1B flag][Ed25519 pub 32B][Ed25519 sig 64B] 或 [1B flag=0x00]。
	// 帧无独立版本字节；版本由签名域前缀 "sproxy-identity-v1"（identitySigDomain）隐含。
	// 未来协议演进需新增帧格式时，应在此扩展一个版本字节并更新 identitySigDomain。
	identityFlagPresent = 0x01
	// identityFlagAbsent 表示握手身份扩展中"对端无身份密钥"。
	identityFlagAbsent = 0x00
)

var (
	// ErrPeerFingerprintMismatch 表示对端身份指纹不匹配本端配置的 pinning 列表（fail-closed）。
	ErrPeerFingerprintMismatch = errors.New("tunnel: 对端身份指纹不匹配（pinning 校验失败）")
	// ErrPeerFingerprintRequired 表示本端配置了对端指纹 pinning，但对端未提供身份（fail-closed）。
	ErrPeerFingerprintRequired = errors.New("tunnel: 已配置对端指纹 pinning，但对端未提供身份")
	// ErrPeerIdentitySignature 表示对端身份签名验证失败——对端宣称的身份公钥与其
	// 身份私钥不匹配（无 proof of possession），可能为冒名方/中间人（fail-closed）。
	ErrPeerIdentitySignature = errors.New("tunnel: 对端身份签名验证失败（无身份私钥持有证明）")
)

// performHandshake 执行 ECDH X25519 密钥交换，返回会话密钥。
// 等价于 performHandshakeWithIdentity(ctx, m, dialer, nil, nil)：不交换身份、不校验 pin。
func performHandshake(ctx context.Context, m *mux.Mux, dialer bool) ([]byte, error) {
	sk, _, err := performHandshakeWithIdentity(ctx, m, dialer, nil, nil)
	return sk, err
}

// performHandshakeWithIdentity 执行 ECDH X25519 密钥交换，并在同一握手流上交换长时身份公钥，
// 按 peerFingerprints 对对端身份指纹做 pinning 校验（fail-closed）。
//
// dialer 为 true 表示发起方（客户端），false 表示接受方（服务端）。
// id 为本端长时身份（可为 nil，表示无身份）；peerFingerprints 为期望的对端指纹列表
// （可为空，表示不校验——向后兼容现状）。
// 返回会话密钥与对端身份指纹（对端未提供身份时为空字符串）。
//
// 握手流程：
//  1. 阶段 1（不变）：交换临时 ECDH X25519 公钥（各 32B），HKDF 派生会话密钥。
//  2. 阶段 2（可选身份扩展，向后兼容）：listener 先写 1 字节身份标志（0x01=带身份公钥，
//     0x00=无身份），dialer 读标志后响应自己的身份标志。旧对端（无扩展）读完 32B ECDH
//     即关闭流，对端读到 EOF 视为"对端未提供身份"。未配置 pin 时不因对端无身份失败。
//
// 旧对端兼容：新端与旧端握手时，身份扩展静默跳过；仅当本端配置了 peerFingerprints 时，
// 对端未提供身份会导致 fail-closed 拒绝（ErrPeerFingerprintRequired）。
func performHandshakeWithIdentity(ctx context.Context, m *mux.Mux, dialer bool, id *Identity, peerFingerprints []string) ([]byte, string, error) {
	curve := ecdh.X25519()
	privateKey, gErr := curve.GenerateKey(rand.Reader)
	if gErr != nil {
		return nil, "", fmt.Errorf("ecdh: generate key: %w", gErr)
	}
	publicKey := privateKey.PublicKey()

	var peerPublic []byte
	var stream mux.Stream

	if dialer {
		s, openErr := m.Open(ctx)
		if openErr != nil {
			return nil, "", fmt.Errorf("ecdh: open stream: %w", openErr)
		}
		defer s.Close()

		ourPub := publicKey.Bytes()
		if _, wErr := s.Write(ourPub); wErr != nil {
			return nil, "", fmt.Errorf("ecdh: write pubkey: %w", wErr)
		}

		peerPub := make([]byte, ecdhPublicKeyLen)
		if _, rErr := io.ReadFull(s, peerPub); rErr != nil {
			return nil, "", fmt.Errorf("ecdh: read peer pubkey: %w", rErr)
		}
		peerPublic = peerPub
		stream = s
	} else {
		s, acceptErr := m.Accept(ctx)
		if acceptErr != nil {
			return nil, "", fmt.Errorf("ecdh: accept stream: %w", acceptErr)
		}
		defer s.Close()

		peerPub := make([]byte, ecdhPublicKeyLen)
		if _, rErr := io.ReadFull(s, peerPub); rErr != nil {
			return nil, "", fmt.Errorf("ecdh: read peer pubkey: %w", rErr)
		}

		ourPub := publicKey.Bytes()
		if _, wErr := s.Write(ourPub); wErr != nil {
			return nil, "", fmt.Errorf("ecdh: write pubkey: %w", wErr)
		}
		peerPublic = peerPub
		stream = s
	}

	peerKey, pErr := curve.NewPublicKey(peerPublic)
	if pErr != nil {
		return nil, "", fmt.Errorf("ecdh: invalid peer public key: %w", pErr)
	}
	sharedSecret, eErr := privateKey.ECDH(peerKey)
	if eErr != nil {
		return nil, "", fmt.Errorf("ecdh: compute shared secret: %w", eErr)
	}

	sessionKey, kErr := hkdf.Key(sha256.New, sharedSecret, []byte(ecdhSalt), "sproxy-tunnel-ecdh-v1", sessionKeyLen)
	if kErr != nil {
		return nil, "", fmt.Errorf("ecdh: derive session key: %w", kErr)
	}

	// 身份交换：listener 先写、dialer 先读（固定方向避免死锁）。
	// 签名消息绑定双方临时 ECDH 公钥（固定顺序 dialer||listener）+ 域分离前缀，
	// 使身份签名成为本次握手的 proof of possession：对端必须持有身份私钥才能签名。
	var dialerPub, listenerPub []byte
	if dialer {
		dialerPub = publicKey.Bytes()
		listenerPub = peerPublic
	} else {
		dialerPub = peerPublic
		listenerPub = publicKey.Bytes()
	}
	sigMsg := identitySigMessage(dialerPub, listenerPub)

	// 身份阶段流读取不感知 ctx：恶意对端完成阶段 1 后可在身份阶段停滞，
	// 无限占住 dialer 的 ensureHandshake / listener 的 Serve goroutine（资源耗尽 DoS）。
	// 用 context.AfterFunc 在 ctx 超时/取消时 abort 握手流，使 io.ReadFull 立即返回，
	// 使 handshakeTimeout 对身份阶段真正兜底。
	stopAbort := context.AfterFunc(ctx, func() { _ = stream.Abort() })
	defer stopAbort()

	var peerFP string
	var idErr error
	if dialer {
		peerFP, idErr = handshakeIdentityDialer(stream, id, peerFingerprints, sigMsg)
	} else {
		peerFP, idErr = handshakeIdentityListener(stream, id, peerFingerprints, sigMsg)
	}
	if idErr != nil {
		return nil, "", idErr
	}

	return sessionKey, peerFP, nil
}

// identitySigMessage 构造身份签名消息：域分离前缀 + 双方临时 ECDH 公钥（dialer||listener）。
func identitySigMessage(dialerECDHPub, listenerECDHPub []byte) []byte {
	buf := make([]byte, 0, len(identitySigDomain)+2*ecdhPublicKeyLen)
	buf = append(buf, identitySigDomain...)
	buf = append(buf, dialerECDHPub...)
	buf = append(buf, listenerECDHPub...)
	return buf
}

// handshakeIdentityDialer 在握手流上执行身份交换的 dialer 侧：
// 先读 listener 的身份标志（EOF=旧对端无扩展），随后按协议响应。
// sigMsg 是双方临时 ECDH 公钥的绑定上下文，对端身份签名须对 sigMsg 有效（proof of possession）。
func handshakeIdentityDialer(s mux.Stream, id *Identity, peerFingerprints []string, sigMsg []byte) (string, error) {
	var flag [1]byte
	if _, err := io.ReadFull(s, flag[:]); err != nil {
		// EOF/错误：对端为旧实现（无身份扩展）。
		return "", checkPinAgainstAbsent(peerFingerprints)
	}
	switch flag[0] {
	case identityFlagPresent:
		peerFP, err := readPeerIdentity(s, sigMsg, peerFingerprints)
		if err != nil {
			return "", err
		}
		// 对端（新实现）在等待本端响应，必须回写标志防死锁。
		if err := writeIdentityFlag(s, id, sigMsg); err != nil {
			return "", err
		}
		return peerFP, nil
	case identityFlagAbsent:
		if len(peerFingerprints) > 0 {
			return "", ErrPeerFingerprintRequired
		}
		// 对端（新实现）无身份，但仍在等待本端响应。
		if err := writeIdentityFlag(s, id, sigMsg); err != nil {
			return "", err
		}
		return "", nil
	default:
		return "", fmt.Errorf("tunnel: 非法身份标志 0x%02x", flag[0])
	}
}

// handshakeIdentityListener 在握手流上执行身份交换的 listener 侧：
// 先写本端身份标志（旧对端可能读完 ECDH 即关闭，写多余字节安全），随后读 dialer 响应。
func handshakeIdentityListener(s mux.Stream, id *Identity, peerFingerprints []string, sigMsg []byte) (string, error) {
	if err := writeIdentityFlag(s, id, sigMsg); err != nil {
		return "", err
	}
	var flag [1]byte
	if _, err := io.ReadFull(s, flag[:]); err != nil {
		// EOF/错误：对端为旧实现（无身份扩展）。
		return "", checkPinAgainstAbsent(peerFingerprints)
	}
	switch flag[0] {
	case identityFlagPresent:
		return readPeerIdentity(s, sigMsg, peerFingerprints)
	case identityFlagAbsent:
		if len(peerFingerprints) > 0 {
			return "", ErrPeerFingerprintRequired
		}
		return "", nil
	default:
		return "", fmt.Errorf("tunnel: 非法身份标志 0x%02x", flag[0])
	}
}

// readPeerIdentity 读取对端身份公钥 + 签名，验签后做指纹 pinning 校验。
// 验签失败（对端宣称的身份公钥与其私钥不匹配，即无 proof of possession）→ ErrPeerIdentitySignature。
func readPeerIdentity(s mux.Stream, sigMsg []byte, peerFingerprints []string) (string, error) {
	pub := make([]byte, ed25519PublicKeyLen)
	if _, err := io.ReadFull(s, pub); err != nil {
		return "", fmt.Errorf("tunnel: 读取对端身份公钥: %w", err)
	}
	sig := make([]byte, ed25519SignatureLen)
	if _, err := io.ReadFull(s, sig); err != nil {
		return "", fmt.Errorf("tunnel: 读取对端身份签名: %w", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), sigMsg, sig) {
		return "", fmt.Errorf("%w: 对端宣称公钥 %s", ErrPeerIdentitySignature, FingerprintFromPublicKey(pub))
	}
	peerFP := FingerprintFromPublicKey(pub)
	if len(peerFingerprints) > 0 && !pinContains(peerFingerprints, peerFP) {
		return "", fmt.Errorf("%w: 期望 %v, 实际 %s", ErrPeerFingerprintMismatch, peerFingerprints, peerFP)
	}
	return peerFP, nil
}

// writeIdentityFlag 写入本端身份标志：有身份写 [0x01][公钥][签名]，无身份写 [0x00]。
// 签名用本端身份私钥对 sigMsg 计算，供对端验签（proof of possession）。
func writeIdentityFlag(s mux.Stream, id *Identity, sigMsg []byte) error {
	if id != nil {
		if _, err := s.Write([]byte{identityFlagPresent}); err != nil {
			return fmt.Errorf("tunnel: 写身份标志: %w", err)
		}
		if _, err := s.Write(id.PublicKey()); err != nil {
			return fmt.Errorf("tunnel: 写身份公钥: %w", err)
		}
		sig := id.Sign(sigMsg)
		if _, err := s.Write(sig); err != nil {
			return fmt.Errorf("tunnel: 写身份签名: %w", err)
		}
		return nil
	}
	if _, err := s.Write([]byte{identityFlagAbsent}); err != nil {
		return fmt.Errorf("tunnel: 写身份标志: %w", err)
	}
	return nil
}

// checkPinAgainstAbsent 在"对端未提供身份"场景下执行 fail-closed 判定：
// 本端配置了 pin 但对端无身份 → 拒绝；未配置 pin → 通过（向后兼容现状）。
func checkPinAgainstAbsent(peerFingerprints []string) error {
	if len(peerFingerprints) > 0 {
		return ErrPeerFingerprintRequired
	}
	return nil
}
