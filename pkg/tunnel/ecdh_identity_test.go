// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// handshakeResult 承载 performHandshakeWithIdentity 的结果（跨 goroutine 通信）。
type handshakeResult struct {
	sk     []byte
	peerFP string
	err    error
}

func runDialerHandshake(ctx context.Context, m *mux.Mux, id *Identity, pins []string) <-chan handshakeResult {
	ch := make(chan handshakeResult, 1)
	go func() {
		sk, peerFP, err := performHandshakeWithIdentity(ctx, m, true, id, pins)
		ch <- handshakeResult{sk: sk, peerFP: peerFP, err: err}
	}()
	return ch
}

// TestHandshakeIdentity_Exchange 验证双方有身份时，握手完成后各自获得对端指纹。
func TestHandshakeIdentity_Exchange(t *testing.T) {
	t.Parallel()
	idA, _ := GenerateIdentity()
	idB, _ := GenerateIdentity()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	ch := runDialerHandshake(ctx, muxA, idA, nil)
	skB, peerFPB, err := performHandshakeWithIdentity(ctx, muxB, false, idB, nil)
	if err != nil {
		t.Fatalf("listener handshake: %v", err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatalf("dialer handshake: %v", r.err)
	}
	if peerFPB != idA.Fingerprint() {
		t.Fatalf("listener should see dialer fingerprint %s, got %s", idA.Fingerprint(), peerFPB)
	}
	if r.peerFP != idB.Fingerprint() {
		t.Fatalf("dialer should see listener fingerprint %s, got %s", idB.Fingerprint(), r.peerFP)
	}
	if len(skB) != 32 || len(r.sk) != 32 {
		t.Fatalf("expected 32-byte session keys, got %d/%d", len(skB), len(r.sk))
	}
}

// TestHandshakeIdentity_NoIdentity 验证双方都无身份时握手成功且对端指纹为空。
func TestHandshakeIdentity_NoIdentity(t *testing.T) {
	t.Parallel()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx := context.Background()
	ch := runDialerHandshake(ctx, muxA, nil, nil)
	_, peerFPB, err := performHandshakeWithIdentity(ctx, muxB, false, nil, nil)
	if err != nil {
		t.Fatalf("listener handshake: %v", err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatalf("dialer handshake: %v", r.err)
	}
	if peerFPB != "" || r.peerFP != "" {
		t.Fatalf("expected empty peer fingerprints, got %q/%q", r.peerFP, peerFPB)
	}
}

// TestHandshakeIdentity_BackwardCompat_OldListener 验证新 dialer（带身份）+ 旧 listener
// （performHandshake 无身份扩展）在未配置 pin 时握手成功。
func TestHandshakeIdentity_BackwardCompat_OldListener(t *testing.T) {
	t.Parallel()
	idA, _ := GenerateIdentity()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx := context.Background()
	ch := runDialerHandshake(ctx, muxA, idA, nil)
	_, err := performHandshake(ctx, muxB, false) // 旧 listener
	if err != nil {
		t.Fatalf("old listener handshake: %v", err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatalf("new dialer vs old listener should succeed, got %v", r.err)
	}
	if r.peerFP != "" {
		t.Fatalf("old listener should present no identity, got %q", r.peerFP)
	}
}

// TestHandshakeIdentity_BackwardCompat_OldDialer 验证新 listener（带身份）+ 旧 dialer
// 在未配置 pin 时握手成功。
func TestHandshakeIdentity_BackwardCompat_OldDialer(t *testing.T) {
	t.Parallel()
	idB, _ := GenerateIdentity()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx := context.Background()
	ch := make(chan error, 1)
	go func() {
		_, err := performHandshake(ctx, muxA, true) // 旧 dialer
		ch <- err
	}()
	_, peerFPB, err := performHandshakeWithIdentity(ctx, muxB, false, idB, nil)
	if err != nil {
		t.Fatalf("new listener vs old dialer should succeed, got %v", err)
	}
	if peerFPB != "" {
		t.Fatalf("old dialer should present no identity, got %q", peerFPB)
	}
	if err := <-ch; err != nil {
		t.Fatalf("old dialer handshake: %v", err)
	}
}

// TestHandshakeIdentity_PinMatch_Dialer 验证 dialer pin listener 指纹匹配时握手成功。
func TestHandshakeIdentity_PinMatch_Dialer(t *testing.T) {
	t.Parallel()
	idA, _ := GenerateIdentity()
	idB, _ := GenerateIdentity()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx := context.Background()
	ch := runDialerHandshake(ctx, muxA, idA, []string{idB.Fingerprint()})
	_, _, err := performHandshakeWithIdentity(ctx, muxB, false, idB, nil)
	if err != nil {
		t.Fatalf("listener handshake: %v", err)
	}
	if r := <-ch; r.err != nil {
		t.Fatalf("dialer pin should match, got %v", r.err)
	}
}

// TestHandshakeIdentity_PinMatch_Listener 验证 listener pin dialer 指纹匹配时握手成功。
func TestHandshakeIdentity_PinMatch_Listener(t *testing.T) {
	t.Parallel()
	idA, _ := GenerateIdentity()
	idB, _ := GenerateIdentity()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx := context.Background()
	ch := runDialerHandshake(ctx, muxA, idA, nil)
	_, _, err := performHandshakeWithIdentity(ctx, muxB, false, idB, []string{idA.Fingerprint()})
	if err != nil {
		t.Fatalf("listener pin should match, got %v", err)
	}
	if r := <-ch; r.err != nil {
		t.Fatalf("dialer handshake: %v", r.err)
	}
}

// TestHandshakeIdentity_PinMismatch_Dialer 验证 dialer pin 不匹配时握手失败（fail-closed）。
func TestHandshakeIdentity_PinMismatch_Dialer(t *testing.T) {
	t.Parallel()
	idA, _ := GenerateIdentity()
	idB, _ := GenerateIdentity()
	wrong, _ := GenerateIdentity()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx := context.Background()
	ch := runDialerHandshake(ctx, muxA, idA, []string{wrong.Fingerprint()})
	// listener 无 pin，其侧握手应成功返回
	_, _, err := performHandshakeWithIdentity(ctx, muxB, false, idB, nil)
	if err != nil {
		t.Fatalf("listener handshake: %v", err)
	}
	r := <-ch
	if r.err == nil {
		t.Fatal("dialer pin mismatch should fail")
	}
	if !errors.Is(r.err, ErrPeerFingerprintMismatch) {
		t.Fatalf("expected ErrPeerFingerprintMismatch, got %v", r.err)
	}
}

// TestHandshakeIdentity_PinMismatch_Listener 验证 listener pin 不匹配时握手失败。
func TestHandshakeIdentity_PinMismatch_Listener(t *testing.T) {
	t.Parallel()
	idA, _ := GenerateIdentity()
	idB, _ := GenerateIdentity()
	wrong, _ := GenerateIdentity()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx := context.Background()
	ch := runDialerHandshake(ctx, muxA, idA, nil)
	_, _, err := performHandshakeWithIdentity(ctx, muxB, false, idB, []string{wrong.Fingerprint()})
	if err == nil {
		t.Fatal("listener pin mismatch should fail")
	}
	if !errors.Is(err, ErrPeerFingerprintMismatch) {
		t.Fatalf("expected ErrPeerFingerprintMismatch, got %v", err)
	}
	<-ch // dialer 侧可能成功或写入已关闭流失败，不关心
}

// TestHandshakeIdentity_PinRequired_OldListener 验证 dialer 配置 pin 但 listener 为旧对端
// （无身份扩展）时 fail-closed 拒绝。
func TestHandshakeIdentity_PinRequired_OldListener(t *testing.T) {
	t.Parallel()
	idA, _ := GenerateIdentity()
	expected, _ := GenerateIdentity()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx := context.Background()
	ch := runDialerHandshake(ctx, muxA, idA, []string{expected.Fingerprint()})
	_, err := performHandshake(ctx, muxB, false) // 旧 listener
	if err != nil {
		t.Fatalf("old listener handshake: %v", err)
	}
	r := <-ch
	if !errors.Is(r.err, ErrPeerFingerprintRequired) {
		t.Fatalf("expected ErrPeerFingerprintRequired, got %v", r.err)
	}
}

// TestHandshakeIdentity_PinRequired_ListenerNoIdentity 验证 dialer 配置 pin 但 listener
// 新对端无身份密钥时 fail-closed 拒绝。
func TestHandshakeIdentity_PinRequired_ListenerNoIdentity(t *testing.T) {
	t.Parallel()
	idA, _ := GenerateIdentity()
	expected, _ := GenerateIdentity()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx := context.Background()
	ch := runDialerHandshake(ctx, muxA, idA, []string{expected.Fingerprint()})
	_, _, err := performHandshakeWithIdentity(ctx, muxB, false, nil, nil)
	if err != nil {
		t.Fatalf("listener handshake: %v", err)
	}
	r := <-ch
	if !errors.Is(r.err, ErrPeerFingerprintRequired) {
		t.Fatalf("expected ErrPeerFingerprintRequired, got %v", r.err)
	}
}

// TestHandshakeIdentity_ListenerPinRequired_OldDialer 验证 listener 配置 pin 但 dialer
// 为旧对端时 fail-closed 拒绝。
func TestHandshakeIdentity_ListenerPinRequired_OldDialer(t *testing.T) {
	t.Parallel()
	idB, _ := GenerateIdentity()
	expected, _ := GenerateIdentity()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx := context.Background()
	ch := make(chan error, 1)
	go func() {
		_, err := performHandshake(ctx, muxA, true) // 旧 dialer
		ch <- err
	}()
	_, _, err := performHandshakeWithIdentity(ctx, muxB, false, idB, []string{expected.Fingerprint()})
	if !errors.Is(err, ErrPeerFingerprintRequired) {
		t.Fatalf("expected ErrPeerFingerprintRequired, got %v", err)
	}
	if err := <-ch; err != nil {
		t.Fatalf("old dialer handshake: %v", err)
	}
}

// TestNewTunnel_WithIdentity_PinMatch 集成验证：双方配置身份 + 相互 pin 时隧道往返成功。
func TestNewTunnel_WithIdentity_PinMatch(t *testing.T) {
	t.Parallel()
	idA, _ := GenerateIdentity()
	idB, _ := GenerateIdentity()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	key, _ := ParseKey(testHexKey)
	tunA := NewTunnel(muxA, key, WithIdentity(idA), WithPeerFingerprints([]string{idB.Fingerprint()}))
	tunB := NewTunnel(muxB, key, WithIdentity(idB), WithPeerFingerprints([]string{idA.Fingerprint()}))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	srvErr := make(chan error, 1)
	go func() {
		srvErr <- tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(w, r.Body)
		}))
	}()
	time.Sleep(50 * time.Millisecond)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/echo", strings.NewReader("ping"))
	resp, err := tunA.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ping" {
		t.Fatalf("expected %q, got %q", "ping", string(body))
	}
	cancel()
	<-srvErr
}

