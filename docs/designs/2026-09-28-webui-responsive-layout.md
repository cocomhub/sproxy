<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# WebUI 响应式布局与容器解放（roadmap 13.1 / V2）设计

> 日期：2026-09-28 ｜ 状态：头脑风暴（待评审） ｜ 范围：Web UI `index.html`/`style.css` 从「固定窄容器 + 无响应式断点」升级为「自适应 + 触屏友好」
> 关联：[2026-09-28-webui-interactive-topology.md](./2026-09-28-webui-interactive-topology.md)（V1，本文件 V2，均属 13.1 WebUI 易用性系列）。

## 1. 背景与目标

### 1.1 现状（源码实证，web/static/）

- `style.css:132`：`.container { max-width: 1000px; margin: 0 auto; padding: 24px; }` —— **固定 1000px 窄容器**；
- `@media` 仅 4 处，**全部是 `prefers-color-scheme: dark`**（暗色主题），**没有任何响应式断点**；
- 主工具行（index.html:16-47）一排按钮 + 搜索框堆在 1000px 内，**大屏浪费空间、中屏需换行、小屏/触屏不可用**；
- 工具栏按钮/输入框全部内联 `style` 固定 px，无统一间距体系。

### 1.2 目标

- **大屏解放**：容器自适应 `min(1400px, 96vw)`，充分使用宽屏；
- **响应式断点**：≥1280（宽屏三档工具栏） / 768-1279（紧凑折行） / <768（竖屏折叠 + 触控友好）；
- **触屏友好**：按钮最小触摸目标 ≥44px、表格横向滚动容器、输入框适配；
- 无 JS 依赖（纯 CSS media），渐进增强。

### 1.3 非目标

- 不做真·移动原生 App；不做 JS 断点驱动（保留纯 CSS）；
- 不改服务端渲染结构（不引框架）。

## 2. 组件与接口

### 2.1 CSS 变量与断点（style.css）

```
:root { --container-max: 1000px; }
@media (min-width: 1280px) { .container { max-width: min(1400px, 96vw); } }
@media (max-width: 767px) {
  .toolbar 折叠成竖向；#main-tab-bar 横向可滚动；
  按钮 min-height:44px；表格容器 overflow-x:auto
}
```

### 2.2 结构配合（index.html 微调）

- 工具行包装成 `.toolbar-group`（flex-wrap）便于断点控制；
- 文件表格外包 `.table-scroll`（宽度 100% + 横向滚动兜底 `max-height`）；
- 主 tab 栏在 <768 改为横向 scrollable strip。

## 3. 错误处理

- 依赖纯 CSS，无运行时错误；
- 触屏 + 桌面并存：hover-only 交互（tooltip/详情）在触屏禁用 fallback。

## 4. 测试 + 变异点

| 用例 | 断言 | 变异 → 应红 |
|------|------|------------|
| Playwright：视口 1920 | `.container` 计算宽度 >1200（容器解放生效） | max-width 恒 1000 → 红 |
| Playwright：视口 600 | 工具栏折行/纵向、无横向溢出 | 不断点 → 溢出红 |
| node:test：无（纯 CSS） | （由 e2e 覆盖） | |
| 触屏目标 | 断点下按钮 min-height≥44 | 删断点 → e2e 触摸断言红 |

## 5. 片划分

- **P1**：容器放大 + ≥1280 / <768 两档断点 + 工具栏/表格触控适配（含 e2e）
- **P2**：768-1279 中间档精细化、主 tab 横向滚动条、暗色主题下断点微调

## 6. 风险与零回归

- 默认 `.container` 1000px 保留（小屏零回归）；放大仅在 ≥1280 视口发生；
- 不引框架、不混 JS；全 e2e 用 Playwright 真浏览器（R10/R18 覆盖）；
- 与 V1 拓扑独立（可并行）。