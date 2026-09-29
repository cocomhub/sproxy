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
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

const (
	ecdhPublicKeyLen = 32 // X25519 public key 长度
	sessionKeyLen    = 32 // AES-256 会话密钥长度

	// identityFlagPresent 表示握手身份扩展中"对端提供了身份公钥"。
	// 身份扩展帧结构：[1B flag][Ed25519 pub 32B][Ed25519 sig 64B] 或 [1B flag=0x00]。
	// 帧无独立版本字节；版本由签名域前缀 "sproxy-identity-v1"（identitySigDomain）隐含。
	// 未来协议演进需新增帧格式时，应在此扩展一个版本字节并更新 identitySigDomain。
	identityFlagPresent = 0x01
	// identityFlagAbsent 表示握手身份扩展中"对端无身份密钥"。
	identityFlagAbsent = 0x00
)

// 协议域分离盐（默认 sproxy 前缀）。const → var 支持运行时自定义前缀：
// stealth-build 隐藏二进制用 SetSaltPrefix 注入非 sproxy 前缀，使 strings 扫描
// 二进制时无 "sproxy-*" 协议指纹（防识别）。⚠️ 握手双方必须使用相同前缀
// （盐参与 HKDF 密钥派生，前缀不同 → 会话密钥不同 → 握手失败），
// 自定义部署需两端（stealth + 标准 sclient/sproxy）同步配置。
var (
	ecdhSalt       = "sproxy-ecdh-salt-v1"
	ecdhInfo       = "sproxy-tunnel-ecdh-v1"
	ecdhStaticSalt = "sproxy-ecdh-static-salt-v1"
	ecdhInfoStatic = "sproxy-tunnel-ecdh-v1-static"
)

// ProtocolSalts 是 5 个协议域分离盐的集合（防协议指纹识别用）。
// 盐本身不保密（域分离标签），但默认 sproxy 前缀可被 strings 识别协议。
// 自定义部署用 DeriveProtocolSalts(共享 key) 或 SetProtocolSalts(显式) 替换——
// 使二进制无 "sproxy-*" 协议指纹。⚠️ 握手双方必须使用**相同盐集**，
// 否则密钥派生不一致 → 握手失败。
type ProtocolSalts struct {
	ECDH        string
	Info        string
	Static      string
	InfoStatic  string
	IdentitySig string
}

// defaultSalts 是默认 sproxy 前缀盐集。
func defaultSalts() ProtocolSalts {
	return ProtocolSalts{
		ECDH:        ecdhSalt,
		Info:        ecdhInfo,
		Static:      ecdhStaticSalt,
		InfoStatic:  ecdhInfoStatic,
		IdentitySig: "sproxy-identity-v1",
	}
}

// DeriveProtocolSalts 用共享密钥确定性派生 5 个盐（同 key 同盐，两端一致）。
// 派生 = HKDF-SHA256(key, salt="protocol-salt-v1", info=<用途域>)。
// 用于：sclient --protocol-salt-key 启动时派生；stealth 构建时本地派生后写死。
// key 为 32B（64 hex）；nil/短 → 默认 sproxy 盐（零回归）。
func DeriveProtocolSalts(key []byte) ProtocolSalts {
	if len(key) != 32 {
		return defaultSalts()
	}
	der := func(info string) string {
		out, err := hkdf.Key(sha256.New, key, []byte("protocol-salt-v1"), info, 16)
		if err != nil {
			return ""
		}
		return hex.EncodeToString(out)
	}
	return ProtocolSalts{
		ECDH:        der("ecdh-salt"),
		Info:        der("ecdh-info"),
		Static:      der("ecdh-static-salt"),
		InfoStatic:  der("ecdh-info-static"),
		IdentitySig: der("identity-sig-domain"),
	}
}

// SetProtocolSalts 应用盐集到全局（进程启动时调用，任何握手前）。
// nil → 复位默认 sproxy 盐。
func SetProtocolSalts(s ProtocolSalts) {
	if s.ECDH == "" && s.Info == "" && s.Static == "" && s.InfoStatic == "" && s.IdentitySig == "" {
		SetSaltPrefix("")
		return
	}
	ecdhSalt = s.ECDH
	ecdhInfo = s.Info
	ecdhStaticSalt = s.Static
	ecdhInfoStatic = s.InfoStatic
	identitySigDomain = s.IdentitySig
}

