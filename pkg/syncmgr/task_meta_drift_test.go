// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncmgr

// task_meta_drift_test.go 钉住 SyncTaskMeta 投影**不得静默漏字段**。
//
// 背景（实测踩到）：W1 给 `SyncTask` 加了 `Kind`/`Transport`/`Carriers`，但 `Manager.List` 返回的是
// **手写投影** `SyncTaskMeta`，投影里没加 ⇒ `GET /api/sync/tasks`（Web UI 的数据源）不含这三项
// ⇒ 载体徽标在真实服务端数据下**永远显示不出来**。JS 单测用合成对象、Go e2e 只查了状态卡，
// 两道都没抓到，最后由「真浏览器 + 真数据」发现。
//
// 判据（不锁死实现，只禁「静默」）：`SyncTask` 的每个对外 JSON 字段，必须**要么**出现在
// `SyncTaskMeta`，**要么**在下面的显式排除表里登记并写明理由。新增字段忘同步投影 ⇒ 红。
// 排除表让「故意不进列表」（如 `results` 太重）成为**可审查的显式决定**，而不是无声遗漏。

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// listProjectionExclusions 是**故意**不进入 `List` 投影的字段（key = JSON 名）及理由。
//
// 这些字段在 `GET /api/sync/tasks` 里不返回：列表是「任务元信息 + 进度」视图，单任务详情
// （`GET /api/sync/tasks/{id}`）才返回完整快照。改这里等于改 API 契约，必须显式。
var listProjectionExclusions = map[string]string{
	"recursive":       "创建参数，列表不需要（详情里有）",
	"sync_empty_dirs": "创建参数，列表不需要（详情里有）",
	"include":         "创建参数，列表不需要（详情里有）",
	"exclude":         "创建参数，列表不需要（详情里有）",
	"follow_symlinks": "创建参数，列表不需要（详情里有）",
	"conflict_policy": "创建参数，列表不需要（详情里有）",
	"results":         "逐文件结果可能很大，列表刻意不返回（详情里有）",
}

// jsonFieldNames 返回结构体对外 JSON 字段名 → Go 字段名（跳过 `json:"-"` 与未导出字段）。
func jsonFieldNames(t *testing.T, v any) map[string]string {
	t.Helper()
	rt := reflect.TypeOf(v)
	out := map[string]string{}
	for f := range rt.Fields() {
		if f.PkgPath != "" { // 未导出
			continue
		}
		name := f.Tag.Get("json")
		if idx := strings.IndexByte(name, ','); idx >= 0 {
			name = name[:idx]
		}
		switch name {
		case "-":
			continue
		case "":
			name = f.Name
		}
		out[name] = f.Name
	}
	return out
}

// TestSyncTaskMetaCoversTaskJSONFields 是**漂移门禁**：List 投影必须覆盖或显式排除 SyncTask 的每个对外字段。
func TestSyncTaskMetaCoversTaskJSONFields(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	taskFields := jsonFieldNames(t, SyncTask{})
	metaFields := jsonFieldNames(t, SyncTaskMeta{})

	var missing []string
	for name, goField := range taskFields {
		if _, ok := metaFields[name]; ok {
			continue
		}
		if _, excluded := listProjectionExclusions[name]; excluded {
			continue
		}
		missing = append(missing, goField+" (json:"+name+")")
	}
	if len(missing) > 0 {
		t.Fatalf("SyncTask 的对外字段既不在 SyncTaskMeta、也没在排除表登记：%v\n"+
			"→ Manager.List 返回 SyncTaskMeta 投影，静默漏字段会让 `GET /api/sync/tasks`（Web UI 数据源）缺数据。\n"+
			"处理：把字段同步加进 SyncTaskMeta 并在 List 中赋值；若确属「列表刻意不返回」，加进本文件"+
			"listProjectionExclusions 并写明理由。", missing)
	}
	// 反向检查：排除表里不该留下已经不存在的字段名（防表腐烂）。
	for name := range listProjectionExclusions {
		if _, ok := taskFields[name]; !ok {
			t.Errorf("排除表登记了 SyncTask 已不存在的字段 %q（请删除该条目）", name)
		}
	}
}

// TestListCarriesCarrierVisibility 是行为级回归：List 必须真的带出 W1 的载体三元组
// （`kind` 恒有；mesh 任务还有 `transport`；终态回填的 `carriers` 也要能穿过去）。
func TestListCarriesCarrierVisibility(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	remote := meshRemote("r-mesh")
	remote.Transport = "relay" // 显式声明，便于断言「声明真的透传到列表 JSON」
	m := newTestManager(t, nil, []RemoteConfig{remote}, nil, nil)
	task, _, err := m.SubmitAndStart(CreateRequest{
		Direction: "push", Remote: "r-mesh", Src: ".", Dst: ".",
		ConflictPolicy: "overwrite",
	})
	if err != nil {
		t.Fatalf("SubmitAndStart: %v", err)
	}
	// 模拟终态回填（真实路径由执行器经 CarrierReporter 上报；这里直接置入以验证投影是否搬运）。
	m.mu.Lock()
	if cur := m.tasks[task.ID]; cur != nil {
		cur.Carriers = map[string]int{"relay": 2, "webrtc": 1}
	}
	m.mu.Unlock()

	metas := m.List("")
	var got *SyncTaskMeta
	for i := range metas {
		if metas[i].ID == task.ID {
			got = &metas[i]
		}
	}
	if got == nil {
		t.Fatalf("List 未返回任务 %s", task.ID)
	}
	if got.Kind != string(RemoteKindMesh) {
		t.Errorf("List 投影 Kind=%q want %q", got.Kind, RemoteKindMesh)
	}
	// 注意：远端未显式声明 transport 时任务快照为空串（= auto，语义由 UI/拨号器按缺省 auto 处理）；
	// 用例里显式配了 relay，故这里要求原样透传——这正是「声明」要能被 UI 看到的原因。
	if got.Transport != "relay" {
		t.Errorf("List 投影 Transport=%q want relay", got.Transport)
	}

	// 客户端（含 Web UI）看到的就是这段 JSON。
	b, err := json.Marshal(*got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"kind", "transport", "carriers"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("List 投影 JSON 缺少 %q（Web UI 载体徽标依赖它）：%s", key, string(b))
		}
	}
	// 深拷贝：改返回值不得影响内部任务（与 Include/Exclude/Results 同原则）。
	got.Carriers["relay"] = 99
	again := m.List("")
	for _, meta := range again {
		if meta.ID == task.ID && meta.Carriers["relay"] != 2 {
			t.Fatalf("List 返回的 Carriers 必须是深拷贝, got %v", meta.Carriers)
		}
	}
}
