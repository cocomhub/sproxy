// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cliflag 提供 cobra flag 读取的类型化 helper，收敛
// `if f := cmd.Flags().Lookup(name); f != nil { v, err := cmd.Flags().GetX(name); ... }`
// 样板。语义：
//   - flag 未注册（Lookup == nil）时跳过（target 保持原值，不报错）——支持「可选 flag 族」
//     （如 mesh connect 不注册 exit 族 flag，FromFlags 跳过对应读取）；
//   - flag 已注册但类型不匹配时错误传播（不静默忽略）——改 flag 类型定义时显式暴露。
//
// 使用场景：meshconn.FromFlags / cloud_download / mesh_node 等大量 flag 读取的命令装配。
package cliflag

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// Changed 返回 flag 是否被显式设置（区分「未指定」与「显式空串/零值」）。
// flag 未注册时返回 false。
func Changed(cmd *cobra.Command, name string) bool {
	f := cmd.Flags().Lookup(name)
	return f != nil && f.Changed
}

// String 读取 string flag；未注册跳过，类型错误传播。
func String(cmd *cobra.Command, name string, target *string) error {
	if cmd.Flags().Lookup(name) == nil {
		return nil
	}
	v, err := cmd.Flags().GetString(name)
	if err != nil {
		return fmt.Errorf("读取 flag --%s: %w", name, err)
	}
	*target = v
	return nil
}

// Bool 读取 bool flag；未注册跳过，类型错误传播。
func Bool(cmd *cobra.Command, name string, target *bool) error {
	if cmd.Flags().Lookup(name) == nil {
		return nil
	}
	v, err := cmd.Flags().GetBool(name)
	if err != nil {
		return fmt.Errorf("读取 flag --%s: %w", name, err)
	}
	*target = v
	return nil
}

// Duration 读取 duration flag；未注册跳过，类型错误传播。
func Duration(cmd *cobra.Command, name string, target *time.Duration) error {
	if cmd.Flags().Lookup(name) == nil {
		return nil
	}
	v, err := cmd.Flags().GetDuration(name)
	if err != nil {
		return fmt.Errorf("读取 flag --%s: %w", name, err)
	}
	*target = v
	return nil
}

// StringSlice 读取 string-slice flag；未注册跳过，类型错误传播。
func StringSlice(cmd *cobra.Command, name string, target *[]string) error {
	if cmd.Flags().Lookup(name) == nil {
		return nil
	}
	v, err := cmd.Flags().GetStringSlice(name)
	if err != nil {
		return fmt.Errorf("读取 flag --%s: %w", name, err)
	}
	*target = v
	return nil
}