// TestNewTunnel_PinMismatch_DialerFails 集成验证：dialer pin 不匹配时 Do fail-closed，
// 不回退到静态密钥。
func TestNewTunnel_PinMismatch_DialerFails(t *testing.T) {
	t.Parallel()
	idA, _ := GenerateIdentity()
	idB, _ := GenerateIdentity()
	wrong, _ := GenerateIdentity()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	key, _ := ParseKey(testHexKey)
	tunA := NewTunnel(muxA, key, WithIdentity(idA), WithPeerFingerprints([]string{wrong.Fingerprint()}))
	tunB := NewTunnel(muxB, key, WithIdentity(idB)) // listener 无 pin

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	time.Sleep(50 * time.Millisecond)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/echo", strings.NewReader("x"))
	_, err := tunA.Do(req)
	if err == nil {
		t.Fatal("expected Do to fail on dialer pin mismatch")
	}
	if !errors.Is(err, ErrPeerFingerprintMismatch) {
		t.Fatalf("expected ErrPeerFingerprintMismatch, got %v", err)
	}
	cancel()
}

// TestHandshakeIdentity_BadSignature_Rejected 验证冒名方宣称受害者公钥但无受害者私钥时，
// 对端验签失败（无 proof of possession）→ fail-closed 拒绝，即使本端未配置 pin。
// 这覆盖 security review 的"冒名方宣称任意公钥但无对应私钥 → 无法完成持有证明 → 被拒"。
func TestHandshakeIdentity_BadSignature_Rejected(t *testing.T) {
	t.Parallel()
	imposterPriv, _ := GenerateIdentity() // 冒名方自己的私钥
	victimPub, _ := GenerateIdentity()    // 被冒充的受害者公钥
	// 冒名方构造身份：宣称受害者的公钥，但用自己私钥签名。
	imposter := &Identity{privateKey: imposterPriv.privateKey, publicKey: victimPub.publicKey}

	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx := context.Background()
	// dialer 无 pin，但仍必须验签 listener 的身份签名（proof of possession）。
	ch := runDialerHandshake(ctx, muxA, nil, nil)
	_, _, err := performHandshakeWithIdentity(ctx, muxB, false, imposter, nil)
	// listener 侧：写完冒名身份后读 dialer 响应。dialer 验签失败不响应 → listener 读到 EOF。
	// 无 pin 时 listener 自身不失败；真正的拒绝发生在 dialer 验签。
	if err != nil {
		t.Fatalf("listener handshake returned error: %v", err)
	}
	r := <-ch
	if !errors.Is(r.err, ErrPeerIdentitySignature) {
		t.Fatalf("expected ErrPeerIdentitySignature on dialer, got %v", r.err)
	}
}

