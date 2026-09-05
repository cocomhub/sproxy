// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
)

// ---- test helpers（mock 服务端 + wrap 信封构造）----

// randSKBytes 生成 32B 随机 SK（测试用）。
func randSKBytes(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return b
}

// testWrapContext 复算 wrap context（与服务端同拼法：prefix[#mesh]）。
// 断言与生产 wrapContextFor 共用同一个 CredentialWrapContextPrefix 常量，
// 保证 mock 与实现端到端一致。
func testWrapContext(t *testing.T, ak string) string {
	t.Helper()
	mesh := accesskey.ParseMesh(ak)
	if mesh == "" {
		return CredentialWrapContextPrefix
	}
	return CredentialWrapContextPrefix + "#" + mesh
}

// testWrapEnvelope 用 wrapSK 包裹 secret，生成与 server 同构的信封。
func testWrapEnvelope(t *testing.T, wrapSK []byte, ak string, secret []byte) *accesskey.WrappedSecret {
	t.Helper()
	wk, err := accesskey.DeriveWrapKey(wrapSK, ak, testWrapContext(t, ak))
	if err != nil {
		t.Fatalf("DeriveWrapKey: %v", err)
	}
	env, err := accesskey.EncryptSecret(ak, secret, wk)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	return env
}

// verifySignedRequest 用 skHex 校验请求的 SproxySig 头（真实签名路径——客户端
// signRequest 必须以 v2 canonical + 可选 entryID 构造，服务端 Verify 要能通过）。
// 返回解析出的 Header 供 entryID 断言。
func verifySignedRequest(t *testing.T, r *http.Request, skHex string) sproxysig.Header {
	t.Helper()
	hdr, err := sproxysig.ParseHeader(r.Header.Get("Authorization"))
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if err := sproxysig.Verify(skHex, hdr, r.Method, r.URL.EscapedPath(), r.URL.RawQuery, time.Now(), 0, 0, nil); err != nil {
		t.Fatalf("Verify 失败（客户端签名未被 mock 服务端接受）: %v", err)
	}
	return hdr
}

func decodeBody(t *testing.T, r *http.Request, v any) {
	t.Helper()
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && err != io.EOF {
		t.Fatalf("decode body: %v", err)
	}
}

// ---- RenewAccessKey ----

