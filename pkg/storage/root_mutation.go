// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// root_mutation.go 定义 `storage.Root` 的**结构性变异事件**（设计 2026-10-28-volume-state-maintenance §4.1）。
//
// 为什么需要它：本地卷的写内核直接用 `storage.Root`（不经 `sync.FS`），因此「卷内容变了」这件事
// 只能在 Root 层观察。把观察点收敛到 Root ⇒ 任何新写路径都自动被观察到，**不需要逐个登记**
// （#874「旁路写忘记维护派生状态」的根因就此消失）。
//
// 边界（诚实记录）：
//   - 结构性变异（目录创建、删除、重命名、链接、时间戳）**自动**发事件；
//   - 文件**内容**写入无法自动观察：`OpenFile` 返回 `*os.File`（签名被 30+ 调用点依赖），
//     包装会破坏类型契约。故提供显式入口 `Root.MarkWritten(rel)`，由**写面适配器**在写成功后
//     调用——目前唯一调用点是 `pkg/rootfs` 的 `WriteFile`（卷的 `sync.FS` 写面，覆盖转存/归档/
//     镜像/备份等全部旁路写）。模版文件 + `AtomicRename` 落位的路径两条事件都会发（幂等）。
//   - 事件**不带锁、不阻塞**：回调在底层 os 操作成功返回后同步调用，实现方必须快速返回
//     （派生状态维护只做内存操作 + 一次 Stat）。
package storage

import (
	"path"
	"strings"
)

// MutationOp 是结构性变异操作类型。
type MutationOp string

const (
	// MutationMkdir 目录（含多级）创建成功。
	MutationMkdir MutationOp = "mkdir"
	// MutationRemove 单个文件/空目录删除成功。
	MutationRemove MutationOp = "remove"
	// MutationRemoveAll 递归删除成功。
	MutationRemoveAll MutationOp = "remove_all"
	// MutationRename 重命名/移动成功（Rel=源，Rel2=目标）。
	MutationRename MutationOp = "rename"
	// MutationLink 硬链接成功（Rel=源，Rel2=新链接）。
	MutationLink MutationOp = "link"
	// MutationChtimes 时间戳修改成功。
	MutationChtimes MutationOp = "chtimes"
	// MutationWrite 文件**内容**写入完成（由写入口显式调用 `MarkWritten`，见下方边界说明）。
	MutationWrite MutationOp = "write"
)

// Mutation 是一次结构性变异的描述。
//
// 路径口径统一为**卷根命名空间**：租户子根（`OpenTenant`）上的变异会被自动加上
// `<owner>/` 前缀，`Root` 恒指向卷根（消费方可直接用 `Root.Stat(m.Rel)`）。
type Mutation struct {
	Op MutationOp
	// Tag 是装配层传入的标识（生产为卷名），供消费方区分卷而不必反查。
	Tag string
	// Root 是**卷根**（不是发生变异的子根）。
	Root *Root
	// Rel 是卷根命名空间下的变异路径（斜杠分隔）。
	Rel string
	// Rel2 仅 rename/link 使用（目标路径）。
	Rel2 string
}

// mutationObserver 是不可变的观察者载荷（tag + 回调 + 前缀 + 卷根）。
// 通过 atomic.Pointer 整体换指针 ⇒ 装配期的写入与运行期读无撕裂。
type mutationObserver struct {
	tag    string
	fn     func(Mutation)
	prefix string
	base   *Root
}

// SetMutationObserver 装配观察者（**构造期调用**：对象发布后不得再改）。
// tag 为装配层标识（生产传卷名）；fn 为 nil 时等价于关闭。
func (rt *Root) SetMutationObserver(tag string, fn func(Mutation)) {
	if fn == nil {
		rt.mutObs.Store(nil)
		return
	}
	rt.mutObs.Store(&mutationObserver{tag: tag, fn: fn, base: rt})
}

// inheritMutationObserver 让子根（租户）继承父根观察者并**归一前缀**：
// 子根内的 rel 在消费方看来是 `<owner>/<rel>`（卷根命名空间）。
// 未装观察者 ⇒ 不做任何事（零回归）。
func (rt *Root) inheritMutationObserver(parent *Root, segment string) {
	obs := parent.mutObs.Load()
	if obs == nil {
		return
	}
	prefix := segment
	if obs.prefix != "" {
		prefix = obs.prefix + "/" + segment
	}
	rt.mutObs.Store(&mutationObserver{tag: obs.tag, fn: obs.fn, prefix: prefix, base: obs.base})
}

// MarkWritten 声明「rel 的内容已写入完成」并发一次 `MutationWrite`。
//
// 为什么需要显式调用：内容写入走 `OpenFile`/`OpenFileEncrypted` 返回的裸文件句柄，Root 无法在
// 关闭时观察（见包注释的边界说明）。**写面适配器**（`pkg/rootfs` 的 WriteFile）在写成功后调用
// 本方法 ⇒ 派生状态（搜索索引）跟随内容写更新，无需每个业务调用点登记。
// 未装观察者时零开销。
func (rt *Root) MarkWritten(rel string) {
	rt.notifyMutation(MutationWrite, rel, "")
}

// notifyMutation 发放一次变异事件（底层操作**成功后**调用）。
// 未装观察者时零开销；回调在调用方 goroutine 同步执行。
func (rt *Root) notifyMutation(op MutationOp, rel, rel2 string) {
	obs := rt.mutObs.Load()
	if obs == nil {
		return
	}
	if rel2 != "" {
		rel2 = obs.join(rel2)
	}
	obs.fn(Mutation{Op: op, Tag: obs.tag, Root: obs.base, Rel: obs.join(rel), Rel2: rel2})
}

// join 把子根相对路径翻译为卷根命名空间路径。
func (o *mutationObserver) join(rel string) string {
	rel = strings.TrimPrefix(rel, "./")
	if o.prefix == "" {
		return rel
	}
	if rel == "" || rel == "." {
		return o.prefix
	}
	return path.Join(o.prefix, rel)
}
