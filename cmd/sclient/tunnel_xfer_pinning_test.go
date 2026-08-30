// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrg/xdg"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/sclientcfg"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/spf13/cobra"
)

// H-1 CLI 级 pinning 测试：验证 `sclient tunnel --xfer tcp` 走真实 xfer/mux 隧道
// （含 ECDH 握手 + 身份交换 + 指纹 pin 校验），配置 peer_fingerprints 后 pin 匹配通、
// 不匹配拒——证明身份 pinning 在真实生产 CLI 路径上端到端生效（非库层死代码）。
//
// 测试拓扑：
//
//	[服务端]  TCP xfer 监听 127.0.0.1:0 → mux(RoleListener) → tunnel.NewTunnel(身份B, pin[A])
//	                ↕ 真实 TCP 回环 + ECDH 握手 + 身份交换
//	[客户端]  sclient tunnel --xfer tcp --hub <addr>（真实 factory 构造）
//	            → WithXfer("tcp") → TunnelDo → doRequestViaXfer → xfer 握手(pin 校验)
//
// 身份与 pin 均通过真实配置路径加载：
//   - 本端身份：XDG 配置目录 sproxy/identity.json（LoadIdentityOptional 懒加载）
//   - 对端 pin：配置文件 peer_fingerprints（client.Config.Validate 阶段校验）
//   - 隧道密钥：access_key/access_key_secret 派生（无密钥不握手，pinning 不生效）
func TestTunnelCmd_Xfer_Pinning_Enforced(t *testing.T) {
	idA, err := tunnel.GenerateIdentity() // 客户端身份
	if err != nil {
		t.Fatal(err)
	}
	idB, err := tunnel.GenerateIdentity() // 服务端身份
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := tunnel.GenerateIdentity() // 无关身份（用于 mismatch）
	if err != nil {
		t.Fatal(err)
	}

	key := mustDeriveTunnelKey(t)

	t.Run("pin_match", func(t *testing.T) {
		addr, cancel, cleanup := startPinningTunnelServer(t, key, idB, []string{idA.Fingerprint()})
		defer cleanup()
		defer cancel()

		ios, outFile := tunnelCLIIO(t)
		cmd := newTunnelXferCmd(t, addr, idB.Fingerprint(), idA, ios)

		cmd.SetArgs([]string{
			"--xfer", "tcp",
			"--output", outFile,
			"-d", "hello-pin-match",
			"http://127.0.0.1:1/echo",
		})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("pin 匹配时隧道请求应成功: %v", err)
		}
		body, rErr := os.ReadFile(outFile)
		if rErr != nil {
			t.Fatalf("读取输出文件失败: %v", rErr)
		}
		if string(body) != "hello-pin-match" {
			t.Fatalf("响应体不匹配: 期望 %q, 实际 %q", "hello-pin-match", string(body))
		}
	})

	t.Run("pin_mismatch_rejected", func(t *testing.T) {
		addr, cancel, cleanup := startPinningTunnelServer(t, key, idB, nil)
		defer cleanup()
		defer cancel()

		ios, _ := tunnelCLIIO(t)
		cmd := newTunnelXferCmd(t, addr, wrong.Fingerprint(), idA, ios)

		cmd.SetArgs([]string{
			"--xfer", "tcp",
			"--output", filepath.Join(t.TempDir(), "out.bin"),
			"-d", "hello",
			"http://127.0.0.1:1/echo",
		})
		err := cmd.Execute()
		if err == nil {
			t.Fatal("pin 不匹配时隧道请求应失败（fail-closed）")
		}
		if !strings.Contains(err.Error(), "对端身份指纹不匹配") {
			t.Fatalf("错误应包含指纹不匹配提示, 实际: %v", err)
		}
	})
}

