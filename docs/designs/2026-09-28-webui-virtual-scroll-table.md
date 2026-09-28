<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# WebUI 表格虚拟滚动 + 固定表头/首列（roadmap 13.1 / V3）设计

> 日期：2026-09-28 ｜ 状态：头脑风暴（待评审） ｜ 范围：文件列表从「整页 innerHTML + 加载更多分页」升级为「虚拟滚动 + sticky 表头/文件名列」
> 关联：[2026-09-28-webui-responsive-layout.md](./2026-09-28-webui-responsive-layout.md)（V2 容器解放）、[2026-09-28-webui-wysiwyg-feedback.md](./2026-09-28-webui-wysiwyg-feedback.md)（V6 反馈）。

## 1. 背景与目标

### 1.1 现状（源码实证，app.js:220-240 + app-render.js:97-137）

- `refreshList()` 整页 `el.innerHTML = buildFileTableHtml(files)`（全部行一次性渲染）；
- `loadMore()` 用「加载更多」分页（`PAGE_LIMIT` 逐页 innerHTML 追加）——**大目录（数千文件）整表全量 DOM，滚动卡顿**；
- 表格无固定表头（滚动时表头滚走）、无固定首列（文件名长时横向滚动丢列头）；
- 行内 5 个操作按钮全部展开（下载/预览/删除/重命名/分享），窄屏横向溢出。

### 1.2 目标

- **虚拟滚动**（windowed rendering）：只渲染视口内行（~30 行），滚动事件换行——大目录 10k 行内存/渲染 O(视口)；
- **固定表头 + 首列**：滚动时表头 sticky、文件名首列 sticky（水平滚动不丢）；
- 兼容既有分页 API（服务端 `/api/files?offset&limit` 不动）——虚拟滚动 = 客户端「无限滚动」分页加载（滚动到底自动请求下页）；
- 操作按钮窄屏收进「⋯」溢出菜单（保可用性）。

### 1.3 非目标

- 不改服务端分页协议；不做服务端搜索分页游标（现有 offset/limit 已够）；
- 不做复杂表格排序/列自定义（后续片）。

## 2. 组件与接口

### 2.1 纯逻辑层（node:test 直测，app-render.js）

```js
// 虚拟滚动核心：给定 total 行、viewH、rowH → 需要渲染的行区间 [start, end]
computeVirtualRange(scrollTop, viewH, rowH, total) -> {start, end, padTop, padBottom}
// 行数据窗口：从已加载数组切出 [start, end] 对应行
sliceRows(rows, start, end)
```

### 2.2 渲染层（app.js）

```
<table class="vtable">（容器 overflow:auto; max-height:60vh）
  thead（sticky top:0）
  第一个 th/td（sticky left:0 + 背景遮底）
  tbody 高度 = total*rowH（撑起滚动条）
    .vrow 只渲染 [start,end]
  滚动到底 → 自动 loadMore()（无限滚动，兼容既有 API）
```

## 3. 错误处理

- 滚动区间计算越界（total 变化）→ clamp 到 [0,total]；
- loadMore 并发保护（in-flight 标志防重复请求）；
- 服务端返回空/出错 → 既有 toast + 保持已渲染行。

## 4. 测试 + 变异点

| 用例 | 断言 | 变异 → 应红 |
|------|------|------------|
| computeVirtualRange | 视口外 padTop/padBottom 正确、区间 ≤ 视口行数+1 | 区间全量 → 大列表红 |
| sliceRows | 切片与 total 一致 | 偏移错 → 红 |
| 无限滚动（Playwright） | 滚到底触发 loadMore，DOM 行数有界（≤2×视口） | 全量渲染 → e2e 红 |
| sticky | 滚动后表头可见、首列可见 | 删 sticky → e2e 红 |
| 溢出菜单（窄屏） | 操作收进 ⋯ 菜单仍可点 | 无菜单 → e2e 红 |

## 5. 片划分

- **P1**：computeVirtualRange + 虚拟渲染 + sticky（含 node test + Playwright）
- **P2**：无限滚动（loadMore 接入）、窄屏溢出菜单、滚动位置记忆（翻页/重命名后恢复）

## 6. 风险与零回归

- 服务端 API 零改动（沿用 offset/limit）；默认行高常量 → 可配置；
- 旧「加载更多」按钮兼容（虚拟滚动时隐藏）；触屏滚动（overflow-y:auto 原生支持）；
- 与 V2 容器解放、V6 反馈体系独立可并行。