// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"log/slog"

	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume/enclosure"
)

// volumes_enclosure.go 是**封装卷的装配接线（阶段 1：封印生效）**：
// 解析 volumes[] 里的封装声明，把封印规则注入**源卷**根，使源目录在卷根命名空间下
// 无法被直连读写（写恒拒、读按声明 hidden/readable）。封装卷自身的后端构造（持特权源
// 视图）在 set 就绪后由装配层补装（推迟装配，与 secretdata 同型）。
//
// 装配顺序不变量（设计 §3）：封印必须在「封装卷已取得特权源视图」之后注入。本函数在
// 卷循环结束后、NewSet 之前执行：此时源卷根已打开（特权句柄仍在装配层手里），封装卷
// 后端尚未构造——封装卷经装配层另行取得的源 FS 句柄不受封印影响（它直接持有源 FS）。
//
// 跨外部卷源（baidupcs/s3/…）：封印需要注入外部卷后端视图，当前未接入（设计 §10 记档），
// **装配期显式拒绝**（禁静默降级：否则会留下「以为封了其实没封」的安全缺口）。
func applyEnclosureSeals(log *slog.Logger, acc *volumeAccum) error {
	decls, err := enclosure.Parse(acc.volumes)
	if err != nil {
		return fmt.Errorf("封装卷声明校验失败（fail-closed）: %w", err)
	}
	if len(decls) == 0 {
		return nil // 无封装声明的部署：行为零回归
	}
	if log == nil {
		log = slog.Default()
	}
	byVol := make(map[string][]storage.SealRule, len(decls))
	for _, d := range decls {
		rt, ok := acc.roots[d.Source]
		if !ok {
			return fmt.Errorf("封装卷 %q 的源卷 %q 不是本地卷：跨外部卷封装尚未接入（装配期显式拒绝，禁静默降级）", d.Vol, d.Source)
		}
		if rt == nil {
			return fmt.Errorf("封装卷 %q 的源卷 %q 根未打开", d.Vol, d.Source)
		}
		byVol[d.Source] = append(byVol[d.Source], d.SealRules()...)
	}
	for name, rules := range byVol {
		if err := acc.roots[name].SetSeals(rules); err != nil {
			return fmt.Errorf("向源卷 %q 注入封印失败（fail-closed）: %w", name, err)
		}
	}
	// 生效可观测（禁静默降级）：逐条打印封装声明与其封印语义。
	for _, d := range decls {
		mode := "hidden(读404+列举过滤)"
		if d.ReadMode == storage.SealReadable {
			mode = "readable(只读透视，写仍禁止)"
		}
		log.Info("封装卷声明生效（源目录已封印，禁止直连）",
			"enclosure", d.Vol, "source", d.Source, "path", d.Path,
			"wrapper", d.Wrapper, "read_mode", mode, "allow_direct_url", d.AllowDirectURL, "max_bytes", d.MaxBytes)
	}
	return nil
}
