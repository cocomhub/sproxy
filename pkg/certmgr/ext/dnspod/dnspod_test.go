// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dnspod

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// endpointFromTestServer 从 httptest.Server 的 URL 提取 endpoint（含 scheme）。
func endpointFromTestServer(ts *httptest.Server) string {
	return ts.URL
}

func TestNewProvider(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	p := New(Config{
		SecretId:  "test-secret-id",
		SecretKey: "test-secret-key",
	})
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
	if p.config.SecretId != "test-secret-id" {
		t.Errorf("SecretId = %q, want %q", p.config.SecretId, "test-secret-id")
	}
	if p.config.SecretKey != "test-secret-key" {
		t.Errorf("SecretKey = %q, want %q", p.config.SecretKey, "test-secret-key")
	}
	if p.endpoint != "dnspod.tencentcloudapi.com" {
		t.Errorf("endpoint = %q, want %q", p.endpoint, "dnspod.tencentcloudapi.com")
	}
}

func TestNewProvider_CustomEndpoint(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	p := New(Config{
		SecretId:  "id",
		SecretKey: "key",
		Endpoint:  "custom.example.com",
	})
	if p.endpoint != "custom.example.com" {
		t.Errorf("endpoint = %q, want %q", p.endpoint, "custom.example.com")
	}
}

func TestSetDNSRecord_EmptyConfig(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	p := New(Config{})
	err := p.SetDNSRecord(context.Background(), "example.com", "token", "keyauth")
	if err == nil {
		t.Error("expected error for empty config")
	}
}

func TestCleanupDNSRecord_EmptyConfig(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	p := New(Config{})
	err := p.CleanupDNSRecord(context.Background(), "example.com", "token", "keyauth")
	if err == nil {
		t.Error("expected error for empty config")
	}
}

func TestSetDNSRecord_Success(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	// Mock server that validates the DNSPod API request format
	mux := http.NewServeMux()
	mux.HandleFunc("/", newTestSetDNSRecordHandler(t))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Use the mock server as endpoint
	p := New(Config{
		SecretId:   "test-secret-id",
		SecretKey:  "test-secret-key",
		Endpoint:   endpointFromTestServer(ts),
		HTTPClient: testutil.IsolatedClient(t),
	})

	err := p.SetDNSRecord(context.Background(), "example.com", "token", "test-key-auth")
	if err != nil {
		t.Fatalf("SetDNSRecord failed: %v", err)
	}
}

// newTestSetDNSRecordHandler 返回验证 DNSPod CreateRecord 请求格式的 mock handler。
func newTestSetDNSRecordHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		// Verify request method
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		// Verify query parameters
		q := r.URL.Query()
		assertQueryParam(t, q, "Action", "CreateRecord")
		assertQueryParam(t, q, "Domain", "example.com")
		assertQueryParam(t, q, "SubDomain", "_acme-challenge")
		assertQueryParam(t, q, "RecordType", "TXT")
		assertQueryParam(t, q, "Value", "test-key-auth")
		assertQueryParam(t, q, "SecretId", "test-secret-id")
		assertQueryParam(t, q, "SignatureMethod", "HmacSHA1")
		assertQueryParamNonEmpty(t, q, "Signature")
		assertQueryParamNonEmpty(t, q, "Timestamp")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"Response": map[string]any{
				"RequestId": "req-123",
				"RecordId":  456,
			},
		})
	}
}

// assertQueryParam 断言 query 参数 key 等于 want（错误消息与既有 mock 文案一致）。
func assertQueryParam(t *testing.T, q url.Values, key, want string) {
	t.Helper()
	if got := q.Get(key); got != want {
		t.Errorf("expected %s=%s, got %s", key, want, got)
	}
}

// assertQueryParamNonEmpty 断言 query 参数 key 非空（错误消息与既有 mock 文案一致）。
func assertQueryParamNonEmpty(t *testing.T, q url.Values, key string) {
	t.Helper()
	if q.Get(key) == "" {
		t.Errorf("expected non-empty %s", key)
	}
}

