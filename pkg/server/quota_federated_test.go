// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// quota_federated_test.go —— 配额查询**联邦化**（片 5，设计 §1.5）。
//
// 设计口径（用户裁定 2026-10-16）：**用户配额按卷持有**（在各卷的配额层 ✓）；**用户视图 =
// 对「该用户可访问的卷」求和** ✓ ⇒ 逐卷读取 + 汇总（`AllowedVolumes(owner)` 复用既有 ACL 收敛 ✓）。
//
// **判别力设计** ✓：
//   - **跨卷求和**：两卷各 1000/2000 ⇒ 总 3000（若只读首卷/只读默认卷 ⇒ 红）；
//   - **ACL 收敛**：`ModeAllow` + 白名单 `{bob}` 的卷对 alice **不可访问** ⇒ 既**不出现**在
//     列表、也**不计入**总和（若只按「有配额层」遍历 ⇒ 红）；
//   - **未知 ≠ 0**：可访问但**无每用户权威**的卷 ⇒ `HasQuota=false` 且整体 `Partial=true`
//     （若把它当 0 计入 ⇒ 用例红）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/capacity"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// federatedFixture 装配 N 个卷，其中前 `withQuota` 个带**每用户配额层**（alice 各自独立 Scope）。
// 返回 Handlers + 各卷的 alice Scope（用于制造用量）。
func federatedFixture(t *testing.T, vols []volume.Volume, withQuota int, defaultName string) (*Handlers, map[string]*quota.Scope) {
	t.Helper()
	vs := registry.NewSet(vols, nil, nil, nil, defaultName)
	t.Cleanup(func() { _ = vs.Close() })
	h := &Handlers{volSet: vs}

	scopes := map[string]*quota.Scope{}
	for i, v := range vols {
		if i >= withQuota {
			break
		}
		aliceScope := quota.NewPool(1<<20).Scope("/tenant/alice", 1<<20)
		scopes[v.Name] = aliceScope
		wrapped := capacity.WrapStorageVolume(
			v.Name,
			bareBE{fs: syncpkg.NewLocalFS(t.TempDir(), nil)},
			func(owner string) *quota.Scope {
				if owner == "alice" {
					return aliceScope
				}
				return nil
			},
			quota.NewPool(1<<20),
			1<<20,
			0,
		)
		if err := vs.AttachExternal(v.Name, wrapped); err != nil {
			t.Fatalf("AttachExternal(%s): %v", v.Name, err)
		}
	}
	return h, scopes
}

// TestFederatedUserQuota_SumsAcrossAccessibleVolumes 钉住跨卷求和（用户视图 = 逐卷求和）。
func TestFederatedUserQuota_SumsAcrossAccessibleVolumes(t *testing.T) {
	t.Parallel()
	h, scopes := federatedFixture(t, []volume.Volume{{Name: "v1"}, {Name: "v2"}}, 2, "v1")
	scopes["v1"].Adjust(0, 1000)
	scopes["v2"].Adjust(0, 2000)

	got := h.federatedUserQuota("alice")
	if got.Used != 3000 {
		t.Fatalf("联邦用量=%d，want 3000（1000+2000；只读单卷则得 1000/2000）", got.Used)
	}
	if len(got.Volumes) != 2 {
		t.Fatalf("卷视图数=%d，want 2", len(got.Volumes))
	}
	if got.Partial {
		t.Fatal("两卷均有每用户权威 ⇒ Partial 必须 false")
	}
	// 逐卷明细必须可定位（诊断用）。
	byName := map[string]int64{}
	for _, v := range got.Volumes {
		byName[v.Volume] = v.Used
		if !v.HasQuota {
			t.Fatalf("卷 %s 应有每用户权威", v.Volume)
		}
	}
	if byName["v1"] != 1000 || byName["v2"] != 2000 {
		t.Fatalf("逐卷明细=%v，want v1=1000 v2=2000", byName)
	}
}

// TestFederatedUserQuota_ExcludesInaccessibleVolumes 钉住 **ACL 收敛**：不可访问的卷既不出现在
// 列表、也不计入总和（否则等于向用户泄露他人卷的用量 = doxxing ✗）。
func TestFederatedUserQuota_ExcludesInaccessibleVolumes(t *testing.T) {
	t.Parallel()
	vols := []volume.Volume{
		{Name: "v1"},
		{Name: "bob-only", ACL: volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{"bob": {}}}},
	}
	h, scopes := federatedFixture(t, vols, 2, "v1")
	scopes["v1"].Adjust(0, 1000)
	scopes["bob-only"].Adjust(0, 999999) // bob 的卷有大量用量

	got := h.federatedUserQuota("alice")
	if got.Used != 1000 {
		t.Fatalf("联邦用量=%d，want 1000（bob-only 卷对 alice 不可访问 ⇒ 不得计入）", got.Used)
	}
	for _, v := range got.Volumes {
		if v.Volume == "bob-only" {
			t.Fatal("不可访问的卷不得出现在 alice 的视图（ACL 收敛）")
		}
	}
}

