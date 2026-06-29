// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package provider_test

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/provider"
)

// stubProvider 是一个简单的 stub，实现 Provider 和 Refresher 接口。
type stubProvider struct {
	err error
}

func (s *stubProvider) Unmarshal(obj any) error {
	if s.err != nil {
		return s.err
	}
	// stub 不做实际解码，只验证接口存在
	return nil
}

func (s *stubProvider) Refresh() error {
	return s.err
}

// compile-time interface checks
var _ provider.Provider = (*stubProvider)(nil)
var _ provider.Refresher = (*stubProvider)(nil)

func TestProviderInterface(t *testing.T) {
	p := &stubProvider{}
	// 验证 stubProvider 可以赋值给 Provider 接口
	var prov provider.Provider = p
	_ = prov
}

func TestRefresherInterface(t *testing.T) {
	p := &stubProvider{}
	// 验证 stubProvider 可以赋值给 Refresher 接口
	var ref provider.Refresher = p
	_ = ref
}
