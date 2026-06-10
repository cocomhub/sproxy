// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux_test

import (
    "testing"

    "github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

func TestFrameRoundTrip(t *testing.T) {
    tests := []struct {
        name    string
        streamID mux.StreamID
        ftype   mux.FrameType
        payload []byte
    }{
        {"data frame", 1, mux.FrameData, []byte("hello")},
        {"open frame", 42, mux.FrameOpen, nil},
        {"close frame", 7, mux.FrameClose, []byte("reason")},
        {"empty payload", 100, mux.FrameData, []byte{}},
        {"large payload", 2, mux.FrameData, make([]byte, 1024)},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            raw := mux.EncodeFrame(tt.streamID, tt.ftype, tt.payload)
            sid, ftype, payload, err := mux.DecodeFrame(raw)
            if err != nil {
                t.Fatal(err)
            }
            if sid != tt.streamID {
                t.Fatalf("expected streamID %d, got %d", tt.streamID, sid)
            }
            if ftype != tt.ftype {
                t.Fatalf("expected frameType %d, got %d", tt.ftype, ftype)
            }
            if len(payload) != len(tt.payload) {
                t.Fatalf("expected payload len %d, got %d", len(tt.payload), len(payload))
            }
        })
    }
}

func TestDecodeFrameTooShort(t *testing.T) {
    _, _, _, err := mux.DecodeFrame([]byte{0, 0, 0, 1}) // only 4 bytes
    if err == nil {
        t.Fatal("expected error for short frame")
    }
}

func TestDecodeFrameTruncated(t *testing.T) {
    // 声明长度 256，但只有 2 字节
    raw := make([]byte, mux.FrameHeaderSize+2)
    raw[0] = 0
    raw[1] = 0
    raw[2] = 0
    raw[3] = 1 // streamID = 1
    raw[4] = 0 // type = Data
    raw[5] = 0 // flags
    raw[6] = 0x01 // length high
    raw[7] = 0x00 // length low = 256
    // only 2 bytes of payload
    raw[mux.FrameHeaderSize] = 'a'
    raw[mux.FrameHeaderSize+1] = 'b'

    _, _, _, err := mux.DecodeFrame(raw)
    if err == nil {
        t.Fatal("expected error for truncated payload")
    }
}
