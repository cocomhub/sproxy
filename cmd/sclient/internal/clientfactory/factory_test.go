// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clientfactory_test

import (
	"errors"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

func TestFactory_Interface(t *testing.T) {
	// New 返回 Factory 接口 — 编译期检查构造函数签名正确
	_ = clientfactory.New("", nil)
	_ = clientfactory.NewMock(nil, nil)
}

func TestMockFactory_Success(t *testing.T) {
	cmd := &cobra.Command{}
	factory := clientfactory.NewMock(nil, nil)
	svc, err := factory.NewClient(cmd)
	if err != nil {
		t.Fatalf("NewClient should not error: %v", err)
	}
	if svc != nil {
		t.Error("NewClient should return the mock service as-is")
	}
}

func TestMockFactory_Error(t *testing.T) {
	expectedErr := errors.New("config error")
	factory := clientfactory.NewMock(nil, expectedErr)

	cmd := &cobra.Command{}
	_, err := factory.NewClient(cmd)
	if err != expectedErr {
		t.Errorf("expected error %v, got %v", expectedErr, err)
	}
}

func TestMockFactory_WithService(t *testing.T) {
	// 确保 mockFactory 可以返回一个非 nil 的 Service
	mockSvc := &mockService{}
	factory := clientfactory.NewMock(mockSvc, nil)

	cmd := &cobra.Command{}
	svc, err := factory.NewClient(cmd)
	if err != nil {
		t.Fatalf("NewClient should not error: %v", err)
	}
	if svc != mockSvc {
		t.Error("NewClient should return the mock service as-is")
	}
}

// mockService 实现 client.Service 接口，用于测试。
type mockService struct {
	client.Service
}
