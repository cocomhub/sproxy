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
	privateKey, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ecdh: generate key: %w", err)
	}
	publicKey := privateKey.PublicKey()

	var peerPublic []byte

	if dialer {
		stream, err := m.Open(ctx)
		if err != nil {
			return nil, fmt.Errorf("ecdh: open stream: %w", err)
		}
		defer stream.Close()

		ourPub := publicKey.Bytes()
		if _, err := stream.Write(ourPub); err != nil {
			return nil, fmt.Errorf("ecdh: write pubkey: %w", err)
		}

		peerPub := make([]byte, ecdhPublicKeyLen)
		if _, err := io.ReadFull(stream, peerPub); err != nil {
			return nil, fmt.Errorf("ecdh: read peer pubkey: %w", err)
		}
		peerPublic = peerPub
	} else {
		stream, err := m.Accept(ctx)
		if err != nil {
			return nil, fmt.Errorf("ecdh: accept stream: %w", err)
		}
		defer stream.Close()

		peerPub := make([]byte, ecdhPublicKeyLen)
		if _, err := io.ReadFull(stream, peerPub); err != nil {
			return nil, fmt.Errorf("ecdh: read peer pubkey: %w", err)
		}

		ourPub := publicKey.Bytes()
		if _, err := stream.Write(ourPub); err != nil {
			return nil, fmt.Errorf("ecdh: write pubkey: %w", err)
		}
		peerPublic = peerPub
	}

	peerKey, err := curve.NewPublicKey(peerPublic)
	if err != nil {
		return nil, fmt.Errorf("ecdh: invalid peer public key: %w", err)
	}
	sharedSecret, err := privateKey.ECDH(peerKey)
	if err != nil {
		return nil, fmt.Errorf("ecdh: compute shared secret: %w", err)
	}

	sessionKey, err := hkdf.Key(sha256.New, sharedSecret, nil, "sproxy-tunnel-ecdh-v1", sessionKeyLen)
	if err != nil {
		return nil, fmt.Errorf("ecdh: derive session key: %w", err)
	}

	return sessionKey, nil
}
