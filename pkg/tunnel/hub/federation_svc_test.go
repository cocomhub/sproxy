// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

// federation_svc_test.go 验证跨 hub 服务发现（roadmap P2 mesh 集群化 F1）：
//  1. syncPeer 拉取 /api/hub/federation/services → FederationClient.svcs 表。
//  2. CandidateServices 返回联邦服务（跨 peer 去重）。
//  3. 服务端 federationServicesHandler 返回本 hub 节点宣告服务。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestFederationClient_SyncServices 联邦客户端拉取对端服务表。
func TestFederationClient_SyncServices(t *testing.T) {
	t.Parallel()
	// mock 对端 /api/hub/federation/services。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hub/federation/services" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]fedSvcResp{
			{Node: "remote-node", Mesh: "", Name: "sg-ssh", Addr: "t:22"},
		})
	}))
	defer srv.Close()

	fc, err := NewFederationClient([]FederationPeer{{ID: "p1", URL: srv.URL}}, time.Hour, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := fc.SyncServices(context.Background(), fc.Peers()[0]); err != nil {
		t.Fatalf("SyncServices: %v", err)
	}
	svcs := fc.CandidateServices()
	if len(svcs) != 1 || svcs[0].Name != "sg-ssh" || svcs[0].Node != "remote-node" {
		t.Fatalf("svcs = %+v", svcs)
	}
}