// SetSaltPrefix 替换全部协议域分离前缀为自定义前缀（防协议指纹识别）。
// prefix 为空 = 复位默认 sproxy 前缀。**调用方必须保证握手对端使用相同前缀**，
// 否则会话密钥派生不一致导致握手失败。应在任何握手/拨号前调用（进程启动时）。
func SetSaltPrefix(prefix string) {
	ecdhSalt = "sproxy-ecdh-salt-v1"
	ecdhInfo = "sproxy-tunnel-ecdh-v1"
	ecdhStaticSalt = "sproxy-ecdh-static-salt-v1"
	ecdhInfoStatic = "sproxy-tunnel-ecdh-v1-static"
	identitySigDomain = "sproxy-identity-v1"
	if prefix == "" {
		return // 空 = 复位默认
	}
	ecdhSalt = prefix + "-ecdh-salt-v1"
	ecdhInfo = prefix + "-tunnel-ecdh-v1"
	ecdhStaticSalt = prefix + "-ecdh-static-salt-v1"
	ecdhInfoStatic = prefix + "-tunnel-ecdh-v1-static"
	identitySigDomain = prefix + "-identity-v1"
}

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
// 等价于 performHandshakeWithIdentity(ctx, m, dialer, nil, nil, nil)：不交换身份、
// 不校验 pin、不绑定静态密钥（纯 ECDH 派生，旧行为）。
func performHandshake(ctx context.Context, m *mux.Mux, dialer bool) ([]byte, error) {
	sk, _, err := performHandshakeWithIdentity(ctx, m, dialer, nil, nil, nil)
	return sk, err
}

// PerformHandshakeConn 在**裸连接**（net.Conn 或任意 io.ReadWriteCloser）上执行
// 与 performHandshakeWithIdentity 相同的 ECDH X25519 握手 + 可选身份交换（proof of
// possession + pinning fail-closed）。与 mux.Stream 版的区别：不走 mux 多路复用，
// 整个握手用同一条连接上的顺序字节流（dialer 先写公钥 → listener 响应 → 身份交换）。
//
// 用于端到端加密字节流形态（DialE2EStream/ServeE2EStream）：外层数据面连接是
// 单流（RelayStream 裸 TCP / webrtc 直连），无需 mux。身份阶段阻塞由 ctx 兜底——
// 调用方应在 ctx 超时/取消时关闭底层连接（net.Conn.SetDeadline 或关闭 conn）以解除
// io.ReadFull 阻塞；本函数不做 Abort（非 mux 流，无 Abort 语义）。
//
// 参数语义与 performHandshakeWithIdentity 一致：id 为本端长时身份（可为 nil，纯
// ECDH）；peerFingerprints 非空时对端必须提供身份且指纹命中（fail-closed）；
// staticKey 非 nil 时参与会话密钥派生（C-1 静态绑定）。
// 返回会话密钥与对端身份指纹（对端未提供身份时为空字符串）。
func PerformHandshakeConn(ctx context.Context, rw io.ReadWriteCloser, dialer bool, id *Identity, peerFingerprints []string, staticKey []byte) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	curve := ecdh.X25519()
	privateKey, gErr := curve.GenerateKey(rand.Reader)
	if gErr != nil {
		return nil, "", fmt.Errorf("ecdh: generate key: %w", gErr)
	}
	publicKey := privateKey.PublicKey()

	peerPublic, err := connExchangePubKey(rw, publicKey, dialer)
	if err != nil {
		return nil, "", err
	}
	sessionKey, err := handshakeECDHKey(curve, privateKey, peerPublic, staticKey)
	if err != nil {
		return nil, "", err
	}

	peerFP, idErr := connIdentityExchange(rw, dialer, publicKey, peerPublic, id, peerFingerprints)
	if idErr != nil {
		return nil, "", idErr
	}
	return sessionKey, peerFP, nil
}

