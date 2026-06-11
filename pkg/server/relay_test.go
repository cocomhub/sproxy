package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

func TestRelayHandlerMissingTarget(t *testing.T) {
	rt := hub.NewRouteTable()
	h := NewRelayHandler(rt, slog.Default())

	body := `{"target":"", "method":"GET", "path":"/api/files"}`
	req := httptest.NewRequest(http.MethodPost, "/api/relay", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	var resp RelayResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", resp.Status)
	}
}

func TestRelayHandlerUnknownTarget(t *testing.T) {
	rt := hub.NewRouteTable()
	h := NewRelayHandler(rt, slog.Default())

	body := `{"target":"unknown", "method":"GET", "path":"/api/files"}`
	req := httptest.NewRequest(http.MethodPost, "/api/relay", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestRelayHandlerBadJSON(t *testing.T) {
	rt := hub.NewRouteTable()
	h := NewRelayHandler(rt, slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/api/relay", strings.NewReader("{bad json"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}
