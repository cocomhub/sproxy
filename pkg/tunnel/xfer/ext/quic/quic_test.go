// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quic_test

import (
	"runtime"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/quic"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

func TestQUIC(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("QUIC network tests not supported on Windows (UDP connectivity issues)")
	}
	xfertest.TestHarness(t, xfertest.Harness{
		Name:   "quic",
		Dial:   quic.Dial,
		Listen: quic.Listen,
	})
}
