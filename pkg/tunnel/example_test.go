// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
)

func ExampleParseKey() {
	key, err := tunnel.ParseKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("len=%d\n", len(key))
	// Output: len=32
}

func ExampleParseKey_invalidLength() {
	_, err := tunnel.ParseKey("00ff")
	fmt.Println(err)
	// Output: key must be 32 bytes (64 hex chars)
}

func ExampleGenerateKey() {
	key, err := tunnel.GenerateKey()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("len=%d\n", len(key))
	// Output: len=64
}

func Example_encryptDecrypt() {
	key, _ := tunnel.ParseKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	plaintext := []byte("hello secure tunnel")

	encrypted, err := tunnel.Encrypt(key, plaintext)
	if err != nil {
		fmt.Println("encrypt error:", err)
		return
	}

	decrypted, err := tunnel.Decrypt(key, encrypted)
	if err != nil {
		fmt.Println("decrypt error:", err)
		return
	}

	fmt.Println(string(decrypted))
	// Output: hello secure tunnel
}

func Example_randomNonce() {
	key, _ := tunnel.ParseKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	plaintext := []byte("same plaintext")

	c1, _ := tunnel.Encrypt(key, plaintext)
	c2, _ := tunnel.Encrypt(key, plaintext)

	fmt.Println(string(c1) != string(c2))
	// Output: true
}

func ExampleEncodeBody() {
	encoded := tunnel.EncodeBody([]byte("hello world"))
	fmt.Println(encoded)
	// Output: aGVsbG8gd29ybGQ=
}

func ExampleDecodeBody() {
	decoded, err := tunnel.DecodeBody("aGVsbG8gd29ybGQ=")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(string(decoded))
	// Output: hello world
}

func ExampleNewHandler() {
	key, _ := tunnel.ParseKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("target response"))
	}))
	defer targetServer.Close()

	mux := http.NewServeMux()
	mux.Handle("POST /tunnel", tunnel.NewHandler(key))
	proxyServer := httptest.NewServer(mux)
	defer proxyServer.Close()

	client, _ := tunnel.NewClient(
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		proxyServer.URL+"/tunnel",
		5*time.Second,
	)

	resp, err := client.Do(&tunnel.Request{
		Method: "GET",
		URL:    targetServer.URL,
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	fmt.Println(resp.Status)
	// Output: 200
}

func ExampleClient_Do() {
	key, _ := tunnel.ParseKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"message":"hello from target"}`)
	}))
	defer targetServer.Close()

	mux := http.NewServeMux()
	mux.Handle("POST /tunnel", tunnel.NewHandler(key))
	proxyServer := httptest.NewServer(mux)
	defer proxyServer.Close()

	client, _ := tunnel.NewClient(
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		proxyServer.URL+"/tunnel",
		5*time.Second,
	)

	resp, err := client.Do(&tunnel.Request{
		Method: "GET",
		URL:    targetServer.URL,
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	body, _ := tunnel.DecodeBody(resp.Body)
	fmt.Println(resp.Status)
	fmt.Println(string(body))
	// Output:
	// 200
	// {"message":"hello from target"}
}

func Example_clientDoWithPOST() {
	key, _ := tunnel.ParseKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, `{"echo":"%s"}`, string(body))
	}))
	defer targetServer.Close()

	mux := http.NewServeMux()
	mux.Handle("POST /tunnel", tunnel.NewHandler(key))
	proxyServer := httptest.NewServer(mux)
	defer proxyServer.Close()

	client, _ := tunnel.NewClient(
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		proxyServer.URL+"/tunnel",
		5*time.Second,
	)

	resp, err := client.Do(&tunnel.Request{
		Method:  "POST",
		URL:     targetServer.URL,
		Headers: map[string]string{"Content-Type": "text/plain"},
		Body:    tunnel.EncodeBody([]byte("hello gcm")),
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	body, _ := tunnel.DecodeBody(resp.Body)
	fmt.Println(resp.Status)
	fmt.Println(string(body))
	// Output:
	// 200
	// {"echo":"hello gcm"}
}

func Example_newHandlerEmptyKey() {
	mux := http.NewServeMux()
	mux.Handle("POST /tunnel", tunnel.NewHandler(nil))

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Post(server.URL+"/tunnel", "application/octet-stream", nil)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer resp.Body.Close()
	fmt.Println(resp.StatusCode)
	// Output: 403
}

func ExampleRequest() {
	req := &tunnel.Request{
		Method:  "POST",
		URL:     "https://api.example.com/echo",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    tunnel.EncodeBody([]byte(`{"key":"value"}`)),
	}

	fmt.Println(req.Method)
	fmt.Println(req.URL)
	fmt.Println(req.Headers["Content-Type"])

	decoded, _ := tunnel.DecodeBody(req.Body)
	fmt.Println(string(decoded))
	// Output:
	// POST
	// https://api.example.com/echo
	// application/json
	// {"key":"value"}
}

func ExampleResponse() {
	resp := tunnel.Response{
		Status: 200,
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		Body: tunnel.EncodeBody([]byte(`{"result":"ok"}`)),
	}

	fmt.Println(resp.Status)
	fmt.Println(resp.Headers["Content-Type"])

	decoded, _ := tunnel.DecodeBody(resp.Body)
	fmt.Println(string(decoded))
	// Output:
	// 200
	// application/json
	// {"result":"ok"}
}

func Example_tamperDetection() {
	key, _ := tunnel.ParseKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	mux := http.NewServeMux()
	mux.Handle("POST /tunnel", tunnel.NewHandler(key))
	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Post(server.URL+"/tunnel", "application/octet-stream", nil)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer resp.Body.Close()
	fmt.Println(resp.StatusCode)
	// Output: 400
}

func ExampleClient_DoHTTP() {
	key, _ := tunnel.ParseKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Response-Header", "test-value")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"method":"%s","path":"%s"}`, r.Method, r.URL.Path)
	}))
	defer targetServer.Close()

	mux := http.NewServeMux()
	mux.Handle("POST /tunnel", tunnel.NewHandler(key))
	proxyServer := httptest.NewServer(mux)
	defer proxyServer.Close()

	client, _ := tunnel.NewClient(
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		proxyServer.URL+"/tunnel",
		5*time.Second,
	)

	req, _ := http.NewRequest("GET", targetServer.URL+"/api/hello", nil)
	req.Header.Set("X-Custom", "test")

	resp, err := client.DoHTTP(req)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer resp.Body.Close()

	fmt.Println(resp.StatusCode)
	fmt.Println(resp.Header.Get("X-Response-Header"))

	body, _ := io.ReadAll(resp.Body)
	fmt.Println(string(body))
	// Output:
	// 200
	// test-value
	// {"method":"GET","path":"/api/hello"}
}

func Example() {
	os.Setenv("TUNNEL_KEY", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	defer os.Unsetenv("TUNNEL_KEY")

	hexKey := os.Getenv("TUNNEL_KEY")

	key, err := tunnel.ParseKey(hexKey)
	if err != nil {
		fmt.Println("parse key error:", err)
		return
	}

	plaintext, err := tunnel.Encrypt(key, []byte("hello"))
	if err != nil {
		return
	}

	decrypted, err := tunnel.Decrypt(key, plaintext)
	if err != nil {
		return
	}

	fmt.Println(string(decrypted))
	// Output: hello
}