func TestFileClient_RenewAccessKey(t *testing.T) {
	const (
		ak      = "sk-0123456789abcdef"
		entryID = "sk-abcdefabcdef"
		newID   = "sk-1234567890ab"
	)
	oldSK := randSKBytes(t)
	oldSKHex := hex.EncodeToString(oldSK)
	newSK := randSKBytes(t)
	expires := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)

	var gotEntryID string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/credentials/"+ak+"/renew", func(w http.ResponseWriter, r *http.Request) {
		hdr := verifySignedRequest(t, r, oldSKHex)
		gotEntryID = hdr.EntryID
		env := testWrapEnvelope(t, oldSK, ak, newSK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ak":             ak,
			"sk_id":          newID,
			"kind":           "secret_wrap",
			"wrap_key_ak":    ak,
			"expires_at":     expires.Format(time.RFC3339),
			"wrapped_secret": env,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc := NewFileClient(srv.URL, WithAccessKey(ak, oldSKHex), WithAccessKeyID(entryID))
	res, err := svc.RenewAccessKey(context.Background())
	if err != nil {
		t.Fatalf("RenewAccessKey: %v", err)
	}

	// 断言解开的正是服务端包裹的新 SK。
	if !bytes.Equal(res.NewSecret, newSK) {
		t.Errorf("NewSecret 不匹配: got %x want %x", res.NewSecret, newSK)
	}
	if res.SKID != newID {
		t.Errorf("SKID = %q, want %q", res.SKID, newID)
	}
	if !res.ExpiresAt.Equal(expires) {
		t.Errorf("ExpiresAt = %v, want %v", res.ExpiresAt, expires)
	}
	if res.AK != ak {
		t.Errorf("AK = %q, want %q", res.AK, ak)
	}
	// 签名头携带了 entryID（v2 精确取条目）。
	if gotEntryID != entryID {
		t.Errorf("请求 entryID = %q, want %q", gotEntryID, entryID)
	}
	// 回填：本端凭据已切换为新 SK + 新 entryID（新 SK 立即可用）。
	if svc.AccessKeySecret() != hex.EncodeToString(newSK) {
		t.Errorf("AccessKeySecret 未回填: got %q want %q", svc.AccessKeySecret(), hex.EncodeToString(newSK))
	}
	if svc.AccessKeyID() != newID {
		t.Errorf("AccessKeyID 未回填: got %q want %q", svc.AccessKeyID(), newID)
	}

	// 关键黑盒：回填后的后续请求必须用【新 SK】也能通过 mock 验签（旧 SK 验不过）。
	mux.HandleFunc("GET /verify-after-renew", func(w http.ResponseWriter, r *http.Request) {
		verifySignedRequest(t, r, hex.EncodeToString(newSK))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	resp, err := svc.doRequest(context.Background(), "GET", "/verify-after-renew", nil, nil)
	if err != nil {
		t.Fatalf("renew 后请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("renew 后请求 status = %d, want 200", resp.StatusCode)
	}
}

func TestFileClient_RenewAccessKey_MeshContext(t *testing.T) {
	// mesh 非空：服务端 wrap context 追加 #<mesh>，客户端必须同拼法才能解开。
	const (
		ak      = "sk-meshA-0123456789abcdef"
		entryID = "sk-abcdefabcdef"
		newID   = "sk-1234567890ab"
	)
	oldSK := randSKBytes(t)
	oldSKHex := hex.EncodeToString(oldSK)
	newSK := randSKBytes(t)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/credentials/"+ak+"/renew", func(w http.ResponseWriter, r *http.Request) {
		verifySignedRequest(t, r, oldSKHex)
		env := testWrapEnvelope(t, oldSK, ak, newSK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ak":             ak,
			"sk_id":          newID,
			"kind":           "secret_wrap",
			"wrap_key_ak":    ak,
			"expires_at":     time.Now().Add(time.Hour).Format(time.RFC3339),
			"wrapped_secret": env,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc := NewFileClient(srv.URL, WithAccessKey(ak, oldSKHex), WithAccessKeyID(entryID))
	res, err := svc.RenewAccessKey(context.Background())
	if err != nil {
		t.Fatalf("RenewAccessKey (mesh): %v", err)
	}
	if !bytes.Equal(res.NewSecret, newSK) {
		t.Errorf("mesh NewSecret 不匹配: got %x want %x", res.NewSecret, newSK)
	}
}

func TestFileClient_RenewAccessKey_NoCredentials(t *testing.T) {
	if _, err := NewFileClient("http://127.0.0.1:1").RenewAccessKey(context.Background()); err == nil {
		t.Error("expected error when access_key_secret not configured")
	}
}

func TestFileClient_RenewAccessKey_DecryptMismatch(t *testing.T) {
	// 服务端用错误的旧 SK 包裹 → 客户端解开失败（GCM auth），不得误认成功。
	const ak = "sk-0123456789abcdef"
	oldSK := randSKBytes(t)
	wrongSK := randSKBytes(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/credentials/"+ak+"/renew", func(w http.ResponseWriter, r *http.Request) {
		verifySignedRequest(t, r, hex.EncodeToString(oldSK))
		env := testWrapEnvelope(t, wrongSK, ak, randSKBytes(t))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ak":             ak,
			"sk_id":          "sk-1234567890ab",
			"kind":           "secret_wrap",
			"wrap_key_ak":    ak,
			"expires_at":     time.Now().Add(time.Hour).Format(time.RFC3339),
			"wrapped_secret": env,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc := NewFileClient(srv.URL, WithAccessKey(ak, hex.EncodeToString(oldSK)))
	if _, err := svc.RenewAccessKey(context.Background()); err == nil {
		t.Error("expected error when wrapped_secret 无法用本端 SK 解开")
	} else if !strings.Contains(err.Error(), "解不开") {
		t.Errorf("错误信息应说明解开失败, got: %v", err)
	}
}

// ---- ListAccessKeys ----

func TestFileClient_ListAccessKeys(t *testing.T) {
	const ak = "sk-0123456789abcdef"
	mySK := randSKBytes(t)
	mySKHex := hex.EncodeToString(mySK)
	otherSK := randSKBytes(t)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/credentials/"+ak+"/sk", func(w http.ResponseWriter, r *http.Request) {
		verifySignedRequest(t, r, mySKHex)
		// per-key wrap：条目 A 用自身 SK 包裹；条目 B 用自身 SK 包裹（客户持有 A 才能解 A）。
		envA := testWrapEnvelope(t, mySK, ak, mySK)
		envB := testWrapEnvelope(t, otherSK, ak, otherSK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ak": ak,
			"sk": []map[string]any{
				{
					"sk_id": "sk-aaaaaaaaaaaa", "created": time.Now().Add(-time.Hour).Format(time.RFC3339),
					"expires": time.Now().Add(time.Hour).Format(time.RFC3339), "status": "active",
					"meta_type": "renew", "wrapped_secret": envA,
				},
				{
					"sk_id": "sk-bbbbbbbbbbbb", "created": time.Now().Add(-2 * time.Hour).Format(time.RFC3339),
					"expires": time.Now().Add(2 * time.Hour).Format(time.RFC3339), "status": "active",
					"meta_type": "initial", "wrapped_secret": envB,
				},
			},
			"total": 2,
			"admin": false,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc := NewFileClient(srv.URL, WithAccessKey(ak, mySKHex))
	infos, err := svc.ListAccessKeys(context.Background(), ak)
	if err != nil {
		t.Fatalf("ListAccessKeys: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("got %d entries, want 2", len(infos))
	}
	// 自己能解开的条目（持有其 SK）→ Decrypted 非空且等于该条目 SK。
	if !bytes.Equal(infos[0].Decrypted, mySK) {
		t.Errorf("entry[0] 应可解开 (Decrypted=%x), want mySK=%x", infos[0].Decrypted, mySK)
	}
	if infos[1].Decrypted != nil {
		t.Errorf("entry[1] 不应可解开（未持有其 SK）, got %x", infos[1].Decrypted)
	}
}

// ---- DeleteSK / ExpireSK / AddAK / DeleteAK / ListAKs ----

func TestFileClient_DeleteSK(t *testing.T) {
	const (
		ak   = "sk-0123456789abcdef"
		skID = "sk-aaaaaaaaaaaa"
	)
	var gotMethod, gotPath string
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/credentials/"+ak+"/sk/"+skID, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc := NewFileClient(srv.URL, WithAccessKey(ak, hex.EncodeToString(randSKBytes(t))))
	if err := svc.DeleteSK(context.Background(), ak, skID); err != nil {
		t.Fatalf("DeleteSK: %v", err)
	}
	if gotMethod != "DELETE" || gotPath != "/api/credentials/"+ak+"/sk/"+skID {
		t.Errorf("请求 = %s %s, want DELETE /api/credentials/%s/sk/%s", gotMethod, gotPath, ak, skID)
	}
}

func TestFileClient_ExpireSK(t *testing.T) {
	const (
		ak   = "sk-0123456789abcdef"
		skID = "sk-aaaaaaaaaaaa"
	)
	until := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	var gotBody struct {
		Until string `json:"until"`
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/credentials/"+ak+"/sk/"+skID+"/expire", func(w http.ResponseWriter, r *http.Request) {
		decodeBody(t, r, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "until": gotBody.Until})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc := NewFileClient(srv.URL, WithAccessKey(ak, hex.EncodeToString(randSKBytes(t))))
	if err := svc.ExpireSK(context.Background(), ak, skID, until); err != nil {
		t.Fatalf("ExpireSK: %v", err)
	}
	if gotBody.Until != until.Format(time.RFC3339) {
		t.Errorf("until = %q, want %q", gotBody.Until, until.Format(time.RFC3339))
	}

	// 零值 → 空串（恢复永久有效）。
	if err := svc.ExpireSK(context.Background(), ak, skID, time.Time{}); err != nil {
		t.Fatalf("ExpireSK(zero): %v", err)
	}
	if gotBody.Until != "" {
		t.Errorf("zero until = %q, want empty", gotBody.Until)
	}
}

func TestFileClient_AddAK(t *testing.T) {
	const (
		ak     = "sk-0123456789abcdef"
		owner  = "tenant-1"
		secret = "feedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface"
	)
	var gotBody struct {
		AK     string `json:"ak"`
		Owner  string `json:"owner"`
		Secret string `json:"secret"`
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/credentials", func(w http.ResponseWriter, r *http.Request) {
		decodeBody(t, r, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"ak": ak, "sk_id": "sk-aaaaaaaaaaaa"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc := NewFileClient(srv.URL, WithAccessKey(ak, hex.EncodeToString(randSKBytes(t))))
	res, err := svc.AddAK(context.Background(), ak, owner, secret)
	if err != nil {
		t.Fatalf("AddAK: %v", err)
	}
	if gotBody.AK != ak || gotBody.Owner != owner || gotBody.Secret != secret {
		t.Errorf("request body = %+v, want ak=%q owner=%q secret set", gotBody, ak, owner)
	}
	if res.SKID != "sk-aaaaaaaaaaaa" {
		t.Errorf("AddAKResult.SKID = %q", res.SKID)
	}
}

func TestFileClient_DeleteAK(t *testing.T) {
	const ak = "sk-0123456789abcdef"
	var gotBody struct {
		Confirm string `json:"confirm"`
		Force   bool   `json:"force"`
	}
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/credentials/"+ak, func(w http.ResponseWriter, r *http.Request) {
		decodeBody(t, r, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "ak": ak})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc := NewFileClient(srv.URL, WithAccessKey(ak, hex.EncodeToString(randSKBytes(t))))
	if err := svc.DeleteAK(context.Background(), ak, true); err != nil {
		t.Fatalf("DeleteAK: %v", err)
	}
	if gotBody.Confirm != ak || !gotBody.Force {
		t.Errorf("request body = %+v, want confirm=%q force=true", gotBody, ak)
	}

	// confirm 与目标 AK 不一致 → 服务端 400 → doJSON 报错（二次确认由 CLI 兜底）。
	mux.HandleFunc("DELETE /api/credentials/other-ak", func(w http.ResponseWriter, r *http.Request) {
		decodeBody(t, r, &gotBody)
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "confirm 必须等于目标 AK"})
	})
	if err := svc.DeleteAK(context.Background(), "other-ak", false); err == nil {
		t.Error("expected error when confirm mismatch")
	}
}

func TestFileClient_ListAKs(t *testing.T) {
	const ak = "sk-0123456789abcdef"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/credentials", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ak": []map[string]any{
				{"ak": ak, "owner": "", "sk_count": 2, "alive_sk": 1},
			},
			"total": 1,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc := NewFileClient(srv.URL, WithAccessKey(ak, hex.EncodeToString(randSKBytes(t))))
	sums, err := svc.ListAKs(context.Background())
	if err != nil {
		t.Fatalf("ListAKs: %v", err)
	}
	if len(sums) != 1 || sums[0].AK != ak || sums[0].SKCount != 2 || sums[0].AliveSK != 1 {
		t.Errorf("ListAKs = %+v, want 1 entry ak=%s sk_count=2 alive_sk=1", sums, ak)
	}
}