// TestNewTunnel_PinMismatch_ListenerServeFails 集成验证：listener pin 不匹配时 Serve fail-closed 终止。
func TestNewTunnel_PinMismatch_ListenerServeFails(t *testing.T) {
	t.Parallel()
	idA, _ := GenerateIdentity()
	idB, _ := GenerateIdentity()
	wrong, _ := GenerateIdentity()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	key, _ := ParseKey(testHexKey)
	tunA := NewTunnel(muxA, key, WithIdentity(idA)) // dialer 无 pin
	tunB := NewTunnel(muxB, key, WithIdentity(idB), WithPeerFingerprints([]string{wrong.Fingerprint()}))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	srvErr := make(chan error, 1)
	go func() {
		srvErr <- tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	}()
	time.Sleep(50 * time.Millisecond)

	// dialer 发起请求（触发握手）。listener 侧 pin 校验失败后 Serve 返回。
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/echo", strings.NewReader("x"))
		_, _ = tunA.Do(req)
	}()
	err := <-srvErr
	if err == nil {
		t.Fatal("expected Serve to fail on listener pin mismatch")
	}
	if !errors.Is(err, ErrPeerFingerprintMismatch) {
		t.Fatalf("expected ErrPeerFingerprintMismatch, got %v", err)
	}
	cancel()
}

