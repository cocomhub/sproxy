// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package xfertest_test

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

func TestConnSuiteWithPipe(t *testing.T) {
	xfertest.ConnSuite(t, func(t *testing.T) (client, server xfer.Conn, cleanup func()) {
		a, b := xfertest.Pipe()
		return a, b, func() { a.Close(); b.Close() }
	})
}