// mustDeriveTunnelKey 派生测试隧道密钥（客户端 factory 与服务端 tunnel 共用，
// 与 sclient access_key/access_key_secret 派生规则一致）。
func mustDeriveTunnelKey(t *testing.T) []byte {
	t.Helper()
	ak := testutil.TestAccessKey()
	sk := testutil.TestKey()
	mesh := tunnel.AccessKeyMesh(ak)
	key, err := tunnel.DeriveTunnelKey(sk, mesh)
	if err != nil {
		t.Fatalf("派生隧道密钥失败: %v", err)
	}
	return key
}

// startPinningTunnelServer 启动一个 TCP xfer 隧道服务端（listener 侧），
// 用身份 id 与 serverPins 构造 Tunnel，handler 回显请求体。
// 返回 listener 地址、取消函数与清理函数。
func startPinningTunnelServer(t *testing.T, key []byte, id *tunnel.Identity, serverPins []string) (string, context.CancelFunc, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	tp := xfer.Get("tcp")
	if tp == nil {
		t.Fatal("TCP xfer 传输层未注册")
	}
	ln, err := tp.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatalf("TCP 监听失败: %v", err)
	}
	addrLn, ok := ln.(interface{ Addr() net.Addr })
	if !ok {
		cancel()
		_ = ln.Close()
		t.Fatal("TCP listener 未暴露 Addr()")
	}
	addr := addrLn.Addr().String()

	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		conn, aErr := ln.Accept(ctx)
		if aErr != nil {
			return
		}
		m := mux.New(conn, mux.RoleListener)
		tun := tunnel.NewTunnel(m, key,
			tunnel.WithIdentity(id),
			tunnel.WithPeerFingerprints(serverPins),
		)
		// Serve 同步执行 ECDH 握手 + accept 循环；ctx 取消时返回。
		_ = tun.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(w, r.Body)
		}))
		_ = m.Close()
	}()

	cleanup := func() {
		cancel()
		_ = ln.Close()
		select {
		case <-serveDone:
		case <-time.After(5 * time.Second):
			t.Log("隧道服务端未在超时内退出")
		}
	}
	return addr, cancel, cleanup
}

// newTunnelXferCmd 构造真实 factory 的 sclient tunnel 命令：
// 配置 access_key/access_key_secret（派生隧道密钥使握手执行）、hub_url（xfer 拨号地址）、
// peer_fingerprints（对端 pin）；本端身份写入 XDG 配置目录。
func newTunnelXferCmd(t *testing.T, hubAddr, serverFP string, localID *tunnel.Identity, ios cli.IOStreams) *cobra.Command {
	t.Helper()
	dir := t.TempDir()

	// 本端身份：写入 XDG 配置目录 sproxy/identity.json。
	oldConfigHome := xdg.ConfigHome
	xdg.ConfigHome = dir
	t.Cleanup(func() { xdg.ConfigHome = oldConfigHome })
	if err := tunnel.SaveIdentity(localID, filepath.Join(dir, "sproxy", "identity.json")); err != nil {
		t.Fatalf("写入身份文件失败: %v", err)
	}

	// 客户端配置：access_key/secret 派生隧道密钥，peer_fingerprints 指定对端 pin。
	cfg := filepath.Join(dir, "sclient.yaml")
	cfgBody := "server_url: https://127.0.0.1:1\n" +
		"access_key: " + testutil.TestAccessKey() + "\n" +
		"access_key_secret: " + testutil.TestKey() + "\n" +
		"hub_url: " + hubAddr + "\n" +
		"peer_fingerprints:\n  - " + serverFP + "\n"
	if err := os.WriteFile(cfg, []byte(cfgBody), 0600); err != nil {
		t.Fatalf("写入配置文件失败: %v", err)
	}

	provider := sclientcfg.New(cfg)
	factory := clientfactory.New(cfg, func() clientfactory.CfgBinder { return provider })
	cmd := NewCmdTunnel(factory, ios)
	cmd.Flags().String("output", "", "指定下载文件的输出路径")
	return cmd
}

// tunnelCLIIO 返回丢弃输出的 IOStreams 与临时输出文件路径。
func tunnelCLIIO(t *testing.T) (cli.IOStreams, string) {
	t.Helper()
	return cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, filepath.Join(t.TempDir(), "out.bin")
}
