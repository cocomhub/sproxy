// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"

	"github.com/cocomhub/sproxy/pkg/server/cloudtasks"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// cloudTaskDeps 组装 cloudtasks 的窄依赖。适配器是**无状态薄委托**（逐调用读取
// *Handlers 的当前装配态），因此构造开销可忽略，也不必在 *Handlers 上加字段
// （R7 的分域 deps 预算只有 12 个嵌入分组）。
func (h *Handlers) cloudTaskDeps() cloudtasks.Deps {
	a := &cloudTasksAdapter{h: h}
	return cloudtasks.Deps{
		Tasks:   a,
		Groups:  a,
		Tenants: a,
		Volumes: a,
		Quota:   a,
		Targets: a,
		Audit:   a,
		Logger:  h.logger,
	}
}

// cloudTaskService 构造云任务编排核心（**单实例**，懒构造后并发复用）。
//
// 单实例是 S3c 的语义要求：归档桶占用登记（archiveUsage）已随转存核心迁入
// cloudtasks.Service，记录（单任务/批量/组归档）与删除（组删除联动）必须同实例才能
// 「按登记释放」（否则退化为按磁盘 stat，文件已被外部删除时释放量为 0）。
func (h *Handlers) cloudTaskService() *cloudtasks.Service {
	h.cloudServiceOnce.Do(func() {
		h.cloudService = cloudtasks.New(h.cloudTaskDeps())
	})
	return h.cloudService
}

// cloudTaskHandler 把编排核心的 HTTP 入口适配成 http.HandlerFunc：owner（任务归属与
// 可见性）在此从认证上下文解析——`ActorFrom` 的 ctx key 是装配层私有，核心按设计
// §4/§9.5 **显式接收 owner**（docs/designs/2026-10-31-cloudtasks-deps-mapping.md：
// 「不要把 ctx→actor 塞进领域包」）。
func (h *Handlers) cloudTaskHandler(fn func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fn(w, r, ActorFrom(r.Context()))
	}
}

// deleteGroupArchive / deleteCloudArchive 是装配层对 core 删除面的**既有调用入口**
// （pkg/server 的组删除联动测试与手工构造夹具直调）；实现已迁入 cloudtasks（P1-c S3c），
// 此处只保留一行委托，避免同一语义出现第二份实现。
func (h *Handlers) deleteGroupArchive(owner, archiveFile string) error {
	return h.cloudTaskService().DeleteGroupArchive(owner, archiveFile)
}

func (h *Handlers) deleteCloudArchive(owner, archiveName string) error {
	return h.cloudTaskService().DeleteCloudArchive(owner, archiveName)
}

// recordBypassWrite 是 cloud 下载转存产物的派生状态登记钩子（routes_setup 注入 cloud
// manager 的 RecordBypassWrite）；实现已迁入 cloudtasks（Service.RecordBypassWrite），
// 与归档转存同源，避免两处「本地卷只补 checksum / 外部卷走整写登记」分流漂移。
func (h *Handlers) recordBypassWrite(owner, rel string, size int64, checksum, volume string, root *storage.Root) {
	h.cloudTaskService().RecordBypassWrite(owner, rel, size, checksum, volume, root)
}
