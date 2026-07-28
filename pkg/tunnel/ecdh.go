// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"context"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

const (
	ecdhPublicKeyLen = 32 // X25519 public key 长度
	sessionKeyLen    = 32 // AES-256 会话密钥长度
	ecdhSalt         = "sproxy-ecdh-salt-v1"
)

// performHandshake 执行 ECDH X25519 密钥交换，返回会话密钥。
// dialer 为 true 表示发起方（客户端），false 表示接受方（服务端）。
// 握手流程：
// 1. dialer 通过 mux.Open 创建一条流
// 2. dialer 发送 ECDH 公钥（32 字节）
// 3. listener 通过 mux.Accept 接收流
// 4. listener 读取 ECDH 公钥，发送自己的 ECDH 公钥
// 5. dialer 读取 listener 的 ECDH 公钥
// 6. 双方计算 HKDF(ECDH(shared_secret)) 作为会话密钥
func performHandshake(ctx context.Context, m *mux.Mux, dialer bool) ([]byte, error) {
	curve := ecdh.X25519()
	privateKey, gErr := curve.GenerateKey(rand.Reader)
	if gErr != nil {
		return nil, fmt.Errorf("ecdh: generate key: %w", gErr)
	}
	publicKey := privateKey.PublicKey()

	var peerPublic []byte

	if dialer {
		s, openErr := m.Open(ctx)
		if openErr != nil {
			return nil, fmt.Errorf("ecdh: open stream: %w", openErr)
		}
		defer s.Close()

		ourPub := publicKey.Bytes()
		if _, wErr := s.Write(ourPub); wErr != nil {
			return nil, fmt.Errorf("ecdh: write pubkey: %w", wErr)
		}

		peerPub := make([]byte, ecdhPublicKeyLen)
		if _, rErr := io.ReadFull(s, peerPub); rErr != nil {
			return nil, fmt.Errorf("ecdh: read peer pubkey: %w", rErr)
		}
		peerPublic = peerPub
	} else {
		s, acceptErr := m.Accept(ctx)
		if acceptErr != nil {
			return nil, fmt.Errorf("ecdh: accept stream: %w", acceptErr)
		}
		defer s.Close()

		peerPub := make([]byte, ecdhPublicKeyLen)
		if _, rErr := io.ReadFull(s, peerPub); rErr != nil {
			return nil, fmt.Errorf("ecdh: read peer pubkey: %w", rErr)
		}

		ourPub := publicKey.Bytes()
		if _, wErr := s.Write(ourPub); wErr != nil {
			return nil, fmt.Errorf("ecdh: write pubkey: %w", wErr)
		}
		peerPublic = peerPub
	}

	peerKey, pErr := curve.NewPublicKey(peerPublic)
	if pErr != nil {
		return nil, fmt.Errorf("ecdh: invalid peer public key: %w", pErr)
	}
	sharedSecret, eErr := privateKey.ECDH(peerKey)
	if eErr != nil {
		return nil, fmt.Errorf("ecdh: compute shared secret: %w", eErr)
	}

	sessionKey, kErr := hkdf.Key(sha256.New, sharedSecret, []byte(ecdhSalt), "sproxy-tunnel-ecdh-v1", sessionKeyLen)
	if kErr != nil {
		return nil, fmt.Errorf("ecdh: derive session key: %w", kErr)
	}

	return sessionKey, nil
}