// performHandshakeWithIdentity 执行 ECDH X25519 密钥交换，并在同一握手流上交换长时身份公钥，
// 按 peerFingerprints 对对端身份指纹做 pinning 校验（fail-closed）。
//
// dialer 为 true 表示发起方（客户端），false 表示接受方（服务端）。
// id 为本端长时身份（可为 nil，表示无身份）；peerFingerprints 为期望的对端指纹列表
// （可为空，表示不校验——向后兼容现状）。
// staticKey 是本端静态隧道密钥（AES-256，来自隧道配置/派生）；nil 表示无密钥模式
// （纯 ECDH 派生，旧行为）；非 nil 时静态密钥参与会话密钥派生（C-1 修复，见
// deriveSessionKey）。**同步发布协议变更**：一端混 key 而另一端不混时，两端派生出的
// sessionKey 不同，首个加密帧 AES-GCM 解密失败即被拒（fail-closed）。
// 返回会话密钥与对端身份指纹（对端未提供身份时为空字符串）。
func performHandshakeWithIdentity(ctx context.Context, m *mux.Mux, dialer bool, id *Identity, peerFingerprints []string, staticKey []byte) ([]byte, string, error) {
	curve := ecdh.X25519()
	privateKey, gErr := curve.GenerateKey(rand.Reader)
	if gErr != nil {
		return nil, "", fmt.Errorf("ecdh: generate key: %w", gErr)
	}
	publicKey := privateKey.PublicKey()

	stream, peerPublic, err := handshakeExchangePubKey(ctx, m, publicKey, dialer)
	if err != nil {
		return nil, "", err
	}
	defer stream.Close()

	sessionKey, err := handshakeECDHKey(curve, privateKey, peerPublic, staticKey)
	if err != nil {
		return nil, "", err
	}

	peerFP, idErr := handshakeIdentityExchange(ctx, stream, dialer, publicKey, peerPublic, id, peerFingerprints)
	if idErr != nil {
		return nil, "", idErr
	}
	return sessionKey, peerFP, nil
}

// handshakeExchangePubKey 在 mux 流上执行阶段 1：打开/接受一条流并交换临时 ECDH 公钥。
// dialer 先写自己的 X25519 公钥再读对端；listener 先读对端再写自己。返回流与对端公钥。
// 中间出错时关闭已打开流（避免泄漏）；成功时交由调用方 defer Close。
func handshakeExchangePubKey(ctx context.Context, m *mux.Mux, publicKey *ecdh.PublicKey, dialer bool) (mux.Stream, []byte, error) {
	if dialer {
		s, openErr := m.Open(ctx)
		if openErr != nil {
			return nil, nil, fmt.Errorf("ecdh: open stream: %w", openErr)
		}
		if _, wErr := s.Write(publicKey.Bytes()); wErr != nil {
			_ = s.Close()
			return nil, nil, fmt.Errorf("ecdh: write pubkey: %w", wErr)
		}
		peerPub := make([]byte, ecdhPublicKeyLen)
		if _, rErr := io.ReadFull(s, peerPub); rErr != nil {
			_ = s.Close()
			return nil, nil, fmt.Errorf("ecdh: read peer pubkey: %w", rErr)
		}
		return s, peerPub, nil
	}
	s, acceptErr := m.Accept(ctx)
	if acceptErr != nil {
		return nil, nil, fmt.Errorf("ecdh: accept stream: %w", acceptErr)
	}
	peerPub := make([]byte, ecdhPublicKeyLen)
	if _, rErr := io.ReadFull(s, peerPub); rErr != nil {
		_ = s.Close()
		return nil, nil, fmt.Errorf("ecdh: read peer pubkey: %w", rErr)
	}
	if _, wErr := s.Write(publicKey.Bytes()); wErr != nil {
		_ = s.Close()
		return nil, nil, fmt.Errorf("ecdh: write pubkey: %w", wErr)
	}
	return s, peerPub, nil
}

