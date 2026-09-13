// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// shortWriter 模拟 mux.Stream 的窗口受限短写：每次最多写 limit 字节，返回 (n, nil)。
// 这正是 io.Writer 契约允许但极易被调用方忽略的行为（真 mux 流的实现见
// pkg/tunnel/mux/stream.go 的 (*stream).Write：等窗口 >0 后只投递 min(len(p), window)）。
type shortWriter struct {
	buf   bytes.Buffer
	limit int
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > w.limit {
		p = p[:w.limit]
	}
	return w.buf.Write(p)
}

// TestWriteFull_LoopsOnShortWrite 钉住 writeFull 的短写循环：无论 w 单次只吃多少字节，
// 最终必须写足全部输入；且对「(0, nil)」这种违约写入返回 io.ErrShortWrite 而非死循环。
func TestWriteFull_LoopsOnShortWrite(t *testing.T) {
	payload := bytes.Repeat([]byte("abc"), 5000) // 15000 B
	for _, limit := range []int{1, 7, 4096} {
		w := &shortWriter{limit: limit}
		if err := writeFull(w, payload); err != nil {
			t.Fatalf("limit=%d writeFull: %v", limit, err)
		}
		if !bytes.Equal(w.buf.Bytes(), payload) {
			t.Fatalf("limit=%d 字节不完整: got %d want %d", limit, w.buf.Len(), len(payload))
		}
	}

	if err := writeFull(zeroWriter{}, []byte("x")); err != io.ErrShortWrite {
		t.Fatalf("(0,nil) 违约写入应返回 io.ErrShortWrite, got %v", err)
	}
}

type zeroWriter struct{}

func (zeroWriter) Write(p []byte) (int, error) { return 0, nil }

// TestEncryptChunk_ShortWriteNotTruncated 钉住 EncryptChunk 的长度前缀与密文体都写足：
// 短写被静默忽略时，密文流会错位/截断，对端解密报 unexpected EOF 或 GCM 认证失败。
func TestEncryptChunk_ShortWriteNotTruncated(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	enc, encErr := NewStreamEncryptor(key, DefaultChunkSize)
	if encErr != nil {
		t.Fatal(encErr)
	}
	plaintext := bytes.Repeat([]byte("payload-"), 3000) // 24000 B
	w := &shortWriter{limit: 100}                       // 远小于单块密文长度
	if _, cErr := enc.EncryptChunk(plaintext, w, []byte(AADStream)); cErr != nil {
		t.Fatalf("EncryptChunk: %v", cErr)
	}

	dec, err := NewStreamDecryptor(key, maxChunkLen)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := dec.DecryptChunk(bytes.NewReader(w.buf.Bytes()), &out, []byte(AADStream)); err != nil {
		t.Fatalf("短写后密文应可完整解密, got %v", err)
	}
	if !bytes.Equal(out.Bytes(), plaintext) {
		t.Fatalf("解密内容不符: got %d bytes", out.Len())
	}
}

// TestTunnel_PlaintextLargeBodyRoundTrip 钉住**明文**（key == nil）隧道下 > 64 KB 的
// 响应体同样完整。回归背景（I1 复审实测）：writeEncryptedResponse 的明文分支曾是
// `io.Copy(stream, buf)`——io.Copy 在 mux.Stream 首次窗口受限短写处返回 io.ErrShortWrite
// 并**提前结束**，而该调用点丢弃返回值，于是 Do() 报成功、读响应体也无错误，响应体却被
// 静默截断在 65466 B（= 65536 流控窗口 − 70 B 元数据）——密文分支会因 GCM 认证失败暴露，
// 明文分支**没有任何症状**，故必须单独钉住。
func TestTunnel_PlaintextLargeBodyRoundTrip(t *testing.T) {
	// 三档：窗口内、刚过窗口（旧实现恰好在此截断）、远超窗口（跨多轮窗口更新）。
	for _, size := range []int{1000, 60000, 70000, 200000, 1000000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			payload := bytes.Repeat([]byte{0xc3}, size)

			a, b := xfertest.Pipe()
			mb := mux.New(b, mux.RoleListener)
			defer func() { _ = mb.Close() }()
			tb := NewTunnel(mb, nil) // nil key ⇒ 明文分支（无握手、无加密）
			go func() {
				_ = tb.Serve(t.Context(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write(payload)
				}))
			}()

			ma := mux.New(a, mux.RoleDialer)
			defer func() { _ = ma.Close() }()
			ta := NewTunnel(ma, nil)

			req, err := http.NewRequest(http.MethodGet, "/plain", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := ta.Do(req)
			if err != nil {
				t.Fatalf("size=%d Do: %v", size, err)
			}
			got, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatalf("size=%d 读响应体: %v", size, err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("size=%d 明文响应体被静默截断: got %d bytes want %d", size, len(got), size)
			}
		})
	}
}

