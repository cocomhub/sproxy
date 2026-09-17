// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//
// 本文件是 BaiduPCS-Go（Apache-2.0，https://github.com/qjfoidnh/BaiduPCS-Go）
// pcsutil/converter/converter.go 的裁剪版本：仅保留无第三方依赖的纯转换函数，
// 移除依赖 go-runewidth 的 ShortDisplay 等展示函数。

// Package converter 提供格式与类型转换工具（裁剪自 BaiduPCS-Go）。
package converter

import (
	"strconv"
	"unsafe"
)

// ToString 将 []byte 转换为 string（零拷贝）。
func ToString(p []byte) string {
	return *(*string)(unsafe.Pointer(&p))
}

// ToBytes 将 string 转换为 []byte（零拷贝）。
func ToBytes(str string) []byte {
	type stringHeader struct {
		Data uintptr
		Len  int
	}
	type sliceHeader struct {
		Data uintptr
		Len  int
		Cap  int
	}
	sh := (*stringHeader)(unsafe.Pointer(&str))
	return *(*[]byte)(unsafe.Pointer(&sliceHeader{
		Data: sh.Data,
		Len:  sh.Len,
		Cap:  sh.Len,
	}))
}

// SliceInt64ToString []int64 转换为 []string。
func SliceInt64ToString(si []int64) (ss []string) {
	ss = make([]string, 0, len(si))
	for k := range si {
		ss = append(ss, strconv.FormatInt(si[k], 10))
	}
	return ss
}

// SliceStringToInt64 []string 转换为 []int64（非法项跳过）。
func SliceStringToInt64(ss []string) (si []int64) {
	si = make([]int64, 0, len(ss))
	for k := range ss {
		i, err := strconv.ParseInt(ss[k], 10, 64)
		if err != nil {
			continue
		}
		si = append(si, i)
	}
	return
}
