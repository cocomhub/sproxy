// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
)

func TestShareCreateCmd_Integration(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/share" && r.Method == http.MethodPost {
			json.NewEncoder(w).Encode(&client.ShareLink{
				Token:        "share-token-123",
				Filename:     "test.txt",
				ExpiresAt:    time.Now().Add(24 * time.Hour).Format(time.RFC3339),
				MaxDownloads: 5,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdShareCreate(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.PersistentFlags().String("server", "", "")
	cmd.Flags().Set("ttl", "24h")
	cmd.SetArgs([]string{"test.txt"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("share create command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "share-token-123") {
		t.Errorf("expected output to contain share token, got: %s", buf.String())
	}
}