// TestTunnel_PlaintextLargeRequestBody 钉住明文隧道下 > 64 KB 的**请求体**也能完整送达。
// 回归背景（与 TestTunnel_PlaintextLargeBodyRoundTrip 同源）：sendRequestBody 的明文分支
// 曾是 `io.Copy(stream, req.Body)`——mux 流短写使 io.Copy 提前返回 io.ErrShortWrite，
// 明文模式下 >64 KB 的上传体因此在写侧必然失败（此处返回值未被丢弃，属响亮失败，
// 但仍是把短写语义用错）。
func TestTunnel_PlaintextLargeRequestBody(t *testing.T) {
	for _, size := range []int{1000, 70000, 300000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			payload := bytes.Repeat([]byte{0x9d}, size)
			gotLen := make(chan int, 1)

			a, b := xfertest.Pipe()
			mb := mux.New(b, mux.RoleListener)
			defer func() { _ = mb.Close() }()
			tb := NewTunnel(mb, nil) // 明文分支
			go func() {
				_ = tb.Serve(t.Context(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					n, _ := io.Copy(io.Discard, r.Body) // 服务端必须能读满整个请求体
					gotLen <- int(n)
					w.WriteHeader(http.StatusOK)
				}))
			}()

			ma := mux.New(a, mux.RoleDialer)
			defer func() { _ = ma.Close() }()
			ta := NewTunnel(ma, nil)

			req, err := http.NewRequest(http.MethodPost, "/up", bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := ta.Do(req)
			if err != nil {
				t.Fatalf("size=%d Do: %v", size, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("size=%d 状态码 %d", size, resp.StatusCode)
			}
			select {
			case n := <-gotLen:
				if n != size {
					t.Fatalf("size=%d 请求体被截断: 服务端读到 %d B", size, n)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("size=%d 服务端未收到完整请求体", size)
			}
		})
	}
}

// TestTunnel_LargeBodyRoundTrip 端到端钉住：经 mux + Tunnel（真加密）的**大于流控窗口
// （64 KB）**的响应体不再被静默截断/错位。回归背景：mux.Stream.Write 是窗口受限短写，
// Tunnel 的密文写入点曾忽略返回的 n，导致 > ~64 KB 的响应体解密报 "unexpected EOF"
// 或 "cipher: message authentication failed"（数据损坏）。
func TestTunnel_LargeBodyRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, 32)
	// 三档：窗口内、刚过窗口、远超窗口（跨多个密文块 + 多轮窗口更新）。
	for _, size := range []int{1000, 60000, 70000, 300000} {
		t.Run("", func(t *testing.T) {
			payload := bytes.Repeat([]byte{0xab}, size)

			a, b := xfertest.Pipe()
			mb := mux.New(b, mux.RoleListener)
			defer func() { _ = mb.Close() }()
			tb := NewTunnel(mb, key)
			go func() {
				_ = tb.Serve(t.Context(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write(payload)
				}))
			}()

			ma := mux.New(a, mux.RoleDialer)
			defer func() { _ = ma.Close() }()
			ta := NewTunnel(ma, key)

			req, err := http.NewRequest(http.MethodGet, "/x", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := ta.Do(req)
			if err != nil {
				t.Fatalf("size=%d Do: %v", size, err)
			}
			got, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatalf("size=%d 读响应体: %v", size, err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("size=%d 内容不符（截断或错位）: got %d bytes want %d", size, len(got), size)
			}
		})
	}
}
