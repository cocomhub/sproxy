// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/volumectx"
	"github.com/spf13/cobra"
)

// volume_uri.go 实现**卷 URI 寻址**：位置参数写 `<卷名>:/<路径>`（`<卷名>:` 或 `<卷名>:/` = 卷根）。
// **这是唯一的卷寻址方式**（用户口径 2026-10-14：「所有常见文件命令都要支持卷 uri 操作，移除多余的
// --volume flag」）：文件与目录一律用同一形式（`ls vault:/a`、`mkdir vault:/a/b`、`rm vault:/a/b.txt`）。
//
// 纯客户端归一：只改写本地参数 + 解析出的卷名（经 `volumectx` 上下文传给客户端工厂），**协议与服务端
// 零改动**（线上仍是 `?volume=` 查询参数）。同一命令内多个位置参数指向**不同卷**时：单卷命令报错
// （不静默取其一），mv/cp 把第二个卷作为**目标卷**（跨卷语义）。
//
// 与 URL 的歧义按语法消除：本形式要求 `:/`（冒号后紧跟**单个**斜杠），故 `https://host/x`
// （冒号后两个斜杠）与 Windows 盘符 `C:\x`（冒号后无反斜杠/斜杠）都不会被误判为卷 URI。

// splitVolumeURI 解析卷 URI 前缀。返回 (卷名, **卷根起算的绝对路径**, true)；无前缀/不合法 → ("", arg, false)。
//
// 返回的路径**保留前导斜杠**：卷 URI 里的路径是「该卷根起算的绝对路径」（用户口径
// 2026-10-13），交给既有 ResolveRemotePath 时**绕过当前工作目录**；否则 `list vault:/`
// 会被当成「当前目录相对」而不是卷根（实测踩过）。
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
		return vol, "/", true // `<卷名>:` / `<卷名>:/` = 卷根
	case strings.HasPrefix(rest, "//"):
		return "", arg, false // URL（scheme://…）不是卷 URI
	case !strings.HasPrefix(rest, "/"):
		return "", arg, false // 必须是 `:/`（避免误判 `a:b` / `C:\x`）
	}
	return vol, rest, true
}

// validVolumeName 校验卷名（与存储层命名约束一致的保守子集：非空、无空白/分隔符/冒号/通配符）。
func validVolumeName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	return !strings.ContainsAny(name, `/\:*?"<>|`+" \t")
}

// applyVolumeContext 归一本次调用的卷上下文（顺序即优先级）：
//  1. 位置参数里的**卷 URI**（`<卷名>:/<路径>`）；
//  2. **当前工作目录所在卷**（`cd <卷名>:/…` 设定，见 state.CurrentVolume）——即「所有操作默认
//     只对当前工作目录所在卷处理」；
//  3. 皆空 = auto（服务端按 ACL/默认卷判定，行为不变）。
//
// 之所以统一在这里做：命令体只消费上下文里的卷名（clientfactory 读它），本函数是**唯一**把
// URI/工作目录翻译成卷名的地方（协议零改动：线上仍是 `?volume=`）。
func applyVolumeContext(cmd *cobra.Command, st *state.State, args []string) error {
	if err := applyVolumeURIs(cmd, args); err != nil {
		return err
	}
	return applyStateVolume(cmd, st)
}

// applyStateVolume 在**URI 未指定卷**时，用当前工作目录所在卷填充卷上下文（URI 优先）。
func applyStateVolume(cmd *cobra.Command, st *state.State) error {
	if st == nil || st.CurrentVolume == "" {
		return nil
	}
	if _, ok := volumectx.From(cmd.Context()); ok {
		return nil // URI 已指定 ⇒ 优先，不被 cwd 覆盖
	}
	cmd.SetContext(volumectx.With(cmd.Context(), st.CurrentVolume))
	return nil
}

// applyMvVolumeContext 是 mv/cp 的卷上下文归一：两侧路径的卷 URI 优先（目标不同卷 ⇒ 目标卷上下文），
// 随后未指定时回落当前工作目录所在卷（同卷改名语义）。
func applyMvVolumeContext(cmd *cobra.Command, st *state.State, args []string) error {
	if err := applyMvVolumeURIs(cmd, args); err != nil {
		return err
	}
	return applyStateVolume(cmd, st)
}

// applyVolumeURIs 就地剥离 args 中的卷 URI 前缀，并把卷名写入**卷上下文**（同一命令内多个位置
// 参数必须指向同一卷，否则报错）。返回冲突/非法时的错误（调用方在 `factory.NewClient` 之前调用，
// 因为工厂读取上下文）。
func applyVolumeURIs(cmd *cobra.Command, args []string) error {
	for i, arg := range args {
		vol, path, ok := splitVolumeURI(arg)
		if !ok {
			continue
		}
		if err := setSourceVolume(cmd, vol); err != nil {
			return err
		}
		args[i] = path
	}
	return nil
}

// applyMvVolumeURIs 处理 mv/cp 的两个路径：源路径的卷写源卷上下文；目标路径带**不同**卷名时写
// 目标卷上下文（跨卷迁移/复制语义，与服务端 move/copy API 一致）；相同卷名 → 同卷 rename/copy。
func applyMvVolumeURIs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	if vol, path, ok := splitVolumeURI(args[0]); ok {
		if err := setSourceVolume(cmd, vol); err != nil {
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
	fromVol, _ := volumectx.From(cmd.Context())
	if toVol == fromVol {
		return nil
	}
	cmd.SetContext(volumectx.WithTarget(cmd.Context(), toVol))
	return nil
}

// setSourceVolume 写入源卷上下文；上下文里已有**不同**卷名 → 报错（同一命令多个位置参数必须
// 指向同一卷，不静默取其一）。
func setSourceVolume(cmd *cobra.Command, vol string) error {
	if cur, ok := volumectx.From(cmd.Context()); ok {
		if cur != vol {
			return fmt.Errorf("卷名冲突：同一命令的多个位置参数指向不同卷（%q 与 %q）", cur, vol)
		}
		return nil
	}
	cmd.SetContext(volumectx.With(cmd.Context(), vol))
	return nil
}
