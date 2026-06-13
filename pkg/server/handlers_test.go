// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http/httptest"
	"testing"
)

func TestParsePagination_Defaults(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/files", nil)
	offset, limit := parsePagination(r)
	if offset != 0 {
		t.Errorf("offset = %d, want 0", offset)
	}
	if limit != 1000 {
		t.Errorf("limit = %d, want 1000", limit)
	}
}

func TestParsePagination_NegativeOffset(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/files?offset=-1", nil)
	offset, _ := parsePagination(r)
	if offset != 0 {
		t.Errorf("offset = %d, want 0", offset)
	}
}

func TestParsePagination_ZeroLimit(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/files?limit=0", nil)
	_, limit := parsePagination(r)
	if limit != 1000 {
		t.Errorf("limit = %d, want 1000", limit)
	}
}

func TestParsePagination_LargeLimit(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/files?limit=99999", nil)
	_, limit := parsePagination(r)
	if limit != 1000 {
		t.Errorf("limit = %d, want 1000", limit)
	}
}

func TestParsePagination_Valid(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/files?offset=10&limit=50", nil)
	offset, limit := parsePagination(r)
	if offset != 10 {
		t.Errorf("offset = %d, want 10", offset)
	}
	if limit != 50 {
		t.Errorf("limit = %d, want 50", limit)
	}
}

func TestParsePagination_NonNumeric(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/files?offset=abc&limit=xyz", nil)
	offset, limit := parsePagination(r)
	if offset != 0 {
		t.Errorf("offset = %d, want 0", offset)
	}
	if limit != 1000 {
		t.Errorf("limit = %d, want 1000", limit)
	}
}

func TestParsePagination_Partial(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/files?offset=5", nil)
	offset, limit := parsePagination(r)
	if offset != 5 {
		t.Errorf("offset = %d, want 5", offset)
	}
	if limit != 1000 {
		t.Errorf("limit = %d, want 1000", limit)
	}
}
