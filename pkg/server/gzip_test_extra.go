// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"io"
	"net/http/httptest"
	"testing"
)

func TestGzipResponseWriter_Flush(t *testing.T) {
	// Flush() 不应 panic
	w := &gzipResponseWriter{
		Writer:         io.Discard,
		ResponseWriter: httptest.NewRecorder(),
	}
	w.Flush()
}
