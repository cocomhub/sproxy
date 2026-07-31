// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"fmt"
	"math"
	"testing"
)

func TestFormatByte_AllUnits(t *testing.T) {
	tests := []struct {
		size float64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{1024*1024 + 1, "1.0 MB"},
		{1024*1024 + 512*1024, "1.5 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{1024 * 1024 * 1024 * 1024, "1.0 TB"},
		{1023, "1023 B"},
		{1024*1024 - 1024, "1023.0 KB"},
		{1024*1024*1024 - 1024*1024, "1023.0 MB"},
		{math.NaN(), "0 B"},
		{math.Inf(1), "0 B"},
		{math.Inf(-1), "0 B"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := FormatByte(tt.size)
			if got != tt.want {
				t.Errorf("FormatByte(%v) = %q, want %q", tt.size, got, tt.want)
			}
		})
	}
}

func TestFormatByte_Negative(t *testing.T) {
	got := FormatByte(-100)
	if got != "0 B" {
		t.Errorf("FormatByte(-100) = %q, want 0 B", got)
	}
}

func TestFormatETA_NonPositive(t *testing.T) {
	tests := []struct {
		seconds int64
		want    string
	}{
		{-1, "--:--"},
		{0, "--:--"},
		{-100, "--:--"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("seconds=%d", tt.seconds), func(t *testing.T) {
			if got := FormatETA(tt.seconds); got != tt.want {
				t.Errorf("FormatETA(%d) = %q, want %q", tt.seconds, got, tt.want)
			}
		})
	}
}

func TestFormatETA_Boundaries(t *testing.T) {
	tests := []struct {
		seconds int64
		want    string
	}{
		{1, "1s"},
		{59, "59s"},
		{61, "1m 1s"},
		{60, "1m 0s"},
		{3599, "59m 59s"},
		{3600, "1h 0m"},
		{3660, "1h 1m"},
		{3661, "1h 1m"},
		{100000, "27h 46m"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := FormatETA(tt.seconds); got != tt.want {
				t.Errorf("FormatETA(%d) = %q, want %q", tt.seconds, got, tt.want)
			}
		})
	}
}
