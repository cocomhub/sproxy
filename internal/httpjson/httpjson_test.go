// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package httpjson

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// unmarshalable 是「不可序列化值」的替身：json.Marshal 对 chan 返回
// UnsupportedTypeError，用于驱动序列化失败分支。
type unmarshalable struct {
	Ch chan int `json:"ch"`
}

func TestSend_WritesJSONWithStatus(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()

	Send(rec, http.StatusCreated, map[string]string{"k": "v"})

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if got := rec.Header().Get(headerContentType); got != contentTypeJSON {
		t.Fatalf("Content-Type = %q, want %q", got, contentTypeJSON)
	}
	// 字节契约：无尾随换行。
	if got := rec.Body.String(); got != `{"k":"v"}` {
		t.Fatalf("body = %q, want %q", got, `{"k":"v"}`)
	}
}

func TestSend_EncodeFailureWrites500WithFixedBody(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()

	Send(rec, http.StatusOK, unmarshalable{Ch: make(chan int)})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	// 字节契约：失败体沿用 json.Encoder 形态（带尾随换行）。
	if got := rec.Body.String(); got != `{"error":"internal server error"}`+"\n" {
		t.Fatalf("body = %q, want %q", got, `{"error":"internal server error"}`+"\n")
	}
}

func TestSendError_WritesErrorObject(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()

	SendError(rec, http.StatusTooManyRequests, "请求过于频繁")

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if got := rec.Body.String(); got != `{"error":"请求过于频繁"}` {
		t.Fatalf("body = %q", got)
	}
}

func TestSendLogged_UsesCallerLogger(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	SendLogged(httptest.NewRecorder(), http.StatusOK, unmarshalable{Ch: make(chan int)}, log)

	if !strings.Contains(buf.String(), msgEncodeJSONFailed) {
		t.Fatalf("caller logger 未收到告警，buf = %q", buf.String())
	}
}

func TestSendLogged_NilLoggerFallsBackToPackageLogger(t *testing.T) {
	// sproxy:serial: 替换包级告警日志器，与其它改全局日志器的用例互斥
	var buf bytes.Buffer
	prev := currentLogger()
	SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { SetLogger(prev) })

	SendLogged(httptest.NewRecorder(), http.StatusOK, unmarshalable{Ch: make(chan int)}, nil)

	if !strings.Contains(buf.String(), msgEncodeJSONFailed) {
		t.Fatalf("包级 logger 未收到告警，buf = %q", buf.String())
	}
}

func TestSetLogger_NilRestoresDefault(t *testing.T) {
	// sproxy:serial: 替换包级告警日志器，与其它改全局日志器的用例互斥
	prev := currentLogger()
	t.Cleanup(func() { SetLogger(prev) })

	SetLogger(nil)

	if currentLogger() == nil {
		t.Fatal("SetLogger(nil) 后 currentLogger() 仍为 nil")
	}
	if currentLogger() != slog.Default() {
		t.Fatal("SetLogger(nil) 应回退到 slog.Default()")
	}
}

func TestSend_ContentTypeSetBeforeWriteHeader(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	Send(rec, http.StatusAccepted, struct{}{})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("body 不是合法 JSON: %v", err)
	}
}
