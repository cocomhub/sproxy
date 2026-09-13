// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// mesh_status_e2e_test.go —— 跨节点（mesh）状态卡的 Web UI 真交互 E2E（W2）。
//
// 断言链条：Hub tab 点击 → 触发 GET /api/mesh/status（W1 新增端点）→ 200 + 卡片渲染出
// 「面 / pin 数 / 节点角色」三行。用**配置态**驱动（e2e harness 不起 listener）：`addr` 取配置值、
// `node.running=false`（未启动角色）——正是「配置启用但未运行」这一最需要被看见的状态。

import (
	"net/http"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/mxschmitt/playwright-go"
)

// e2eMeshPin 是测试用 Ed25519 指纹（规范形：sha256:<64 hex>）。
const e2eMeshPin = "sha256:3f2a1b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708"

// TestMeshStatus_RendersCard 钉住状态卡渲染与端点可达。
func TestMeshStatus_RendersCard(t *testing.T) {
	baseURL, _, cleanup := testServerCfg(t, func(cfg *server.Config) {
		// 两个面都启用（只读/写），地址显式指定以便断言。
		cfg.RemoteRead.Enabled = true
		cfg.RemoteRead.Listen = "127.0.0.1:19000"
		cfg.RemoteWrite.Enabled = true
		cfg.RemoteWrite.Listen = "127.0.0.1:19001"
		// 一条 mesh_readers 条目 ⇒ 读面 pin=1；scope=rw ⇒ 写面 pin=1。
		cfg.Volumes = []server.VolumeConfig{{
			Name: "main", Root: cfg.StorageRoot,
			ACL: &server.VolumeACLConfig{
				Mode:   server.VolumeACLAllow,
				Owners: []string{"anonymous"},
				MeshReaders: []server.VolumeMeshReaderConfig{{
					Node: "nodeA", Fingerprint: e2eMeshPin, Owner: "anonymous", Scope: "rw",
				}},
			},
		}}
		// 节点角色：配置启用但不启动 ⇒ 卡片必须显示「未运行」。
		cfg.Mesh.Node.Enabled = true
		cfg.Mesh.Node.NodeID = "node-e2e"
		cfg.Mesh.Node.WebRTC = true
		cfg.Mesh.HubURL = "https://hub.example.com:18083"
		cfg.Mesh.NodeID = "node-e2e-signaling"
	})
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	if err := page.Locator("#stats-btn").Click(); err != nil {
		t.Fatalf("click stats-btn: %v", err)
	}
	if err := waitLoc(page, "#stats-modal", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("stats-modal 未显示: %v", err)
	}

	// 点击 Hub tab → 触发 GET /api/mesh/status（与 /api/hub/* 并行请求）。
	resp, err := page.ExpectResponse("**/api/mesh/status", func() error {
		return page.Locator("#hub-tab").Click()
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("点击 Hub tab 未触发 GET /api/mesh/status: %v", err)
	}
	if got := resp.Status(); got != http.StatusOK {
		t.Fatalf("GET /api/mesh/status status=%d want 200", got)
	}

	// 卡片必须渲染：标题 + 两个面（地址/pin）+ 节点角色（未运行）。
	for _, want := range []string{"跨节点（mesh）", "只读面", "127.0.0.1:19000", "pin 1",
		"写面", "127.0.0.1:19001", "node-e2e", "未运行"} {
		waitTextVisible(t, page, "#hub-panel", want, 8000)
	}
}