func TestCleanupDNSRecord_Success(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	callCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		callCount++
		q := r.URL.Query()

		switch callCount {
		case 1:
			// First call: RecordList
			if q.Get("Action") != "RecordList" {
				t.Errorf("expected Action=RecordList, got %s", q.Get("Action"))
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"Response": map[string]any{
					"RequestId": "req-list",
					"RecordList": []map[string]any{
						{"RecordId": 789, "Value": "test-key-auth"},
						{"RecordId": 790, "Value": "other-value"},
					},
				},
			})
		case 2:
			// Second call: DeleteRecord
			if q.Get("Action") != "DeleteRecord" {
				t.Errorf("expected Action=DeleteRecord, got %s", q.Get("Action"))
			}
			if q.Get("RecordId") != "789" {
				t.Errorf("expected RecordId=789, got %s", q.Get("RecordId"))
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"Response": map[string]any{
					"RequestId": "req-del",
				},
			})
		}
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := New(Config{
		SecretId:   "test-secret-id",
		SecretKey:  "test-secret-key",
		Endpoint:   endpointFromTestServer(ts),
		HTTPClient: testutil.IsolatedClient(t),
	})

	err := p.CleanupDNSRecord(context.Background(), "example.com", "token", "test-key-auth")
	if err != nil {
		t.Fatalf("CleanupDNSRecord failed: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected 2 API calls, got %d", callCount)
	}
}

func TestCleanupDNSRecord_NoMatchingRecord(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	callCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		callCount++
		q := r.URL.Query()
		if q.Get("Action") != "RecordList" {
			t.Errorf("expected Action=RecordList, got %s", q.Get("Action"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"Response": map[string]any{
				"RequestId": "req-list",
				"RecordList": []map[string]any{
					{"RecordId": 789, "Value": "other-value"},
				},
			},
		})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := New(Config{
		SecretId:   "test-secret-id",
		SecretKey:  "test-secret-key",
		Endpoint:   endpointFromTestServer(ts),
		HTTPClient: testutil.IsolatedClient(t),
	})

	err := p.CleanupDNSRecord(context.Background(), "example.com", "token", "test-key-auth")
	if err != nil {
		t.Fatalf("CleanupDNSRecord failed: %v", err)
	}

	if callCount != 1 {
		t.Errorf("expected 1 API call (no delete needed), got %d", callCount)
	}
}

func TestSetDNSRecord_APIError(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"Response": map[string]any{
				"Error": map[string]string{
					"Code":    "InvalidParameter",
					"Message": "Domain not found",
				},
			},
		})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := New(Config{
		SecretId:   "test-secret-id",
		SecretKey:  "test-secret-key",
		Endpoint:   endpointFromTestServer(ts),
		HTTPClient: testutil.IsolatedClient(t),
	})

	err := p.SetDNSRecord(context.Background(), "example.com", "token", "keyauth")
	if err == nil {
		t.Fatal("expected error for API error response")
	}
	if !strings.Contains(err.Error(), "InvalidParameter") {
		t.Errorf("expected error to contain 'InvalidParameter', got %v", err)
	}
}

func TestSetDNSRecord_HTTPError(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintln(w, "internal error")
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := New(Config{
		SecretId:   "test-secret-id",
		SecretKey:  "test-secret-key",
		Endpoint:   endpointFromTestServer(ts),
		HTTPClient: testutil.IsolatedClient(t),
	})

	err := p.SetDNSRecord(context.Background(), "example.com", "token", "keyauth")
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
}

func TestCleanupDNSRecord_InvalidJSONResponse(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "not valid json")
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := New(Config{
		SecretId:   "test-secret-id",
		SecretKey:  "test-secret-key",
		Endpoint:   endpointFromTestServer(ts),
		HTTPClient: testutil.IsolatedClient(t),
	})

	// callAPIWithResult is called with nil result for SetDNSRecord,
	// so it won't try to parse JSON. Let's verify CleanupDNSRecord
	// which calls callAPIWithResult with a result struct.
	err := p.CleanupDNSRecord(context.Background(), "example.com", "token", "keyauth")
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
}

