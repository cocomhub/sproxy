// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudfilename

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

var (
	ErrEntryEmptyURL       = errors.New("cloud download entry: URL is empty")
	ErrEntryInvalidURL     = errors.New("cloud download entry: invalid URL")
	ErrEntryBadScheme      = errors.New("cloud download entry: unsupported URL scheme (only http/https)")
	ErrEntryMissingHost    = errors.New("cloud download entry: missing host")
	ErrEntryDupURL         = errors.New("cloud download entry: duplicate URL with different filename")
	ErrEntryUnsafeFilename = errors.New("cloud download entry: filename contains unsafe characters")
)

// ValidateEntry 校验单个条目的 URL 格式（scheme + host）。
func ValidateEntry(e Entry) error {
	if e.URL == "" {
		return ErrEntryEmptyURL
	}
	u, err := url.Parse(e.URL)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrEntryInvalidURL, e.URL)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return ErrEntryBadScheme
	}
	if u.Host == "" {
		return ErrEntryMissingHost
	}
	return nil
}

// ValidateEntries 校验一组条目：全部通过 URL 格式校验，且同 URL 不允许出现
// 不同 Filename。返回首个错误（哨兵错误）。
func ValidateEntries(entries []Entry) error {
	urlFilenames := make(map[string]string, len(entries))
	for _, e := range entries {
		if err := ValidateEntry(e); err != nil {
			return err
		}
		if prev, ok := urlFilenames[e.URL]; ok && prev != e.Filename {
			return fmt.Errorf("%w: %q (%q vs %q)", ErrEntryDupURL, e.URL, prev, e.Filename)
		}
		urlFilenames[e.URL] = e.Filename
	}
	return nil
}
