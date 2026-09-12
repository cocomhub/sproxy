// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// shortWriter 模拟 mux.Stream 的窗口受限短写：每次最多写 limit 字节，返回 (n, nil)。
// 这正是 io.Writer 契约允许但极易被调用方忽略的行为（见 TestWrite_BiggerThanWindow）。
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
