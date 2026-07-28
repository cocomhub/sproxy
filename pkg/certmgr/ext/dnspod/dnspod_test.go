// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dnspod

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// endpointFromTestServer 从 httptest.Server 的 URL 提取 endpoint（含 scheme）。
func endpointFromTestServer(ts *httptest.Server) string {
	return ts.URL
}

func TestNewProvider(t *testing.T) {
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
	p := New(Config{})
	err := p.SetDNSRecord(context.Background(), "example.com", "token", "keyauth")
	if err == nil {
		t.Error("expected error for empty config")
	}
}

func TestCleanupDNSRecord_EmptyConfig(t *testing.T) {
	p := New(Config{})
	err := p.CleanupDNSRecord(context.Background(), "example.com", "token", "keyauth")
	if err == nil {
		t.Error("expected error for empty config")
	}
}

func TestSetDNSRecord_Success(t *testing.T) {
	// Mock server that validates the DNSPod API request format
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Verify request method
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		// Verify query parameters
		q := r.URL.Query()
		if q.Get("Action") != "CreateRecord" {
			t.Errorf("expected Action=CreateRecord, got %s", q.Get("Action"))
		}
		if q.Get("Domain") != "example.com" {
			t.Errorf("expected Domain=example.com, got %s", q.Get("Domain"))
		}
		if q.Get("SubDomain") != "_acme-challenge" {
			t.Errorf("expected SubDomain=_acme-challenge, got %s", q.Get("SubDomain"))
		}
		if q.Get("RecordType") != "TXT" {
			t.Errorf("expected RecordType=TXT, got %s", q.Get("RecordType"))
		}
		if q.Get("Value") != "test-key-auth" {
			t.Errorf("expected Value=test-key-auth, got %s", q.Get("Value"))
		}
		if q.Get("SecretId") != "test-secret-id" {
			t.Errorf("expected SecretId=test-secret-id, got %s", q.Get("SecretId"))
		}
		if q.Get("SignatureMethod") != "HmacSHA1" {
			t.Errorf("expected SignatureMethod=HmacSHA1, got %s", q.Get("SignatureMethod"))
		}
		if q.Get("Signature") == "" {
			t.Error("expected non-empty Signature")
		}
		if q.Get("Timestamp") == "" {
			t.Error("expected non-empty Timestamp")
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

	// Use the mock server as endpoint
	p := New(Config{
		SecretId:  "test-secret-id",
		SecretKey: "test-secret-key",
		Endpoint:  endpointFromTestServer(ts),
	})

	err := p.SetDNSRecord(context.Background(), "example.com", "token", "test-key-auth")
	if err != nil {
		t.Fatalf("SetDNSRecord failed: %v", err)
	}
}

func TestCleanupDNSRecord_Success(t *testing.T) {
	callCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		callCount++
		q := r.URL.Query()

		if callCount == 1 {
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
		} else if callCount == 2 {
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
		SecretId:  "test-secret-id",
		SecretKey: "test-secret-key",
		Endpoint:  endpointFromTestServer(ts),
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
		SecretId:  "test-secret-id",
		SecretKey: "test-secret-key",
		Endpoint:  endpointFromTestServer(ts),
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
		SecretId:  "test-secret-id",
		SecretKey: "test-secret-key",
		Endpoint:  endpointFromTestServer(ts),
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
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintln(w, "internal error")
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := New(Config{
		SecretId:  "test-secret-id",
		SecretKey: "test-secret-key",
		Endpoint:  endpointFromTestServer(ts),
	})

	err := p.SetDNSRecord(context.Background(), "example.com", "token", "keyauth")
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
}

func TestCleanupDNSRecord_InvalidJSONResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "not valid json")
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := New(Config{
		SecretId:  "test-secret-id",
		SecretKey: "test-secret-key",
		Endpoint:  endpointFromTestServer(ts),
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
	// Compile-time check: *Provider implements the DNSProvider interface
	var _ interface {
		SetDNSRecord(ctx context.Context, domain, token, keyAuth string) error
		CleanupDNSRecord(ctx context.Context, domain, token, keyAuth string) error
	} = New(Config{})
}

func TestSplitDomain(t *testing.T) {
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
		SecretId:  "test-secret-id",
		SecretKey: "test-secret-key",
		Endpoint:  endpointFromTestServer(ts),
	})

	err := p.SetDNSRecord(context.Background(), "www.example.com", "token", "test-key-auth")
	if err != nil {
		t.Fatalf("SetDNSRecord for subdomain failed: %v", err)
	}
}

func TestCleanupDNSRecord_Subdomain(t *testing.T) {
	callCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		callCount++
		q := r.URL.Query()

		if callCount == 1 {
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
		} else if callCount == 2 {
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
		SecretId:  "test-secret-id",
		SecretKey: "test-secret-key",
		Endpoint:  endpointFromTestServer(ts),
	})

	err := p.CleanupDNSRecord(context.Background(), "sub.example.com", "token", "test-key-auth")
	if err != nil {
		t.Fatalf("CleanupDNSRecord for subdomain failed: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected 2 API calls, got %d", callCount)
	}
}