// TestFederatedUserQuota_UnknownAuthorityIsPartialNotZero 钉住「**未知 ≠ 0**」：可访问但**无每用户
// 权威**的卷 ⇒ 该卷 `HasQuota=false`，且整体 `Partial=true`（数值不完整，禁静默当 0 ✗）。
func TestFederatedUserQuota_UnknownAuthorityIsPartialNotZero(t *testing.T) {
	t.Parallel()
	// 只给 v1 装配配额层；v2 可访问但**无**每用户权威。
	h, scopes := federatedFixture(t, []volume.Volume{{Name: "v1"}, {Name: "v2"}}, 1, "v1")
	scopes["v1"].Adjust(0, 1000)

	got := h.federatedUserQuota("alice")
	if !got.Partial {
		t.Fatal("存在无每用户权威的可访问卷 ⇒ Partial 必须 true（未知不得当 0）")
	}
	if got.Used != 1000 {
		t.Fatalf("联邦用量=%d，want 1000（有权威的卷仍须计入）", got.Used)
	}
	var found bool
	for _, v := range got.Volumes {
		if v.Volume == "v2" {
			found = true
			if v.HasQuota {
				t.Fatal("v2 无每用户权威 ⇒ HasQuota 必须 false")
			}
			if v.Used != 0 {
				t.Fatalf("v2 used=%d，want 0（值可为 0，但 HasQuota=false 表达未知）", v.Used)
			}
		}
	}
	if !found {
		t.Fatal("可访问但无权威的卷仍须出现在视图中（表达未知，而非省略）")
	}
}

// TestFederatedUserQuota_UnlimitedVolumeMakesTotalUnlimited 钉住「任一卷不限 ⇒ 总量不限」：
// 上限为 0 表示**不限**（不是「零配额」✗）⇒ 求和时不得把 0 当普通值相加。
func TestFederatedUserQuota_UnlimitedVolumeMakesTotalUnlimited(t *testing.T) {
	t.Parallel()
	pool1 := quota.NewPool(1 << 20)
	pool2 := quota.NewPool(1 << 20)
	alice1 := pool1.Scope("/tenant/alice", 4096)
	alice2 := pool2.Scope("/tenant/alice", 0) // 0 = 不限
	vs := registry.NewSet([]volume.Volume{{Name: "v1"}, {Name: "v2"}}, nil, nil, nil, "v1")
	t.Cleanup(func() { _ = vs.Close() })
	h := &Handlers{volSet: vs}
	for _, item := range []struct {
		name  string
		scope *quota.Scope
	}{{"v1", alice1}, {"v2", alice2}} {
		wrapped := capacity.WrapStorageVolume(
			item.name,
			bareBE{fs: syncpkg.NewLocalFS(t.TempDir(), nil)},
			func(owner string) *quota.Scope {
				if owner == "alice" {
					return item.scope
				}
				return nil
			},
			quota.NewPool(1<<20),
			1<<20,
			0,
		)
		if err := vs.AttachExternal(item.name, wrapped); err != nil {
			t.Fatalf("AttachExternal(%s): %v", item.name, err)
		}
	}

	got := h.federatedUserQuota("alice")
	if !got.Unlimited {
		t.Fatalf("任一卷不限（上限 0）⇒ Unlimited 必须 true（cap=%d）", got.Capacity)
	}
	if got.Partial {
		t.Fatal("两卷均有每用户权威 ⇒ Partial 必须 false")
	}
}

// TestUserQuotaHandler_OwnerFromAuthNotRequest 钉住**红线**：`GET /api/quota` 的 owner 恒取自
// **认证**（`ActorFrom`）—— 请求参数 `?owner=` **不得**生效（否则任意用户可窥探他人配额 ✗）。
func TestUserQuotaHandler_OwnerFromAuthNotRequest(t *testing.T) {
	t.Parallel()
	h, scopes := federatedFixture(t, []volume.Volume{{Name: "v1"}}, 1, "v1")
	scopes["v1"].Adjust(0, 1234)

	req := httptest.NewRequest(http.MethodGet, "/api/quota?owner=bob", nil)
	req = req.WithContext(withActor(req.Context(), "alice"))
	rec := httptest.NewRecorder()
	h.userQuotaHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d，want 200", rec.Code)
	}
	var got federatedQuotaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("JSON 解析失败: %v（body=%s）", err, rec.Body.String())
	}
	if got.Owner != "alice" {
		t.Fatalf("owner=%q，want alice（请求参数 ?owner=bob **不得**生效）", got.Owner)
	}
	if got.Used != 1234 {
		t.Fatalf("used=%d，want 1234", got.Used)
	}
}
