// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import "testing"

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
		{1024*1024 + 512*1024, "1.5 MB"},
		{1024 * 1024 * 1024, "1024.0 MB"},
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

func TestFormatETA_Negative(t *testing.T) {
	if got := FormatETA(-1); got != "--:--" {
		t.Errorf("FormatETA(-1) = %q, want --:--", got)
	}
}

func TestFormatETA_Zero(t *testing.T) {
	if got := FormatETA(0); got != "--:--" {
		t.Errorf("FormatETA(0) = %q, want --:--", got)
	}
}

func TestFormatETA_Hours(t *testing.T) {
	if got := FormatETA(3661); got != "1h 1m" {
		t.Errorf("FormatETA(3661) = %q, want 1h 1m", got)
	}
}

func TestFormatETA_Minutes(t *testing.T) {
	if got := FormatETA(125); got != "2m 5s" {
		t.Errorf("FormatETA(125) = %q, want 2m 5s", got)
	}
}

func TestFormatETA_Seconds(t *testing.T) {
	if got := FormatETA(45); got != "45s" {
		t.Errorf("FormatETA(45) = %q, want 45s", got)
	}
}
