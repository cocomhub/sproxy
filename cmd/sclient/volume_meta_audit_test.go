// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/cli"
)

// TestNewCmdVolumeMetaAudit_Flags 锁定 `volume meta-audit` 参数。
func TestNewCmdVolumeMetaAudit_Flags(t *testing.T) {
	t.Parallel()
	cmd := NewCmdVolumeMetaAudit(nil, cli.IOStreams{})
	for _, name := range []string{"volume", "refresh", "wait", "interval"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("缺少 --%s flag", name)
		}
	}
}
