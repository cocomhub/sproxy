// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// verify_iterate_test.go 钉住巡检走**分批遍历**（`checksum.Iterator`）而不是全量 `GetAll()`：
// 大库下 GetAll 会复制整份台账（10 万条 ≈ 6MB），巡检本身是低频但大内存动作。

import (
	"maps"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/checksum"
)

type iterSpyStore struct {
	entries     map[string]string
	iterCalls   atomic.Int64
	getAllCalls atomic.Int64
}

func (s *iterSpyStore) Get(k string) (string, bool) { v, ok := s.entries[k]; return v, ok }
func (s *iterSpyStore) Set(k, v string)             { s.entries[k] = v }
func (s *iterSpyStore) Delete(k string)             { delete(s.entries, k) }
func (s *iterSpyStore) Rename(from, to string) {
	s.entries[to] = s.entries[from]
	delete(s.entries, from)
}
func (s *iterSpyStore) DeletePrefix(prefix string) { _ = prefix }
func (s *iterSpyStore) GetAll() map[string]string {
	s.getAllCalls.Add(1)
	return maps.Clone(s.entries)
}
func (s *iterSpyStore) Len() int { return len(s.entries) }
func (s *iterSpyStore) Iterate(_ int, fn func(key, checksum string) bool) {
	s.iterCalls.Add(1)
	for k, v := range s.entries {
		if !fn(k, v) {
			return
		}
	}
}

// TestVerifyOneOwner_UsesIterator：巡检必须走 Iterate，不得调用 GetAll。
//
// 变异：verifyOneOwner 改回 `cs.GetAll()` ⇒ getAllCalls=1 ⇒ 红。
func TestVerifyOneOwner_UsesIterator(t *testing.T) {
	t.Parallel()
	h := newAssemblyTestHandlers(t, t.TempDir())
	spy := &iterSpyStore{entries: map[string]string{"user/a.txt": "sum"}}
	h.checksumStores = map[string]checksum.ChecksumStoreIface{"alice": spy}

	var rep verifyReport
	h.verifyService().VerifyOneOwner("alice", &rep, "")

	if got := spy.iterCalls.Load(); got != 1 {
		t.Fatalf("应走分批遍历 Iterate，calls=%d", got)
	}
	if got := spy.getAllCalls.Load(); got != 0 {
		t.Fatalf("巡检不应调用 GetAll（全量拷贝），calls=%d", got)
	}
	if rep.Total != 1 {
		t.Fatalf("应核对 1 条，got total=%d rep=%+v", rep.Total, rep)
	}
}
