// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestList_MultiVolumeSameNameChecksum 钉住 §15.1.1 修复：**同一 owner 在两卷上的同名文件**
// 必须各自拿到自己的 checksum（旧台账键无卷维度 ⇒ 后写覆盖先写，list 张冠李戴）。
//
// 变异：`checksumSnapshotFor` 改回全量 `checksumSnapshot`（无卷维度）⇒ 本用例红。
func TestList_MultiVolumeSameNameChecksum(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableVolumes(t, "default", "vol2")
	env.enableWriteDefaults()

	sum := func(s string) string {
		h := sha256.Sum256([]byte(s))
		return hex.EncodeToString(h[:])
	}
	csDefault, csVol2 := sum("alpha-default"), sum("bravo-vol2")

	// 直接落盘（绕过 upload 路由，专测「读侧按卷取 checksum」）。
	for vol, body := range map[string]string{"default": "alpha-default", "vol2": "bravo-vol2"} {
		dir := filepath.Join(env.volDirs[vol], "alice", "user")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", vol, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", vol, err)
		}
	}
	env.svc.RecordBypassChecksum("alice", "user/a.txt", csDefault, "default")
	env.svc.RecordBypassChecksum("alice", "user/a.txt", csVol2, "vol2")

	res, err := env.svc.List(t.Context(), ListQuery{Owner: "alice"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := map[string]string{}
	for _, f := range res.Files {
		if f.Name == "a.txt" {
			got[f.Volume] = f.Checksum
		}
	}
	if len(got) != 2 {
		t.Fatalf("应列出两卷的 a.txt，got %+v（files=%+v）", got, res.Files)
	}
	if got["default"] != csDefault {
		t.Fatalf("default 卷 a.txt checksum = %q，want %q（跨卷同名被覆盖）", got["default"], csDefault)
	}
	if got["vol2"] != csVol2 {
		t.Fatalf("vol2 卷 a.txt checksum = %q，want %q", got["vol2"], csVol2)
	}
}