// connExchangePubKey 在裸连接（net.Conn / io.ReadWriteCloser）上执行阶段 1 公钥交换
// （dialer 先写后读，listener 先读后写），返回对端公钥。
func connExchangePubKey(rw io.ReadWriteCloser, publicKey *ecdh.PublicKey, dialer bool) ([]byte, error) {
	if dialer {
		if _, wErr := rw.Write(publicKey.Bytes()); wErr != nil {
			return nil, fmt.Errorf("ecdh: write pubkey: %w", wErr)
		}
		peerPub := make([]byte, ecdhPublicKeyLen)
		if _, rErr := io.ReadFull(rw, peerPub); rErr != nil {
			return nil, fmt.Errorf("ecdh: read peer pubkey: %w", rErr)
		}
		return peerPub, nil
	}
	peerPub := make([]byte, ecdhPublicKeyLen)
	if _, rErr := io.ReadFull(rw, peerPub); rErr != nil {
		return nil, fmt.Errorf("ecdh: read peer pubkey: %w", rErr)
	}
	if _, wErr := rw.Write(publicKey.Bytes()); wErr != nil {
		return nil, fmt.Errorf("ecdh: write pubkey: %w", wErr)
	}
	return peerPub, nil
}

// handshakeECDHKey 基于对端公钥计算 ECDH 共享密钥并派生出会话密钥（分层 HKDF，
// 供相参阶段 2 使用）。对端公钥非法 / 共享密钥计算失败 / 派生失败均返回 error。
func handshakeECDHKey(curve ecdh.Curve, privateKey *ecdh.PrivateKey, peerPublic, staticKey []byte) ([]byte, error) {
	peerKey, pErr := curve.NewPublicKey(peerPublic)
	if pErr != nil {
		return nil, fmt.Errorf("ecdh: invalid peer public key: %w", pErr)
	}
	sharedSecret, eErr := privateKey.ECDH(peerKey)
	if eErr != nil {
		return nil, fmt.Errorf("ecdh: compute shared secret: %w", eErr)
	}
	return deriveSessionKey(sharedSecret, staticKey)
}

// handshakeIdentityExchange 在 mux 流上执行阶段 2 身份交换：签名消息绑定双方临时 ECDH
// 公钥（固定顺序 dialer||listener）+ 域分离前缀，listener 先写、dialer 先读（避免死锁）。
// 用 context.AfterFunc 在 ctx 超时/取消时 abort 握手流，使 io.ReadFull 立即返回（身份阶段
// 阻塞兜底），对端指纹 pinning 校验在 handshakeIdentityDialer/Listener 内部完成。
func handshakeIdentityExchange(ctx context.Context, stream mux.Stream, dialer bool, publicKey *ecdh.PublicKey, peerPublic []byte, id *Identity, peerFingerprints []string) (string, error) {
	dialerPub, listenerPub := signedECDHPubs(dialer, publicKey, peerPublic)
	sigMsg := identitySigMessage(dialerPub, listenerPub)
	stopAbort := context.AfterFunc(ctx, func() { _ = stream.Abort() })
	defer stopAbort()
	if dialer {
		return handshakeIdentityDialer(stream, id, peerFingerprints, sigMsg)
	}
	return handshakeIdentityListener(stream, id, peerFingerprints, sigMsg)
}

// connIdentityExchange 在裸连接上执行阶段 2 身份交换（无 mux，故无 Abort 语义；
// 阻塞由调用方 ctx 兜底——超时则关闭底层 conn 解除 io.ReadFull）。其余与
// handshakeIdentityExchange 一致。
func connIdentityExchange(rw io.ReadWriteCloser, dialer bool, publicKey *ecdh.PublicKey, peerPublic []byte, id *Identity, peerFingerprints []string) (string, error) {
	dialerPub, listenerPub := signedECDHPubs(dialer, publicKey, peerPublic)
	sigMsg := identitySigMessage(dialerPub, listenerPub)
	if dialer {
		return handshakeIdentityDialer(rw, id, peerFingerprints, sigMsg)
	}
	return handshakeIdentityListener(rw, id, peerFingerprints, sigMsg)
}

// signedECDHPubs 返回身份签名的消息公钥对（固定顺序 dialer||listener）。
func signedECDHPubs(dialer bool, publicKey *ecdh.PublicKey, peerPublic []byte) (dialerPub, listenerPub []byte) {
	if dialer {
		return publicKey.Bytes(), peerPublic
	}
	return peerPublic, publicKey.Bytes()
}

