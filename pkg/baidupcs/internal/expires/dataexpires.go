// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package expires

import (
	"time"
)

type (
	DataExpires interface {
		Data() interface{}
		Expires
	}

	dataExpires struct {
		data interface{}
		Expires
	}
)

func NewDataExpires(data interface{}, dur time.Duration) DataExpires {
	return &dataExpires{
		data:    data,
		Expires: NewExpires(dur),
	}
}

func (de *dataExpires) Data() interface{} {
	return de.data
}
