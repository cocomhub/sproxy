// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// checksum_access.go 是卷维度 checksum 访问的**唯一适配点**（设计 §15.1.1）：
// 台账实现支持 `checksum.VolumeKeyedStore` 时按卷读写，否则回落旧的「无卷」方法
// （测试替身 / 未升级实现零回归）。

import "github.com/cocomhub/sproxy/pkg/checksum"

func volumeKeyed(cs checksum.ChecksumStoreIface) (checksum.VolumeKeyedStore, bool) {
	v, ok := cs.(checksum.VolumeKeyedStore)
	return v, ok
}

// checksumGet 读某卷的 rel checksum（卷键优先，旧无卷键兜底）。
func checksumGet(cs checksum.ChecksumStoreIface, vol, rel string) (string, bool) {
	if v, ok := volumeKeyed(cs); ok && vol != "" {
		return v.GetInVolume(vol, rel)
	}
	return cs.Get(rel)
}

// checksumSet 写某卷的 rel checksum（不支持卷维度时回落旧方法）。
func checksumSet(cs checksum.ChecksumStoreIface, vol, rel, sum string) {
	if v, ok := volumeKeyed(cs); ok && vol != "" {
		v.SetInVolume(vol, rel, sum)
		return
	}
	cs.Set(rel, sum)
}

// checksumSnapshotForVolume 返回该卷的 `rel → checksum` 视图（旧键兜底合并）。
func checksumSnapshotForVolume(cs checksum.ChecksumStoreIface, vol string) map[string]string {
	v, ok := volumeKeyed(cs)
	if !ok || vol == "" {
		return cs.GetAll()
	}
	out := v.SnapshotLegacy()
	for rel, sum := range v.SnapshotInVolume(vol) {
		out[rel] = sum
	}
	return out
}

// checksumSnapshotsAll 返回所有卷的 checksum 视图（跨卷结果集用；不支持卷维度时只有 "" 一个键）。
func checksumSnapshotsAll(cs checksum.ChecksumStoreIface) map[string]map[string]string {
	v, ok := volumeKeyed(cs)
	if !ok {
		return map[string]map[string]string{"": cs.GetAll()}
	}
	legacy := v.SnapshotLegacy()
	out := map[string]map[string]string{}
	for key, sum := range cs.GetAll() {
		vol, rel, cut := checksum.SplitVolumeKey(key)
		if !cut {
			continue
		}
		if out[vol] == nil {
			out[vol] = make(map[string]string, len(legacy))
			for k, val := range legacy {
				out[vol][k] = val
			}
		}
		out[vol][rel] = sum
	}
	if len(out) == 0 {
		out[""] = legacy
	}
	return out
}

// checksumRename 同卷重命名卷维度键（不支持卷维度时回落旧方法）。
func checksumRename(cs checksum.ChecksumStoreIface, vol, from, to string) {
	if v, ok := volumeKeyed(cs); ok && vol != "" {
		v.RenameInVolume(vol, from, to)
		return
	}
	cs.Rename(from, to)
}
