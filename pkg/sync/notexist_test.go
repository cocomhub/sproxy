// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// TestIsNotExist not-exist 契约①：`errors.Is(err, os/fs.ErrNotExist)`（含包装链）。
func TestIsNotExist(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"os.ErrNotExist", os.ErrNotExist, true},
		{"fs.ErrNotExist", fs.ErrNotExist, true},
		{"包装链", errors.Join(errors.New("ctx"), os.ErrNotExist), true},
		{"PathError", &os.PathError{Op: "open", Path: "x", Err: os.ErrNotExist}, true},
		{"真错误", errors.New("connection reset"), false},
		{"io.EOF 不算不存在", io.EOF, false},
	}
	for _, c := range cases {
		if got := IsNotExist(c.err); got != c.want {
			t.Errorf("%s: IsNotExist=%v, want %v", c.name, got, c.want)
		}
	}
}

// TestAbsent not-exist 契约②：`(nil, nil)` 表示不存在；其余组合必须严格区分
// （真错误**不得**被当作不存在——否则账本把读失败当空账本 ⇒ 限额 fail-open ✗）。
func TestAbsent(t *testing.T) {
	t.Parallel()
	rc := io.NopCloser(strings.NewReader("x"))
	cases := []struct {
		name string
		err  error
		rc   io.ReadCloser
		want bool
	}{
		{"契约② (nil,nil)", nil, nil, true},
		{"契约① ErrNotExist", os.ErrNotExist, nil, true},
		{"存在 (rc,nil)", nil, rc, false},
		{"真错误 (nil,err)", errors.New("boom"), nil, false},
	}
	for _, c := range cases {
		if got := Absent(c.err, c.rc); got != c.want {
			t.Errorf("%s: Absent=%v, want %v", c.name, got, c.want)
		}
	}
}
