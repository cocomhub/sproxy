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

// TestDeleteAndRmdir_CleanVolumeKeyedChecksum 钉住 §15.1.2 遗留项清理：删除/rmdir 必须按
// **文件所在卷**清理卷维度 checksum，否则残留记录会让「同名重传」读到旧值。
//
// 变异：`checksumDelete`/`checksumDeletePrefix` 改回旧的 `cs.Delete`/`cs.DeletePrefix`
// ⇒ 卷维度键残留 ⇒ 本用例红。
func TestDeleteAndRmdir_CleanVolumeKeyedChecksum(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableVolumes(t, "default", "vol2")
	env.enableWriteDefaults()

	sum := func(s string) string {
		h := sha256.Sum256([]byte(s))
		return hex.EncodeToString(h[:])
	}
	csDefault, csVol2 := sum("alpha"), sum("bravo")
	for vol, body := range map[string]string{"default": "alpha", "vol2": "bravo"} {
		dir := filepath.Join(env.volDirs[vol], "alice", "user", "d")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", vol, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", vol, err)
		}
	}
	env.svc.RecordBypassChecksum("alice", "user/d/a.txt", csDefault, "default")
	env.svc.RecordBypassChecksum("alice", "user/d/a.txt", csVol2, "vol2")

	cs := env.svc.rt.checksumStore("alice")
	// ① 单文件删除：只清目标卷键。
	if _, err := env.svc.DeleteFile(t.Context(), DeleteFileInput{
		Owner: "alice", RemotePath: "d/a.txt", ExpectedChecksum: csDefault, ExplicitVol: "default",
	}); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, ok := checksumGet(cs, "default", "user/d/a.txt"); ok {
		t.Fatal("删除后 default 卷的 checksum 应被清理")
	}
	if got, ok := checksumGet(cs, "vol2", "user/d/a.txt"); !ok || got != csVol2 {
		t.Fatalf("删除不应影响其它卷，vol2 got %q ok=%v", got, ok)
	}

	// ② rmdir：清目标卷子树键。
	if _, err := env.svc.RemoveDir(t.Context(), "alice", "d", "vol2", true); err != nil {
		t.Fatalf("RemoveDir: %v", err)
	}
	if _, ok := checksumGet(cs, "vol2", "user/d/a.txt"); ok {
		t.Fatal("rmdir 后 vol2 卷的子树 checksum 应被清理")
	}
}
