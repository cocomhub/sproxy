// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ws_test

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

func TestWS(t *testing.T) {
	xfertest.TestHarness(t, xfertest.Harness{
		Name:   "ws",
		Dial:   ws.Dial,
		Listen: ws.Listen,
	})
}
