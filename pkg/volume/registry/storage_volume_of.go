// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"github.com/cocomhub/sproxy/pkg/volume"
)

// storageVolumeOf 把「外部后端 + 已知卷名」提升为**统一存储卷视图**。
//
// 与 `pkg/volume/capacity.storageVolumeOf`（capacity 内部）同形——因 registry 不能反向
// import capacity（成环），此处持有一份最小提升（U1：NewSet 把外部卷句柄同步进
// `storageVolumes` 用）。外部卷句柄若是 `capacity.Backend`（已实现 StorageVolume 四方法）
// 可原样放入；缺 `Kind` 的裸 ExternalBackend 经本类型补全卷名与 KindStorage。
type storageVolumeOf struct {
	ExternalBackend
	name string
}

func (s storageVolumeOf) Name() string      { return s.name }
func (s storageVolumeOf) Kind() volume.Kind { return volume.KindStorage }
