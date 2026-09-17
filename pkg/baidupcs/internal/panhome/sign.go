// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package panhome

import (
	"encoding/base64"

	"github.com/cocomhub/sproxy/pkg/baidupcs/internal/netdisksign"
)

type (
	// SignRes 签名结果
	SignRes interface {
		Sign() string
		Timestamp() string
	}

	signRes struct {
		sign      string
		timestamp string
	}
)

func (sr *signRes) Sign() string {
	return sr.sign
}
func (sr *signRes) Timestamp() string {
	return sr.timestamp
}

func (ph *PanHome) Signature() (sign SignRes, err error) {
	err = ph.getSignInfo()
	if err != nil {
		return nil, err
	}

	o := netdisksign.Sign2(ph.sign3, ph.sign1)
	signed := base64.StdEncoding.EncodeToString(o)
	return &signRes{
		sign:      signed,
		timestamp: ph.timestamp,
	}, nil
}
