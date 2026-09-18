// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clientfactory_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/certmgr"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// genDirectCACert 生成本测试用的自签服务端证书文件并返回路径（同 auto_tls 形态）。
func genDirectCACert(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	certFile := filepath.Join(dir, "srv-cert.pem")
	keyFile := filepath.Join(dir, "srv-key.pem")
	if err := certmgr.GenerateSelfSignedCert(certFile, keyFile); err != nil {
		t.Fatalf("生成自签证书失败: %v", err)
	}
	return certFile
}

// newDirectCmd 构造 factory.NewClient 直连模式（非 xfer）所需的 flag 最小集。
func newDirectCmd(t *testing.T, sets map[string]string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.Flags().String("server", "", "")
	cmd.Flags().String("xfer", "", "")
	cmd.Flags().String("hub", "", "")
	cmd.Flags().String("ca-file", "", "")
	cmd.Flags().Bool("insecure", false, "")
	cmd.Flags().Bool("allow-transport-fallback", false, "")
	cmd.Flags().String("chunk-size", "", "")
	cmd.Flags().String("volume", "", "")
	cmd.Flags().String("access-key", "", "")
	cmd.Flags().String("access-key-secret", "", "")
	cmd.Flags().String("access-key-id", "", "")
	cmd.Flags().String("client-cert", "", "")
	cmd.Flags().String("client-key", "", "")
	cmd.Flags().Bool("client-cert-allow-missing", false, "")
	for k, v := range sets {
		if err := cmd.Flags().Set(k, v); err != nil {
			t.Fatalf("set flag %s=%s: %v", k, v, err)
		}
	}
	return cmd
}

// TestFactory_NewClient_DirectCAWired 验证：直连模式（非 xfer）下 --ca-file <自签CA>
// 经 factory.NewClient 装配到 FileClient（不报错、无 InitError——CA 文件被消费）。
// RootCAs 的具体装配由 pkg/client 内部包测试断言（cafile_test.go）。
func TestFactory_NewClient_DirectCAWired(t *testing.T) {
	t.Parallel()
	certFile := genDirectCACert(t)
	binder := &mockCfgBinder{data: map[string]any{"server_url": "https://127.0.0.1:18083"}}
	f := clientfactory.New("test.yaml", func() clientfactory.CfgBinder { return binder })

	svc, err := f.NewClient(newDirectCmd(t, map[string]string{"ca-file": certFile}))
	if err != nil {
		t.Fatalf("NewClient（--ca-file 直连）应成功: %v", err)
	}
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
	if svc.InitError() != nil {
		t.Fatalf("--ca-file 有效 CA 不应有 InitError: %v", svc.InitError())
	}
}

// TestFactory_NewClient_DirectInsecureWired 验证：直连模式 --insecure 装配
// InsecureSkipVerify=true（直连面安全 flag 生效，不报错）。
func TestFactory_NewClient_DirectInsecureWired(t *testing.T) {
	t.Parallel()
	binder := &mockCfgBinder{data: map[string]any{"server_url": "https://127.0.0.1:18083"}}
	f := clientfactory.New("test.yaml", func() clientfactory.CfgBinder { return binder })

	svc, err := f.NewClient(newDirectCmd(t, map[string]string{"insecure": "true"}))
	if err != nil {
		t.Fatalf("NewClient（--insecure 直连）应成功: %v", err)
	}
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
	if svc.InitError() != nil {
		t.Fatalf("--insecure 直连不应有 InitError: %v", svc.InitError())
	}
}

// TestFactory_NewClient_DirectCAAndInsecureMutuallyExclusive 验证：直连面 --ca-file
// 与 --insecure 互斥（fail-closed，对齐 xfer 面语义）。
func TestFactory_NewClient_DirectCAAndInsecureMutuallyExclusive(t *testing.T) {
	t.Parallel()
	certFile := genDirectCACert(t)
	binder := &mockCfgBinder{data: map[string]any{"server_url": "https://127.0.0.1:18083"}}
	f := clientfactory.New("test.yaml", func() clientfactory.CfgBinder { return binder })

	_, err := f.NewClient(newDirectCmd(t, map[string]string{"ca-file": certFile, "insecure": "true"}))
	if err == nil {
		t.Fatal("直连面 ca-file 与 insecure 互斥应报错（fail-closed）")
	}
	if !strings.Contains(err.Error(), "ca-file") && !strings.Contains(err.Error(), "insecure") {
		t.Fatalf("错误应提及 ca-file/insecure, got: %v", err)
	}
}

// compile-time: 保证 client 包被引用（InitError 断言用）。
var _ = client.NewFileClient
