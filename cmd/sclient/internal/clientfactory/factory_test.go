// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clientfactory_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestFactory_Constructor(t *testing.T) {
	// New 和 NewMock 返回 Factory 接口 — 编译期检查构造函数签名正确
	_ = clientfactory.New("", nil)
	_ = clientfactory.NewMock(nil, nil)
}

func TestFactoryLazy_GetProvider(t *testing.T) {
	// 验证延迟获取：init 时传入 nil provider，PersistentPreRunE 后提供真实值
	var called bool
	_ = clientfactory.New("", func() clientfactory.CfgBinder {
		called = true
		return nil
	})
	if called {
		t.Error("CfgBinder function should not be called during New()")
	}
}

func TestMockFactory_NilService(t *testing.T) {
	cmd := &cobra.Command{}
	factory := clientfactory.NewMock(nil, nil)
	svc, err := factory.NewClient(cmd)
	if err != nil {
		t.Fatalf("NewClient should not error: %v", err)
	}
	if svc != nil {
		t.Error("NewClient should return nil service as-is")
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

// mockCfgBinder 实现 CfgBinder 接口，用于生产 Factory 测试。
type mockCfgBinder struct {
	data map[string]any
}

func (m *mockCfgBinder) BindPFlag(key string, flag *pflag.Flag) {}

func (m *mockCfgBinder) Unmarshal(obj any) error {
	b, err := json.Marshal(m.data)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, obj)
}

func TestFactory_NewClient_NilProvider(t *testing.T) {
	f := clientfactory.New("test.yaml", func() clientfactory.CfgBinder { return nil })
	cmd := &cobra.Command{}
	svc, err := f.NewClient(cmd)
	if err == nil {
		t.Fatal("expected error for nil provider")
	}
	if svc != nil {
		t.Fatal("expected nil service when provider is nil")
	}
}

func TestFactory_NewClient_WithConfig(t *testing.T) {
	binder := &mockCfgBinder{
		data: map[string]any{
			"server_url": "http://127.0.0.1:18083",
		},
	}
	f := clientfactory.New("test.yaml", func() clientfactory.CfgBinder { return binder })
	cmd := &cobra.Command{}
	cmd.Flags().String("server", "", "")
	svc, err := f.NewClient(cmd)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
}

func TestFactory_NewClient_FlagOverridesServer(t *testing.T) {
	binder := &mockCfgBinder{
		data: map[string]any{
			"server_url": "http://original:8080",
		},
	}
	f := clientfactory.New("test.yaml", func() clientfactory.CfgBinder { return binder })
	cmd := &cobra.Command{}
	cmd.Flags().String("server", "", "")
	cmd.Flags().String("auth-token", "", "")
	cmd.Flags().Int64("chunk-size", 0, "")
	if err := cmd.Flags().Set("server", "http://override:8080"); err != nil {
		t.Fatal(err)
	}
	svc, err := f.NewClient(cmd)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
}

func TestFactory_NewClient_WithAuthToken(t *testing.T) {
	binder := &mockCfgBinder{
		data: map[string]any{
			"server_url": "http://127.0.0.1:18083",
			"auth_token": "test-token",
		},
	}
	f := clientfactory.New("test.yaml", func() clientfactory.CfgBinder { return binder })
	cmd := &cobra.Command{}
	cmd.Flags().String("server", "", "")
	cmd.Flags().String("auth-token", "", "")
	svc, err := f.NewClient(cmd)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
}
