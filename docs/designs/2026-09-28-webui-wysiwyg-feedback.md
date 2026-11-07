<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# WebUI 所见即所得反馈体系（roadmap 13.1 / V6）设计

> 日期：2026-09-28 ｜ 状态：头脑风暴（待评审） ｜ 范围：危险操作分级确认、操作成功定位、骨架屏加载、空态引导——统一反馈体验
> 关联：[2026-09-28-webui-dialog-component.md](./2026-09-28-webui-dialog-component.md)（V4 对话框）、[2026-09-28-webui-virtual-scroll-table.md](./2026-09-28-webui-virtual-scroll-table.md)（V3 表格）。

## 1. 背景与目标

### 1.1 现状（源码实证，app.js）

- 危险操作（删除/清空/批量）全部同一 `confirm()`（无分级）；
- 操作成功仅 toast（无「定位到结果」——删除后停留原目录，重命名后不高亮新名）；
- 列表加载整页「加载中...」文本（无骨架屏，感知慢）；
- 空态仅「暂无文件」（无引导文案/操作建议）；
- 无「操作撤销」能力（删除/重命名后不可回滚）。

### 1.2 目标

- **危险分级**：删除/清空/批量 = danger 红色确认 + 强调「不可撤销」；普通操作 = 默认确认；
- **操作成功定位**：重命名 → 高亮新文件名；上传完成 → 定位新文件；删除 → 停留原目录 + 显示剩余条数；
- **骨架屏加载**：列表加载时渲染行级骨架（灰条），避免「加载中...」白闪；
- **空态引导**：空目录 → 引导文案（「拖拽文件到这里上传」+ 新建目录按钮）；空搜索 → 「无匹配，清除搜索」；
- **可撤销（轻量）**：删除/重命名后 toast 附「撤销」按钮（3-5s 窗口，复用回收站软删）。

### 1.3 非目标

- 不做全局 Undo 栈（只做单操作快速撤销）；
- 不做复杂交互动画框架（CSS transition 即可）。

## 2. 组件与接口

### 2.1 纯逻辑层（node:test 直测，app-render.js）

```js
// 危险分级映射：action → {danger, desc, undoable}
dangerProfile(action) -> {danger: bool, desc: string, undoable: bool}
// 骨架屏行：count → HTML（固定行高/随机灰宽）
buildSkeletonRows(count)
// 空态：context（dir/搜索/trash）→ 引导 HTML
buildEmptyState(context, hasQuery)
// 撤销窗口：actions 映射（delete→softDelete, rename→undoRename）
undoActionFor(action) -> apiCall | null
```

### 2.2 交互层（app.js）

```
危险操作 → showConfirm(dangerProfile(action))
成功 → toast(action 文案) + 定位（scrollIntoView/高亮 .highlight-new）
加载 → skeleton；空态 → buildEmptyState
可撤销 → toast 尾部「撤销」按钮 → 3-5s 内调用 undoActionFor
```

## 3. 错误处理

- 撤销超时/失败 → toast 错误 + 不破坏已变更状态；
- 定位目标已不存在（并发删除）→ 回退刷新列表；
- 骨架屏与虚拟滚动（V3）兼容——骨架只在前几帧/初始加载用。

## 4. 测试 + 变异点

| 用例 | 断言 | 变异 → 应红 |
|------|------|------------|
| dangerProfile | delete→danger/undoable；mkdir→非 danger | 分级错 → 红 |
| buildSkeletonRows | 行数/class 正确 | 无 class → 红 |
| buildEmptyState | 不同 context 文案不同 | 文案同 → 红 |
| undoActionFor | delete→softDelete、rename→undo 映射 | 映射错 → 红 |
| Playwright 重命名定位 | 重命名后新名高亮 | 不高亮 → e2e 红 |
| Playwright 撤销 | 删除后 5s 内点撤销恢复 | 无撤销 → e2e 红 |

## 5. 片划分

- **P1**：dangerProfile + 骨架屏 + 空态引导 + 操作定位（含 node test + Playwright）
- **P2**：撤销窗口（软删复用回收站）、危险操作震感/颜色增强、动画打磨

## 6. 风险与零回归

- 全部新行为为增强（原有 toast/confirm 语义保留）；撤销仅对「软删/重命名」轻量实现（不破坏既有删除语义）；
- 与 V3/V4/V5 独立可并行；全 e2e 真浏览器（R10/R18）。