func TestProvider_ImplementsDNSProvider(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	// Compile-time check: *Provider implements the DNSProvider interface
	var _ interface {
		SetDNSRecord(ctx context.Context, domain, token, keyAuth string) error
		CleanupDNSRecord(ctx context.Context, domain, token, keyAuth string) error
	} = New(Config{})
}

func TestSplitDomain(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	tests := []struct {
		domain   string
		wantRoot string
		wantSub  string
		desc     string
	}{
		{"example.com", "example.com", "", "根域名"},
		{"sub.example.com", "example.com", "sub", "一级子域名"},
		{"deep.sub.example.com", "example.com", "deep.sub", "多级子域名"},
		{"a", "a", "", "单标签域名"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			root, sub := splitDomain(tt.domain)
			if root != tt.wantRoot {
				t.Errorf("splitDomain(%q) root = %q, want %q", tt.domain, root, tt.wantRoot)
			}
			if sub != tt.wantSub {
				t.Errorf("splitDomain(%q) sub = %q, want %q", tt.domain, sub, tt.wantSub)
			}
		})
	}
}

func TestSubDomainPrefix(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	tests := []struct {
		sub  string
		want string
		desc string
	}{
		{"", "_acme-challenge", "根域名"},
		{"sub", "_acme-challenge.sub", "一级子域名"},
		{"deep.sub", "_acme-challenge.deep.sub", "多级子域名"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			got := subDomainPrefix(tt.sub)
			if got != tt.want {
				t.Errorf("subDomainPrefix(%q) = %q, want %q", tt.sub, got, tt.want)
			}
		})
	}
}

func TestSetDNSRecord_Subdomain(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	// Mock server that validates the DNSPod API request format for subdomain
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("Domain") != "example.com" {
			t.Errorf("expected Domain=example.com, got %s", q.Get("Domain"))
		}
		if q.Get("SubDomain") != "_acme-challenge.www" {
			t.Errorf("expected SubDomain=_acme-challenge.www, got %s", q.Get("SubDomain"))
		}
		if q.Get("Action") != "CreateRecord" {
			t.Errorf("expected Action=CreateRecord, got %s", q.Get("Action"))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"Response": map[string]any{
				"RequestId": "req-123",
				"RecordId":  456,
			},
		})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := New(Config{
		SecretId:   "test-secret-id",
		SecretKey:  "test-secret-key",
		Endpoint:   endpointFromTestServer(ts),
		HTTPClient: testutil.IsolatedClient(t),
	})

	err := p.SetDNSRecord(context.Background(), "www.example.com", "token", "test-key-auth")
	if err != nil {
		t.Fatalf("SetDNSRecord for subdomain failed: %v", err)
	}
}

func TestCleanupDNSRecord_Subdomain(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	callCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		callCount++
		q := r.URL.Query()

		switch callCount {
		case 1:
			// RecordList
			if q.Get("Domain") != "example.com" {
				t.Errorf("expected Domain=example.com, got %s", q.Get("Domain"))
			}
			if q.Get("SubDomain") != "_acme-challenge.sub" {
				t.Errorf("expected SubDomain=_acme-challenge.sub, got %s", q.Get("SubDomain"))
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"Response": map[string]any{
					"RequestId": "req-list",
					"RecordList": []map[string]any{
						{"RecordId": 789, "Value": "test-key-auth"},
					},
				},
			})
		case 2:
			// DeleteRecord
			if q.Get("Action") != "DeleteRecord" {
				t.Errorf("expected Action=DeleteRecord, got %s", q.Get("Action"))
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"Response": map[string]any{
					"RequestId": "req-del",
				},
			})
		}
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := New(Config{
		SecretId:   "test-secret-id",
		SecretKey:  "test-secret-key",
		Endpoint:   endpointFromTestServer(ts),
		HTTPClient: testutil.IsolatedClient(t),
	})

	err := p.CleanupDNSRecord(context.Background(), "sub.example.com", "token", "test-key-auth")
	if err != nil {
		t.Fatalf("CleanupDNSRecord for subdomain failed: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected 2 API calls, got %d", callCount)
	}
}
