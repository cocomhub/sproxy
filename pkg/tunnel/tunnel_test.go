// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testKey 是一个固定的 32 字节密钥（64 十六进制字符），用于测试。
var testKey, _ = hex.DecodeString("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

// testHexKey 是 testKey 的十六进制字符串表示。
const testHexKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// 引用 GenerateKey 避免包级别编译错误
var _ = GenerateKey

func TestNewHandler_returnsForbiddenOnEmptyKey(t *testing.T) {
	h := NewHandler(nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/tunnel", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestNewHandlerForwardsAbsoluteURL(t *testing.T) {
	// 创建一个测试用后端服务器
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "hello")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer backend.Close()

	handler := NewHandler(testKey, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client, err := NewClient(testHexKey, ts.URL, 0, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	req, _ := http.NewRequest("GET", backend.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Custom") != "hello" {
		t.Fatalf("expected X-Custom=hello, got %q", resp.Header.Get("X-Custom"))
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"status":"ok"}` {
		t.Fatalf("expected body %q, got %q", `{"status":"ok"}`, string(body))
	}
}

func TestNewLocalHandler_dispatchesRelativeURL(t *testing.T) {
	// 本地 handler：模拟文件列表
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"files":[{"name":"test.txt","size":123}]}`))
	})

	handler := NewLocalHandler(testKey, local, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client, err := NewClient(testHexKey, ts.URL, 0, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// 使用相对路径 /api/files
	req, _ := http.NewRequest("GET", "/api/files", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ctype := resp.Header.Get("Content-Type"); ctype != "application/json" {
		t.Fatalf("expected Content-Type=application/json, got %q", ctype)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"files":[{"name":"test.txt","size":123}]}` {
		t.Fatalf("unexpected body: %q", string(body))
	}
}

func TestNewLocalHandler_dispatchesWithQueryAndHeaders(t *testing.T) {
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Query().Get("filename") != "test.txt" {
			t.Errorf("expected filename=test.txt, got %q", r.URL.Query().Get("filename"))
		}
		if r.Header.Get("X-File-Checksum") != "abc123" {
			t.Errorf("expected X-File-Checksum=abc123, got %q", r.Header.Get("X-File-Checksum"))
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	})

	handler := NewLocalHandler(testKey, local, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client, err := NewClient(testHexKey, ts.URL, 0, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	req, _ := http.NewRequest("POST", "/delete?filename=test.txt", nil)
	req.Header.Set("X-File-Checksum", "abc123")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestNewLocalHandler_dispatchesUploadBody(t *testing.T) {
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte("file-content")) {
			t.Errorf("expected body to contain 'file-content', got %q", body)
		}
		w.Header().Set("X-File-Checksum", "abc")
		_, _ = w.Write([]byte(`{"success":true,"file_checksum":"abc"}`))
	})

	handler := NewLocalHandler(testKey, local, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client, err := NewClient(testHexKey, ts.URL, 0, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	req, _ := http.NewRequest("POST", "/upload", strings.NewReader("file-content"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xxx")
	req.Header.Set("X-File-Checksum", "abc")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if cs := resp.Header.Get("X-File-Checksum"); cs != "abc" {
		t.Fatalf("expected X-File-Checksum=abc, got %q", cs)
	}
}

func TestNewLocalHandler_forwardsAbsoluteURL(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("external-response"))
	}))
	defer backend.Close()

	// localHandler 不为 nil，但绝对 URL 应走 forwardExternal
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("should-not-reach"))
	})

	handler := NewLocalHandler(testKey, local, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client, err := NewClient(testHexKey, ts.URL, 0, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	req, _ := http.NewRequest("GET", backend.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "external-response" {
		t.Fatalf("expected 'external-response', got %q", string(body))
	}
}

func TestNewLocalHandler_withNil_actsLikeNewHandler(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	// NewLocalHandler with nil localHandler
	handler := NewLocalHandler(testKey, nil, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client, err := NewClient(testHexKey, ts.URL, 0, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	req, _ := http.NewRequest("GET", backend.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestIsRelativePath(t *testing.T) {
	tests := []struct {
		url      string
		expected bool
	}{
		{"/upload", true},
		{"/api/files?name=test", true},
		{"/", true},
		{"https://example.com/api", false},
		{"http://localhost:8080/test", false},
		{"absolute/path", false}, // 不以 / 开头
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			got := isRelativePath(tc.url)
			if got != tc.expected {
				t.Errorf("isRelativePath(%q) = %v, want %v", tc.url, got, tc.expected)
			}
		})
	}
}

func TestNewLocalHandler_responseHeadersInMetadata(t *testing.T) {
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "val")
		w.Header().Set("X-File-Checksum", "deadbeef")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	})

	handler := NewLocalHandler(testKey, local, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client, err := NewClient(testHexKey, ts.URL, 0, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	req, _ := http.NewRequest("GET", "/api/notfound", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Custom") != "val" {
		t.Fatalf("expected X-Custom=val, got %q", resp.Header.Get("X-Custom"))
	}
	if resp.Header.Get("X-File-Checksum") != "deadbeef" {
		t.Fatalf("expected X-File-Checksum=deadbeef, got %q", resp.Header.Get("X-File-Checksum"))
	}
}

func TestNewClient_rejectsInvalidKey(t *testing.T) {
	_, err := NewClient("short", "http://localhost/tunnel", 0, nil)
	if err == nil {
		t.Fatal("expected error for invalid key")
	}
}

var _ = testKey
var _ = GenerateKey

func TestEncryptDecrypt_Roundtrip(t *testing.T) {
	plaintext := []byte("hello encryption!")
	ciphertext, err := Encrypt(testKey, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := Decrypt(testKey, ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("expected %q, got %q", plaintext, decrypted)
	}
}

func TestEncryptDecrypt_EmptyPlaintext(t *testing.T) {
	ciphertext, err := Encrypt(testKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := Decrypt(testKey, ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if len(decrypted) != 0 {
		t.Fatalf("expected empty, got %d bytes", len(decrypted))
	}
}

func TestEncryptDecrypt_ShortKey(t *testing.T) {
	_, err := Encrypt([]byte("short"), []byte("data"))
	if err == nil {
		t.Error("expected error for short key")
	}
	_, err = Decrypt([]byte("short"), []byte("data"))
	if err == nil {
		t.Error("expected error for short key")
	}
}

func BenchmarkEncryptDecrypt(b *testing.B) {
	key := testKey
	plaintext := []byte(`{"method":"GET","url":"/api/files","headers":{}}`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ciphertext, err := Encrypt(key, plaintext)
		if err != nil {
			b.Fatal(err)
		}
		decrypted, err := Decrypt(key, ciphertext)
		if err != nil {
			b.Fatal(err)
		}
		if !bytes.Equal(decrypted, plaintext) {
			b.Fatal("decrypted != plaintext")
		}
	}
}

// Test metadata frame encoding/decoding roundtrip
func TestMetadataFrameRoundtrip(t *testing.T) {
	meta := []byte(`{"method":"POST","url":"/upload","headers":{"X-File-Checksum":"abc"}}`)
	frame, err := encodeMetadataFrame(testKey, meta)
	if err != nil {
		t.Fatalf("encodeMetadataFrame: %v", err)
	}

	decoded, err := decodeMetadataFrame(bytes.NewReader(frame), testKey)
	if err != nil {
		t.Fatalf("decodeMetadataFrame: %v", err)
	}

	if !bytes.Equal(decoded, meta) {
		t.Fatalf("roundtrip mismatch:\n  got:  %s\n  want: %s", decoded, meta)
	}
}

// Test that local handler receives the correct request method and URL
func TestNewLocalHandler_preservesMethodAndURL(t *testing.T) {
	var capturedMethod, capturedURL string
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedURL = r.URL.String()
		w.WriteHeader(http.StatusOK)
	})

	handler := NewLocalHandler(testKey, local, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client, err := NewClient(testHexKey, ts.URL, 0, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	tests := []struct {
		method, url string
	}{
		{"GET", "/api/files"},
		{"POST", "/upload"},
		{"GET", "/download?filename=test.txt"},
		{"POST", "/delete?filename=test.txt"},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("%s %s", tc.method, tc.url), func(t *testing.T) {
			capturedMethod = ""
			capturedURL = ""
			req, _ := http.NewRequest(tc.method, tc.url, nil)
			_, err := client.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			if capturedMethod != tc.method {
				t.Errorf("expected method %s, got %s", tc.method, capturedMethod)
			}
			if capturedURL != tc.url {
				t.Errorf("expected URL %s, got %s", tc.url, capturedURL)
			}
		})
	}
}

// Test that handler body io.Reader works with multipart form parsing
func TestNewLocalHandler_bodyMultiPartCompatible(t *testing.T) {
	boundary := "testBoundary123"
	multipartBody := fmt.Sprintf("--%s\r\nContent-Disposition: form-data; name=\"file\"; filename=\"hello.txt\"\r\nContent-Type: text/plain\r\n\r\nhello world\r\n--%s--\r\n", boundary, boundary)

	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			t.Errorf("ParseMultipartForm error: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("FormFile error: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer file.Close()
		body, _ := io.ReadAll(file)
		if string(body) != "hello world" {
			t.Errorf("unexpected file body: %q", string(body))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true}`))
	})

	handler := NewLocalHandler(testKey, local, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client, err := NewClient(testHexKey, ts.URL, 0, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	req, _ := http.NewRequest("POST", "/upload", strings.NewReader(multipartBody))
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.Header.Set("X-File-Checksum", "abc")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// Test that large streaming body works through dispatchLocal
func TestNewLocalHandler_largeStreamingBody(t *testing.T) {
	// 1MB payload
	payload := bytes.Repeat([]byte("A"), 1024*1024)

	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		written, _ := io.Copy(io.Discard, r.Body)
		if written != int64(len(payload)) {
			t.Errorf("expected to read %d bytes, got %d", len(payload), written)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload[:100]) // 小响应
	})

	handler := NewLocalHandler(testKey, local, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client, err := NewClient(testHexKey, ts.URL, 0, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	req, _ := http.NewRequest("POST", "/upload", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != string(payload[:100]) {
		t.Fatalf("unexpected body: %q", body)
	}
}

// Test that response headers with multiple values are preserved
func TestNewLocalHandler_multiValueResponseHeaders(t *testing.T) {
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "session=abc")
		w.Header().Add("Set-Cookie", "token=xyz")
		w.WriteHeader(http.StatusOK)
	})

	handler := NewLocalHandler(testKey, local, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client, err := NewClient(testHexKey, ts.URL, 0, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	req, _ := http.NewRequest("GET", "/", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	cookies := resp.Header.Values("Set-Cookie")
	if len(cookies) != 2 {
		t.Fatalf("expected 2 Set-Cookie values, got %d: %v", len(cookies), cookies)
	}
	if cookies[0] != "session=abc" || cookies[1] != "token=xyz" {
		t.Fatalf("unexpected Set-Cookie values: %v", cookies)
	}
}

// Test the full roundtrip: request and response metadata
func TestNewLocalHandler_metadataRoundtrip(t *testing.T) {
	// Custom request headers should arrive intact
	var gotRequestHeaders map[string]string
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequestHeaders = map[string]string{
			"X-Custom":        r.Header.Get("X-Custom"),
			"Content-Type":    r.Header.Get("Content-Type"),
			"X-File-Checksum": r.Header.Get("X-File-Checksum"),
		}
		w.Header().Set("Content-Length", "5")
		_, _ = w.Write([]byte("hello"))
	})

	handler := NewLocalHandler(testKey, local, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client, err := NewClient(testHexKey, ts.URL, 0, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	req, _ := http.NewRequest("POST", "/upload", strings.NewReader("data"))
	req.Header.Set("X-Custom", "my-value")
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xxx")
	req.Header.Set("X-File-Checksum", "deadbeef")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if gotRequestHeaders["X-Custom"] != "my-value" {
		t.Errorf("expected X-Custom=my-value, got %q", gotRequestHeaders["X-Custom"])
	}
	if gotRequestHeaders["Content-Type"] != "multipart/form-data; boundary=xxx" {
		t.Errorf("expected Content-Type=multipart/form-data, got %q", gotRequestHeaders["Content-Type"])
	}
	if gotRequestHeaders["X-File-Checksum"] != "deadbeef" {
		t.Errorf("expected X-File-Checksum=deadbeef, got %q", gotRequestHeaders["X-File-Checksum"])
	}
}

// Test that empty body works in dispatchLocal
func TestNewLocalHandler_emptyBody(t *testing.T) {
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if len(body) != 0 {
			t.Fatalf("expected empty body, got %d bytes", len(body))
		}
		w.WriteHeader(http.StatusNoContent)
	})

	handler := NewLocalHandler(testKey, local, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client, err := NewClient(testHexKey, ts.URL, 0, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	req, _ := http.NewRequest("GET", "/api/empty", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}
}

// Verify that a relative URL without a leading slash is NOT dispatched locally
func TestNewLocalHandler_rejectsNonLeadingSlash(t *testing.T) {
	localWasCalled := false
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localWasCalled = true
		w.WriteHeader(http.StatusOK)
	})

	handler := NewLocalHandler(testKey, local, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client, err := NewClient(testHexKey, ts.URL, 0, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// URL 不以 "/" 开头，不触发本地 dispatch，走 forwardExternal
	// 外部转发会失败，预期错误或 502 而非 200
	req, _ := http.NewRequest("GET", "api/files", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Logf("expected error for non-relative URL: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 && localWasCalled {
		t.Fatal("local handler was called for non-leading-slash URL")
	}
}

// JSON roundtrip for xml/json metadata
func TestResponseJSON(t *testing.T) {
	resp := Response{
		Proto:         "HTTP/1.1",
		Status:        200,
		Headers:       make(http.Header),
		ContentLength: 0,
	}
	resp.Headers.Set("Content-Type", "application/json")
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded Response
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Proto != resp.Proto || decoded.Status != resp.Status || decoded.ContentLength != resp.ContentLength {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", resp, decoded)
	}
}
