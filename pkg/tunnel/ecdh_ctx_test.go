// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// TestHandshakeIdentity_IdentityReadBoundedByCtx 验证身份阶段的流读取受 ctx 约束：
// 恶意对端完成阶段 1（交换 ECDH 公钥）后停滞不写身份标志，本端身份读取必须在
// ctx 超时后返回（经 context.AfterFunc → stream.Abort），而非无限期占住 goroutine。
// 配置 pin 时停滞对端被 fail-closed 拒绝（ErrPeerFingerprintRequired）。
func TestHandshakeIdentity_IdentityReadBoundedByCtx(t *testing.T) {
	idA, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}

	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	// 恶意 listener：读完 dialer ECDH 公钥、写回自己的公钥后停滞，不进入身份扩展。
	curve := ecdh.X25519()
	maliciousKey, gErr := curve.GenerateKey(rand.Reader)
	if gErr != nil {
		t.Fatal(gErr)
	}
	maliciousPub := maliciousKey.PublicKey().Bytes()

	block := make(chan struct{})
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		s, aErr := muxB.Accept(context.Background())
		if aErr != nil {
			return
		}
		defer s.Close()
		peerPub := make([]byte, ecdhPublicKeyLen)
		if _, rErr := io.ReadFull(s, peerPub); rErr != nil {
			return
		}
		if _, wErr := s.Write(maliciousPub); wErr != nil {
			return
		}
		// 停滞：保持连接打开但不写身份标志，模拟恶意/故障对端。
		<-block
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, hErr := performHandshakeWithIdentity(ctx, muxA, true, idA, []string{pinned.Fingerprint()})
	elapsed := time.Since(start)
	close(block)

	if !errors.Is(hErr, ErrPeerFingerprintRequired) {
		t.Fatalf("配置 pin 时停滞对端应 fail-closed 拒绝（ErrPeerFingerprintRequired）, 实际: %v", hErr)
	}
	// 身份读取受 ctx 约束：300ms 超时应立即返回，而非无限期阻塞。
	if elapsed > 5*time.Second {
		t.Fatalf("身份阶段读取未受 ctx 约束: 耗时 %v", elapsed)
	}
	select {
	case <-peerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("恶意 listener goroutine 未在超时内退出")
	}
}
