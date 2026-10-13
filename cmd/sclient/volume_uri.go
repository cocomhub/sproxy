// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// volume_uri.go 实现**卷 URI 寻址**：位置参数写 `<卷名>:/<路径>`（`<卷名>:` 或 `<卷名>:/` = 卷根）
// 等价于 `--volume <卷名>` + 纯路径。为什么要它：加密卷/非默认卷的文件操作不必再记住 flag，
// 路径自述归属（用户要求 2026-10-13：「secretdata 卷 A → A:/a/b/c」）。
//
// 纯客户端归一：只改写本地参数 + `--volume`/`--to-volume` flag，**协议与服务端零改动**
// （线上仍是 `?volume=` 查询参数）。冲突（位置参数与显式 flag 卷名不一致）→ 报错，不静默取其一。
//
// 与 URL 的歧义按语法消除：本形式要求 `:/`（冒号后紧跟**单个**斜杠），故 `https://host/x`
// （冒号后两个斜杠）与 Windows 盘符 `C:\x`（冒号后无反斜杠/斜杠）都不会被误判为卷 URI。

// splitVolumeURI 解析卷 URI 前缀。返回 (卷名, 卷内相对路径, true)；无前缀/不合法 → ("", arg, false)。
func splitVolumeURI(arg string) (vol, path string, ok bool) {
	idx := strings.Index(arg, ":")
	if idx <= 0 {
		return "", arg, false
	}
	vol, rest := arg[:idx], arg[idx+1:]
	if !validVolumeName(vol) {
		return "", arg, false
	}
	switch {
	case rest == "", rest == "/":
		return vol, "", true // `<卷名>:` / `<卷名>:/` = 卷根
	case strings.HasPrefix(rest, "//"):
		return "", arg, false // URL（scheme://…）不是卷 URI
	case !strings.HasPrefix(rest, "/"):
		return "", arg, false // 必须是 `:/`（避免误判 `a:b` / `C:\x`）
	}
	return vol, strings.TrimPrefix(rest, "/"), true
}

// validVolumeName 校验卷名（与存储层命名约束一致的保守子集：非空、无空白/分隔符/冒号/通配符）。
func validVolumeName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	return !strings.ContainsAny(name, `/\:*?"<>|`+" \t")
}

// applyVolumeURIs 就地剥离 args 中的卷 URI 前缀，并把卷名写入 `--volume`（同一命令内多个位置
// 参数必须指向同一卷，否则报错）。返回冲突/非法时的错误（调用方在 `factory.NewClient` 之前调用，
// 因为工厂读取 `--volume` flag）。
func applyVolumeURIs(cmd *cobra.Command, args []string) error {
	for i, arg := range args {
		vol, path, ok := splitVolumeURI(arg)
		if !ok {
			continue
		}
		if err := setVolumeFlag(cmd, "volume", vol); err != nil {
			return err
		}
		args[i] = path
	}
	return nil
}

// applyMvVolumeURIs 处理 mv 的两个路径：源路径的卷写 `--volume`；目标路径带**不同**卷名时写
// `--to-volume`（跨卷迁移语义，与服务端 move API 一致）；相同卷名 → 同卷 rename。
func applyMvVolumeURIs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	if vol, path, ok := splitVolumeURI(args[0]); ok {
		if err := setVolumeFlag(cmd, "volume", vol); err != nil {
			return err
		}
		args[0] = path
	}
	if len(args) < 2 {
		return nil
	}
	toVol, toPath, ok := splitVolumeURI(args[1])
	if !ok {
		return nil
	}
	args[1] = toPath
	fromVol, _ := cmd.Flags().GetString("volume")
	if toVol == fromVol {
		return nil
	}
	return setVolumeFlag(cmd, flagToVolume, toVol)
}

// setVolumeFlag 把卷名写入指定 flag；该 flag 已有**不同**显式值 → 报错（不静默取其一）。
func setVolumeFlag(cmd *cobra.Command, flag, vol string) error {
	f := cmd.Flags().Lookup(flag)
	if f == nil {
		return fmt.Errorf("命令不支持卷寻址（缺少 --%s）", flag)
	}
	if f.Changed {
		if cur, _ := cmd.Flags().GetString(flag); cur != vol {
			return fmt.Errorf("卷名冲突：位置参数指定 %q，--%s 指定 %q", vol, flag, cur)
		}
		return nil
	}
	return cmd.Flags().Set(flag, vol)
}
