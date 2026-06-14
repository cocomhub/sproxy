// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"strings"
	"testing"
)

func TestStaticFS_ContainsIndexHTML(t *testing.T) {
	data, err := StaticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("无法读取 static/index.html: %v", err)
	}
	if len(data) == 0 {
		t.Error("static/index.html 为空")
	}
}

func TestStaticFS_ContainsExpectedContent(t *testing.T) {
	data, err := StaticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("无法读取 static/index.html: %v", err)
	}
	content := string(data)
	expectedSubstrings := []string{"<title", "<html", "<head", "<body"}
	for _, s := range expectedSubstrings {
		if !strings.Contains(content, s) {
			t.Errorf("未找到预期元素: %q", s)
		}
	}
}
