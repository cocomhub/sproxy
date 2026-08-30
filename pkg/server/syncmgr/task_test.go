// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncmgr

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSyncTask_JSONRoundTrip(t *testing.T) {
	task := &SyncTask{
		ID:             "sync-abc-1",
		Direction:      "push",
		Remote:         "r1",
		Src:            "dir",
		Dst:            "",
		Recursive:      true,
		Include:        []string{"*.go"},
		Exclude:        []string{"*.tmp"},
		ConflictPolicy: "skip",
		SyncEmptyDirs:  true,
		FollowSymlinks: false,
		Status:         "completed",
		FilesTotal:     2,
		FilesDone:      2,
		BytesTotal:     100,
		BytesDone:      100,
		Results:        []SyncFileResult{{Path: "a.txt", Action: "created", Size: 10}},
		Error:          "",
		CreatedAt:      time.Now().Truncate(time.Second),
		UpdatedAt:      time.Now().Truncate(time.Second),
		ExpiresAt:      time.Now().Add(time.Hour).Truncate(time.Second),
		ReservedSize:   42,
	}

	data, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var restored SyncTask
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.ReservedSize != 0 {
		t.Fatalf("ReservedSize 不应持久化，got %d", restored.ReservedSize)
	}
	if restored.ID != task.ID || restored.Direction != task.Direction || restored.Remote != task.Remote {
		t.Fatalf("标识字段不符: %+v", restored)
	}
	if restored.Src != task.Src || restored.Dst != task.Dst || restored.Recursive != task.Recursive {
		t.Fatalf("路径字段不符: %+v", restored)
	}
	if len(restored.Include) != 1 || restored.Include[0] != "*.go" {
		t.Fatalf("include 不符: %+v", restored.Include)
	}
	if len(restored.Exclude) != 1 || restored.Exclude[0] != "*.tmp" {
		t.Fatalf("exclude 不符: %+v", restored.Exclude)
	}
	if restored.ConflictPolicy != "skip" || !restored.SyncEmptyDirs || restored.FollowSymlinks {
		t.Fatalf("策略字段不符: %+v", restored)
	}
	if restored.Status != "completed" || restored.FilesTotal != 2 || restored.FilesDone != 2 ||
		restored.BytesTotal != 100 || restored.BytesDone != 100 {
		t.Fatalf("进度字段不符: %+v", restored)
	}
	if len(restored.Results) != 1 || restored.Results[0].Path != "a.txt" || restored.Results[0].Action != "created" {
		t.Fatalf("results 不符: %+v", restored.Results)
	}
	if !restored.CreatedAt.Equal(task.CreatedAt) || !restored.ExpiresAt.Equal(task.ExpiresAt) {
		t.Fatalf("时间字段不符: %+v", restored)
	}
}

func TestApplyConfigDefaults(t *testing.T) {
	cfg := &Config{}
	applyConfigDefaults(cfg)
	if cfg.MaxConcurrent != 3 {
		t.Fatalf("MaxConcurrent 默认应为 3，got %d", cfg.MaxConcurrent)
	}
	if cfg.TaskTTL != 24*time.Hour {
		t.Fatalf("TaskTTL 默认应为 24h，got %v", cfg.TaskTTL)
	}

	cfg2 := &Config{MaxConcurrent: 5, TaskTTL: time.Hour}
	applyConfigDefaults(cfg2)
	if cfg2.MaxConcurrent != 5 || cfg2.TaskTTL != time.Hour {
		t.Fatalf("非零值不应被覆盖: %+v", cfg2)
	}
}
