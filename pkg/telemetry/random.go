// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// TraceID generates a 16-byte (32 hex) trace id using crypto/rand.
func TraceID() string {
	var b [16]byte
	_ = randBytes(b[:])
	return hex.EncodeToString(b[:])
}

// SpanID generates an 8-byte (16 hex) span id using crypto/rand.
func SpanID() string {
	var b [8]byte
	_ = randBytes(b[:])
	return hex.EncodeToString(b[:])
}

func randBytes(b []byte) error {
	_, err := rand.Read(b)
	return err
}

// NewTraceparent builds a W3C traceparent header value for the given trace/span.
func NewTraceparent(traceID, spanID string) string {
	return "00-" + traceID + "-" + spanID + "-01"
}

// ParseTraceparent parses a W3C traceparent header value, returning the traceID
// and spanID. ok is false for any malformed or unsupported value.
func ParseTraceparent(s string) (traceID, spanID string, ok bool) {
	parts := strings.Split(s, "-")
	if len(parts) != 4 || parts[0] != "00" {
		return "", "", false
	}
	if len(parts[1]) != 32 || len(parts[2]) != 16 {
		return "", "", false
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return "", "", false
	}
	if _, err := hex.DecodeString(parts[2]); err != nil {
		return "", "", false
	}
	return parts[1], parts[2], true
}