// failWriteStream 实现 mux.Stream，Write 恒失败（模拟"对端为旧实现，读完 ECDH 后
// 关闭流"导致本端写身份扩展失败）。
type failWriteStream struct{}

func (m *failWriteStream) Read(p []byte) (int, error) { return 0, io.EOF }
func (m *failWriteStream) Write(p []byte) (int, error) {
	return 0, errors.New("stream closed by peer")
}
func (m *failWriteStream) Close() error      { return nil }
func (m *failWriteStream) CloseWrite() error { return nil }
func (m *failWriteStream) Abort() error      { return nil }
func (m *failWriteStream) ID() mux.StreamID  { return 0 }

// TestHandshakeIdentityListener_WriteFail_TreatAsOldPeer 验证发现 1：
// listener 写身份扩展失败（对端为旧实现读完 ECDH 即关闭）且未配置 pin 时，
// 握手不应失败——视为"对端未提供身份"（向后兼容，避免与旧 dialer 的 ECDH 会话
// 密钥不一致导致隧道静默错位）；配置 pin 时仍 fail-closed 拒绝。
func TestHandshakeIdentityListener_WriteFail_TreatAsOldPeer(t *testing.T) {
	sigMsg := identitySigMessage(make([]byte, ecdhPublicKeyLen), make([]byte, ecdhPublicKeyLen))

	// 无 pin：写失败 → 视为旧对端（无身份扩展），不报错。
	fp, err := handshakeIdentityListener(&failWriteStream{}, nil, nil, sigMsg)
	if err != nil {
		t.Fatalf("无 pin 时写失败应视为旧对端（不报错），实际 err=%v", err)
	}
	if fp != "" {
		t.Fatalf("对端未提供身份，指纹应为空，实际 %q", fp)
	}

	// 配置 pin：写失败 → fail-closed 报错（无法校验对端身份）。
	if _, err := handshakeIdentityListener(&failWriteStream{}, nil, []string{"sha256:0000"}, sigMsg); err == nil {
		t.Fatal("配置 pin 时写身份扩展失败应 fail-closed 报错")
	}
}