func deriveSessionKey(sharedSecret, staticKey []byte) ([]byte, error) {
	baseKey, err := hkdf.Key(sha256.New, sharedSecret, []byte(ecdhSalt), ecdhInfo, sessionKeyLen)
	if err != nil {
		return nil, fmt.Errorf("ecdh: derive base session key: %w", err)
	}
	if staticKey == nil {
		return baseKey, nil
	}
	salt := make([]byte, 0, len(ecdhStaticSalt)+len(staticKey))
	salt = append(salt, ecdhStaticSalt...)
	salt = append(salt, staticKey...)
	sessionKey, err := hkdf.Key(sha256.New, baseKey, salt, ecdhInfoStatic, sessionKeyLen)
	if err != nil {
		return nil, fmt.Errorf("ecdh: derive static-bound session key: %w", err)
	}
	return sessionKey, nil
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
func handshakeIdentityDialer(rw io.ReadWriteCloser, id *Identity, peerFingerprints []string, sigMsg []byte) (string, error) {
	var flag [1]byte
	if _, err := io.ReadFull(rw, flag[:]); err != nil {
		// EOF/错误：对端为旧实现（无身份扩展）。
		return "", checkPinAgainstAbsent(peerFingerprints)
	}
	switch flag[0] {
	case identityFlagPresent:
		peerFP, err := readPeerIdentity(rw, sigMsg, peerFingerprints)
		if err != nil {
			return "", err
		}
		// 对端（新实现）在等待本端响应，必须回写标志防死锁。
		if err := writeIdentityFlag(rw, id, sigMsg); err != nil {
			return "", err
		}
		return peerFP, nil
	case identityFlagAbsent:
		if len(peerFingerprints) > 0 {
			return "", ErrPeerFingerprintRequired
		}
		// 对端（新实现）无身份，但仍在等待本端响应。
		if err := writeIdentityFlag(rw, id, sigMsg); err != nil {
			return "", err
		}
		return "", nil
	default:
		return "", fmt.Errorf("tunnel: 非法身份标志 0x%02x", flag[0])
	}
}

// handshakeIdentityListener 在握手流上执行身份交换的 listener 侧：
// 先写本端身份标志（旧对端可能读完 ECDH 即关闭，写多余字节安全），随后读 dialer 响应。
func handshakeIdentityListener(rw io.ReadWriteCloser, id *Identity, peerFingerprints []string, sigMsg []byte) (string, error) {
	if err := writeIdentityFlag(rw, id, sigMsg); err != nil {
		// 写身份扩展失败：对端可能在读完 ECDH 公钥后即关闭（旧实现无身份扩展），
		// 与下方 EOF 分支语义一致——未配置 pin 时视为"对端未提供身份"（向后兼容：
		// 避免新旧对端混用时监听侧握手失败回退静态密钥而对端用 ECDH 会话密钥，
		// 导致两端密钥不一致）；配置 pin 时仍 fail-closed 拒绝。
		if len(peerFingerprints) == 0 {
			return "", nil
		}
		return "", err
	}
	var flag [1]byte
	if _, err := io.ReadFull(rw, flag[:]); err != nil {
		// EOF/错误：对端为旧实现（无身份扩展）。
		return "", checkPinAgainstAbsent(peerFingerprints)
	}
	switch flag[0] {
	case identityFlagPresent:
		return readPeerIdentity(rw, sigMsg, peerFingerprints)
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
func readPeerIdentity(rw io.ReadWriteCloser, sigMsg []byte, peerFingerprints []string) (string, error) {
	pub := make([]byte, ed25519PublicKeyLen)
	if _, err := io.ReadFull(rw, pub); err != nil {
		return "", fmt.Errorf("tunnel: 读取对端身份公钥: %w", err)
	}
	sig := make([]byte, ed25519SignatureLen)
	if _, err := io.ReadFull(rw, sig); err != nil {
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
func writeIdentityFlag(rw io.ReadWriteCloser, id *Identity, sigMsg []byte) error {
	if id != nil {
		if _, err := rw.Write([]byte{identityFlagPresent}); err != nil {
			return fmt.Errorf("tunnel: 写身份标志: %w", err)
		}
		if _, err := rw.Write(id.PublicKey()); err != nil {
			return fmt.Errorf("tunnel: 写身份公钥: %w", err)
		}
		sig := id.Sign(sigMsg)
		if _, err := rw.Write(sig); err != nil {
			return fmt.Errorf("tunnel: 写身份签名: %w", err)
		}
		return nil
	}
	if _, err := rw.Write([]byte{identityFlagAbsent}); err != nil {
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
