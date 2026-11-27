// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// backend_capability_forward_test.go 是「**后端可选能力必须被容量包装器转发**」的门禁（R61）。
//
// 背景（同类错误已犯 3 次，必须机械化）：
//   - #1320：Settings 三能力（`SettingsProvider`/`SettingsWriter`/`SetSettingsStore`）加了接口但
//     `capacity.Backend` 不转发 ⇒ 装配层只拿得到包装器 ⇒ 新能力在生产路径上**完全不可达**；
//   - #1325：`SettingsReloader` 同样漏转发 ⇒ 「重载失败可见」的修复在生产上等于没修；
//   - #1333：`IndexRefresher` 漏转发 ⇒ `refresh` 走早返回，索引重建与设置重载双双不执行。
//
// 判据：`pkg/volume/registry` 里每个「**后端可实现的单方法能力接口**」都必须在
// `pkg/volume/capacity.Backend` 上有一个同名转发方法（或显式列入 `notForwarded` 并写明理由）。
//
// 为什么不能只靠人工清单：漏掉一处的表现是「功能静默无效」（不报错、不红、测试全绿），
// 无法从行为侧发现。故：
//  1. **机械发现**（扫 registry 源码的 interface 声明，不依赖手工维护的方法表）；
//  2. **强制归类**（发现的新接口既没转发、也没进 `notForwarded` ⇒ 红）；
//  3. **双侧护栏**（发现数/实际校验数低于下限 ⇒ 红，防扫描面被改窄或判定空转）。

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// notForwarded 是「不需要/不应该由容量包装器转发」的接口及其理由（每条都必须有）。
var notForwarded = map[string]string{
	// Schema 由 registry 构造**新实例**读取（backendSchema → NewBackend → be.(SchemaProvider)），
	// 不经过装配期的包装器句柄 ⇒ 转发反而读不到（包装器持有的实例 Extra 为空、无法构造）。
	"SchemaProvider": "由 backendSchema 构造新实例读取，不经装配句柄",
	// SettingsStore 是**存储**接口（由 StateStore 实现），不是后端能力：后端只接收它
	// （SetSettingsStore 参数），从不实现它。
	"SettingsStore": "存储侧接口（StateStore 实现），非后端能力",
	// VolumeCounter 由配额池实现（`QuotaVolume` 侧），后端不感知。
	"VolumeCounter": "配额侧接口（池实现），非后端能力",
}

// nonCapabilityIfaces 是基础接口（非「可选能力」），不参与归类。
var nonCapabilityIfaces = map[string]bool{
	"ExternalBackend": true,
	"StorageVolume":   true,
	"QuotaVolume":     true,
	"Volume":          true,
}

const (
	// forwardAlias 是包装器上刻意改名的转发（避免 pkg/volume/capacity 依赖 secrets 具体类型）。
	forwardAlias = "SecretsManagerAny"
	// 扫描面下限：registry 当前有 11 个单方法能力接口（含 3 个 notForwarded）。只增不减。
	minDiscovered = 11
)

// ifaceRE 匹配接口声明行（含行尾内联注释，如 `// NOSONAR`）。
var ifaceRE = regexp.MustCompile(`(?m)^type ([A-Za-z][A-Za-z0-9_]*) interface \{(?:\s*//[^\n]*)?$`)

// methodFirstRE 匹配方法行首（`Name(`）。
var methodFirstRE = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_]*)\(`)

// TestBackendCapabilitiesForwarded 断言每个后端能力接口都被 capacity.Backend 转发。
func TestBackendCapabilitiesForwarded(t *testing.T) {
	t.Parallel()
	regDir := filepath.Join(moduleRoot(t), "pkg", "volume", "registry")
	entries, err := os.ReadDir(regDir)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", regDir, err)
	}

	discovered := map[string]string{} // 接口名 → 唯一方法名
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(filepath.Join(regDir, e.Name()))
		if rerr != nil {
			t.Fatalf("读取 %s 失败: %v", e.Name(), rerr)
		}
		for name, method := range singleMethodInterfaces(string(src)) {
			if !nonCapabilityIfaces[name] {
				discovered[name] = method
			}
		}
	}
	if len(discovered) < minDiscovered {
		t.Fatalf("扫描到的能力接口只有 %d 个（下限 %d）⇒ 扫描面被改窄或文件已移动，门禁失效",
			len(discovered), minDiscovered)
	}

	capSrc, err := os.ReadFile(filepath.Join(moduleRoot(t), "pkg", "volume", "capacity", "capacity.go"))
	if err != nil {
		t.Fatalf("读取 capacity.go 失败: %v", err)
	}
	forwarded := map[string]bool{}
	for _, m := range regexp.MustCompile(`func \(b \*Backend\) ([A-Za-z0-9_]+)\(`).FindAllStringSubmatch(string(capSrc), -1) {
		forwarded[m[1]] = true
	}

	names := make([]string, 0, len(discovered))
	for name := range discovered {
		names = append(names, name)
	}
	sort.Strings(names)

	var missing []string
	verified := 0
	for _, name := range names {
		if _, skip := notForwarded[name]; skip {
			verified++
			continue
		}
		method := discovered[name]
		if forwarded[method] || forwarded[forwardAlias] && strings.Contains(method, "Manager") {
			verified++
			continue
		}
		missing = append(missing, name+"."+method)
	}
	// 护栏：实际校验到的必须覆盖「发现数 − 跳过数」，否则判定逻辑空转（本门禁第一版就空转过一次）。
	if want := len(discovered) - len(notForwarded); verified < want {
		t.Fatalf("实际校验的能力 %d 个，应为 %d 个 ⇒ 判定逻辑空转", verified, want)
	}
	if len(missing) > 0 {
		t.Fatalf("以下后端能力未在 capacity.Backend 上转发（装配层只拿得到包装器 ⇒ 能力在生产路径不可达）：\n  %s\n"+
			"处置：在 pkg/volume/capacity/capacity.go 加同名转发方法；确不需要转发时加入 notForwarded 并写明理由。",
			strings.Join(missing, "\n  "))
	}
}

// singleMethodInterfaces 返回 src 中「有效方法行恰好一行」的接口 → 该方法名。
// 有效行 = 去掉空行与 `//` 注释行后的行。
func singleMethodInterfaces(src string) map[string]string {
	out := map[string]string{}
	for _, loc := range ifaceRE.FindAllStringSubmatchIndex(src, -1) {
		name := src[loc[2]:loc[3]]
		rest := src[loc[1]:]
		end := strings.Index(rest, "\n}")
		if end < 0 {
			continue
		}
		var lines []string
		for _, line := range strings.Split(rest[:end], "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "//") {
				continue
			}
			lines = append(lines, line)
		}
		if len(lines) != 1 {
			continue // 多方法/内嵌接口 ⇒ 不是「单方法能力」
		}
		m := methodFirstRE.FindStringSubmatch(lines[0])
		if m == nil {
			continue // 内嵌接口（`io.Closer`）没有括号方法名
		}
		out[name] = m[1]
	}
	return out
}
