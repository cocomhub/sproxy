// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// StreamID 是虚拟流标识符。
// 控制流使用 StreamID=0，用户流从 1 开始。
type StreamID uint32

// FrameType 标识帧的用途。
type FrameType byte

const (
	FrameData         FrameType = 0x00 // 用户流数据
	FrameOpen         FrameType = 0x01 // 通知远端打开新流
	FrameClose        FrameType = 0x02 // 关闭指定流
	FramePing         FrameType = 0x03 // 心跳探测
	FramePong         FrameType = 0x04 // 心跳回复
	FrameCloseWrite   FrameType = 0x05 // 写半关闭（不再有更多数据发送）
	FrameReject       FrameType = 0x06 // 拒绝流创建（acceptCh 满或达到 maxStreams）
	FrameWindowUpdate FrameType = 0x07 // 窗口更新（流控）
)

// FrameHeaderSize 是帧头部的固定字节数。
const FrameHeaderSize = 8

// 帧头部格式：
//
//	[0:4]  StreamID  — 4 字节 big-endian，虚拟流标识符
//	[4]    FrameType — 1 字节，帧类型
//	[5]    Flags     — 1 字节，保留标志位（当前未使用）
//	[6:8]  Length    — 2 字节 big-endian，负载长度（最大 65535 字节）
const (
	headerStreamIDOff = 0
	headerTypeOff     = 4
	headerFlagsOff    = 5
	headerLengthOff   = 6
	headerSize        = 8
)

var (
	// ErrFrameTooShort 是帧数据不足 FrameHeaderSize 时的错误。
	ErrFrameTooShort = errors.New("mux: frame too short")
	// ErrFrameTruncated 是帧负载长度少于声明值时的错误。
	ErrFrameTruncated = errors.New("mux: frame payload truncated")
	// ErrFrameTooLarge 是负载超过 MaxFramePayload（无法编码）时的错误。
	ErrFrameTooLarge = errors.New("mux: frame payload too large")
)

// MaxFramePayload 是单帧负载的上限（帧头的 Length 字段为 2 字节）。
//
// **硬约束**：负载超过本值的帧**无法编码**。曾经此处是「静默截断到 65535」——
// 那会丢字节：发送方以为发出 N 字节、对端只收到 65535，字节流从此错位（上层分块加密
// 表现为 GCM 认证失败）。issue #213 的根因即此（流写窗口 DefaultWindowSize=65536
// 恰好越过该上限）。现在改为**显式错误**，并配合 stream.Write 的上限收敛。
const MaxFramePayload = 65535

// EncodeFrame 编码一个完整帧。
//
// 负载超过 MaxFramePayload 返回 ErrFrameTooLarge（**绝不截断**：截断=静默丢字节）。
func EncodeFrame(streamID StreamID, ftype FrameType, payload []byte) ([]byte, error) {
	if payload == nil {
		payload = []byte{}
	}
	length := len(payload)
	if length > MaxFramePayload {
		return nil, fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, length, MaxFramePayload)
	}
	buf := make([]byte, headerSize+length)
	binary.BigEndian.PutUint32(buf[headerStreamIDOff:], uint32(streamID))
	buf[headerTypeOff] = byte(ftype)
	buf[headerFlagsOff] = 0
	binary.BigEndian.PutUint16(buf[headerLengthOff:], uint16(length))
	copy(buf[headerSize:], payload)
	return buf, nil
}

// DecodeFrame 解码一个完整帧。
func DecodeFrame(raw []byte) (StreamID, FrameType, []byte, error) {
	if len(raw) < headerSize {
		return 0, 0, nil, fmt.Errorf("%w: got %d bytes, need %d", ErrFrameTooShort, len(raw), headerSize)
	}
	sid := StreamID(binary.BigEndian.Uint32(raw[headerStreamIDOff:]))
	ftype := FrameType(raw[headerTypeOff])
	length := int(binary.BigEndian.Uint16(raw[headerLengthOff:]))
	if len(raw) < headerSize+length {
		return 0, 0, nil, fmt.Errorf("%w: declared %d, got %d", ErrFrameTruncated, length, len(raw)-headerSize)
	}
	payload := make([]byte, length)
	copy(payload, raw[headerSize:headerSize+length])
	return sid, ftype, payload, nil
}
