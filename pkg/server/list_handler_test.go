// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"
)

func TestSortFileEntries(t *testing.T) {
	t.Parallel()
	entries := []fileInfo{
		{Name: "b.txt", Size: 100, ModTime: 1000},
		{Name: "a.txt", Size: 200, ModTime: 500},
		{Name: "c.txt", Size: 50, ModTime: 2000},
	}

	// 按 name 升序
	sorted := make([]fileInfo, len(entries))
	copy(sorted, entries)
	sortFileEntries(sorted, "name", "asc")
	if sorted[0].Name != "a.txt" || sorted[2].Name != "c.txt" {
		t.Errorf("name asc: expected a,b,c got %s,%s,%s", sorted[0].Name, sorted[1].Name, sorted[2].Name)
	}

	// 按 name 降序
	copy(sorted, entries)
	sortFileEntries(sorted, "name", "desc")
	if sorted[0].Name != "c.txt" || sorted[2].Name != "a.txt" {
		t.Errorf("name desc: expected c,b,a got %s,%s,%s", sorted[0].Name, sorted[1].Name, sorted[2].Name)
	}

	// 按 size 升序
	copy(sorted, entries)
	sortFileEntries(sorted, "size", "asc")
	if sorted[0].Size != 50 || sorted[2].Size != 200 {
		t.Errorf("size asc: expected 50,100,200 got %d,%d,%d", sorted[0].Size, sorted[1].Size, sorted[2].Size)
	}

	// 按 size 降序
	copy(sorted, entries)
	sortFileEntries(sorted, "size", "desc")
	if sorted[0].Size != 200 || sorted[2].Size != 50 {
		t.Errorf("size desc: expected 200,100,50 got %d,%d,%d", sorted[0].Size, sorted[1].Size, sorted[2].Size)
	}

	// 按 time 升序
	copy(sorted, entries)
	sortFileEntries(sorted, "time", "asc")
	if sorted[0].ModTime != 500 || sorted[2].ModTime != 2000 {
		t.Errorf("time asc: expected 500,1000,2000 got %d,%d,%d", sorted[0].ModTime, sorted[1].ModTime, sorted[2].ModTime)
	}

	// 按 time 降序
	copy(sorted, entries)
	sortFileEntries(sorted, "time", "desc")
	if sorted[0].ModTime != 2000 || sorted[2].ModTime != 500 {
		t.Errorf("time desc: expected 2000,1000,500 got %d,%d,%d", sorted[0].ModTime, sorted[1].ModTime, sorted[2].ModTime)
	}

	// 默认按 name 升序
	copy(sorted, entries)
	sortFileEntries(sorted, "", "")
	if sorted[0].Name != "a.txt" || sorted[2].Name != "c.txt" {
		t.Errorf("default: expected a,b,c got %s,%s,%s", sorted[0].Name, sorted[1].Name, sorted[2].Name)
	}
}
