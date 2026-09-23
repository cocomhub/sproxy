// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

// federation_race_test.go 验证 NewFederationClient 并发安全（不共享可变 Transport）：
// 并发构造多个客户端（各含 TLS 定制）→ -race 无 DATA RACE。

import (
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestFederationClient_ConcurrentNew 并发构造不 race。
func TestFederationClient_ConcurrentNew(t *testing.T) {
	t.Parallel()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, err := NewFederationClient([]FederationPeer{
				{ID: "p1", URL: "http://127.0.0.1:1", InsecureSkipVerify: true},
				{ID: "p2", URL: "https://127.0.0.1:2"},
			}, time.Second, time.Second, testutil.DiscardLogger())
			if err != nil {
				t.Errorf("NewFederationClient: %v", err)
			}
		})
	}
	wg.Wait()
}
