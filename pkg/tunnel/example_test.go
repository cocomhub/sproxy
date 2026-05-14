// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel_test

import (
	"bytes"
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

	req, _ := http.NewRequest("GET", targetServer.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	fmt.Println(resp.Status)
	// Output: 200 OK
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
		Headers: http.Header{
			"Content-Type": {"application/json"},
		},
		Body: tunnel.EncodeBody([]byte(`{"result":"ok"}`)),
	}

	fmt.Println(resp.Status)
	fmt.Println(resp.Headers.Get("Content-Type"))

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

	// 发送损坏的帧（非法 metadata），服务端应返回 400
	resp, err := http.Post(server.URL+"/tunnel", "application/x-tunnel-frame", bytes.NewReader([]byte("corrupted frame data")))
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer resp.Body.Close()
	fmt.Println(resp.StatusCode)
	// Output: 400
}

func ExampleClient_Do() {
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

	resp, err := client.Do(req)
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

func ExampleClient_Do_largeBody() {
	key, _ := tunnel.ParseKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, `{"size":%d}`, len(body))
	}))
	defer targetServer.Close()

	mux := http.NewServeMux()
	mux.Handle("POST /tunnel", tunnel.NewHandler(key))
	proxyServer := httptest.NewServer(mux)
	defer proxyServer.Close()

	client, _ := tunnel.NewClient(
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		proxyServer.URL+"/tunnel",
		10*time.Second,
	)

	// 使用 ≥ 64KB 的数据验证流式传输（2 个完整 chunk）
	largeBody := make([]byte, 128*1024)
	for i := range largeBody {
		largeBody[i] = byte('A' + (i % 26))
	}

	req, _ := http.NewRequest("POST", targetServer.URL, bytes.NewReader(largeBody))
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer resp.Body.Close()

	fmt.Println(resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	fmt.Println(string(body))
	// Output:
	// 200
	// {"size":131072}
}

func ExampleClient_Do_streamResponse() {
	key, _ := tunnel.ParseKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	const responseSize = 128 * 1024 // 128KB

	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := make([]byte, responseSize)
		for i := range data {
			data[i] = byte('Z' - (i % 26))
		}
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}))
	defer targetServer.Close()

	mux := http.NewServeMux()
	mux.Handle("POST /tunnel", tunnel.NewHandler(key))
	proxyServer := httptest.NewServer(mux)
	defer proxyServer.Close()

	client, _ := tunnel.NewClient(
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		proxyServer.URL+"/tunnel",
		10*time.Second,
	)

	req, _ := http.NewRequest("GET", targetServer.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer resp.Body.Close()

	// 流式消费：io.Copy 边解密边读，内存占用恒定
	var buf bytes.Buffer
	n, err := io.Copy(&buf, resp.Body)
	if err != nil {
		fmt.Println("copy error:", err)
		return
	}

	fmt.Println(resp.StatusCode)
	fmt.Println(n == responseSize)
	// Output:
	// 200
	// true
}

func Example_encryptStreamDecryptStream() {
	key, _ := tunnel.ParseKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	original := []byte("hello streaming world with chunked AES-256-GCM encryption")
	var encrypted bytes.Buffer

	n, err := tunnel.EncryptStream(key, bytes.NewReader(original), &encrypted)
	if err != nil {
		fmt.Println("encrypt error:", err)
		return
	}
	_ = n

	var decrypted bytes.Buffer
	n2, err := tunnel.DecryptStream(key, bytes.NewReader(encrypted.Bytes()), &decrypted)
	if err != nil {
		fmt.Println("decrypt error:", err)
		return
	}
	_ = n2

	fmt.Println(string(decrypted.Bytes()))
	// Output: hello streaming world with chunked AES-256-GCM encryption
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
