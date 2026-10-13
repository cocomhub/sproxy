// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// persist_root_test.go 钉住「云任务状态 JSON 经 storage.Root 落盘」：
//   - 加密卷：状态 JSON（含 URL/文件名/目标路径等用户可见元数据）**密文落盘**，且可读回；
//   - 封印（封装卷占用的区间）：状态写入恒拒（fail-closed，不再绕过封印写元数据）。
//
// 变异：写回退裸 os.WriteFile → 明文断言红；读回退裸 os.ReadFile → 恢复断言红；
// 写不经 Root（无 sealCheck）→ 封印用例红。

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// scanPlaintext 递归扫描 dir，返回包含 needle 的文件路径（用于断言「盘上无明文」）。
func scanPlaintext(t *testing.T, dir, needle string) []string {
	t.Helper()
	var hits []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // 遍历容错：只关心是否出现明文
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		if bytes.Contains(b, []byte(needle)) {
			hits = append(hits, p)
		}
		return nil
	})
	return hits
}

func TestCloudState_EncryptedVolumeCiphertextAndRecover(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 1024*1024, nil, testLogger())
	env := newCloudTestEnv(t, dir)
	tnt := env.tenantFor("")
	if tnt == nil || tnt.Root() == nil {
		t.Fatal("tenant 不可用")
	}
	if serr := tnt.Root().SetEncryption(bytes.Repeat([]byte{9}, 32)); serr != nil {
		t.Fatalf("SetEncryption: %v", serr)
	}
	mgr1 := newCloudTestManagerInEnv(t, env, sm, env.tenantFor, defaultCloudDownloadConfig())
	t.Cleanup(mgr1.Close)

	const marker = "secret-report.bin"
	task, err := mgr1.CreateTask("url", "https://example.com/"+marker, marker, 128, "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// 布局不变（仍是 <租户根>/meta/cloud/<id>.json），但内容不许是明文 JSON。
	statePath := filepath.Join(mgr1.PersistDirFor(""), task.ID+".json")
	raw, rerr := os.ReadFile(statePath)
	if rerr != nil {
		t.Fatalf("读取状态文件（裸读应仍可打开）: %v", rerr)
	}
	if strings.Contains(string(raw), marker) || strings.Contains(string(raw), "https://example.com/") {
		t.Fatalf("加密卷上状态 JSON 不得明文落盘: %s", statePath)
	}
	if hits := scanPlaintext(t, dir, marker); len(hits) > 0 {
		t.Fatalf("加密卷上不得出现明文文件名（命中 %v）", hits)
	}

	// 恢复：同一 env 上新建 manager（模拟重启）→ 状态可经 Root 解密读回。
	mgr2 := newCloudTestManagerInEnv(t, env, sm, env.tenantFor, defaultCloudDownloadConfig())
	t.Cleanup(mgr2.Close)
	got, ok := mgr2.GetTask(task.ID, "")
	if !ok {
		t.Fatal("重启后应恢复出该任务（状态经 Root 解密可读）")
	}
	if got.URL != "https://example.com/"+marker || got.Filename != marker {
		t.Fatalf("恢复的状态不符: url=%q filename=%q", got.URL, got.Filename)
	}
}

func TestCloudState_SealedStatePathRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 1024*1024, nil, testLogger())
	env := newCloudTestEnv(t, dir)
	tnt := env.tenantFor("")
	if tnt == nil || tnt.Root() == nil {
		t.Fatal("tenant 不可用")
	}
	if serr := tnt.Root().SetSeals([]storage.SealRule{{Path: "meta", Wrapper: "vault"}}); serr != nil {
		t.Fatalf("SetSeals: %v", serr)
	}
	mgr := newCloudTestManagerInEnv(t, env, sm, env.tenantFor, defaultCloudDownloadConfig())
	t.Cleanup(mgr.Close)

	task, _ := mgr.CreateTask("url", "https://example.com/x.zip", "x.zip", 64, "", TaskParams{Save: true})
	if err := mgr.saveTask(task); !errors.Is(err, storage.ErrSealed) {
		t.Fatalf("封印区间写状态应被拒（ErrSealed）, got %v", err)
	}
}
