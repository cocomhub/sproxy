// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package httpjson

import (
	"bytes"
	"encoding/json"
	"errors"
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

func TestSendOpt_UsesCallerLogger(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	SendOpt(httptest.NewRecorder(), http.StatusOK, unmarshalable{Ch: make(chan int)}, Opt{Logger: log})

	if !strings.Contains(buf.String(), msgEncodeJSONFailed) {
		t.Fatalf("caller logger 未收到告警，buf = %q", buf.String())
	}
}

func TestSendOpt_HandlerAttrInWarn(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	SendOpt(httptest.NewRecorder(), http.StatusOK, unmarshalable{Ch: make(chan int)}, Opt{Logger: log, Handler: "hubNodesHandler"})

	if got := buf.String(); !strings.Contains(got, msgEncodeJSONFailed) || !strings.Contains(got, "hubNodesHandler") {
		t.Fatalf("告警应带 handler 属性，buf = %q", got)
	}
}

func TestWriteBody_WritesPreSerializedBytes(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()

	WriteBody(rec, http.StatusOK, []byte(`{"cached":true}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get(headerContentType); got != contentTypeJSON {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rec.Body.String(); got != `{"cached":true}` {
		t.Fatalf("body = %q（不得二次序列化/追加换行）", got)
	}
}

func TestSendErr_ReportsMarshalFailure(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()

	err := SendErr(rec, http.StatusOK, unmarshalable{Ch: make(chan int)}, Opt{})

	if err == nil {
		t.Fatal("SendErr 应返回序列化错误")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500（仍要回固定错误体，不能半截响应）", rec.Code)
	}
}

func TestSendErr_ReportsWriteFailure(t *testing.T) {
	t.Parallel()
	err := SendErr(failingWriter{}, http.StatusOK, map[string]string{"k": "v"}, Opt{})

	if err == nil {
		t.Fatal("SendErr 应返回写出错误")
	}
}

func TestSendOpt_CustomMsg(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	SendOpt(httptest.NewRecorder(), http.StatusOK, unmarshalable{Ch: make(chan int)}, Opt{Logger: log, Msg: "custom encode msg"})

	if got := buf.String(); !strings.Contains(got, "custom encode msg") {
		t.Fatalf("日志文案应可覆盖，buf = %q", got)
	}
}

// failingWriter 让所有写入失败，用于验证写出错误被上报。
type failingWriter struct{}

func (failingWriter) Header() http.Header       { return http.Header{} }
func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }
func (failingWriter) WriteHeader(int)           {}

func TestSendOpt_NilLoggerFallsBackToPackageLogger(t *testing.T) {
	// sproxy:serial: 替换包级告警日志器，与其它改全局日志器的用例互斥
	var buf bytes.Buffer
	prev := currentLogger()
	SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { SetLogger(prev) })

	SendOpt(httptest.NewRecorder(), http.StatusOK, unmarshalable{Ch: make(chan int)}, Opt{})

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
